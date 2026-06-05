// W3 ④(2026-06-05)RedisOfflineCacheRepo 单测。
//
// 用 miniredis 跑纯内存 redis,验证:
//   - Append → Range 闭环
//   - sinceMs 开区间过滤(>=、<、>)
//   - TTL 刷新(连续 Append 重置过期时间)
//   - 同 ts_ms 多帧不互相覆盖(encodeMember seq 后缀生效)
package data

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"
)

func newTestRepo(t *testing.T, ttl time.Duration) (*RedisOfflineCacheRepo, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisOfflineCacheRepo(rdb, ttl), mr
}

func mustFrame(topic string, tsMs int64, payload string) *pushv1.PushFrame {
	return &pushv1.PushFrame{
		Topic:   topic,
		Payload: []byte(payload),
		TsMs:    tsMs,
		TraceId: "trace-" + topic,
	}
}

// 用例 1:Append → Range 闭环(无 sinceMs)。
func TestOfflineCache_AppendRangeRoundTrip(t *testing.T) {
	repo, _ := newTestRepo(t, time.Minute)
	ctx := context.Background()

	for i := int64(1); i <= 3; i++ {
		if err := repo.Append(ctx, 100, mustFrame("pandora.team.update", i*1000, "p")); err != nil {
			t.Fatalf("Append err=%v", err)
		}
	}

	got, err := repo.Range(ctx, 100, 0)
	if err != nil {
		t.Fatalf("Range err=%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len=%d want=3", len(got))
	}
	for i, f := range got {
		wantTs := int64(i+1) * 1000
		if f.ScoreMs != wantTs {
			t.Fatalf("i=%d score=%d want=%d", i, f.ScoreMs, wantTs)
		}
		if f.Frame.GetTsMs() != wantTs {
			t.Fatalf("i=%d frame.ts_ms=%d want=%d", i, f.Frame.GetTsMs(), wantTs)
		}
		if f.Frame.GetTopic() != "pandora.team.update" {
			t.Fatalf("i=%d topic=%q", i, f.Frame.GetTopic())
		}
	}
}

// 用例 2:sinceMs 边界(开区间)。score > sinceMs 才返回。
func TestOfflineCache_RangeSinceMsOpenInterval(t *testing.T) {
	repo, _ := newTestRepo(t, time.Minute)
	ctx := context.Background()

	for _, ts := range []int64{1000, 2000, 3000} {
		if err := repo.Append(ctx, 200, mustFrame("pandora.match.progress", ts, "p")); err != nil {
			t.Fatalf("Append err=%v", err)
		}
	}

	// sinceMs=2000 → 只剩 3000(2000 也被排除,开区间)
	got, err := repo.Range(ctx, 200, 2000)
	if err != nil {
		t.Fatalf("Range err=%v", err)
	}
	if len(got) != 1 || got[0].ScoreMs != 3000 {
		t.Fatalf("got=%+v want single ts=3000", got)
	}

	// sinceMs=0 → 全部
	got2, _ := repo.Range(ctx, 200, 0)
	if len(got2) != 3 {
		t.Fatalf("sinceMs=0 len=%d want=3", len(got2))
	}

	// sinceMs=3000 → 空
	got3, _ := repo.Range(ctx, 200, 3000)
	if len(got3) != 0 {
		t.Fatalf("sinceMs=3000 len=%d want=0", len(got3))
	}
}

// 用例 3:TTL 每次 Append 刷新。
func TestOfflineCache_TTLRefreshOnAppend(t *testing.T) {
	repo, mr := newTestRepo(t, 5*time.Second)
	ctx := context.Background()

	_ = repo.Append(ctx, 300, mustFrame("pandora.chat.private", 1000, "p1"))
	mr.FastForward(3 * time.Second) // 剩 2s
	if err := repo.Append(ctx, 300, mustFrame("pandora.chat.private", 2000, "p2")); err != nil {
		t.Fatalf("Append err=%v", err)
	}
	// 再 FastForward 3s,若 TTL 没刷新就过期了;若刷新过 TTL=5s 剩 2s
	mr.FastForward(3 * time.Second)

	got, err := repo.Range(ctx, 300, 0)
	if err != nil {
		t.Fatalf("Range err=%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("after TTL refresh + 3s len=%d want=2 (cache should still alive)", len(got))
	}
}

// 用例 4:同 ts_ms 多帧不互相覆盖(seq 后缀生效)。
func TestOfflineCache_SameTsMultipleFrames(t *testing.T) {
	repo, _ := newTestRepo(t, time.Minute)
	ctx := context.Background()

	// 3 帧同样 ts_ms,但 payload 不同;若没 seq 后缀,ZSET 会按 member 完全相同去重
	for i := 0; i < 3; i++ {
		if err := repo.Append(ctx, 400, mustFrame("pandora.team.update", 1000, "same-ts")); err != nil {
			t.Fatalf("i=%d Append err=%v", i, err)
		}
	}

	got, err := repo.Range(ctx, 400, 0)
	if err != nil {
		t.Fatalf("Range err=%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("same-ts len=%d want=3 (seq suffix must prevent dedup)", len(got))
	}
}
