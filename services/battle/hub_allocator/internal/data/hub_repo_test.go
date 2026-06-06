// hub_repo_test.go — hub_allocator data 层 Redis 实现测试(miniredis)。
package data

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

const testTTL = 30 * time.Minute

func newRepo(t *testing.T) (*RedisHubRepo, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis run: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisHubRepo(rdb), mr
}

func sampleShard(pod string, shardID uint32, lastHbMs int64) *hubv1.HubShardStorageRecord {
	return &hubv1.HubShardStorageRecord{
		HubPodName:      pod,
		HubAddr:         "127.0.0.1:7778",
		Region:          "global",
		ShardId:         shardID,
		PlayerCount:     0,
		Capacity:        500,
		State:           "ready",
		LastHeartbeatMs: lastHbMs,
		CreatedAtMs:     1000,
	}
}

func TestShard_CreateGetRoundtrip(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	if err := repo.CreateShard(ctx, sampleShard("pandora-hub-global-1", 1, 0), testTTL); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, found, err := repo.GetShard(ctx, "pandora-hub-global-1")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.ShardId != 1 || got.Capacity != 500 || got.State != "ready" {
		t.Fatalf("shard mismatch: %+v", got)
	}
	// 新建不进 active(等首次心跳)
	stale, err := repo.RangeStaleShards(ctx, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("range stale: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("freshly-created shard must not be in active, got %v", stale)
	}
}

func TestShard_ListShards(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, sampleShard("pandora-hub-global-1", 1, 0), testTTL)
	_ = repo.CreateShard(ctx, sampleShard("pandora-hub-global-2", 2, 0), testTTL)

	shards, err := repo.ListShards(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(shards) != 2 {
		t.Fatalf("want 2 shards, got %d", len(shards))
	}
}

func TestShard_UpdateWithLock(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, sampleShard("pandora-hub-global-1", 1, 0), testTTL)

	err := repo.UpdateShardWithLock(ctx, "pandora-hub-global-1", 3, func(s *hubv1.HubShardStorageRecord) error {
		s.PlayerCount += 5
		return nil
	}, testTTL)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _, _ := repo.GetShard(ctx, "pandora-hub-global-1")
	if got.PlayerCount != 5 {
		t.Fatalf("want count 5, got %d", got.PlayerCount)
	}
}

func TestShard_HeartbeatKnownAndOrphan(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, sampleShard("pandora-hub-global-1", 1, 0), testTTL)

	now := time.Now().UnixMilli()
	found, err := repo.HeartbeatShard(ctx, "pandora-hub-global-1", 100, "ready", now, testTTL)
	if err != nil || !found {
		t.Fatalf("heartbeat known: found=%v err=%v", found, err)
	}
	got, _, _ := repo.GetShard(ctx, "pandora-hub-global-1")
	if got.PlayerCount != 100 || got.LastHeartbeatMs != now {
		t.Fatalf("heartbeat not reconciled: %+v", got)
	}
	// 心跳后进 active
	stale, _ := repo.RangeStaleShards(ctx, now)
	if len(stale) != 1 || stale[0] != "pandora-hub-global-1" {
		t.Fatalf("active after heartbeat = %v", stale)
	}

	// 孤儿 pod
	found, err = repo.HeartbeatShard(ctx, "pandora-hub-ghost-9", 1, "ready", now, testTTL)
	if err != nil {
		t.Fatalf("heartbeat orphan err: %v", err)
	}
	if found {
		t.Fatal("orphan heartbeat should return found=false")
	}
}

func TestShard_RangeStaleExcludesNeverHeartbeated(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, sampleShard("pandora-hub-global-1", 1, 0), testTTL)

	// 从未心跳(score 0,不在 active),不应出现在 stale
	stale, err := repo.RangeStaleShards(ctx, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("never-heartbeated must not be stale, got %v", stale)
	}
}

func TestShard_RemoveAndRemoveActive(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)
	_ = repo.CreateShard(ctx, sampleShard("pandora-hub-global-1", 1, 0), testTTL)
	now := time.Now().UnixMilli()
	_, _ = repo.HeartbeatShard(ctx, "pandora-hub-global-1", 1, "ready", now, testTTL)

	if err := repo.RemoveActive(ctx, "pandora-hub-global-1"); err != nil {
		t.Fatalf("remove active: %v", err)
	}
	stale, _ := repo.RangeStaleShards(ctx, now)
	if len(stale) != 0 {
		t.Fatalf("after remove active, want none, got %v", stale)
	}

	if err := repo.RemoveShard(ctx, "pandora-hub-global-1"); err != nil {
		t.Fatalf("remove shard: %v", err)
	}
	_, found, _ := repo.GetShard(ctx, "pandora-hub-global-1")
	if found {
		t.Fatal("shard should be gone after remove")
	}
	shards, _ := repo.ListShards(ctx)
	if len(shards) != 0 {
		t.Fatalf("shards set should be empty, got %d", len(shards))
	}
}

func TestAssignment_Roundtrip(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	rec := &hubv1.HubAssignmentStorageRecord{
		PlayerId:     1001,
		HubPodName:   "pandora-hub-global-1",
		HubAddr:      "127.0.0.1:7778",
		ShardId:      1,
		Region:       "global",
		TeamId:       7,
		AssignedAtMs: 2000,
	}
	if err := repo.SetAssignment(ctx, rec, testTTL); err != nil {
		t.Fatalf("set assignment: %v", err)
	}
	got, found, err := repo.GetAssignment(ctx, 1001)
	if err != nil || !found {
		t.Fatalf("get assignment: found=%v err=%v", found, err)
	}
	if got.HubPodName != "pandora-hub-global-1" || got.TeamId != 7 {
		t.Fatalf("assignment mismatch: %+v", got)
	}

	if err := repo.DeleteAssignment(ctx, 1001); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, _ := repo.GetAssignment(ctx, 1001); found {
		t.Fatal("assignment should be deleted")
	}
}

func TestTeamShard_Roundtrip(t *testing.T) {
	ctx := context.Background()
	repo, _ := newRepo(t)

	if _, found, _ := repo.GetTeamShard(ctx, 7); found {
		t.Fatal("missing team shard should be not-found")
	}
	if err := repo.SetTeamShard(ctx, 7, "pandora-hub-global-2", testTTL); err != nil {
		t.Fatalf("set team shard: %v", err)
	}
	pod, found, err := repo.GetTeamShard(ctx, 7)
	if err != nil || !found {
		t.Fatalf("get team shard: found=%v err=%v", found, err)
	}
	if pod != "pandora-hub-global-2" {
		t.Fatalf("team shard mismatch: %s", pod)
	}
}
