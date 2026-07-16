package data

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

func capacityTestCredential() CredentialIdentity {
	return CredentialIdentity{
		Gen: 7, JTI: "j7", InstanceUID: "uid-A", ProtocolEpoch: 1,
		TokenSHA256: "sha-j7", Kid: "kid-1", WriterEpoch: testWriterEpoch,
	}
}

func capacityTestReservation(playerID uint64, assignmentID string, nowMs int64) ReservationIdentity {
	return ReservationIdentity{
		PlayerID: playerID, AssignmentID: assignmentID, InstanceUID: "uid-A",
		ProtocolEpoch: 1, WriterEpoch: testWriterEpoch,
		ExpiresAtMs:           nowMs + (2 * time.Hour).Milliseconds(),
		AssignmentExpiresAtMs: nowMs + (3 * time.Hour).Milliseconds(),
	}
}

func capacityTestPlacedReservation(playerID uint64, assignmentID string, nowMs int64) ReservationIdentity {
	reservation := capacityTestReservation(playerID, assignmentID, nowMs)
	reservation.PlacementVersion = 7
	reservation.PlacementOperationID = "11111111-1111-4111-8111-111111111111"
	reservation.SourceMatchID = 9001
	return reservation
}

func mustReserveCapacity(t *testing.T, repo *RedisHubAuthRepo, pod string,
	reservation ReservationIdentity, nowMs int64) ReserveResult {
	t.Helper()
	result, err := repo.ReserveAssignment(context.Background(), pod, reservation, nowMs, 30_000, testTTL)
	if err != nil || !result.OK {
		t.Fatalf("reserve result=%+v err=%v", result, err)
	}
	return result
}

func TestCapacityLedgerReservationSurvivesZeroHeartbeatsUntilAbsoluteExpiry(t *testing.T) {
	ctx := context.Background()
	repo, mr := newAuthRepo(t)
	const pod = "pandora-hub-capacity-reservation"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	reservation := capacityTestReservation(101, uuid.NewString(), now)
	mustReserveCapacity(t, repo, pod, reservation, now)

	// reservation HASH/ZSET retention 取最晚单项绝对到期+guard，不能被较短 shardTTL 截断。
	if ttl := mr.TTL(reservationsKey(pod)); ttl <= testTTL {
		t.Fatalf("reservation key ttl=%s must outlive shard ttl=%s", ttl, testTTL)
	}
	for i := 0; i < 3; i++ {
		if _, err := repo.ActivateHeartbeat(ctx, pod, capacityTestCredential(),
			activateInput(0, "ready", now+int64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 1 || shard.GetReservedCount() != 1 ||
		shard.GetConnectedOwnershipCount() != 0 || shard.GetReportedConnectedCount() != 0 {
		t.Fatalf("zero-player heartbeats deleted reservation or corrupted projection: %+v", shard)
	}
	if count, _ := repo.rdb.HLen(ctx, reservationsKey(pod)).Result(); count != 1 {
		t.Fatalf("reservation disappeared after heartbeat: count=%d", count)
	}

	// 直接把单项绝对期限推进到过去，下一次合法 heartbeat 才可机械回收。
	raw, err := repo.rdb.HGet(ctx, reservationsKey(pod), reservation.AssignmentID).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	record := &hubv1.HubReservationStorageRecord{}
	if err := proto.Unmarshal(raw, record); err != nil {
		t.Fatal(err)
	}
	record.ExpiresAtMs = time.Now().UnixMilli() - 1
	raw, _ = proto.Marshal(record)
	if err := repo.rdb.HSet(ctx, reservationsKey(pod), reservation.AssignmentID, raw).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ActivateHeartbeat(ctx, pod, capacityTestCredential(), activateInput(0, "ready", now+10)); err != nil {
		t.Fatal(err)
	}
	shard, _, _ = shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 0 || shard.GetReservedCount() != 0 ||
		shard.GetConnectedOwnershipCount() != 0 {
		t.Fatalf("expired reservation was not reclaimed: %+v", shard)
	}
}

func TestAdmissionSequenceResponseLossDelayedOldAckAndLogout(t *testing.T) {
	ctx := context.Background()
	repo, mr := newAuthRepo(t)
	const pod = "pandora-hub-admission-sequence"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	reservation := capacityTestReservation(101, uuid.NewString(), now)
	mustReserveCapacity(t, repo, pod, reservation, now)
	credential := capacityTestCredential()
	oldAdmissionID, newAdmissionID := uuid.NewString(), uuid.NewString()

	first, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation, oldAdmissionID, 10, now+1, testTTL)
	if err != nil || !first.Admitted || first.AlreadyAdmitted || first.CapacityOccupancy != 1 {
		t.Fatalf("first admission=%+v err=%v", first, err)
	}
	retry, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation, oldAdmissionID, 10, now+2, testTTL)
	if err != nil || !retry.Admitted || !retry.AlreadyAdmitted || retry.CapacityOccupancy != 1 {
		t.Fatalf("response-loss retry=%+v err=%v", retry, err)
	}
	// Every new physical owner must consume a successor prepared by the ticket
	// issuance path; a live old session alone is not an admission capability.
	mustReserveCapacity(t, repo, pod, reservation, now+2)
	replacement, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation, newAdmissionID, 20, now+3, testTTL)
	if err != nil || !replacement.Admitted || replacement.CapacityOccupancy != 1 {
		t.Fatalf("new connection replacement=%+v err=%v", replacement, err)
	}

	sessionBefore, _ := repo.rdb.HGet(ctx, sessionsKey(pod), reservation.AssignmentID).Bytes()
	shardBefore, _ := repo.rdb.Get(ctx, shardKey(pod)).Bytes()
	shardTTLBefore := mr.TTL(shardKey(pod))
	if delayed, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation,
		oldAdmissionID, 10, now+4, testTTL); err != nil || !delayed.Conflict || delayed.Admitted {
		t.Fatalf("delayed old admission=%+v err=%v", delayed, err)
	}
	if got, _ := repo.rdb.HGet(ctx, sessionsKey(pod), reservation.AssignmentID).Bytes(); !bytes.Equal(got, sessionBefore) {
		t.Fatal("delayed old admission mutated the new owner")
	}
	if oldLogout, err := repo.AcknowledgeDeparture(ctx, pod, credential, reservation,
		oldAdmissionID, 10, now+5, testTTL); err != nil || !oldLogout.Conflict || oldLogout.Departed {
		t.Fatalf("old logout=%+v err=%v", oldLogout, err)
	}
	if got, _ := repo.rdb.HGet(ctx, sessionsKey(pod), reservation.AssignmentID).Bytes(); !bytes.Equal(got, sessionBefore) {
		t.Fatal("old logout deleted or rewrote the new owner")
	}
	if got, _ := repo.rdb.Get(ctx, shardKey(pod)).Bytes(); !bytes.Equal(got, shardBefore) ||
		mr.TTL(shardKey(pod)) != shardTTLBefore {
		t.Fatal("conflict path mutated shard bytes or refreshed ttl")
	}

	departed, err := repo.AcknowledgeDeparture(ctx, pod, credential, reservation,
		newAdmissionID, 20, now+6, testTTL)
	if err != nil || !departed.Departed || departed.Conflict || departed.CapacityOccupancy != 0 {
		t.Fatalf("new owner departure=%+v err=%v", departed, err)
	}
	idempotent, err := repo.AcknowledgeDeparture(ctx, pod, credential, reservation,
		newAdmissionID, 20, now+7, testTTL)
	if err != nil || !idempotent.Departed || !idempotent.AlreadyDeparted || idempotent.Conflict {
		t.Fatalf("departure response-loss retry=%+v err=%v", idempotent, err)
	}
}

func TestSuccessorLeaseOldDepartureBeforeNewAdmission(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-successor-departure-first"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	reservation := capacityTestPlacedReservation(101, uuid.NewString(), now)
	credential := capacityTestCredential()
	oldAdmissionID, newAdmissionID := uuid.NewString(), uuid.NewString()
	mustReserveCapacity(t, repo, pod, reservation, now)
	if admitted, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation,
		oldAdmissionID, 10, now+1, testTTL); err != nil || !admitted.Admitted {
		t.Fatalf("old admission=%+v err=%v", admitted, err)
	}

	// IssueDSTicket while the old owner is still connected prepares one exact
	// successor but must not double-count the physical seat.
	mustReserveCapacity(t, repo, pod, reservation, now+2)
	if count, _ := repo.rdb.HLen(ctx, successorsKey(pod)).Result(); count != 1 {
		t.Fatalf("successor count=%d", count)
	}
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 1 || shard.GetReservedCount() != 0 ||
		shard.GetConnectedOwnershipCount() != 1 {
		t.Fatalf("connected successor double-counted capacity: %+v", shard)
	}

	departed, err := repo.AcknowledgeDeparture(ctx, pod, credential, reservation,
		oldAdmissionID, 10, now+3, testTTL)
	if err != nil || !departed.Departed || departed.Conflict || departed.CapacityOccupancy != 1 ||
		departed.ReservedCount != 1 || departed.ConnectedCount != 0 {
		t.Fatalf("old departure did not atomically expose successor seat: %+v err=%v", departed, err)
	}
	if count, _ := repo.rdb.HLen(ctx, successorsKey(pod)).Result(); count != 1 {
		t.Fatalf("old departure deleted successor count=%d", count)
	}

	// Another placement lineage cannot consume the detached capability.
	wrongPlacement := reservation
	wrongPlacement.PlacementVersion++
	wrongPlacement.PlacementOperationID = "22222222-2222-4222-8222-222222222222"
	if rejected, rejectErr := repo.AcknowledgeAdmission(ctx, pod, credential, wrongPlacement,
		newAdmissionID, 20, now+4, testTTL); errcode.As(rejectErr) != errcode.ErrUnauthorized || rejected.Admitted {
		t.Fatalf("wrong placement admission=%+v err=%v", rejected, rejectErr)
	}

	newAdmission, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation,
		newAdmissionID, 20, now+5, testTTL)
	if err != nil || !newAdmission.Admitted || newAdmission.CapacityOccupancy != 1 ||
		newAdmission.ReservedCount != 0 || newAdmission.ConnectedCount != 1 {
		t.Fatalf("new admission=%+v err=%v", newAdmission, err)
	}
	if count, _ := repo.rdb.HLen(ctx, successorsKey(pod)).Result(); count != 0 {
		t.Fatalf("new admission did not consume successor count=%d", count)
	}

	// A replayed old Logout after the new owner exists is fenced by exact
	// admission_seq+UUID and must not delete the successor owner (ABA).
	oldReplay, err := repo.AcknowledgeDeparture(ctx, pod, credential, reservation,
		oldAdmissionID, 10, now+6, testTTL)
	if err != nil || !oldReplay.Conflict || oldReplay.Departed {
		t.Fatalf("old departure replay=%+v err=%v", oldReplay, err)
	}
}

func TestSuccessorLeaseAdmissionBeforeOldDeparture(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-successor-admission-first"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	reservation := capacityTestPlacedReservation(101, uuid.NewString(), now)
	credential := capacityTestCredential()
	oldAdmissionID, newAdmissionID := uuid.NewString(), uuid.NewString()
	mustReserveCapacity(t, repo, pod, reservation, now)
	if admitted, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation,
		oldAdmissionID, 10, now+1, testTTL); err != nil || !admitted.Admitted {
		t.Fatalf("old admission=%+v err=%v", admitted, err)
	}
	mustReserveCapacity(t, repo, pod, reservation, now+2)
	if admitted, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation,
		newAdmissionID, 20, now+3, testTTL); err != nil || !admitted.Admitted || admitted.CapacityOccupancy != 1 {
		t.Fatalf("new admission=%+v err=%v", admitted, err)
	}
	if oldDeparture, err := repo.AcknowledgeDeparture(ctx, pod, credential, reservation,
		oldAdmissionID, 10, now+4, testTTL); err != nil || !oldDeparture.Conflict || oldDeparture.Departed {
		t.Fatalf("old departure=%+v err=%v", oldDeparture, err)
	}
	if current, err := repo.InspectAssignmentSeat(ctx, pod, AssignmentInstanceIdentity{
		PlayerID: reservation.PlayerID, AssignmentID: reservation.AssignmentID,
		InstanceUID: reservation.InstanceUID, ProtocolEpoch: reservation.ProtocolEpoch,
		WriterEpoch: reservation.WriterEpoch, PlacementVersion: reservation.PlacementVersion,
		PlacementOperationID: reservation.PlacementOperationID, SourceMatchID: reservation.SourceMatchID,
	}); err != nil || !current.Connected || current.AdmissionID != newAdmissionID || current.AdmissionSeq != 20 {
		t.Fatalf("replacement owner=%+v err=%v", current, err)
	}
}

func TestSuccessorLeaseRotatesOnlyToNewerPlacementVersion(t *testing.T) {
	for _, detached := range []bool{false, true} {
		name := "connected"
		if detached {
			name = "detached"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			repo, _ := newAuthRepo(t)
			pod := "pandora-hub-successor-rotate-" + name
			now := time.Now().UnixMilli()
			activateReady(t, repo, pod, now, 0)
			now = time.Now().UnixMilli() + 1
			old := capacityTestPlacedReservation(101, uuid.NewString(), now)
			oldAdmissionID := uuid.NewString()
			mustReserveCapacity(t, repo, pod, old, now)
			if admitted, err := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), old,
				oldAdmissionID, 10, now+1, testTTL); err != nil || !admitted.Admitted {
				t.Fatalf("old admission=%+v err=%v", admitted, err)
			}
			mustReserveCapacity(t, repo, pod, old, now+2)
			// Storage records may contain fields written by a newer binary.  Moving a
			// successor to a newer placement capability must preserve those bytes.
			unknown := []byte{0xa0, 0x06, 0x01} // field 100, varint 1
			rawSuccessors, rawErr := repo.rdb.HGetAll(ctx, successorsKey(pod)).Result()
			if rawErr != nil || len(rawSuccessors) != 1 {
				t.Fatalf("successor before rotation=%d err=%v", len(rawSuccessors), rawErr)
			}
			for field, raw := range rawSuccessors {
				record := &hubv1.HubReservationStorageRecord{}
				if err := proto.Unmarshal([]byte(raw), record); err != nil {
					t.Fatal(err)
				}
				record.ProtoReflect().SetUnknown(unknown)
				payload, err := proto.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				if err := repo.rdb.HSet(ctx, successorsKey(pod), field, payload).Err(); err != nil {
					t.Fatal(err)
				}
			}
			if detached {
				if departed, err := repo.AcknowledgeDeparture(ctx, pod, capacityTestCredential(), old,
					oldAdmissionID, 10, now+3, testTTL); err != nil || !departed.Departed {
					t.Fatalf("old departure=%+v err=%v", departed, err)
				}
			}

			fork := old
			fork.PlacementOperationID = "22222222-2222-4222-8222-222222222222"
			if result, err := repo.ReserveAssignment(ctx, pod, fork, now+4, 30_000, testTTL); err != nil ||
				result.OK || result.Reason != "assignment-successor-conflict" {
				t.Fatalf("same-version fork=%+v err=%v", result, err)
			}
			newer := old
			newer.PlacementVersion++
			newer.PlacementOperationID = "33333333-3333-4333-8333-333333333333"
			if result, err := repo.ReserveAssignment(ctx, pod, newer, now+5, 30_000, testTTL); err != nil || !result.OK {
				t.Fatalf("newer successor=%+v err=%v", result, err)
			}
			if count, _ := repo.rdb.HLen(ctx, successorsKey(pod)).Result(); count != 1 {
				t.Fatalf("rotated successor cardinality=%d", count)
			}
			expectedField, err := successorCapability(pod, newer)
			if err != nil {
				t.Fatal(err)
			}
			rotatedRaw, err := repo.rdb.HGet(ctx, successorsKey(pod), expectedField).Bytes()
			if err != nil {
				t.Fatalf("rotated successor capability missing: %v", err)
			}
			rotatedRecord := &hubv1.HubReservationStorageRecord{}
			if err := proto.Unmarshal(rotatedRaw, rotatedRecord); err != nil {
				t.Fatal(err)
			}
			if got := rotatedRecord.ProtoReflect().GetUnknown(); !bytes.Equal(got, unknown) {
				t.Fatalf("rotation dropped unknown fields: got=%x want=%x", got, unknown)
			}
			if stale, staleErr := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), old,
				uuid.NewString(), 20, now+6, testTTL); errcode.As(staleErr) != errcode.ErrUnauthorized || stale.Admitted {
				t.Fatalf("stale placement admission=%+v err=%v", stale, staleErr)
			}
			if admitted, err := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), newer,
				uuid.NewString(), 20, now+7, testTTL); err != nil || !admitted.Admitted || admitted.CapacityOccupancy != 1 {
				t.Fatalf("newer placement admission=%+v err=%v", admitted, err)
			}
		})
	}
}

func TestSuccessorLeaseExpiryAndReleaseCancellation(t *testing.T) {
	t.Run("expiry-does-not-delete-live-owner", func(t *testing.T) {
		ctx := context.Background()
		repo, _ := newAuthRepo(t)
		const pod = "pandora-hub-successor-expiry"
		now := time.Now().UnixMilli()
		activateReady(t, repo, pod, now, 0)
		now = time.Now().UnixMilli() + 1
		reservation := capacityTestPlacedReservation(101, uuid.NewString(), now)
		oldAdmissionID := uuid.NewString()
		mustReserveCapacity(t, repo, pod, reservation, now)
		if admitted, err := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), reservation,
			oldAdmissionID, 10, now+1, testTTL); err != nil || !admitted.Admitted {
			t.Fatalf("old admission=%+v err=%v", admitted, err)
		}
		mustReserveCapacity(t, repo, pod, reservation, now+2)
		rawSuccessors, err := repo.rdb.HGetAll(ctx, successorsKey(pod)).Result()
		if err != nil || len(rawSuccessors) != 1 {
			t.Fatalf("successors=%d err=%v", len(rawSuccessors), err)
		}
		for field, raw := range rawSuccessors {
			record := &hubv1.HubReservationStorageRecord{}
			if err := proto.Unmarshal([]byte(raw), record); err != nil {
				t.Fatal(err)
			}
			record.ExpiresAtMs = time.Now().UnixMilli() - 1
			payload, _ := proto.Marshal(record)
			if err := repo.rdb.HSet(ctx, successorsKey(pod), field, payload).Err(); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := repo.ActivateHeartbeat(ctx, pod, capacityTestCredential(),
			activateInput(0, "ready", now+3)); err != nil {
			t.Fatal(err)
		}
		if count, _ := repo.rdb.HLen(ctx, successorsKey(pod)).Result(); count != 0 {
			t.Fatalf("expired successor count=%d", count)
		}
		shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
		if shard.GetPlayerCount() != 1 || shard.GetConnectedOwnershipCount() != 1 || shard.GetReservedCount() != 0 {
			t.Fatalf("successor expiry deleted live owner: %+v", shard)
		}
		if admission, admissionErr := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), reservation,
			uuid.NewString(), 20, now+4, testTTL); errcode.As(admissionErr) != errcode.ErrUnauthorized || admission.Admitted {
			t.Fatalf("admission without successor=%+v err=%v", admission, admissionErr)
		}
	})

	for _, departureFirst := range []bool{false, true} {
		name := "connected"
		if departureFirst {
			name = "detached"
		}
		t.Run("release-cancels-"+name+"-successor-without-lineage", func(t *testing.T) {
			ctx := context.Background()
			repo, _ := newAuthRepo(t)
			pod := "pandora-hub-successor-release-" + name
			now := time.Now().UnixMilli()
			activateReady(t, repo, pod, now, 0)
			now = time.Now().UnixMilli() + 1
			reservation := capacityTestPlacedReservation(101, uuid.NewString(), now)
			admissionID := uuid.NewString()
			mustReserveCapacity(t, repo, pod, reservation, now)
			if admitted, err := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), reservation,
				admissionID, 10, now+1, testTTL); err != nil || !admitted.Admitted {
				t.Fatalf("old admission=%+v err=%v", admitted, err)
			}
			mustReserveCapacity(t, repo, pod, reservation, now+2)
			if departureFirst {
				if departed, err := repo.AcknowledgeDeparture(ctx, pod, capacityTestCredential(), reservation,
					admissionID, 10, now+3, testTTL); err != nil || !departed.Departed {
					t.Fatalf("old departure=%+v err=%v", departed, err)
				}
			}
			// Deliberately omit placement lineage: rolling durable cleanup records
			// may only retain the exact assignment/base instance owner.
			expected := AssignmentInstanceIdentity{PlayerID: reservation.PlayerID,
				AssignmentID: reservation.AssignmentID, InstanceUID: reservation.InstanceUID,
				ProtocolEpoch: reservation.ProtocolEpoch, WriterEpoch: reservation.WriterEpoch}
			result, err := repo.ReleaseAssignmentSeatExact(ctx, pod, expected, testTTL)
			if departureFirst {
				if err != nil || !result.Released || result.DepartureRequired || result.Conflict {
					t.Fatalf("detached release=%+v err=%v", result, err)
				}
			} else if err != nil || !result.DepartureRequired || result.Released || result.Conflict {
				t.Fatalf("connected release=%+v err=%v", result, err)
			}
			if count, _ := repo.rdb.HLen(ctx, successorsKey(pod)).Result(); count != 0 {
				t.Fatalf("release did not cancel successor count=%d", count)
			}
			if !departureFirst {
				if departed, err := repo.AcknowledgeDeparture(ctx, pod, capacityTestCredential(), reservation,
					admissionID, 10, now+4, testTTL); err != nil || !departed.Departed || departed.CapacityOccupancy != 0 {
					t.Fatalf("post-cancel physical departure=%+v err=%v", departed, err)
				}
			}
		})
	}
}

func TestSuccessorCleanupRejectsDuplicateCapabilities(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-successor-duplicate"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	reservation := capacityTestPlacedReservation(101, uuid.NewString(), now)
	mustReserveCapacity(t, repo, pod, reservation, now)
	if admitted, err := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), reservation,
		uuid.NewString(), 10, now+1, testTTL); err != nil || !admitted.Admitted {
		t.Fatalf("old admission=%+v err=%v", admitted, err)
	}
	mustReserveCapacity(t, repo, pod, reservation, now+2)
	rawSuccessors, _ := repo.rdb.HGetAll(ctx, successorsKey(pod)).Result()
	var raw string
	for _, value := range rawSuccessors {
		raw = value
	}
	other := reservation
	other.PlacementVersion++
	other.PlacementOperationID = "22222222-2222-4222-8222-222222222222"
	otherField, err := successorCapability(pod, other)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.rdb.HSet(ctx, successorsKey(pod), otherField, raw).Err(); err != nil {
		t.Fatal(err)
	}
	expected := AssignmentInstanceIdentity{PlayerID: reservation.PlayerID,
		AssignmentID: reservation.AssignmentID, InstanceUID: reservation.InstanceUID,
		ProtocolEpoch: reservation.ProtocolEpoch, WriterEpoch: reservation.WriterEpoch}
	if snapshot, err := repo.InspectAssignmentSeat(ctx, pod, expected); err != nil || !snapshot.Conflict {
		t.Fatalf("duplicate inspect=%+v err=%v", snapshot, err)
	}
	if released, err := repo.ReleaseAssignmentSeatExact(ctx, pod, expected, testTTL); err != nil ||
		!released.Conflict || released.Released || released.DepartureRequired {
		t.Fatalf("duplicate release=%+v err=%v", released, err)
	}
	if count, _ := repo.rdb.HLen(ctx, successorsKey(pod)).Result(); count != 2 {
		t.Fatalf("conflict mutated duplicate successors count=%d", count)
	}
}

func TestSuccessorCleanupAllowsOlderBindingAfterBindCrash(t *testing.T) {
	for _, detached := range []bool{false, true} {
		name := "connected"
		if detached {
			name = "detached"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			repo, _ := newAuthRepo(t)
			pod := "pandora-hub-successor-bind-crash-" + name
			now := time.Now().UnixMilli()
			activateReady(t, repo, pod, now, 0)
			now = time.Now().UnixMilli() + 1
			old := capacityTestPlacedReservation(101, uuid.NewString(), now)
			admissionID := uuid.NewString()
			mustReserveCapacity(t, repo, pod, old, now)
			if admitted, err := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), old,
				admissionID, 10, now+1, testTTL); err != nil || !admitted.Admitted {
				t.Fatalf("old admission=%+v err=%v", admitted, err)
			}
			mustReserveCapacity(t, repo, pod, old, now+2)
			if detached {
				if departed, err := repo.AcknowledgeDeparture(ctx, pod, capacityTestCredential(), old,
					admissionID, 10, now+3, testTTL); err != nil || !departed.Departed {
					t.Fatalf("old departure=%+v err=%v", departed, err)
				}
			}

			// Simulate the crash window: assignment/placement v8 committed, while
			// Redis still carries the exact same owner's pre-bind successor v7.
			expected := AssignmentInstanceIdentity{PlayerID: old.PlayerID, AssignmentID: old.AssignmentID,
				InstanceUID: old.InstanceUID, ProtocolEpoch: old.ProtocolEpoch, WriterEpoch: old.WriterEpoch,
				PlacementVersion:     old.PlacementVersion + 1,
				PlacementOperationID: "44444444-4444-4444-8444-444444444444", SourceMatchID: old.SourceMatchID}
			snapshot, err := repo.InspectAssignmentSeat(ctx, pod, expected)
			if err != nil || snapshot.Conflict || (detached && !snapshot.Reserved) || (!detached && !snapshot.Connected) {
				t.Fatalf("bind-crash inspect=%+v err=%v", snapshot, err)
			}
			released, err := repo.ReleaseAssignmentSeatExact(ctx, pod, expected, testTTL)
			if detached {
				if err != nil || !released.Released || released.Conflict || released.DepartureRequired {
					t.Fatalf("detached bind-crash release=%+v err=%v", released, err)
				}
			} else {
				if err != nil || released.Conflict || !released.DepartureRequired || released.Released {
					t.Fatalf("connected bind-crash release=%+v err=%v", released, err)
				}
				if departed, departErr := repo.AcknowledgeDeparture(ctx, pod, capacityTestCredential(), old,
					admissionID, 10, now+4, testTTL); departErr != nil || !departed.Departed || departed.CapacityOccupancy != 0 {
					t.Fatalf("bind-crash exact departure=%+v err=%v", departed, departErr)
				}
			}
			if count, _ := repo.rdb.HLen(ctx, successorsKey(pod)).Result(); count != 0 {
				t.Fatalf("bind-crash cleanup retained stale successor count=%d", count)
			}
		})
	}
}

func TestSuccessorCleanupIgnoresAndDeletesExpiredNewerBinding(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-successor-expired-newer"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	old := capacityTestPlacedReservation(101, uuid.NewString(), now)
	admissionID := uuid.NewString()
	mustReserveCapacity(t, repo, pod, old, now)
	if admitted, err := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), old,
		admissionID, 10, now+1, testTTL); err != nil || !admitted.Admitted {
		t.Fatalf("old admission=%+v err=%v", admitted, err)
	}
	newer := old
	newer.PlacementVersion++
	newer.PlacementOperationID = "55555555-5555-4555-8555-555555555555"
	mustReserveCapacity(t, repo, pod, newer, now+2)
	if departed, err := repo.AcknowledgeDeparture(ctx, pod, capacityTestCredential(), old,
		admissionID, 10, now+3, testTTL); err != nil || !departed.Departed {
		t.Fatalf("old departure=%+v err=%v", departed, err)
	}
	expected := AssignmentInstanceIdentity{PlayerID: old.PlayerID, AssignmentID: old.AssignmentID,
		InstanceUID: old.InstanceUID, ProtocolEpoch: old.ProtocolEpoch, WriterEpoch: old.WriterEpoch,
		PlacementVersion: old.PlacementVersion, PlacementOperationID: old.PlacementOperationID,
		SourceMatchID: old.SourceMatchID}
	if snapshot, err := repo.InspectAssignmentSeat(ctx, pod, expected); err != nil || !snapshot.Conflict {
		t.Fatalf("live newer successor must conflict: snapshot=%+v err=%v", snapshot, err)
	}
	rawSuccessors, err := repo.rdb.HGetAll(ctx, successorsKey(pod)).Result()
	if err != nil || len(rawSuccessors) != 1 {
		t.Fatalf("newer successor=%d err=%v", len(rawSuccessors), err)
	}
	for field, raw := range rawSuccessors {
		record := &hubv1.HubReservationStorageRecord{}
		if err := proto.Unmarshal([]byte(raw), record); err != nil {
			t.Fatal(err)
		}
		record.ExpiresAtMs = time.Now().UnixMilli() - 1
		payload, err := proto.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.rdb.HSet(ctx, successorsKey(pod), field, payload).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if snapshot, err := repo.InspectAssignmentSeat(ctx, pod, expected); err != nil ||
		!snapshot.AlreadyAbsent || snapshot.Conflict || snapshot.Reserved {
		t.Fatalf("expired newer successor must be logically absent: snapshot=%+v err=%v", snapshot, err)
	}
	if released, err := repo.ReleaseAssignmentSeatExact(ctx, pod, expected, testTTL); err != nil ||
		!released.Released || released.Conflict || released.DepartureRequired {
		t.Fatalf("expired newer successor release=%+v err=%v", released, err)
	}
	if count, _ := repo.rdb.HLen(ctx, successorsKey(pod)).Result(); count != 0 {
		t.Fatalf("expired newer successor was not deleted count=%d", count)
	}
}

func TestSuccessorLoaderRejectsCapabilityFromAnotherPod(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-successor-pod-binding"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	reservation := capacityTestPlacedReservation(101, uuid.NewString(), now)
	mustReserveCapacity(t, repo, pod, reservation, now)
	if admitted, err := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), reservation,
		uuid.NewString(), 10, now+1, testTTL); err != nil || !admitted.Admitted {
		t.Fatalf("admission=%+v err=%v", admitted, err)
	}
	mustReserveCapacity(t, repo, pod, reservation, now+2)
	rawSuccessors, err := repo.rdb.HGetAll(ctx, successorsKey(pod)).Result()
	if err != nil || len(rawSuccessors) != 1 {
		t.Fatalf("successor=%d err=%v", len(rawSuccessors), err)
	}
	foreignField, err := successorCapability(pod+"-other", reservation)
	if err != nil {
		t.Fatal(err)
	}
	for oldField, raw := range rawSuccessors {
		if err := repo.rdb.HDel(ctx, successorsKey(pod), oldField).Err(); err != nil {
			t.Fatal(err)
		}
		if err := repo.rdb.HSet(ctx, successorsKey(pod), foreignField, raw).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.ReserveAssignment(ctx, pod, reservation, now+3, 30_000, testTTL); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("foreign-pod capability must fail closed: err=%v", err)
	}
}

func TestReportedPlayersAreAuditOnlyWithUntrackedSafetyGate(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-reported-audit"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	credential := capacityTestCredential()

	if _, err := repo.ActivateHeartbeat(ctx, pod, credential, activateInput(1, "ready", now+1)); err != nil {
		t.Fatal(err)
	}
	if route, err := repo.CheckRoutable(ctx, pod, time.Now().UnixMilli(), 30_000); err != nil ||
		route.OK || route.Reason != "untracked-connected-players" {
		t.Fatalf("reported=1 ledger=0 must not route: result=%+v err=%v", route, err)
	}
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 0 || shard.GetConnectedOwnershipCount() != 0 ||
		shard.GetReportedConnectedCount() != 1 {
		t.Fatalf("reported player was incorrectly backfilled into authority: %+v", shard)
	}

	// 下一次实报回到 0 后可分配；接纳后 heartbeat 漏报 0 也不得删除 connected owner。
	if _, err := repo.ActivateHeartbeat(ctx, pod, credential, activateInput(0, "ready", now+2)); err != nil {
		t.Fatal(err)
	}
	reservation := capacityTestReservation(101, uuid.NewString(), time.Now().UnixMilli())
	mustReserveCapacity(t, repo, pod, reservation, time.Now().UnixMilli())
	if admission, err := repo.AcknowledgeAdmission(ctx, pod, credential, reservation,
		uuid.NewString(), 1, time.Now().UnixMilli(), testTTL); err != nil || !admission.Admitted {
		t.Fatalf("admission=%+v err=%v", admission, err)
	}
	if _, err := repo.ActivateHeartbeat(ctx, pod, credential, activateInput(0, "ready", now+3)); err != nil {
		t.Fatal(err)
	}
	shard, _, _ = shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 1 || shard.GetConnectedOwnershipCount() != 1 ||
		shard.GetReportedConnectedCount() != 0 {
		t.Fatalf("heartbeat omission deleted connected owner: %+v", shard)
	}
	if route, err := repo.CheckRoutable(ctx, pod, time.Now().UnixMilli(), 30_000); err != nil || !route.OK {
		t.Fatalf("reported omission below owned count must remain routable: result=%+v err=%v", route, err)
	}
}

func TestHeartbeatMaxPlayersAndPlayerListRejectionsHaveZeroSideEffects(t *testing.T) {
	ctx := context.Background()
	repo, mr := newAuthRepo(t)
	const pod = "pandora-hub-heartbeat-zero-side-effects"
	now := time.Now().UnixMilli()
	if _, err := repo.InitAuth(ctx, pod, "uid-A", activateAuthTTL); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StagePending(ctx, pod, credFor(pod, "uid-A", 1, 7, "j7"), activateAuthTTL); err != nil {
		t.Fatal(err)
	}
	seedWarmingShard(t, repo, pod, now)
	credential := capacityTestCredential()
	authBefore, _ := repo.rdb.Get(ctx, authKey(pod)).Bytes()
	shardBefore, _ := repo.rdb.Get(ctx, shardKey(pod)).Bytes()
	authTTLBefore, shardTTLBefore := mr.TTL(authKey(pod)), mr.TTL(shardKey(pod))

	badInputs := []ActivateHeartbeatInput{
		{PlayerCount: 0, MaxPlayers: 16, State: "ready", AuthTTL: activateAuthTTL, ShardTTL: testTTL},
		{PlayerCount: 1, PlayerIDs: nil, MaxPlayers: 500, State: "ready", AuthTTL: activateAuthTTL, ShardTTL: testTTL},
		{PlayerCount: 2, PlayerIDs: []uint64{1, 1}, MaxPlayers: 500, State: "ready", AuthTTL: activateAuthTTL, ShardTTL: testTTL},
		{PlayerCount: 1, PlayerIDs: []uint64{0}, MaxPlayers: 500, State: "ready", AuthTTL: activateAuthTTL, ShardTTL: testTTL},
	}
	for index, input := range badInputs {
		if _, err := repo.ActivateHeartbeat(ctx, pod, credential, input); err == nil {
			t.Fatalf("bad heartbeat %d unexpectedly accepted", index)
		}
		authAfter, _ := repo.rdb.Get(ctx, authKey(pod)).Bytes()
		shardAfter, _ := repo.rdb.Get(ctx, shardKey(pod)).Bytes()
		if !bytes.Equal(authAfter, authBefore) || !bytes.Equal(shardAfter, shardBefore) ||
			mr.TTL(authKey(pod)) != authTTLBefore || mr.TTL(shardKey(pod)) != shardTTLBefore {
			t.Fatalf("bad heartbeat %d mutated auth/shard bytes or ttl", index)
		}
	}
	auth, _, _ := repo.GetAuth(ctx, pod)
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if auth.GetActive() != nil || auth.GetPending() == nil || shard.GetState() != "warming" ||
		shard.GetReportedMaxPlayers() != 0 {
		t.Fatalf("rejected heartbeat promoted or readied shard: auth=%+v shard=%+v", auth, shard)
	}
}

func TestConcurrentReservationsAndAssignmentIdentityConflict(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-concurrent-capacity"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	bumpShard(t, repo, pod, func(shard *hubv1.HubShardStorageRecord) {
		shard.Capacity = 1
		shard.ReportedMaxPlayers = 1
	})
	reservations := []ReservationIdentity{
		capacityTestReservation(101, uuid.NewString(), now),
		capacityTestReservation(102, uuid.NewString(), now),
	}
	type outcome struct {
		index  int
		result ReserveResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, len(reservations))
	var wg sync.WaitGroup
	for index := range reservations {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			result, err := repo.ReserveAssignment(ctx, pod, reservations[index], now, 30_000, testTTL)
			outcomes <- outcome{index: index, result: result, err: err}
		}(index)
	}
	close(start)
	wg.Wait()
	close(outcomes)
	winner := -1
	for result := range outcomes {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.result.OK {
			if winner >= 0 {
				t.Fatal("capacity=1 admitted two concurrent reservations")
			}
			winner = result.index
		} else if result.result.Reason != "shard-full" {
			t.Fatalf("loser reason=%q", result.result.Reason)
		}
	}
	if winner < 0 {
		t.Fatal("no concurrent reservation won")
	}
	if duplicate, err := repo.ReserveAssignment(ctx, pod, reservations[winner], now, 30_000, testTTL); err != nil || !duplicate.OK || duplicate.PlayerCount != 1 {
		t.Fatalf("exact reservation retry=%+v err=%v", duplicate, err)
	}
	conflictIdentity := reservations[winner]
	conflictIdentity.PlayerID += 1000
	if conflict, err := repo.ReserveAssignment(ctx, pod, conflictIdentity, now, 30_000, testTTL); err != nil || conflict.OK || conflict.Reason != "assignment-reservation-conflict" {
		t.Fatalf("same assignment/different player conflict=%+v err=%v", conflict, err)
	}
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 1 || shard.GetReservedCount() != 1 {
		t.Fatalf("concurrent reservation projection=%+v", shard)
	}
}

func TestNewGameServerUIDHeartbeatPrunesOldOwnershipLedger(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-uid-rebuild"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	reservation := capacityTestReservation(101, uuid.NewString(), now)
	mustReserveCapacity(t, repo, pod, reservation, now)
	if admission, err := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), reservation,
		uuid.NewString(), 1, now+1, testTTL); err != nil || !admission.Admitted {
		t.Fatalf("old uid admission=%+v err=%v", admission, err)
	}

	auth, err := repo.InitAuth(ctx, pod, "uid-B", activateAuthTTL)
	if err != nil || auth.GetProtocolEpoch() != 2 {
		t.Fatalf("uid rebuild auth=%+v err=%v", auth, err)
	}
	newCredential := credFor(pod, "uid-B", 2, 8, "j8")
	if _, err := repo.StagePending(ctx, pod, newCredential, activateAuthTTL); err != nil {
		t.Fatal(err)
	}
	newIdentity := CredentialIdentity{
		Gen: 8, JTI: "j8", InstanceUID: "uid-B", ProtocolEpoch: 2,
		TokenSHA256: "sha-j8", Kid: "kid-1", WriterEpoch: testWriterEpoch,
	}
	if _, err := repo.ActivateHeartbeat(ctx, pod, newIdentity, activateInput(0, "ready", now+2)); err != nil {
		t.Fatal(err)
	}
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	sessions, _ := repo.rdb.HLen(ctx, sessionsKey(pod)).Result()
	if sessions != 0 || shard.GetPlayerCount() != 0 || shard.GetConnectedOwnershipCount() != 0 ||
		shard.GetGameserverUid() != "uid-B" || shard.GetAuthEpoch() != 2 {
		t.Fatalf("new uid did not atomically prune old ownership: sessions=%d shard=%+v", sessions, shard)
	}
}

func TestExpiredReservationReleaseStillCommitsExactCleanup(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-expired-release"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	reservation := capacityTestReservation(101, uuid.NewString(), now)
	mustReserveCapacity(t, repo, pod, reservation, now)
	raw, _ := repo.rdb.HGet(ctx, reservationsKey(pod), reservation.AssignmentID).Bytes()
	record := &hubv1.HubReservationStorageRecord{}
	if err := proto.Unmarshal(raw, record); err != nil {
		t.Fatal(err)
	}
	record.ExpiresAtMs = time.Now().UnixMilli() - 1
	raw, _ = proto.Marshal(record)
	if err := repo.rdb.HSet(ctx, reservationsKey(pod), reservation.AssignmentID, raw).Err(); err != nil {
		t.Fatal(err)
	}
	released, err := repo.ReleaseAssignmentSeat(ctx, pod, AssignmentInstanceIdentity{
		PlayerID: reservation.PlayerID, AssignmentID: reservation.AssignmentID,
		InstanceUID: reservation.InstanceUID, ProtocolEpoch: reservation.ProtocolEpoch,
		WriterEpoch: reservation.WriterEpoch,
	}, testTTL)
	if err != nil || !released {
		t.Fatalf("expired exact release=%v err=%v", released, err)
	}
	if exists, _ := repo.rdb.HExists(ctx, reservationsKey(pod), reservation.AssignmentID).Result(); exists {
		t.Fatal("expired reservation remained after exact release")
	}
	shard, _, _ := shardRepoFor(repo).GetShard(ctx, pod)
	if shard.GetPlayerCount() != 0 || shard.GetReservedCount() != 0 {
		t.Fatalf("expired release did not repair projection: %+v", shard)
	}
}

func TestReleaseAssignmentSeatExactThreeStatesAndConflictIsReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		admitted bool
	}{
		{name: "reservation", admitted: false},
		{name: "connected-session", admitted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo, mr := newAuthRepo(t)
			pod := "pandora-hub-exact-release-" + tc.name
			now := time.Now().UnixMilli()
			activateReady(t, repo, pod, now, 0)
			now = time.Now().UnixMilli() + 1
			reservation := capacityTestReservation(101, uuid.NewString(), now)
			mustReserveCapacity(t, repo, pod, reservation, now)
			ledgerKey := reservationsKey(pod)
			admissionID := ""
			if tc.admitted {
				admissionID = uuid.NewString()
				admission, err := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), reservation,
					admissionID, 1, now+1, testTTL)
				if err != nil || !admission.Admitted {
					t.Fatalf("admission=%+v err=%v", admission, err)
				}
				ledgerKey = sessionsKey(pod)
			}

			expected := AssignmentInstanceIdentity{
				PlayerID: reservation.PlayerID, AssignmentID: reservation.AssignmentID,
				InstanceUID: reservation.InstanceUID, ProtocolEpoch: reservation.ProtocolEpoch,
				WriterEpoch: reservation.WriterEpoch,
			}
			recordBefore, err := repo.rdb.HGet(ctx, ledgerKey, reservation.AssignmentID).Bytes()
			if err != nil {
				t.Fatal(err)
			}
			shardBefore, err := repo.rdb.Get(ctx, shardKey(pod)).Bytes()
			if err != nil {
				t.Fatal(err)
			}
			ledgerTTLBefore, shardTTLBefore := mr.TTL(ledgerKey), mr.TTL(shardKey(pod))

			conflicting := expected
			conflicting.PlayerID++
			conflict, err := repo.ReleaseAssignmentSeatExact(ctx, pod, conflicting, testTTL)
			if err != nil || !conflict.Conflict || conflict.Released || conflict.AlreadyAbsent {
				t.Fatalf("conflict result=%+v err=%v", conflict, err)
			}
			recordAfter, err := repo.rdb.HGet(ctx, ledgerKey, reservation.AssignmentID).Bytes()
			if err != nil {
				t.Fatal(err)
			}
			shardAfter, err := repo.rdb.Get(ctx, shardKey(pod)).Bytes()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(recordAfter, recordBefore) || !bytes.Equal(shardAfter, shardBefore) ||
				mr.TTL(ledgerKey) != ledgerTTLBefore || mr.TTL(shardKey(pod)) != shardTTLBefore {
				t.Fatal("identity conflict mutated the canonical seat or shard projection")
			}

			released, err := repo.ReleaseAssignmentSeatExact(ctx, pod, expected, testTTL)
			if tc.admitted {
				if err != nil || !released.DepartureRequired || released.Released ||
					released.AlreadyAbsent || released.Conflict {
					t.Fatalf("connected owner must require departure: result=%+v err=%v", released, err)
				}
				unchanged, readErr := repo.rdb.HGet(ctx, ledgerKey, reservation.AssignmentID).Bytes()
				if readErr != nil || !bytes.Equal(unchanged, recordBefore) {
					t.Fatalf("departure-required release mutated live owner: err=%v", readErr)
				}
				departure, departureErr := repo.AcknowledgeDeparture(ctx, pod, capacityTestCredential(),
					reservation, admissionID, 1, now+2, testTTL)
				if departureErr != nil || !departure.Departed {
					t.Fatalf("physical departure=%+v err=%v", departure, departureErr)
				}
			} else if err != nil || !released.Released || released.DepartureRequired ||
				released.AlreadyAbsent || released.Conflict {
				t.Fatalf("released result=%+v err=%v", released, err)
			}
			if exists, err := repo.rdb.HExists(ctx, ledgerKey, reservation.AssignmentID).Result(); err != nil || exists {
				t.Fatalf("exact seat remained exists=%v err=%v", exists, err)
			}
			shard, _, err := shardRepoFor(repo).GetShard(ctx, pod)
			if err != nil || shard.GetPlayerCount() != 0 || shard.GetReservedCount() != 0 ||
				shard.GetConnectedOwnershipCount() != 0 {
				t.Fatalf("release projection=%+v err=%v", shard, err)
			}

			absent, err := repo.ReleaseAssignmentSeatExact(ctx, pod, expected, testTTL)
			if err != nil || absent.Released || !absent.AlreadyAbsent || absent.Conflict {
				t.Fatalf("already-absent result=%+v err=%v", absent, err)
			}
		})
	}
}

func TestReleaseConnectedOwnerConsumesOnlyExactUIDTeardownProof(t *testing.T) {
	ctx := context.Background()
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-exact-uid-teardown"
	now := time.Now().UnixMilli()
	activateReady(t, repo, pod, now, 0)
	now = time.Now().UnixMilli() + 1
	reservation := capacityTestReservation(101, uuid.NewString(), now)
	mustReserveCapacity(t, repo, pod, reservation, now)
	if admission, err := repo.AcknowledgeAdmission(ctx, pod, capacityTestCredential(), reservation,
		uuid.NewString(), 1, now+2, testTTL); err != nil || !admission.Admitted {
		t.Fatalf("admission=%+v err=%v", admission, err)
	}
	expected := AssignmentInstanceIdentity{PlayerID: reservation.PlayerID,
		AssignmentID: reservation.AssignmentID, InstanceUID: reservation.InstanceUID,
		ProtocolEpoch: reservation.ProtocolEpoch, WriterEpoch: reservation.WriterEpoch}

	if err := repo.RecordInstanceTeardownProof(ctx, pod, "uid-other", time.Hour); err != nil {
		t.Fatal(err)
	}
	if result, err := repo.ReleaseAssignmentSeatExact(ctx, pod, expected, testTTL); err != nil ||
		!result.DepartureRequired || result.Released {
		t.Fatalf("wrong UID proof authorized cleanup: result=%+v err=%v", result, err)
	}
	if err := repo.RecordInstanceTeardownProof(ctx, pod, reservation.InstanceUID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if result, err := repo.ReleaseAssignmentSeatExact(ctx, pod, expected, testTTL); err != nil ||
		!result.Released || result.DepartureRequired || result.Conflict {
		t.Fatalf("exact UID proof did not release owner: result=%+v err=%v", result, err)
	}
	if exists, _ := repo.rdb.HExists(ctx, sessionsKey(pod), reservation.AssignmentID).Result(); exists {
		t.Fatal("exact torn-down UID session remained")
	}
	shard, _, err := shardRepoFor(repo).GetShard(ctx, pod)
	if err != nil || shard.GetPlayerCount() != 0 || shard.GetConnectedOwnershipCount() != 0 {
		t.Fatalf("teardown cleanup did not repair projection: shard=%+v err=%v", shard, err)
	}
}

func TestMaxPlayersMismatchUsesInvalidStateCode(t *testing.T) {
	repo, _ := newAuthRepo(t)
	const pod = "pandora-hub-max-code"
	now := time.Now().UnixMilli()
	if _, err := repo.InitAuth(context.Background(), pod, "uid-A", activateAuthTTL); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StagePending(context.Background(), pod,
		credFor(pod, "uid-A", 1, 7, "j7"), activateAuthTTL); err != nil {
		t.Fatal(err)
	}
	seedWarmingShard(t, repo, pod, now)
	input := activateInput(0, "ready", now)
	input.MaxPlayers = 16
	if _, err := repo.ActivateHeartbeat(context.Background(), pod, capacityTestCredential(), input); errcode.As(err) != errcode.ErrInvalidState {
		t.Fatalf("max mismatch code=%v err=%v", errcode.As(err), err)
	}
}
