package data

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

func TestRedisBattleTicketAuthorizerLegacyRosterAndZeroMutation(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Unix(1_800_000_000, 0)
	authorizer := NewRedisBattleTicketAuthorizer(rdb, false, 30*time.Second)
	authorizer.now = func() time.Time { return now }
	record := &dsv1.BattleStorageRecord{
		MatchId: 9001, AllocationId: "local-allocation-1", DsPodName: "pandora-battle-local-9001",
		DsAddr: "127.0.0.1:7801", State: "running", PlayerIds: []uint64{1001, 1002},
		LastHeartbeatMs: now.Add(-time.Second).UnixMilli(),
	}
	raw, err := proto.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	key := admissionBattleProjectionKey(record.MatchId)
	if err := rdb.Set(context.Background(), key, raw, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	target, err := authorizer.AuthorizeBattleTicket(context.Background(), 1001, record.MatchId)
	if err != nil {
		t.Fatalf("roster member rejected: %v", err)
	}
	if target.DSAddr != record.DsAddr || target.PodName != record.DsPodName {
		t.Fatalf("target=%+v, want current projection addr/pod", target)
	}
	before, _ := rdb.Get(context.Background(), key).Bytes()
	ttlBefore := mr.TTL(key)
	if _, err := authorizer.AuthorizeBattleTicket(context.Background(), 9999, record.MatchId); errcode.As(err) != errcode.ErrPermissionDeny {
		t.Fatalf("non-member code=%v err=%v", errcode.As(err), err)
	}
	after, _ := rdb.Get(context.Background(), key).Bytes()
	if !bytes.Equal(before, after) || mr.TTL(key) != ttlBefore {
		t.Fatal("rejected ticket authorization mutated projection bytes or TTL")
	}

	record.DsAddr = ""
	setAdmissionProto(t, mr, key, record)
	if _, err := authorizer.AuthorizeBattleTicket(context.Background(), 1001, record.MatchId); errcode.As(err) != errcode.ErrPermissionDeny {
		t.Fatalf("empty target address code=%v err=%v", errcode.As(err), err)
	}
	record.DsAddr = "127.0.0.1:7801"
	record.PlayerIds = nil
	setAdmissionProto(t, mr, key, record)
	if _, err := authorizer.AuthorizeBattleTicket(context.Background(), 1001, record.MatchId); errcode.As(err) != errcode.ErrPermissionDeny {
		t.Fatalf("empty roster code=%v err=%v", errcode.As(err), err)
	}
	record.PlayerIds = []uint64{1001}
	record.LastHeartbeatMs = now.Add(-time.Minute).UnixMilli()
	setAdmissionProto(t, mr, key, record)
	if _, err := authorizer.AuthorizeBattleTicket(context.Background(), 1001, record.MatchId); errcode.As(err) != errcode.ErrPermissionDeny {
		t.Fatalf("stale battle code=%v err=%v", errcode.As(err), err)
	}
}

// TestInspectBattleRouteExplicitThreeState 覆盖 Hub 门三态权威判定(Codex 复审 P0,2026-07-15):
// 只有显式终态(ended/abandoned)返回 TERMINAL;记录缺失、roster 漂移、stale、match 不符
// 一律 UNKNOWN——它们在 AuthorizeBattleTicket 里都折叠成 ErrPermissionDeny,不得当终态证明。
func TestInspectBattleRouteExplicitThreeState(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Unix(1_800_000_000, 0)
	authorizer := NewRedisBattleTicketAuthorizer(rdb, false, 30*time.Second)
	authorizer.now = func() time.Time { return now }
	ctx := context.Background()
	const matchID = uint64(9001)
	key := admissionBattleProjectionKey(matchID)

	assertRoute := func(name string, playerID uint64, wantState BattleRouteState, wantCode errcode.Code) {
		t.Helper()
		state, err := authorizer.InspectBattleRoute(ctx, playerID, matchID)
		if state != wantState {
			t.Fatalf("%s: state=%v, want %v (err=%v)", name, state, wantState, err)
		}
		if wantState == BattleRouteUnknown {
			if errcode.As(err) != wantCode {
				t.Fatalf("%s: code=%v err=%v, want %v", name, errcode.As(err), err, wantCode)
			}
		} else if err != nil {
			t.Fatalf("%s: unexpected err %v", name, err)
		}
	}

	// 记录缺失 → UNKNOWN(可能是终局清理,也可能是 TTL 漂移;无版本化 lease 不可区分)。
	assertRoute("missing projection", 1001, BattleRouteUnknown, errcode.ErrUnavailable)

	record := &dsv1.BattleStorageRecord{
		MatchId: matchID, DsPodName: "pandora-battle-9001", DsAddr: "127.0.0.1:7801",
		State: "running", PlayerIds: []uint64{1001},
		LastHeartbeatMs: now.Add(-time.Second).UnixMilli(),
	}
	setAdmissionProto(t, mr, key, record)

	// live roster 成员 → ACTIVE。
	assertRoute("live member", 1001, BattleRouteActive, 0)
	// running 但玩家非成员(roster 漂移)→ UNKNOWN,绝不可判 TERMINAL。
	assertRoute("roster drift non-member", 9999, BattleRouteUnknown, errcode.ErrUnavailable)

	// stale 心跳 → UNKNOWN(DS 可能崩溃,不能证明终局)。
	record.LastHeartbeatMs = now.Add(-time.Minute).UnixMilli()
	setAdmissionProto(t, mr, key, record)
	assertRoute("stale heartbeat", 1001, BattleRouteUnknown, errcode.ErrUnavailable)

	// 显式终态 ended → TERMINAL(DS 自报终局,与成员身份无关,心跳新鲜度无关,立即放行)。
	record.State = "ended"
	setAdmissionProto(t, mr, key, record)
	assertRoute("ended member", 1001, BattleRouteTerminal, 0)
	assertRoute("ended non-member", 9999, BattleRouteTerminal, 0)
	// ended 即使心跳很新也立即 Terminal(正常结算回大厅不能被屏障拖慢)。
	record.LastHeartbeatMs = now.Add(-time.Second).UnixMilli()
	setAdmissionProto(t, mr, key, record)
	assertRoute("ended fresh heartbeat", 1001, BattleRouteTerminal, 0)

	// abandoned = 心跳超时判死:再入屏障生效(pkg/placement.DSFenceReentryBarrier=27s)。
	// 分区的旧 DS 可能仍有可玩玩家,必须等它的 20s 自我 fencing 上限 + 7s 余量
	// (4s 响应在途 + 1s 检测粒度 + ≥2s 服务间时钟漂移预留)过去。
	record.State = "abandoned"
	record.LastHeartbeatMs = now.Add(-time.Minute).UnixMilli() // 屏障已过 → Terminal
	setAdmissionProto(t, mr, key, record)
	assertRoute("abandoned past barrier", 1001, BattleRouteTerminal, 0)

	record.LastHeartbeatMs = now.Add(-16 * time.Second).UnixMilli() // abandon(15s)刚发生,屏障未过
	setAdmissionProto(t, mr, key, record)
	assertRoute("abandoned inside barrier", 1001, BattleRouteUnknown, errcode.ErrUnavailable)

	// 旧屏障值(25s)必须仍在新屏障内:锁住 2026-07-18 的时钟漂移预留扩容,
	// 任何把余量改回 ≤5s 的回退都会让本用例转为 Terminal 而失败。
	record.LastHeartbeatMs = now.Add(-25 * time.Second).UnixMilli()
	setAdmissionProto(t, mr, key, record)
	assertRoute("abandoned inside skew reserve", 1001, BattleRouteUnknown, errcode.ErrUnavailable)

	record.LastHeartbeatMs = now.Add(-27 * time.Second).UnixMilli() // 恰好到达屏障边界(等于即放行)
	setAdmissionProto(t, mr, key, record)
	assertRoute("abandoned at barrier boundary", 1001, BattleRouteTerminal, 0)

	record.LastHeartbeatMs = 0 // 从未有过成功心跳:DS 从未取得授权租约,不可能有玩家,立即 Terminal
	setAdmissionProto(t, mr, key, record)
	assertRoute("abandoned never heartbeated", 1001, BattleRouteTerminal, 0)
	record.LastHeartbeatMs = now.Add(-time.Minute).UnixMilli()

	// match_id 不符(投影被其它对局覆写)→ UNKNOWN。
	record.State = "ended"
	record.MatchId = 42
	setAdmissionProto(t, mr, key, record)
	assertRoute("match mismatch", 1001, BattleRouteUnknown, errcode.ErrUnavailable)
}

func TestRedisBattleTicketAuthorizerModelBRequiresExactActiveProjection(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	credential, record, projection := validBattleAdmissionState()
	setAdmissionProto(t, mr, admissionBattleAuthKey(credential.MatchID), record)
	setAdmissionProto(t, mr, admissionBattleProjectionKey(credential.MatchID), projection)
	authorizer := NewRedisBattleTicketAuthorizer(rdb, true, 30*time.Second)
	authorizer.now = func() time.Time { return admissionTestNow }
	target, err := authorizer.AuthorizeBattleTicket(context.Background(), 1001, credential.MatchID)
	if err != nil {
		t.Fatalf("active roster member rejected: %v", err)
	}
	if target.DSAddr != projection.DsAddr || target.InstanceUID != credential.InstanceUID ||
		target.InstanceEpoch != credential.ProtocolEpoch {
		t.Fatalf("target=%+v, want current active projection", target)
	}
	before := battleTicketSnapshot(t, rdb, credential.MatchID)
	if _, err := authorizer.AuthorizeBattleTicket(context.Background(), 1001, credential.MatchID); err != nil {
		t.Fatalf("second active roster check rejected: %v", err)
	}
	assertBattleTicketSnapshotEqual(t, before, battleTicketSnapshot(t, rdb, credential.MatchID))

	blankIdentityRecord := proto.Clone(record).(*dsv1.BattleDSAuthStorageRecord)
	blankIdentityProjection := proto.Clone(projection).(*dsv1.BattleStorageRecord)
	blankIdentityRecord.InstanceUid = ""
	blankIdentityRecord.InstanceEpoch = 0
	blankIdentityRecord.Active.InstanceUid = ""
	blankIdentityRecord.Active.InstanceEpoch = 0
	blankIdentityProjection.GameserverUid = ""
	blankIdentityProjection.InstanceEpoch = 0
	setAdmissionProto(t, mr, admissionBattleAuthKey(credential.MatchID), blankIdentityRecord)
	setAdmissionProto(t, mr, admissionBattleProjectionKey(credential.MatchID), blankIdentityProjection)
	if _, err := authorizer.AuthorizeBattleTicket(context.Background(), 1001, credential.MatchID); errcode.As(err) != errcode.ErrPermissionDeny {
		t.Fatalf("blank UID/epoch code=%v err=%v", errcode.As(err), err)
	}
	setAdmissionProto(t, mr, admissionBattleAuthKey(credential.MatchID), record)
	setAdmissionProto(t, mr, admissionBattleProjectionKey(credential.MatchID), projection)

	projection.LastVerifiedJti = "stale-jti"
	setAdmissionProto(t, mr, admissionBattleProjectionKey(credential.MatchID), projection)
	if _, err := authorizer.AuthorizeBattleTicket(context.Background(), 1001, credential.MatchID); errcode.As(err) != errcode.ErrPermissionDeny {
		t.Fatalf("drift code=%v err=%v", errcode.As(err), err)
	}

	setAdmissionProto(t, mr, admissionBattleProjectionKey(credential.MatchID), &dsv1.BattleStorageRecord{
		MatchId: credential.MatchID, State: "running", DsPodName: credential.Pod,
	})
	if _, err := authorizer.AuthorizeBattleTicket(context.Background(), 1001, credential.MatchID); errcode.As(err) != errcode.ErrPermissionDeny {
		t.Fatalf("empty roster code=%v err=%v", errcode.As(err), err)
	}
}

type battleTicketKeySnapshot struct {
	exists bool
	raw    []byte
	pttl   time.Duration
}

type battleTicketStateSnapshot struct {
	auth       battleTicketKeySnapshot
	projection battleTicketKeySnapshot
}

func battleTicketSnapshot(t *testing.T, rdb redis.Cmdable, matchID uint64) battleTicketStateSnapshot {
	t.Helper()
	return battleTicketStateSnapshot{
		auth:       battleTicketReadKeySnapshot(t, rdb, admissionBattleAuthKey(matchID)),
		projection: battleTicketReadKeySnapshot(t, rdb, admissionBattleProjectionKey(matchID)),
	}
}

func battleTicketReadKeySnapshot(t *testing.T, rdb redis.Cmdable, key string) battleTicketKeySnapshot {
	t.Helper()
	ctx := context.Background()
	raw, err := rdb.Get(ctx, key).Bytes()
	exists := true
	if err == redis.Nil {
		exists = false
		raw = nil
	} else if err != nil {
		t.Fatalf("snapshot GET %s: %v", key, err)
	}
	pttl, err := rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("snapshot PTTL %s: %v", key, err)
	}
	return battleTicketKeySnapshot{exists: exists, raw: append([]byte(nil), raw...), pttl: pttl}
}

func assertBattleTicketSnapshotEqual(t *testing.T, before, after battleTicketStateSnapshot) {
	t.Helper()
	assertKey := func(name string, want, got battleTicketKeySnapshot) {
		t.Helper()
		if want.exists != got.exists || !bytes.Equal(want.raw, got.raw) || want.pttl != got.pttl {
			t.Fatalf("%s changed by read-only authorization: before={exists:%v bytes:%x pttl:%s} after={exists:%v bytes:%x pttl:%s}",
				name, want.exists, want.raw, want.pttl, got.exists, got.raw, got.pttl)
		}
	}
	assertKey("auth", before.auth, after.auth)
	assertKey("projection", before.projection, after.projection)
}

func writeBattleTicketRaw(t *testing.T, rdb redis.Cmdable, key string, raw []byte, ttl time.Duration) {
	t.Helper()
	if err := rdb.Set(context.Background(), key, raw, ttl).Err(); err != nil {
		t.Fatalf("SET %s: %v", key, err)
	}
}

func marshalBattleTicketProto(t *testing.T, message proto.Message) []byte {
	t.Helper()
	raw, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRedisBattleTicketAuthorizerModelBRejectMatrixZeroMutation(t *testing.T) {
	type testCase struct {
		name              string
		wantCode          errcode.Code
		mutate            func(*dsv1.BattleDSAuthStorageRecord, *dsv1.BattleStorageRecord)
		missingAuth       bool
		missingProjection bool
		badAuth           bool
		badProjection     bool
		redisFailure      bool
		persistent        bool
		playerID          uint64
	}
	pendingWithWriter := func(record *dsv1.BattleDSAuthStorageRecord, writer uint32) {
		pending := proto.Clone(record.GetActive()).(*dsv1.BattleDSCredential)
		pending.Gen++
		pending.Jti = "pending-credential-jti"
		pending.WriterEpoch = writer
		record.Pending = pending
		record.HighWaterGen = pending.Gen
		record.Phase = dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_ROTATING
	}
	tests := []testCase{
		{name: "auth-missing", wantCode: errcode.ErrPermissionDeny, missingAuth: true},
		{name: "projection-missing", wantCode: errcode.ErrPermissionDeny, missingProjection: true},
		{name: "auth-bad-protobuf", wantCode: errcode.ErrUnavailable, badAuth: true},
		{name: "projection-bad-protobuf", wantCode: errcode.ErrUnavailable, badProjection: true},
		{name: "redis-client-failure", wantCode: errcode.ErrUnavailable, redisFailure: true},
		{name: "phase-unspecified", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.Phase = dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_UNSPECIFIED
		}},
		{name: "phase-bootstrap", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.Phase = dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_BOOTSTRAP
		}},
		{name: "phase-quarantined", wantCode: errcode.ErrPermissionDeny, persistent: true, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.Phase = dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_QUARANTINED
		}},
		{name: "phase-terminating-persistent-tombstone", wantCode: errcode.ErrPermissionDeny, persistent: true, mutate: func(r *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			r.Phase = dsv1.BattleAuthPhase_BATTLE_AUTH_PHASE_TERMINATING
			p.State = "ended"
		}},
		{name: "match-mismatch", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.MatchId++
		}},
		{name: "allocation-empty", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.AllocationId = ""
		}},
		{name: "allocation-mismatch", wantCode: errcode.ErrPermissionDeny, mutate: func(_ *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			p.AllocationId = "other-allocation"
		}},
		{name: "pod-mismatch", wantCode: errcode.ErrPermissionDeny, mutate: func(_ *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			p.DsPodName = "other-pod"
		}},
		{name: "target-address-empty", wantCode: errcode.ErrPermissionDeny, mutate: func(_ *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			p.DsAddr = ""
		}},
		{name: "uid-empty", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			r.InstanceUid, r.Active.InstanceUid, p.GameserverUid = "", "", ""
		}},
		{name: "uid-mismatch", wantCode: errcode.ErrPermissionDeny, mutate: func(_ *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			p.GameserverUid = "rebuilt-uid"
		}},
		{name: "active-uid-mismatch", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.Active.InstanceUid = "other-active-uid"
		}},
		{name: "epoch-zero", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			r.InstanceEpoch, r.Active.InstanceEpoch, p.InstanceEpoch = 0, 0, 0
		}},
		{name: "epoch-mismatch", wantCode: errcode.ErrPermissionDeny, mutate: func(_ *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			p.InstanceEpoch++
		}},
		{name: "active-epoch-mismatch", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.Active.InstanceEpoch++
		}},
		{name: "required-writer-legacy", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.RequiredWriterEpoch = 1
		}},
		{name: "required-writer-future", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.RequiredWriterEpoch = auth.DSAuthWriterEpochV2 + 1
		}},
		{name: "active-writer-legacy", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.Active.WriterEpoch = 1
		}},
		{name: "pending-writer-legacy", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			pendingWithWriter(r, 1)
		}},
		{name: "projection-writer-legacy", wantCode: errcode.ErrPermissionDeny, mutate: func(_ *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			p.LastVerifiedWriterEpoch = 1
		}},
		{name: "high-water-behind-active", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.HighWaterGen = r.Active.Gen - 1
		}},
		{name: "active-missing", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.Active = nil
		}},
		{name: "active-gen-zero", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.Active.Gen = 0
		}},
		{name: "active-jti-empty", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.Active.Jti = ""
		}},
		{name: "active-kid-empty", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.Active.Kid = ""
		}},
		{name: "active-hash-empty", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.Active.TokenSha256 = ""
		}},
		{name: "active-expired", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.Active.ExpMs = uint64(admissionTestNow.Add(-time.Millisecond).UnixMilli())
		}},
		{name: "auth-heartbeat-stale", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, _ *dsv1.BattleStorageRecord) {
			r.LastActiveHeartbeatMs = admissionTestNow.Add(-time.Minute).UnixMilli()
		}},
		{name: "auth-heartbeat-future", wantCode: errcode.ErrPermissionDeny, mutate: func(r *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			r.LastActiveHeartbeatMs = admissionTestNow.Add(time.Second).UnixMilli()
			p.LastHeartbeatMs = r.LastActiveHeartbeatMs
		}},
		{name: "projection-heartbeat-stale", wantCode: errcode.ErrPermissionDeny, mutate: func(_ *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			p.LastHeartbeatMs = admissionTestNow.Add(-time.Minute).UnixMilli()
		}},
		{name: "projection-heartbeat-future", wantCode: errcode.ErrPermissionDeny, mutate: func(_ *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			p.LastHeartbeatMs = admissionTestNow.Add(time.Second).UnixMilli()
		}},
		{name: "heartbeat-mismatch", wantCode: errcode.ErrPermissionDeny, mutate: func(_ *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			p.LastHeartbeatMs = admissionTestNow.Add(-2 * time.Second).UnixMilli()
		}},
		{name: "last-verified-gen-mismatch", wantCode: errcode.ErrPermissionDeny, mutate: func(_ *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			p.LastVerifiedGen++
		}},
		{name: "last-verified-jti-mismatch", wantCode: errcode.ErrPermissionDeny, mutate: func(_ *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			p.LastVerifiedJti = "stale-jti"
		}},
		{name: "empty-roster", wantCode: errcode.ErrPermissionDeny, mutate: func(_ *dsv1.BattleDSAuthStorageRecord, p *dsv1.BattleStorageRecord) {
			p.PlayerIds = nil
		}},
		{name: "player-not-in-roster", wantCode: errcode.ErrPermissionDeny, playerID: 9999},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			healthyRDB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = healthyRDB.Close() })
			credential, record, projection := validBattleAdmissionState()
			if tc.mutate != nil {
				tc.mutate(record, projection)
			}
			authRaw := marshalBattleTicketProto(t, record)
			projectionRaw := marshalBattleTicketProto(t, projection)
			if tc.badAuth {
				authRaw = []byte{0x0a}
			}
			if tc.badProjection {
				projectionRaw = []byte{0x0a}
			}
			ttl := time.Hour
			if tc.persistent {
				ttl = 0
			}
			if !tc.missingAuth {
				writeBattleTicketRaw(t, healthyRDB, admissionBattleAuthKey(credential.MatchID), authRaw, ttl)
			}
			if !tc.missingProjection {
				writeBattleTicketRaw(t, healthyRDB, admissionBattleProjectionKey(credential.MatchID), projectionRaw, ttl)
			}
			before := battleTicketSnapshot(t, healthyRDB, credential.MatchID)
			if tc.persistent && (!before.auth.exists || !before.projection.exists ||
				before.auth.pttl >= 0 || before.projection.pttl >= 0) {
				t.Fatalf("persistent terminal/quarantine fixture does not have PTTL=-1: %+v", before)
			}

			authorityRDB := redis.UniversalClient(healthyRDB)
			if tc.redisFailure {
				failedRDB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
				if err := failedRDB.Close(); err != nil {
					t.Fatal(err)
				}
				authorityRDB = failedRDB
			}
			authorizer := NewRedisBattleTicketAuthorizer(authorityRDB, true, 30*time.Second)
			authorizer.now = func() time.Time { return admissionTestNow }
			playerID := tc.playerID
			if playerID == 0 {
				playerID = 1001
			}
			_, err := authorizer.AuthorizeBattleTicket(context.Background(), playerID, credential.MatchID)
			if errcode.As(err) != tc.wantCode {
				t.Fatalf("code=%v err=%v, want %v", errcode.As(err), err, tc.wantCode)
			}
			assertBattleTicketSnapshotEqual(t, before, battleTicketSnapshot(t, healthyRDB, credential.MatchID))
		})
	}
}
