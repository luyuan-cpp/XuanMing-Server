package data

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

func exactInvariantBattle(matchID uint64, state string) *dsv1.BattleStorageRecord {
	return &dsv1.BattleStorageRecord{
		MatchId: matchID, State: state, AllocationId: "91000000-0000-4000-8000-000000000001",
		DsPodName: "pod-invariant", DsAddr: "10.0.0.1:7777",
		GameserverUid: "gameserver-uid-invariant", PodUid: "pod-uid-invariant",
		ReleaseTrack: "stable", InstanceEpoch: 1,
		AllocatedAtMs: 10, LastHeartbeatMs: 20,
	}
}

func seedRawBattleForInvariant(t *testing.T, repo *RedisBattleRepo, record *dsv1.BattleStorageRecord, ttl time.Duration) []byte {
	t.Helper()
	payload, err := proto.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.rdb.Set(context.Background(), battleKey(record.GetMatchId()), payload, ttl).Err(); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestBattleStorageWriteRejectsPartialPhysicalIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*dsv1.BattleStorageRecord){
		"missing pod name":        func(b *dsv1.BattleStorageRecord) { b.DsPodName = "" },
		"missing gameserver uid":  func(b *dsv1.BattleStorageRecord) { b.GameserverUid = "" },
		"missing pod uid":         func(b *dsv1.BattleStorageRecord) { b.PodUid = "" },
		"missing release track":   func(b *dsv1.BattleStorageRecord) { b.ReleaseTrack = "" },
		"noncanonical pod uid":    func(b *dsv1.BattleStorageRecord) { b.PodUid = " pod-uid" },
		"unsupported exact state": func(b *dsv1.BattleStorageRecord) { b.State = "future_state" },
		"non RFC4122 UUIDv4": func(b *dsv1.BattleStorageRecord) {
			b.AllocationId = "91000000-0000-4000-0000-000000000001"
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := exactInvariantBattle(9101, "running")
			mutate(candidate)
			if err := validateBattleStorageWrite(candidate); err == nil {
				t.Fatalf("unsafe candidate accepted: %+v", candidate)
			}
		})
	}

	for _, state := range []string{"allocating", BattleStateAllocationUncertain,
		BattleStateAllocationReconcileEmptyTombstone} {
		candidate := exactInvariantBattle(9102, state)
		if err := validateBattleStorageWrite(candidate); err == nil {
			t.Fatalf("identity-bearing %s accepted", state)
		}
	}
}

func TestBattleStorageCreationEntriesRejectUnsafeIdentityWithoutRedisSideEffect(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)

	claim := &dsv1.BattleStorageRecord{
		MatchId: 9110, State: "allocating", AllocationId: "91100000-0000-4000-8000-000000000001",
		DsPodName: "partial-pod",
	}
	if claimed, _, err := repo.ClaimBattle(ctx, claim, testTTL); err == nil || claimed {
		t.Fatalf("partial claim accepted: claimed=%v err=%v", claimed, err)
	}
	if mr.Exists(battleKey(9110)) {
		t.Fatal("rejected claim wrote Redis")
	}

	create := exactInvariantBattle(9111, "ready")
	create.PodUid = ""
	if err := repo.CreateBattle(ctx, create, testTTL); err == nil {
		t.Fatal("legacy/partial CreateBattle accepted")
	}
	if mr.Exists(battleKey(9111)) {
		t.Fatal("rejected create wrote Redis")
	}

	withUnknown := exactInvariantBattle(9112, "ready")
	withUnknown.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	if err := repo.CreateBattle(ctx, withUnknown, testTTL); err == nil {
		t.Fatal("new record carrying protobuf unknown fields was accepted")
	}
	if mr.Exists(battleKey(9112)) {
		t.Fatal("rejected unknown-field create wrote Redis")
	}
}

func TestStrictModelBWriteGateIsIrreversibleAndFailsClosedOnLegacyShape(t *testing.T) {
	ctx := context.Background()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := NewRedisBattleRepo(rdb)
	legacy := &dsv1.BattleStorageRecord{
		MatchId: 9113, State: "warming", AllocationId: "91130000-0000-4000-8000-000000000001",
		DsPodName: "legacy-local-pod", DsAddr: "127.0.0.1:7777",
	}
	if err := repo.CreateBattle(ctx, legacy, testTTL); err != nil {
		t.Fatalf("legacy/local writer unexpectedly strict before activation: %v", err)
	}
	if repo.StrictModelBWritesEnabled() {
		t.Fatal("strict gate enabled before Model-B activation")
	}

	repo.EnableStrictModelBWrites()
	repo.EnableStrictModelBWrites()
	if !repo.StrictModelBWritesEnabled() {
		t.Fatal("strict gate did not remain enabled")
	}
	before, err := rdb.Get(ctx, battleKey(legacy.GetMatchId())).Bytes()
	if err != nil {
		t.Fatal(err)
	}
	err = repo.UpdateBattleKeepTTL(ctx, legacy.GetMatchId(), 3, func(b *dsv1.BattleStorageRecord) error {
		b.LastHeartbeatMs++
		return nil
	})
	if err == nil {
		t.Fatal("strict gate wrote a preflight-unsafe legacy record")
	}
	after, getErr := rdb.Get(ctx, battleKey(legacy.GetMatchId())).Bytes()
	if getErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("strict rejection mutated legacy record: err=%v", getErr)
	}
}

func TestBattleStoragePodUIDCannotBeClearedOrRebound(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	record := exactInvariantBattle(9120, "running")
	if err := repo.CreateBattle(ctx, record, testTTL); err != nil {
		t.Fatal(err)
	}

	for _, replacement := range []string{"", "pod-uid-other"} {
		before, err := repo.rdb.Get(ctx, battleKey(record.GetMatchId())).Bytes()
		if err != nil {
			t.Fatal(err)
		}
		err = repo.UpdateBattleWithLock(ctx, record.GetMatchId(), 3, func(b *dsv1.BattleStorageRecord) error {
			b.PodUid = replacement
			return nil
		}, testTTL)
		if err == nil {
			t.Fatalf("pod_uid replacement %q accepted", replacement)
		}
		after, getErr := repo.rdb.Get(ctx, battleKey(record.GetMatchId())).Bytes()
		if getErr != nil || !bytes.Equal(before, after) {
			t.Fatalf("rejected pod_uid replacement mutated record: err=%v", getErr)
		}
	}
}

func TestBattleStorageNormalTransitionCannotDropOrAlterUnknownFields(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mutate func(*dsv1.BattleStorageRecord)
	}{
		{name: "drop", mutate: func(b *dsv1.BattleStorageRecord) { b.ProtoReflect().SetUnknown(nil) }},
		{name: "alter", mutate: func(b *dsv1.BattleStorageRecord) {
			b.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x02})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := newRepo(t)
			record := exactInvariantBattle(9121, "running")
			record.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
			seedRawBattleForInvariant(t, repo, record, testTTL)
			before, err := repo.rdb.Get(ctx, battleKey(record.GetMatchId())).Bytes()
			if err != nil {
				t.Fatal(err)
			}
			err = repo.UpdateBattleKeepTTL(ctx, record.GetMatchId(), 3, func(b *dsv1.BattleStorageRecord) error {
				tc.mutate(b)
				return nil
			})
			if err == nil {
				t.Fatal("unknown-field mutation accepted")
			}
			after, getErr := repo.rdb.Get(ctx, battleKey(record.GetMatchId())).Bytes()
			if getErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("unknown-field rejection mutated Redis: err=%v", getErr)
			}
		})
	}
}

func TestLegacyBattleOnlyAllowsExactPodUIDBackfillAndPreservesUnknownFields(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	legacy := exactInvariantBattle(9130, "running")
	legacy.PodUid = ""
	// Unknown field 100, varint 1. The exact-backfill gate compares and retains
	// unknown bytes so a rolling writer cannot silently discard a future field.
	legacy.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
	seedRawBattleForInvariant(t, repo, legacy, testTTL)
	originalTTL := mr.TTL(battleKey(legacy.GetMatchId()))

	if err := repo.UpdateBattleKeepTTL(ctx, legacy.GetMatchId(), 3, func(b *dsv1.BattleStorageRecord) error {
		b.PodUid = "pod-uid-resolved"
		return nil
	}); err != nil {
		t.Fatalf("exact legacy backfill: %v", err)
	}
	got, found, err := repo.GetBattle(ctx, legacy.GetMatchId())
	if err != nil || !found || got.GetPodUid() != "pod-uid-resolved" {
		t.Fatalf("backfilled record: found=%v got=%+v err=%v", found, got, err)
	}
	if !bytes.Equal(got.ProtoReflect().GetUnknown(), legacy.ProtoReflect().GetUnknown()) {
		t.Fatal("exact backfill discarded unknown fields")
	}
	if ttl := mr.TTL(battleKey(legacy.GetMatchId())); ttl != originalTTL {
		t.Fatalf("exact backfill changed TTL: got=%v want=%v", ttl, originalTTL)
	}
}

func TestLegacyBattleRejectsStateAdvanceWritebackAndPartialRepair(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	for _, tc := range []struct {
		name   string
		match  uint64
		mutate func(*dsv1.BattleStorageRecord)
	}{
		{
			name: "state advance with backfill", match: 9140,
			mutate: func(b *dsv1.BattleStorageRecord) { b.PodUid, b.State = "pod-uid-resolved", "ended" },
		},
		{
			name: "heartbeat writeback", match: 9141,
			mutate: func(b *dsv1.BattleStorageRecord) { b.LastHeartbeatMs++ },
		},
		{
			name: "other identity change with backfill", match: 9142,
			mutate: func(b *dsv1.BattleStorageRecord) {
				b.PodUid, b.GameserverUid = "pod-uid-resolved", "gameserver-other"
			},
		},
		{
			name: "unknown bytes reordered with backfill", match: 9144,
			mutate: func(b *dsv1.BattleStorageRecord) {
				b.PodUid = "pod-uid-resolved"
				// Same unknown fields, different raw order. proto.Equal treats
				// these as equivalent, but a rolling writer must preserve the
				// original bytes exactly.
				b.ProtoReflect().SetUnknown([]byte{0xa8, 0x06, 0x02, 0xa0, 0x06, 0x01})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy := exactInvariantBattle(tc.match, "running")
			legacy.PodUid = ""
			if tc.match == 9144 {
				legacy.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01, 0xa8, 0x06, 0x02})
			}
			before := seedRawBattleForInvariant(t, repo, legacy, testTTL)
			err := repo.UpdateBattleKeepTTL(ctx, tc.match, 3, func(b *dsv1.BattleStorageRecord) error {
				tc.mutate(b)
				return nil
			})
			if err == nil {
				t.Fatal("unsafe legacy write accepted")
			}
			after, getErr := repo.rdb.Get(ctx, battleKey(tc.match)).Bytes()
			if getErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected legacy write mutated Redis: err=%v", getErr)
			}
		})
	}

	partial := exactInvariantBattle(9143, "running")
	partial.PodUid = ""
	partial.GameserverUid = ""
	before := seedRawBattleForInvariant(t, repo, partial, testTTL)
	err := repo.UpdateBattleKeepTTL(ctx, partial.GetMatchId(), 3, func(b *dsv1.BattleStorageRecord) error {
		b.PodUid = "pod-uid-resolved"
		return nil
	})
	if err == nil {
		t.Fatal("partial legacy identity repair accepted")
	}
	after, getErr := repo.rdb.Get(ctx, battleKey(partial.GetMatchId())).Bytes()
	if getErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("partial repair mutated Redis: err=%v", getErr)
	}
}

func TestAuthTransactionMutationGateRejectsLegacyRecord(t *testing.T) {
	ctx := context.Background()
	f := newBattleAuthFixture(t)
	const matchID = uint64(9150)
	legacy := exactInvariantBattle(matchID, "warming")
	legacy.PodUid = ""
	legacy.InstanceEpoch = 0
	before := seedRawBattleForInvariant(t, f.battle, legacy, testTTL)
	fenced, err := f.auth.FencePreactiveReleaseExpected(ctx, matchID, BattleExpectedInstance{
		AllocationID: legacy.GetAllocationId(), InstanceUID: legacy.GetGameserverUid(),
	})
	if err == nil || fenced {
		t.Fatalf("legacy auth transaction accepted: fenced=%v err=%v", fenced, err)
	}
	after, getErr := f.rdb.Get(ctx, battleKey(matchID)).Bytes()
	if getErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected auth transaction mutated Redis: err=%v", getErr)
	}
}

func TestQuarantineMutationGateRejectsLegacyMissingPodUIDWithZeroSideEffect(t *testing.T) {
	ctx := context.Background()
	f := newBattleAuthFixture(t)
	const matchID = uint64(9151)
	const allocationID = "91510000-0000-4000-8000-000000000001"
	seedModelBBattle(t, f, matchID, allocationID, "pod-quarantine-invariant", "uid-quarantine-invariant")
	_, identity := prepareAndStage(t, f, matchID, allocationID,
		"pod-quarantine-invariant", "uid-quarantine-invariant", true)
	if _, err := f.auth.ActivateHeartbeat(ctx, matchID, identity, activateInput()); err != nil {
		t.Fatal(err)
	}

	// Reproduce the rolling-upgrade shape that the standalone quarantine
	// command used to rewrite while its strict gate was left disabled.
	battle, found, err := f.battle.GetBattle(ctx, matchID)
	if err != nil || !found {
		t.Fatalf("read battle: found=%v err=%v", found, err)
	}
	battle.PodUid = ""
	legacyRaw, err := proto.Marshal(battle)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.rdb.Set(ctx, battleKey(matchID), legacyRaw, testTTL).Err(); err != nil {
		t.Fatal(err)
	}

	authBefore := rawRedisBytes(t, f.rdb, battleAuthKey(matchID))
	battleBefore := rawRedisBytes(t, f.rdb, battleKey(matchID))
	authTTLBefore := f.mr.TTL(battleAuthKey(matchID))
	battleTTLBefore := f.mr.TTL(battleKey(matchID))
	activeScoreBefore, scoreErr := f.rdb.ZScore(ctx, activeKey, fmt.Sprint(matchID)).Result()
	if scoreErr != nil {
		t.Fatal(scoreErr)
	}

	result, err := f.auth.QuarantineExpected(ctx, matchID, BattleQuarantineExpected{
		AllocationID: allocationID,
		Credential:   identity,
	}, testTTL, testTTL)
	if err == nil || result.AuthQuarantined || result.ProjectionAbandoned {
		t.Fatalf("legacy quarantine accepted: result=%+v err=%v", result, err)
	}
	activeScoreAfter, scoreErr := f.rdb.ZScore(ctx, activeKey, fmt.Sprint(matchID)).Result()
	if scoreErr != nil || activeScoreAfter != activeScoreBefore ||
		!bytes.Equal(authBefore, rawRedisBytes(t, f.rdb, battleAuthKey(matchID))) ||
		!bytes.Equal(battleBefore, rawRedisBytes(t, f.rdb, battleKey(matchID))) ||
		f.mr.TTL(battleAuthKey(matchID)) != authTTLBefore ||
		f.mr.TTL(battleKey(matchID)) != battleTTLBefore {
		t.Fatalf("rejected quarantine mutated Redis: score=%v/%v score_err=%v", activeScoreBefore, activeScoreAfter, scoreErr)
	}
}
