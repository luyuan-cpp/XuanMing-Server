// battle_test.go — ds_allocator data 层 Redis 实现测试(miniredis)。
package data

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

type loseSuccessfulWatchResponseClient struct {
	redis.UniversalClient
	remaining    atomic.Int32
	afterSuccess func()
}

func (c *loseSuccessfulWatchResponseClient) Watch(
	ctx context.Context,
	fn func(*redis.Tx) error,
	keys ...string,
) error {
	err := c.UniversalClient.Watch(ctx, fn, keys...)
	if err == nil && c.remaining.CompareAndSwap(1, 0) {
		if c.afterSuccess != nil {
			c.afterSuccess()
		}
		return errors.New("simulated EXEC response lost after commit")
	}
	return err
}

const testTTL = 2 * time.Hour

func newRepo(t *testing.T) (*RedisBattleRepo, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisBattleRepo(rdb), mr
}

func sampleBattle(matchID uint64, lastHbMs int64) *dsv1.BattleStorageRecord {
	return &dsv1.BattleStorageRecord{
		MatchId:         matchID,
		DsPodName:       "pandora-battle-1",
		DsAddr:          "127.0.0.1:30001",
		State:           "ready",
		PlayerIds:       []uint64{10, 20, 30},
		MapId:           1,
		GameMode:        "5v5_ranked",
		AllocatedAtMs:   1000,
		LastHeartbeatMs: lastHbMs,
		PlayerCount:     3,
	}
}

func TestCreateGetRoundtrip(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	if err := repo.CreateBattle(ctx, sampleBattle(1, 5000), testTTL); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, found, err := repo.GetBattle(ctx, 1)
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.DsAddr != "127.0.0.1:30001" || got.PlayerCount != 3 || len(got.PlayerIds) != 3 {
		t.Fatalf("battle mismatch: %+v", got)
	}

	// active ZSET 应含 match 1
	ids, err := repo.RangeActiveBattles(ctx)
	if err != nil {
		t.Fatalf("range active: %v", err)
	}
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("active = %v, want [1]", ids)
	}
}

func TestGetMissReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	if _, found, err := repo.GetBattle(ctx, 999); err != nil || found {
		t.Fatalf("expected not found, got found=%v err=%v", found, err)
	}
}

func TestRangeStaleBattles(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	// match 1 心跳 1000(旧),match 2 心跳 9000(新)
	if err := repo.CreateBattle(ctx, sampleBattle(1, 1000), testTTL); err != nil {
		t.Fatalf("create 1: %v", err)
	}
	if err := repo.CreateBattle(ctx, sampleBattle(2, 9000), testTTL); err != nil {
		t.Fatalf("create 2: %v", err)
	}

	// 阈值 5000:只有 match 1(1000 ≤ 5000)算超时
	stale, err := repo.RangeStaleBattles(ctx, 5000)
	if err != nil {
		t.Fatalf("range stale: %v", err)
	}
	if len(stale) != 1 || stale[0] != 1 {
		t.Fatalf("stale = %v, want [1]", stale)
	}
}

func TestUpdateBattleWithLock(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	if err := repo.CreateBattle(ctx, sampleBattle(1, 1000), testTTL); err != nil {
		t.Fatalf("create: %v", err)
	}

	err := repo.UpdateBattleWithLock(ctx, 1, 3, func(b *dsv1.BattleStorageRecord) error {
		b.State = "running"
		b.LastHeartbeatMs = 8000
		b.PlayerCount = 10
		return nil
	}, testTTL)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	got, _, _ := repo.GetBattle(ctx, 1)
	if got.State != "running" || got.PlayerCount != 10 {
		t.Fatalf("after update: %+v", got)
	}
	// active ZSET score 应刷新到 8000
	stale, _ := repo.RangeStaleBattles(ctx, 5000)
	if len(stale) != 0 {
		t.Fatalf("after heartbeat refresh, stale should be empty, got %v", stale)
	}
}

func TestUpdateBattleWithLockNotFound(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	err := repo.UpdateBattleWithLock(ctx, 42, 3, func(*dsv1.BattleStorageRecord) error { return nil }, testTTL)
	if errcode.As(err) != errcode.ErrDSPodNotFound {
		t.Fatalf("expected ErrDSPodNotFound, got %v", err)
	}
}

func TestDeleteBattle(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	if err := repo.CreateBattle(ctx, sampleBattle(1, 1000), testTTL); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.DeleteBattle(ctx, 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := repo.GetBattle(ctx, 1); found {
		t.Fatal("battle 1 should be deleted")
	}
	ids, _ := repo.RangeActiveBattles(ctx)
	if len(ids) != 0 {
		t.Fatalf("active should be empty, got %v", ids)
	}
}

func TestClaimBattleTrueConcurrencySingleWinner(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	const goroutines = 32
	var (
		wg      sync.WaitGroup
		winners int
		mu      sync.Mutex
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claim := &dsv1.BattleStorageRecord{
				MatchId: 77, State: "allocating", AllocationId: fmt.Sprintf("claim-%d", i),
				AllocatedAtMs: 1000, LastHeartbeatMs: 1000,
			}
			claimed, _, err := repo.ClaimBattle(ctx, claim, testTTL)
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			if claimed {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("claim winners=%d, want exactly 1", winners)
	}
}

func TestFenceBattleAllocationPersistsUntilStrictFencedFinalize(t *testing.T) {
	ctx := context.Background()
	repo, mr := newRepo(t)
	claim := &dsv1.BattleStorageRecord{
		MatchId: 78, State: "allocating", AllocationId: "alloc-78",
		AllocatedAtMs: 1000, LastHeartbeatMs: 1000,
	}
	// 新增 RMW 路径必须保留未来版本写入的 unknown fields，满足滚动更新兼容红线。
	futureWire := []byte{0xa0, 0x06, 0x07} // field 100(varint)=7
	claim.ProtoReflect().SetUnknown(futureWire)
	if claimed, _, err := repo.ClaimBattle(ctx, claim, testTTL); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if ttl := mr.TTL(battleKey(78)); ttl <= 0 {
		t.Fatalf("claim should initially expire, ttl=%s", ttl)
	}
	if fenced, err := repo.FenceBattleAllocation(ctx, 78, "stale-allocation"); err != nil || fenced {
		t.Fatalf("stale fence: fenced=%v err=%v", fenced, err)
	}
	if fenced, err := repo.FenceBattleAllocation(ctx, 78, "alloc-78"); err != nil || !fenced {
		t.Fatalf("owner fence: fenced=%v err=%v", fenced, err)
	}
	got, found, err := repo.GetBattle(ctx, 78)
	if err != nil || !found || got.GetState() != BattleStateAllocationUncertain {
		t.Fatalf("fenced record: found=%v got=%+v err=%v", found, got, err)
	}
	if ttl := mr.TTL(battleKey(78)); ttl != 0 {
		t.Fatalf("fenced claim must be persistent, ttl=%s", ttl)
	}
	if unknown := got.ProtoReflect().GetUnknown(); string(unknown) != string(futureWire) {
		t.Fatalf("fence discarded future protobuf fields: got=%x want=%x", unknown, futureWire)
	}
	if deleted, err := repo.DeleteBattleIfAllocationMatches(ctx, 78, "alloc-78", ""); err != nil || deleted {
		t.Fatalf("uncertain claim must reject cleanup: deleted=%v err=%v", deleted, err)
	}
	warming := proto.Clone(got).(*dsv1.BattleStorageRecord)
	warming.State, warming.DsPodName, warming.DsAddr = "warming", "pod-78", "10.0.0.78:7777"
	if finalized, err := repo.FinalizeBattleAllocation(ctx, warming, testTTL); err != nil || finalized {
		t.Fatalf("legacy finalize bypassed persistent fence: finalized=%v err=%v", finalized, err)
	}
	if finalized, err := repo.FinalizeFencedBattleAllocation(ctx, warming, testTTL); err != nil || !finalized {
		t.Fatalf("strict fenced finalize: finalized=%v err=%v", finalized, err)
	}
	final, found, err := repo.GetBattle(ctx, 78)
	if err != nil || !found || final.GetState() != "warming" || final.GetDsPodName() != "pod-78" {
		t.Fatalf("finalized record: found=%v got=%+v err=%v", found, final, err)
	}
	if ttl := mr.TTL(battleKey(78)); ttl != 0 {
		t.Fatalf("strict finalize must keep preactive fence persistent, ttl=%s", ttl)
	}
	if unknown := final.ProtoReflect().GetUnknown(); string(unknown) != string(futureWire) {
		t.Fatalf("finalize discarded future protobuf fields: got=%x want=%x", unknown, futureWire)
	}
}

func TestFinalizeFencedBattleAllocationAppliedResponseLostUsesStrictReadback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base, mr := newRepo(t)
	const matchID = uint64(79)
	futureWire := []byte{0xf8, 0x07, 0x01}
	claim := &dsv1.BattleStorageRecord{
		MatchId: matchID, State: "allocating", AllocationId: "alloc-79",
		AllocatedAtMs: 10, LastHeartbeatMs: 10,
	}
	claim.ProtoReflect().SetUnknown(append([]byte(nil), futureWire...))
	if claimed, _, err := base.ClaimBattle(ctx, claim, testTTL); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if fenced, err := base.FenceBattleAllocation(ctx, matchID, "alloc-79"); err != nil || !fenced {
		t.Fatalf("fence: fenced=%v err=%v", fenced, err)
	}

	lost := &loseSuccessfulWatchResponseClient{UniversalClient: base.rdb, afterSuccess: cancel}
	lost.remaining.Store(1)
	repo := NewRedisBattleRepo(lost)
	warming := &dsv1.BattleStorageRecord{
		MatchId: matchID, State: "warming", AllocationId: "alloc-79",
		DsPodName: "battle-79", DsAddr: "10.0.0.79:7777", GameserverUid: "uid-79",
		AllocatedAtMs: 20, LastHeartbeatMs: 20,
	}
	finalized, err := repo.FinalizeFencedBattleAllocation(ctx, warming, testTTL)
	if err != nil || !finalized {
		t.Fatalf("response-lost finalize not recovered by read-back: finalized=%v err=%v", finalized, err)
	}
	if lost.remaining.Load() != 0 {
		t.Fatal("test did not inject post-commit response loss")
	}
	verifyCtx := context.Background()
	got, found, err := base.GetBattle(verifyCtx, matchID)
	if err != nil || !found || got.GetAllocationId() != "alloc-79" ||
		got.GetGameserverUid() != "uid-79" || got.GetDsPodName() != "battle-79" ||
		got.GetState() != "warming" {
		t.Fatalf("strict read-back mismatch: found=%v got=%+v err=%v", found, got, err)
	}
	if unknown := got.ProtoReflect().GetUnknown(); string(unknown) != string(futureWire) {
		t.Fatalf("response-lost finalize discarded future fields: got=%x want=%x", unknown, futureWire)
	}
	if ttl := mr.TTL(battleKey(matchID)); ttl != 0 {
		t.Fatalf("preactive warming fence regained TTL: %v", ttl)
	}
	if _, err := base.rdb.ZScore(verifyCtx, activeKey, fmt.Sprint(matchID)).Result(); err != nil {
		t.Fatalf("read-back success did not continue finalize index: %v", err)
	}
}

func TestFinalizeFencedBattleAllocationResponseLostRejectsMismatchedReadback(t *testing.T) {
	ctx := context.Background()
	base, mr := newRepo(t)
	const matchID = uint64(80)
	claim := &dsv1.BattleStorageRecord{
		MatchId: matchID, State: "allocating", AllocationId: "alloc-80",
		AllocatedAtMs: 10, LastHeartbeatMs: 10,
	}
	if claimed, _, err := base.ClaimBattle(ctx, claim, testTTL); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if fenced, err := base.FenceBattleAllocation(ctx, matchID, "alloc-80"); err != nil || !fenced {
		t.Fatalf("fence: fenced=%v err=%v", fenced, err)
	}
	warming := &dsv1.BattleStorageRecord{
		MatchId: matchID, State: "warming", AllocationId: "alloc-80",
		DsPodName: "battle-80", DsAddr: "10.0.0.80:7777", GameserverUid: "uid-80",
		PlayerIds: []uint64{1, 2}, PlayerCount: 2,
		AllocatedAtMs: 20, LastHeartbeatMs: 20,
	}
	var hookErr error
	lost := &loseSuccessfulWatchResponseClient{
		UniversalClient: base.rdb,
		afterSuccess: func() {
			current, found, err := base.GetBattle(ctx, matchID)
			if err != nil || !found {
				hookErr = fmt.Errorf("read committed finalize: found=%v err=%v", found, err)
				return
			}
			current.PlayerIds = []uint64{999}
			payload, err := marshalBattle(current)
			if err != nil {
				hookErr = err
				return
			}
			hookErr = base.rdb.Set(ctx, battleKey(matchID), payload, 0).Err()
		},
	}
	lost.remaining.Store(1)
	finalized, err := NewRedisBattleRepo(lost).FinalizeFencedBattleAllocation(ctx, warming, testTTL)
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil || finalized {
		t.Fatalf("mismatched response-lost read-back was accepted: finalized=%v err=%v", finalized, err)
	}
	if ttl := mr.TTL(battleKey(matchID)); ttl != 0 {
		t.Fatalf("mismatched read-back lost persistent fence: ttl=%v", ttl)
	}
}

func TestFinalizeAndCleanupAreFencedByAllocationID(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	claim := &dsv1.BattleStorageRecord{
		MatchId: 88, State: "allocating", AllocationId: "winner",
		AllocatedAtMs: 1000, LastHeartbeatMs: 1000,
	}
	claimed, _, err := repo.ClaimBattle(ctx, claim, testTTL)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	loser := proto.Clone(claim).(*dsv1.BattleStorageRecord)
	loser.State, loser.DsPodName, loser.AllocationId = "warming", "pod-loser", "loser"
	if ok, err := repo.FinalizeBattleAllocation(ctx, loser, testTTL); err != nil || ok {
		t.Fatalf("stale finalize: ok=%v err=%v", ok, err)
	}
	winner := proto.Clone(claim).(*dsv1.BattleStorageRecord)
	winner.State, winner.DsPodName, winner.DsAddr = "warming", "pod-winner", "127.0.0.1:30088"
	if ok, err := repo.FinalizeBattleAllocation(ctx, winner, testTTL); err != nil || !ok {
		t.Fatalf("winner finalize: ok=%v err=%v", ok, err)
	}
	if deleted, err := repo.DeleteBattleIfAllocationMatches(ctx, 88, "loser", "pod-loser"); err != nil || deleted {
		t.Fatalf("stale cleanup: deleted=%v err=%v", deleted, err)
	}
	got, found, err := repo.GetBattle(ctx, 88)
	if err != nil || !found || got.AllocationId != "winner" || got.DsPodName != "pod-winner" {
		t.Fatalf("winner overwritten/deleted: found=%v got=%+v err=%v", found, got, err)
	}
}

func TestCleanupNeverDeletesReadyWinner(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	rec := sampleBattle(99, 2000)
	rec.AllocationId = "winner"
	if err := repo.CreateBattle(ctx, rec, testTTL); err != nil {
		t.Fatalf("create: %v", err)
	}
	deleted, err := repo.DeleteBattleIfAllocationMatches(ctx, 99, "winner", rec.DsPodName)
	if err != nil || deleted {
		t.Fatalf("ready cleanup must be fenced: deleted=%v err=%v", deleted, err)
	}
	if _, found, _ := repo.GetBattle(ctx, 99); !found {
		t.Fatal("ready record was deleted")
	}
}
