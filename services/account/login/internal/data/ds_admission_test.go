package data

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/middleware"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

var admissionTestNow = time.Unix(1_800_000_000, 0)

func validHubAdmissionState() (*middleware.VerifiedCredential, *hubv1.HubShardAuthStorageRecord, *hubv1.HubShardStorageRecord) {
	credential := &middleware.VerifiedCredential{
		DSType: auth.DSTypeHub, Pod: "hub-1", InstanceUID: "hub-uid", ProtocolEpoch: 7,
		Gen: 11, JTI: "hub-credential-jti", ExpMs: admissionTestNow.Add(time.Hour).UnixMilli(),
		Kid: "kid-hub", TokenSHA256: "hash-hub", WriterEpoch: auth.DSAuthWriterEpochV2,
	}
	active := &hubv1.HubDSCredential{
		Gen: credential.Gen, Jti: credential.JTI, ExpMs: uint64(credential.ExpMs), Kid: credential.Kid,
		InstanceUid: credential.InstanceUID, ProtocolEpoch: credential.ProtocolEpoch,
		TokenSha256: credential.TokenSHA256, WriterEpoch: credential.WriterEpoch,
	}
	record := &hubv1.HubShardAuthStorageRecord{
		PodName: credential.Pod, InstanceUid: credential.InstanceUID, ProtocolEpoch: credential.ProtocolEpoch,
		Phase: hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE, Active: active, HighWaterGen: active.Gen,
		RequiredWriterEpoch:   auth.DSAuthWriterEpochV2,
		LastActiveHeartbeatMs: admissionTestNow.Add(-time.Second).UnixMilli(),
	}
	projection := &hubv1.HubShardStorageRecord{
		HubPodName: credential.Pod, State: "ready", GameserverUid: credential.InstanceUID,
		AuthEpoch: credential.ProtocolEpoch, LastVerifiedGen: credential.Gen,
		LastVerifiedJti: credential.JTI, LastVerifiedWriterEpoch: credential.WriterEpoch,
		LastHeartbeatMs: admissionTestNow.Add(-time.Second).UnixMilli(),
	}
	return credential, record, projection
}

func validBattleAdmissionState() (*middleware.VerifiedCredential, *dsv1.BattleDSAuthStorageRecord, *dsv1.BattleStorageRecord) {
	credential := &middleware.VerifiedCredential{
		DSType: auth.DSTypeBattle, MatchID: 9001, Pod: "battle-1", InstanceUID: "battle-uid",
		ProtocolEpoch: 5, Gen: 17, JTI: "battle-credential-jti",
		ExpMs: admissionTestNow.Add(time.Hour).UnixMilli(), Kid: "kid-battle",
		TokenSHA256: "hash-battle", WriterEpoch: auth.DSAuthWriterEpochV2,
	}
	active := &dsv1.BattleDSCredential{
		Gen: credential.Gen, Jti: credential.JTI, ExpMs: uint64(credential.ExpMs), Kid: credential.Kid,
		InstanceUid: credential.InstanceUID, InstanceEpoch: credential.ProtocolEpoch,
		TokenSha256: credential.TokenSHA256, WriterEpoch: credential.WriterEpoch,
	}
	record := &dsv1.BattleDSAuthStorageRecord{
		MatchId: credential.MatchID, DsPodName: credential.Pod, InstanceUid: credential.InstanceUID,
		InstanceEpoch: credential.ProtocolEpoch, Phase: dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_ACTIVE,
		Active: active, HighWaterGen: active.Gen, RequiredWriterEpoch: auth.DSAuthWriterEpochV2,
		AllocationId: "allocation-1", LastActiveHeartbeatMs: admissionTestNow.Add(-time.Second).UnixMilli(),
	}
	projection := &dsv1.BattleStorageRecord{
		MatchId: credential.MatchID, DsPodName: credential.Pod, DsAddr: "10.0.0.17:7000", State: "running",
		PlayerIds:     []uint64{1001, 1002},
		GameserverUid: credential.InstanceUID, InstanceEpoch: credential.ProtocolEpoch,
		LastVerifiedGen: credential.Gen, LastVerifiedJti: credential.JTI,
		LastVerifiedWriterEpoch: credential.WriterEpoch, AllocationId: record.AllocationId,
		LastHeartbeatMs: record.LastActiveHeartbeatMs,
	}
	return credential, record, projection
}

func newAdmissionCheckerFixture(t *testing.T) (*miniredis.Miniredis, *redis.Client, *RedisDSAdmissionChecker) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	checker := NewRedisDSAdmissionChecker(rdb, 30*time.Second)
	checker.now = func() time.Time { return admissionTestNow }
	return mr, rdb, checker
}

func setAdmissionProto(t *testing.T, mr *miniredis.Miniredis, key string, message proto.Message) {
	t.Helper()
	raw, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	mr.Set(key, string(raw))
}

func TestRedisDSAdmissionCheckerHubStrictActive(t *testing.T) {
	mr, rdb, checker := newAdmissionCheckerFixture(t)
	defer rdb.Close()
	credential, record, projection := validHubAdmissionState()
	setAdmissionProto(t, mr, admissionHubAuthKey(credential.Pod), record)
	setAdmissionProto(t, mr, admissionHubProjectionKey(credential.Pod), projection)

	binding, err := checker.CheckActive(context.Background(), credential.Pod, credential)
	if err != nil || !binding.Complete() || binding.DSType != auth.DSTypeHub || binding.MatchID != 0 {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}

	tests := []struct {
		name   string
		mutate func(*middleware.VerifiedCredential, *hubv1.HubShardAuthStorageRecord, *hubv1.HubShardStorageRecord)
	}{
		{"stale-heartbeat", func(_ *middleware.VerifiedCredential, r *hubv1.HubShardAuthStorageRecord, _ *hubv1.HubShardStorageRecord) {
			r.LastActiveHeartbeatMs = admissionTestNow.Add(-time.Minute).UnixMilli()
		}},
		{"wrong-uid", func(c *middleware.VerifiedCredential, _ *hubv1.HubShardAuthStorageRecord, _ *hubv1.HubShardStorageRecord) {
			c.InstanceUID = "rebuilt-uid"
		}},
		{"future-writer", func(c *middleware.VerifiedCredential, _ *hubv1.HubShardAuthStorageRecord, _ *hubv1.HubShardStorageRecord) {
			c.WriterEpoch = 3
		}},
		{"required-writer-future", func(_ *middleware.VerifiedCredential, r *hubv1.HubShardAuthStorageRecord, _ *hubv1.HubShardStorageRecord) {
			r.RequiredWriterEpoch = 3
		}},
		{"wrong-hash", func(c *middleware.VerifiedCredential, _ *hubv1.HubShardAuthStorageRecord, _ *hubv1.HubShardStorageRecord) {
			c.TokenSHA256 = "other"
		}},
		{"draining", func(_ *middleware.VerifiedCredential, _ *hubv1.HubShardAuthStorageRecord, p *hubv1.HubShardStorageRecord) {
			p.State = "draining"
		}},
		{"projection-wrong-uid", func(_ *middleware.VerifiedCredential, _ *hubv1.HubShardAuthStorageRecord, p *hubv1.HubShardStorageRecord) {
			p.GameserverUid = "other"
		}},
		{"projection-wrong-verified", func(_ *middleware.VerifiedCredential, _ *hubv1.HubShardAuthStorageRecord, p *hubv1.HubShardStorageRecord) {
			p.LastVerifiedGen++
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, r, p := validHubAdmissionState()
			tc.mutate(c, r, p)
			setAdmissionProto(t, mr, admissionHubAuthKey(c.Pod), r)
			setAdmissionProto(t, mr, admissionHubProjectionKey(c.Pod), p)
			if _, err := checker.CheckActive(context.Background(), c.Pod, c); errcode.As(err) != errcode.ErrUnauthorized {
				t.Fatalf("code=%v err=%v", errcode.As(err), err)
			}
		})
	}
	t.Run("projection-missing", func(t *testing.T) {
		c, r, _ := validHubAdmissionState()
		setAdmissionProto(t, mr, admissionHubAuthKey(c.Pod), r)
		mr.Del(admissionHubProjectionKey(c.Pod))
		if _, err := checker.CheckActive(context.Background(), c.Pod, c); errcode.As(err) != errcode.ErrUnauthorized {
			t.Fatalf("code=%v err=%v", errcode.As(err), err)
		}
	})
}

func TestRedisDSAdmissionCheckerBattleProjectionAndFailure(t *testing.T) {
	mr, rdb, checker := newAdmissionCheckerFixture(t)
	defer rdb.Close()
	credential, record, projection := validBattleAdmissionState()
	setAdmissionProto(t, mr, admissionBattleAuthKey(credential.MatchID), record)
	setAdmissionProto(t, mr, admissionBattleProjectionKey(credential.MatchID), projection)

	binding, err := checker.CheckActive(context.Background(), credential.Pod, credential)
	if err != nil || !binding.Complete() || binding.MatchID != credential.MatchID || len(binding.PlayerIDs) != 2 {
		t.Fatalf("binding=%+v err=%v", binding, err)
	}

	t.Run("allocation-mismatch", func(t *testing.T) {
		c, r, p := validBattleAdmissionState()
		p.AllocationId = "other-allocation"
		setAdmissionProto(t, mr, admissionBattleAuthKey(c.MatchID), r)
		setAdmissionProto(t, mr, admissionBattleProjectionKey(c.MatchID), p)
		if _, err := checker.CheckActive(context.Background(), c.Pod, c); errcode.As(err) != errcode.ErrUnauthorized {
			t.Fatalf("code=%v err=%v", errcode.As(err), err)
		}
	})

	t.Run("projection-match-mismatch", func(t *testing.T) {
		c, r, p := validBattleAdmissionState()
		p.MatchId++
		setAdmissionProto(t, mr, admissionBattleAuthKey(c.MatchID), r)
		setAdmissionProto(t, mr, admissionBattleProjectionKey(c.MatchID), p)
		if _, err := checker.CheckActive(context.Background(), c.Pod, c); errcode.As(err) != errcode.ErrUnauthorized {
			t.Fatalf("code=%v err=%v", errcode.As(err), err)
		}
	})

	t.Run("empty-roster", func(t *testing.T) {
		c, r, p := validBattleAdmissionState()
		p.PlayerIds = nil
		setAdmissionProto(t, mr, admissionBattleAuthKey(c.MatchID), r)
		setAdmissionProto(t, mr, admissionBattleProjectionKey(c.MatchID), p)
		if _, err := checker.CheckActive(context.Background(), c.Pod, c); errcode.As(err) != errcode.ErrUnauthorized {
			t.Fatalf("code=%v err=%v", errcode.As(err), err)
		}
	})

	t.Run("redis-failure-unavailable", func(t *testing.T) {
		c, _, _ := validBattleAdmissionState()
		mr.Close()
		if _, err := checker.CheckActive(context.Background(), c.Pod, c); errcode.As(err) != errcode.ErrUnavailable {
			t.Fatalf("code=%v err=%v", errcode.As(err), err)
		}
	})
}

func TestAdmissionReplayOwnerCanonicalUUIDAndTuple(t *testing.T) {
	credential, _, _ := validHubAdmissionState()
	binding := bindingFromCredential(credential)
	const admissionID = "123e4567-e89b-42d3-a456-426614174000"
	one, err := binding.AdmissionAttemptOwner(admissionID)
	if err != nil || len(one) != 64 {
		t.Fatalf("owner=%q err=%v", one, err)
	}
	two, _ := binding.AdmissionAttemptOwner(admissionID)
	if one != two {
		t.Fatal("same admission tuple must have deterministic owner")
	}
	binding.CredentialGen++
	three, _ := binding.AdmissionAttemptOwner(admissionID)
	if three != one {
		t.Fatal("ordinary credential rotation must keep stable attempt owner")
	}
	credentialHash, err := binding.AcceptedCredentialHash()
	if err != nil || len(credentialHash) != 64 {
		t.Fatalf("credential hash=%q err=%v", credentialHash, err)
	}
	binding.CredentialGen++
	rotatedHash, _ := binding.AcceptedCredentialHash()
	if rotatedHash == credentialHash {
		t.Fatal("full accepted credential hash must include rotation tuple")
	}
	for _, bad := range []string{"", "not-a-uuid", "00000000-0000-0000-0000-000000000000",
		"123e4567-e89b-12d3-a456-426614174000", "123E4567-E89B-42D3-A456-426614174000",
		"{123e4567-e89b-42d3-a456-426614174000}"} {
		if _, err := binding.AdmissionAttemptOwner(bad); err == nil {
			t.Fatalf("non-canonical admission id %q accepted", bad)
		}
	}
}
