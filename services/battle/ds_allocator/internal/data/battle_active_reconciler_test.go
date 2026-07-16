package data

import (
	"context"
	"testing"
	"time"

	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/redis/go-redis/v9"
)

func TestActiveIndexRequiredFailClosed(t *testing.T) {
	active := []string{
		"allocating", "warming", "ready", "running",
		BattleStateAllocationUncertain,
		BattleStateAllocationReconcileReleasePending,
		BattleStateAllocationReconcileEmptyTombstone,
		BattleStatePreactiveReleasePending,
		BattleStateAllocationAbortPending,
	}
	for _, state := range active {
		required, err := activeIndexRequired(state, false)
		if err != nil || !required {
			t.Fatalf("state %q: required=%t err=%v", state, required, err)
		}
	}
	if required, err := activeIndexRequired("ended", true); err != nil || required {
		t.Fatalf("ended: required=%t err=%v", required, err)
	}
	if required, err := activeIndexRequired("abandoned", false); err != nil || required {
		t.Fatalf("retained abandoned: required=%t err=%v", required, err)
	}
	if required, err := activeIndexRequired("abandoned", true); err != nil || !required {
		t.Fatalf("persistent abandoned: required=%t err=%v", required, err)
	}
	if _, err := activeIndexRequired("future_state", true); err == nil {
		t.Fatal("unknown canonical state must fail closed")
	}
}

func TestReconcileBattleActiveIndexRestoresLostCanonicalCandidates(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)

	ended := sampleBattle(205, 1005)
	ended.State = "ended"
	records := []*dsv1.BattleStorageRecord{
		sampleBattle(201, 1001),
		{MatchId: 202, State: BattleStateAllocationUncertain, AllocationId: "10000000-0000-4000-8000-000000000202", LastHeartbeatMs: 1002},
		{MatchId: 203, State: BattleStateAllocationReconcileEmptyTombstone, AllocationId: "10000000-0000-4000-8000-000000000203", LastHeartbeatMs: 1003},
		{MatchId: 204, State: "abandoned", AllocationId: "10000000-0000-4000-8000-000000000204", LastHeartbeatMs: 1004},
		ended,
		{MatchId: 206, State: "abandoned", AllocationId: "10000000-0000-4000-8000-000000000206", LastHeartbeatMs: 1006},
	}
	for _, record := range records {
		if err := repo.CreateBattle(ctx, record, time.Hour); err != nil {
			t.Fatalf("create %d: %v", record.GetMatchId(), err)
		}
	}
	// Model-B recovery fences are permanent. A terminal retained audit has a
	// TTL and must not be resurrected; the persistent abandoned handoff must.
	for _, id := range []uint64{202, 203, 204} {
		if err := repo.rdb.Persist(ctx, battleKey(id)).Err(); err != nil {
			t.Fatalf("persist %d: %v", id, err)
		}
	}
	if err := repo.rdb.Del(ctx, activeKey).Err(); err != nil {
		t.Fatal(err)
	}

	if err := repo.ReconcileBattleActiveIndex(ctx, 2); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, err := repo.RangeActiveBattles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{201, 202, 203, 204}
	if len(got) != len(want) {
		t.Fatalf("active=%v want=%v", got, want)
	}
	seen := make(map[uint64]bool, len(got))
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("active=%v missing=%d", got, id)
		}
	}
	if mr.Exists(activeKey) == false {
		t.Fatal("active index was not rebuilt")
	}
}

func TestReconcileBattleActiveIndexCorruptCanonicalRecordFailsClosed(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	if err := repo.rdb.Set(ctx, battleKey(301), []byte("not-protobuf"), 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReconcileBattleActiveIndex(ctx, 128); err == nil {
		t.Fatal("corrupt canonical record must fail reconciliation")
	}
	ids, err := repo.RangeActiveBattles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("partial active index write on failed scan: %v", ids)
	}
}

func TestReconcileBattleActiveIndexPreservesExistingImmediateRecoveryScore(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	record := &dsv1.BattleStorageRecord{
		MatchId: 401, State: "abandoned", AllocationId: "10000000-0000-4000-8000-000000000401",
		LastHeartbeatMs: time.Now().UnixMilli(),
	}
	if err := repo.CreateBattle(ctx, record, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := repo.rdb.Persist(ctx, battleKey(401)).Err(); err != nil {
		t.Fatal(err)
	}
	// score=0 is an intentional durable wake-up written by quarantine/release
	// transitions. Reconciliation repairs missing members only; overwriting an
	// existing score from the projection would postpone required cleanup.
	if err := repo.rdb.ZAdd(ctx, activeKey, redis.Z{Score: 0, Member: 401}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReconcileBattleActiveIndex(ctx, 128); err != nil {
		t.Fatal(err)
	}
	score, err := repo.rdb.ZScore(ctx, activeKey, "401").Result()
	if err != nil || score != 0 {
		t.Fatalf("score=%v err=%v, want existing immediate score 0", score, err)
	}
}
