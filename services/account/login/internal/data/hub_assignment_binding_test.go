package data

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

func testHubBinding() HubAssignmentBinding {
	return HubAssignmentBinding{
		PodName:       "hub-cn-1",
		InstanceUID:   "uid-a",
		ProtocolEpoch: 7,
		CredentialGen: 42,
		CredentialJTI: "credential-jti-a",
		AssignmentID:  "assignment-a",
		WriterEpoch:   2,
		ReleaseTrack:  auth.ReleaseTrackStable,
	}
}

func testAssignmentRecord() *hubv1.HubAssignmentStorageRecord {
	return &hubv1.HubAssignmentStorageRecord{
		PlayerId:        1001,
		HubPodName:      "hub-cn-1",
		HubInstanceUid:  "uid-a",
		AuthEpoch:       7,
		AuthGen:         42,
		AuthJti:         "credential-jti-a",
		AssignmentId:    "assignment-a",
		AuthWriterEpoch: 2,
		ReleaseTrack:    auth.ReleaseTrackStable,
	}
}

func newAssignmentChecker(t *testing.T) (*miniredis.Miniredis, *RedisHubAssignmentChecker) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{
		Addr:         mr.Addr(),
		DialTimeout:  100 * time.Millisecond,
		ReadTimeout:  100 * time.Millisecond,
		WriteTimeout: 100 * time.Millisecond,
		MaxRetries:   0,
	})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, NewRedisHubAssignmentChecker(rdb)
}

func putAssignment(t *testing.T, mr *miniredis.Miniredis, rec *hubv1.HubAssignmentStorageRecord) {
	t.Helper()
	payload, err := proto.Marshal(rec)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	if err := mr.Set(hubPlayerAssignmentKey(rec.GetPlayerId()), string(payload)); err != nil {
		t.Fatalf("miniredis.Set: %v", err)
	}
}

func putActiveHubAuth(t *testing.T, mr *miniredis.Miniredis, mutate ...func(*hubv1.HubShardAuthStorageRecord)) {
	t.Helper()
	now := time.Now()
	rec := &hubv1.HubShardAuthStorageRecord{
		PodName: "hub-cn-1", InstanceUid: "uid-a", ProtocolEpoch: 7,
		Phase:        hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE,
		Active:       &hubv1.HubDSCredential{Gen: 42, Jti: "credential-jti-a", ExpMs: uint64(now.Add(time.Hour).UnixMilli()), Kid: "kid", InstanceUid: "uid-a", ProtocolEpoch: 7, TokenSha256: "hash", WriterEpoch: 2},
		HighWaterGen: 42, RequiredWriterEpoch: 2, LastActiveHeartbeatMs: now.Add(-time.Second).UnixMilli(),
	}
	for _, fn := range mutate {
		fn(rec)
	}
	payload, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := mr.Set(hubAuthAuthorityKey("hub-cn-1"), string(payload)); err != nil {
		t.Fatal(err)
	}
	shard := &hubv1.HubShardStorageRecord{
		HubPodName: "hub-cn-1", State: "ready", GameserverUid: "uid-a", AuthEpoch: 7,
		LastVerifiedGen: 42, LastVerifiedJti: "credential-jti-a", LastVerifiedWriterEpoch: 2,
		LastHeartbeatMs: now.Add(-time.Second).UnixMilli(),
		ReleaseTrack:    auth.ReleaseTrackStable,
	}
	shardPayload, err := proto.Marshal(shard)
	if err != nil {
		t.Fatal(err)
	}
	if err := mr.Set(admissionHubProjectionKey("hub-cn-1"), string(shardPayload)); err != nil {
		t.Fatal(err)
	}
}

func TestRedisHubAssignmentCheckerB1BindsReleaseTrack(t *testing.T) {
	mr, checker := newAssignmentChecker(t)
	putAssignment(t, mr, testAssignmentRecord())
	putActiveHubAuth(t, mr)
	if err := checker.CheckCurrentB1(context.Background(), 1001, "hub-cn-1", "uid-a", 7,
		"assignment-a", auth.ReleaseTrackStable); err != nil {
		t.Fatalf("CheckCurrentB1 stable: %v", err)
	}
	if err := checker.CheckCurrentB1(context.Background(), 1001, "hub-cn-1", "uid-a", 7,
		"assignment-a", auth.ReleaseTrackCanary); errcode.As(err) != errcode.ErrLoginTicketInvalid {
		t.Fatalf("track mismatch code=%v err=%v", errcode.As(err), err)
	}
}

func TestRedisHubAssignmentCheckerExactMatch(t *testing.T) {
	mr, checker := newAssignmentChecker(t)
	putAssignment(t, mr, testAssignmentRecord())
	putActiveHubAuth(t, mr)
	if err := checker.CheckCurrent(context.Background(), 1001, testHubBinding()); err != nil {
		t.Fatalf("CheckCurrent exact match: %v", err)
	}
}

func TestRedisHubAssignmentCheckerRequiresCurrentActiveCredential(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*hubv1.HubShardAuthStorageRecord)
	}{
		{"missing-active", func(r *hubv1.HubShardAuthStorageRecord) { r.Active = nil }},
		{"pending-not-active", func(r *hubv1.HubShardAuthStorageRecord) {
			r.Active.Jti = "old"
			r.Pending = &hubv1.HubDSCredential{Gen: 42, Jti: "credential-jti-a"}
			r.Phase = hubv1.HubAuthPhase_HUB_AUTH_PHASE_ROTATING
		}},
		{"uid-rebuild", func(r *hubv1.HubShardAuthStorageRecord) { r.InstanceUid = "uid-new"; r.Active.InstanceUid = "uid-new" }},
		{"epoch", func(r *hubv1.HubShardAuthStorageRecord) { r.ProtocolEpoch++; r.Active.ProtocolEpoch++ }},
		{"gen", func(r *hubv1.HubShardAuthStorageRecord) { r.Active.Gen++ }},
		{"jti", func(r *hubv1.HubShardAuthStorageRecord) { r.Active.Jti = "new" }},
		{"low-required-writer", func(r *hubv1.HubShardAuthStorageRecord) { r.RequiredWriterEpoch = 1 }},
		{"stale-heartbeat", func(r *hubv1.HubShardAuthStorageRecord) {
			r.LastActiveHeartbeatMs = time.Now().Add(-time.Minute).UnixMilli()
		}},
		{"future-heartbeat", func(r *hubv1.HubShardAuthStorageRecord) {
			r.LastActiveHeartbeatMs = time.Now().Add(time.Minute).UnixMilli()
		}},
		{"quarantined", func(r *hubv1.HubShardAuthStorageRecord) { r.Phase = hubv1.HubAuthPhase_HUB_AUTH_PHASE_QUARANTINED }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mr, checker := newAssignmentChecker(t)
			putAssignment(t, mr, testAssignmentRecord())
			putActiveHubAuth(t, mr, tc.mutate)
			if err := checker.CheckCurrent(context.Background(), 1001, testHubBinding()); errcode.As(err) != errcode.ErrLoginTicketInvalid {
				t.Fatalf("code=%v err=%v", errcode.As(err), err)
			}
		})
	}
}

func TestRedisHubAssignmentCheckerRequiresReadyVerifiedProjection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*hubv1.HubShardStorageRecord)
	}{
		{"draining", func(r *hubv1.HubShardStorageRecord) { r.State = "draining" }},
		{"stopping", func(r *hubv1.HubShardStorageRecord) { r.State = "stopping" }},
		{"wrong-uid", func(r *hubv1.HubShardStorageRecord) { r.GameserverUid = "uid-new" }},
		{"wrong-epoch", func(r *hubv1.HubShardStorageRecord) { r.AuthEpoch++ }},
		{"wrong-gen", func(r *hubv1.HubShardStorageRecord) { r.LastVerifiedGen++ }},
		{"wrong-jti", func(r *hubv1.HubShardStorageRecord) { r.LastVerifiedJti = "new" }},
		{"stale-heartbeat", func(r *hubv1.HubShardStorageRecord) { r.LastHeartbeatMs = time.Now().Add(-time.Minute).UnixMilli() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr, checker := newAssignmentChecker(t)
			putAssignment(t, mr, testAssignmentRecord())
			putActiveHubAuth(t, mr)
			raw, err := mr.Get(admissionHubProjectionKey("hub-cn-1"))
			if err != nil {
				t.Fatal(err)
			}
			projection := &hubv1.HubShardStorageRecord{}
			if err := proto.Unmarshal([]byte(raw), projection); err != nil {
				t.Fatal(err)
			}
			tc.mutate(projection)
			payload, _ := proto.Marshal(projection)
			mr.Set(admissionHubProjectionKey("hub-cn-1"), string(payload))
			if err := checker.CheckCurrent(context.Background(), 1001, testHubBinding()); errcode.As(err) != errcode.ErrLoginTicketInvalid {
				t.Fatalf("code=%v err=%v", errcode.As(err), err)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		mr, checker := newAssignmentChecker(t)
		putAssignment(t, mr, testAssignmentRecord())
		putActiveHubAuth(t, mr)
		mr.Del(admissionHubProjectionKey("hub-cn-1"))
		if err := checker.CheckCurrent(context.Background(), 1001, testHubBinding()); errcode.As(err) != errcode.ErrLoginTicketInvalid {
			t.Fatalf("code=%v err=%v", errcode.As(err), err)
		}
	})
}

func TestRedisHubAssignmentCheckerRejectsNonV2WriterRecords(t *testing.T) {
	for _, tc := range []struct {
		name          string
		requiredEpoch uint32
		writerEpoch   uint32
	}{
		{name: "low-required-active-claims-1", requiredEpoch: 1, writerEpoch: 1},
		{name: "required-2-future-active-claims-3", requiredEpoch: auth.DSAuthWriterEpochV2, writerEpoch: 3},
		{name: "future-required-active-claims-3", requiredEpoch: 3, writerEpoch: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr, checker := newAssignmentChecker(t)
			assignment := testAssignmentRecord()
			assignment.AuthWriterEpoch = tc.writerEpoch
			putAssignment(t, mr, assignment)
			putActiveHubAuth(t, mr, func(r *hubv1.HubShardAuthStorageRecord) {
				r.RequiredWriterEpoch = tc.requiredEpoch
				r.Active.WriterEpoch = tc.writerEpoch
				r.Pending = proto.Clone(r.Active).(*hubv1.HubDSCredential)
			})
			binding := testHubBinding()
			binding.WriterEpoch = tc.writerEpoch
			assignmentBefore, err := mr.Get(hubPlayerAssignmentKey(1001))
			if err != nil {
				t.Fatal(err)
			}
			authBefore, err := mr.Get(hubAuthAuthorityKey("hub-cn-1"))
			if err != nil {
				t.Fatal(err)
			}
			if err := checker.CheckCurrent(context.Background(), 1001, binding); errcode.As(err) != errcode.ErrLoginTicketInvalid {
				t.Fatalf("code=%v err=%v", errcode.As(err), err)
			}
			assignmentAfter, _ := mr.Get(hubPlayerAssignmentKey(1001))
			authAfter, _ := mr.Get(hubAuthAuthorityKey("hub-cn-1"))
			if assignmentAfter != assignmentBefore || authAfter != authBefore {
				t.Fatal("rejected non-v2 assignment validation mutated Redis")
			}
		})
	}
}

type changingAssignmentRedis struct {
	assignmentReads            int
	first, second, auth, shard []byte
}

func (s *changingAssignmentRedis) Get(_ context.Context, key string) *redis.StringCmd {
	s.assignmentReads++
	if s.assignmentReads == 1 {
		return redis.NewStringResult(string(s.first), nil)
	}
	return redis.NewStringResult(string(s.second), nil)
}

func (s *changingAssignmentRedis) MGet(_ context.Context, _ ...string) *redis.SliceCmd {
	return redis.NewSliceResult([]interface{}{string(s.auth), string(s.shard)}, nil)
}

func TestRedisHubAssignmentCheckerDoubleCollectDetectsCrossSlotRace(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	first := testAssignmentRecord()
	second := proto.Clone(first).(*hubv1.HubAssignmentStorageRecord)
	second.AssignmentId = "winner-changed"
	authRec := &hubv1.HubShardAuthStorageRecord{PodName: "hub-cn-1", InstanceUid: "uid-a", ProtocolEpoch: 7, Phase: hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE, Active: &hubv1.HubDSCredential{Gen: 42, Jti: "credential-jti-a", ExpMs: uint64(now.Add(time.Hour).UnixMilli()), Kid: "kid", InstanceUid: "uid-a", ProtocolEpoch: 7, TokenSha256: "hash", WriterEpoch: 2}, HighWaterGen: 42, RequiredWriterEpoch: 2, LastActiveHeartbeatMs: now.Add(-time.Second).UnixMilli()}
	shardRec := &hubv1.HubShardStorageRecord{HubPodName: "hub-cn-1", State: "ready", GameserverUid: "uid-a", AuthEpoch: 7, LastVerifiedGen: 42, LastVerifiedJti: "credential-jti-a", LastVerifiedWriterEpoch: 2, LastHeartbeatMs: now.Add(-time.Second).UnixMilli()}
	firstRaw, _ := proto.Marshal(first)
	secondRaw, _ := proto.Marshal(second)
	authRaw, _ := proto.Marshal(authRec)
	shardRaw, _ := proto.Marshal(shardRec)
	checker := &RedisHubAssignmentChecker{rdb: &changingAssignmentRedis{first: firstRaw, second: secondRaw, auth: authRaw, shard: shardRaw}, now: func() time.Time { return now }, maxHeartbeatAge: 30 * time.Second}
	if err := checker.CheckCurrent(context.Background(), 1001, testHubBinding()); errcode.As(err) != errcode.ErrLoginTicketInvalid {
		t.Fatalf("cross-slot assignment race must fail, code=%v err=%v", errcode.As(err), err)
	}
}

func TestRedisHubAssignmentCheckerStableRetryAllowsCredentialRotation(t *testing.T) {
	mr, checker := newAssignmentChecker(t)
	putAssignment(t, mr, testAssignmentRecord()) // ticket/assignment 仍钉 A1(gen42/jti-a)
	putActiveHubAuth(t, mr, func(r *hubv1.HubShardAuthStorageRecord) {
		r.Active.Gen = 43
		r.Active.Jti = "credential-jti-b"
		r.HighWaterGen = 43
	})
	shardRaw, _ := mr.Get(admissionHubProjectionKey("hub-cn-1"))
	shard := &hubv1.HubShardStorageRecord{}
	if err := proto.Unmarshal([]byte(shardRaw), shard); err != nil {
		t.Fatal(err)
	}
	shard.LastVerifiedGen = 43
	shard.LastVerifiedJti = "credential-jti-b"
	updatedShard, _ := proto.Marshal(shard)
	mr.Set(admissionHubProjectionKey("hub-cn-1"), string(updatedShard))

	stable := testHubBinding()
	active := stable
	active.CredentialGen = 43
	active.CredentialJTI = "credential-jti-b"
	authRawText, _ := mr.Get(hubAuthAuthorityKey("hub-cn-1"))
	authRecord := &hubv1.HubShardAuthStorageRecord{}
	if err := proto.Unmarshal([]byte(authRawText), authRecord); err != nil {
		t.Fatal(err)
	}
	active.ExpMs = int64(authRecord.GetActive().GetExpMs())
	active.Kid = authRecord.GetActive().GetKid()
	active.TokenSHA256 = authRecord.GetActive().GetTokenSha256()
	if err := checker.CheckCurrentStable(context.Background(), 1001, stable, active); err != nil {
		t.Fatalf("stable retry after active rotation: %v", err)
	}
	if err := checker.CheckCurrent(context.Background(), 1001, active); errcode.As(err) != errcode.ErrLoginTicketInvalid {
		t.Fatalf("strict first admission must still reject stale assignment tuple: code=%v err=%v", errcode.As(err), err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*hubv1.HubDSCredential)
	}{
		{"kid", func(c *hubv1.HubDSCredential) { c.Kid = "other-kid" }},
		{"hash", func(c *hubv1.HubDSCredential) { c.TokenSha256 = "other-hash" }},
		{"exp", func(c *hubv1.HubDSCredential) { c.ExpMs++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := proto.Clone(authRecord).(*hubv1.HubShardAuthStorageRecord)
			tc.mutate(changed.Active)
			changedRaw, _ := proto.Marshal(changed)
			mr.Set(hubAuthAuthorityKey("hub-cn-1"), string(changedRaw))
			if err := checker.CheckCurrentStable(context.Background(), 1001, stable, active); errcode.As(err) != errcode.ErrLoginTicketInvalid {
				t.Fatalf("full active tuple drift code=%v err=%v", errcode.As(err), err)
			}
			mr.Set(hubAuthAuthorityKey("hub-cn-1"), authRawText)
		})
	}
}

func TestRedisHubAssignmentCheckerMissingAndEveryBindingMismatch(t *testing.T) {
	_, checker := newAssignmentChecker(t)
	if err := checker.CheckCurrent(context.Background(), 1001, testHubBinding()); errcode.As(err) != errcode.ErrLoginTicketInvalid {
		t.Fatalf("missing code=%v err=%v", errcode.As(err), err)
	}

	tests := []struct {
		name   string
		mutate func(*hubv1.HubAssignmentStorageRecord)
	}{
		{"player", func(r *hubv1.HubAssignmentStorageRecord) { r.PlayerId = 1002 }},
		{"assignment", func(r *hubv1.HubAssignmentStorageRecord) { r.AssignmentId = "assignment-b" }},
		{"pod", func(r *hubv1.HubAssignmentStorageRecord) { r.HubPodName = "hub-cn-2" }},
		{"uid-rebuild", func(r *hubv1.HubAssignmentStorageRecord) { r.HubInstanceUid = "uid-b" }},
		{"epoch", func(r *hubv1.HubAssignmentStorageRecord) { r.AuthEpoch++ }},
		{"gen", func(r *hubv1.HubAssignmentStorageRecord) { r.AuthGen++ }},
		{"credential-jti", func(r *hubv1.HubAssignmentStorageRecord) { r.AuthJti = "credential-jti-b" }},
		{"writer-epoch", func(r *hubv1.HubAssignmentStorageRecord) { r.AuthWriterEpoch++ }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mr, currentChecker := newAssignmentChecker(t)
			rec := testAssignmentRecord()
			tc.mutate(rec)
			// player_id 被篡改时仍写进被校验玩家的 key，验证消息体身份也会严格比较。
			payload, err := proto.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			if err := mr.Set(hubPlayerAssignmentKey(1001), string(payload)); err != nil {
				t.Fatal(err)
			}
			if err := currentChecker.CheckCurrent(context.Background(), 1001, testHubBinding()); errcode.As(err) != errcode.ErrLoginTicketInvalid {
				t.Fatalf("code=%v err=%v", errcode.As(err), err)
			}
		})
	}
}

func TestRedisHubAssignmentCheckerBadProtoAndRedisFailureUnavailable(t *testing.T) {
	t.Run("bad-proto", func(t *testing.T) {
		mr, checker := newAssignmentChecker(t)
		if err := mr.Set(hubPlayerAssignmentKey(1001), "not-protobuf"); err != nil {
			t.Fatal(err)
		}
		if err := checker.CheckCurrent(context.Background(), 1001, testHubBinding()); errcode.As(err) != errcode.ErrUnavailable {
			t.Fatalf("code=%v err=%v", errcode.As(err), err)
		}
	})

	t.Run("redis-down", func(t *testing.T) {
		mr, checker := newAssignmentChecker(t)
		mr.Close()
		if err := checker.CheckCurrent(context.Background(), 1001, testHubBinding()); errcode.As(err) != errcode.ErrUnavailable {
			t.Fatalf("code=%v err=%v", errcode.As(err), err)
		}
	})
}
