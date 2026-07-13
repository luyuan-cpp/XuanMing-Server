package data

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

const testMarketLockKey = "pandora:auction:market:42"

func TestRedisMarketLocker_RenewsPastOriginalTTLAndRemainsExclusive(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	var failStops atomic.Int32
	locker := newRedisMarketLocker(rdb, time.Second, 80*time.Millisecond, 10*time.Millisecond,
		func(context.Context, uint32, error) { failStops.Add(1) })
	release, err := locker.Lock(context.Background(), 42)
	if err != nil {
		t.Fatalf("首次加锁失败: %v", err)
	}
	defer release()

	// Redis 的测试时钟累计前进 1.4s,已经超过最初 1s TTL;两次真实等待让 ttl/3
	// 续租 ticker 在两段 FastForward 之间至少执行一次并重置 TTL。
	time.Sleep(400 * time.Millisecond)
	mr.FastForward(700 * time.Millisecond)
	time.Sleep(400 * time.Millisecond)
	mr.FastForward(700 * time.Millisecond)
	if !mr.Exists(testMarketLockKey) {
		t.Fatal("跨过原始 TTL 后锁已消失,续租未生效")
	}

	contender := newRedisMarketLocker(rdb, time.Second, 80*time.Millisecond, 10*time.Millisecond,
		func(context.Context, uint32, error) { failStops.Add(1) })
	contenderRelease, err := contender.Lock(context.Background(), 42)
	if err == nil {
		contenderRelease()
		t.Fatal("原持有者仍在续租时,第二个持有者不应取得同一 market 锁")
	}
	if got := failStops.Load(); got != 0 {
		t.Fatalf("正常续租不应触发 fail-stop,got=%d", got)
	}
}

func TestRedisMarketLocker_ReleaseStopsRenewalAndAllowsReacquire(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	var firstFailStops atomic.Int32
	first := newRedisMarketLocker(rdb, time.Second, 80*time.Millisecond, 10*time.Millisecond,
		func(context.Context, uint32, error) { firstFailStops.Add(1) })
	releaseFirst, err := first.Lock(context.Background(), 42)
	if err != nil {
		t.Fatalf("首次加锁失败: %v", err)
	}
	releaseFirst()
	// release 必须幂等,第二次调用不能重复 close channel 或触碰 Redis。
	releaseFirst()
	if mr.Exists(testMarketLockKey) {
		t.Fatal("release 返回后锁 key 仍存在")
	}

	var secondFailStops atomic.Int32
	second := newRedisMarketLocker(rdb, time.Second, 80*time.Millisecond, 10*time.Millisecond,
		func(context.Context, uint32, error) { secondFailStops.Add(1) })
	releaseSecond, err := second.Lock(context.Background(), 42)
	if err != nil {
		t.Fatalf("release 后同一 market 无法重新加锁: %v", err)
	}

	// 超过 first 的一个续租周期;若旧 goroutine 泄漏,它会拿旧 token Extend 失败并触发 hook。
	time.Sleep(400 * time.Millisecond)
	if got := firstFailStops.Load(); got != 0 {
		t.Fatalf("release 后旧续租 goroutine 仍在运行,fail-stop=%d", got)
	}
	if got := secondFailStops.Load(); got != 0 {
		t.Fatalf("新持有者正常续租不应触发 fail-stop,got=%d", got)
	}
	releaseSecond()
}

func TestRedisMarketLocker_ExtendFailureTriggersFailStopOnce(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	type failure struct {
		marketID uint32
		err      error
	}
	failures := make(chan failure, 2)
	var failStops atomic.Int32
	locker := newRedisMarketLocker(rdb, time.Second, 80*time.Millisecond, 10*time.Millisecond,
		func(_ context.Context, marketID uint32, err error) {
			failStops.Add(1)
			failures <- failure{marketID: marketID, err: err}
		})
	release, err := locker.Lock(context.Background(), 42)
	if err != nil {
		t.Fatalf("首次加锁失败: %v", err)
	}

	// 模拟 token 在续租前丢失:Extend 返回 false,nil,必须立即 fail-stop。
	mr.Del(testMarketLockKey)
	select {
	case got := <-failures:
		if got.marketID != 42 {
			t.Fatalf("fail-stop market_id=%d,want=42", got.marketID)
		}
		if got.err == nil {
			t.Fatal("fail-stop 应携带续租失败原因")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Extend 失败后未触发 fail-stop")
	}

	// 续租循环在第一次失败后必须退出,不能每个 tick 重复触发进程终止 hook。
	time.Sleep(400 * time.Millisecond)
	if got := failStops.Load(); got != 1 {
		t.Fatalf("fail-stop 触发次数=%d,want=1", got)
	}
	release()
}

func TestNewRedisMarketLocker_ClampsTTLToInvariant(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	tooLarge := newRedisMarketLocker(rdb, 31*time.Second, time.Second, time.Millisecond,
		func(context.Context, uint32, error) {})
	if tooLarge.ttl != maxMarketLockTTL {
		t.Fatalf("超上限 TTL=%s,want=%s", tooLarge.ttl, maxMarketLockTTL)
	}
	tooSmall := newRedisMarketLocker(rdb, time.Millisecond, time.Second, time.Millisecond,
		func(context.Context, uint32, error) {})
	if tooSmall.ttl != minMarketLockTTL {
		t.Fatalf("秒级 EXPIRE 下 TTL=%s,want=%s", tooSmall.ttl, minMarketLockTTL)
	}
}

func TestRedisMarketLocker_MaxWaitBoundsBlockedRedisIO(t *testing.T) {
	// net.Pipe 的服务端故意不读不回，模拟 TCP 已连接但 Redis 不响应。只有客户端启用
	// ContextTimeoutEnabled，SETNX 的 socket read 才会服从 Lock 建立的完整 maxWait deadline。
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close() })
	var dialed atomic.Bool
	rdb := redis.NewClient(&redis.Options{
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			if dialed.Swap(true) {
				return nil, errors.New("unexpected redis redial")
			}
			return clientConn, nil
		},
		DialTimeout:           time.Second,
		ReadTimeout:           5 * time.Second,
		WriteTimeout:          5 * time.Second,
		ContextTimeoutEnabled: true,
		MaxRetries:            -1,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	locker := newRedisMarketLocker(rdb, time.Second, 80*time.Millisecond, 10*time.Millisecond,
		func(context.Context, uint32, error) {})
	started := time.Now()
	_, err := locker.Lock(context.Background(), 42)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("Redis 不响应时 Lock 应在 maxWait 后失败")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Lock elapsed=%s, maxWait=80ms；context deadline 未约束 Redis I/O", elapsed)
	}
}
