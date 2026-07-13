// market_locker.go 实现 biz.MarketLocker:用 Redis 单写者 token 保证「跨实例 per-market 单写者」
// (限制#2:多实例一致性,不变量 §10「Redis lock TTL ≤ 30s」)。
//
// 进程内 striped lock 只在单实例内串行;多实例部署时同一 market 可能落到不同实例并发撮合 →
// 订单簿(Redis ZSET)与权威库(MySQL)被并发改。本锁用于正常运行时串行和降低数据库冲突；
// Redis 锁不是 fencing token，权威正确性仍必须由 MySQL 行锁、条件状态迁移和唯一键兜底。
// 推荐再叠一致性哈希路由(同一 market 固定落同一实例)把锁竞争降到最低。
package data

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/redislock"
)

// RedisMarketLocker 用 pkg/redislock 实现跨实例 per-market 单写者锁。
type RedisMarketLocker struct {
	locker     *redislock.RedisLocker
	ttl        time.Duration // 锁 TTL(≤ 30s,不变量 §10)
	maxWait    time.Duration // 抢锁最大等待(超时 → ErrAuctionMarketBusy)
	retryEvery time.Duration // 抢锁重试间隔
	failStop   marketLockFailStop
}

// marketLockFailStop 在续租失败、进程已经无法再证明自己是唯一写者时终止进程。
// 生产构造固定使用 os.Exit(1);私有构造仅供同包测试注入无副作用 hook。
type marketLockFailStop func(context.Context, uint32, error)

const (
	maxMarketLockTTL       = 30 * time.Second
	minMarketLockTTL       = time.Second // redislock.Extend 使用 Redis EXPIRE(秒级)
	maxRedisCommandTimeout = 2 * time.Second
)

// NewRedisMarketLocker 构造。ttl/maxWait/retryEvery ≤ 0 时取安全默认。
func NewRedisMarketLocker(rdb redis.UniversalClient, ttl, maxWait, retryEvery time.Duration) *RedisMarketLocker {
	return newRedisMarketLocker(rdb, ttl, maxWait, retryEvery, func(_ context.Context, _ uint32, _ error) {
		os.Exit(1)
	})
}

func newRedisMarketLocker(
	rdb redis.UniversalClient,
	ttl, maxWait, retryEvery time.Duration,
	failStop marketLockFailStop,
) *RedisMarketLocker {
	if ttl <= 0 || ttl > maxMarketLockTTL {
		ttl = maxMarketLockTTL // 不变量 §10:Redis lock TTL ≤ 30s
	} else if ttl < minMarketLockTTL {
		// pkg/redislock 的 Extend 走秒级 EXPIRE;小于 1 秒会被截成 0 并立即删锁。
		ttl = minMarketLockTTL
	}
	if maxWait <= 0 {
		maxWait = 3 * time.Second
	}
	if retryEvery <= 0 {
		retryEvery = 20 * time.Millisecond
	}
	if failStop == nil {
		failStop = func(_ context.Context, _ uint32, _ error) { os.Exit(1) }
	}
	return &RedisMarketLocker{
		locker:     redislock.NewRedisLocker(rdb, "pandora:auction:market:"),
		ttl:        ttl,
		maxWait:    maxWait,
		retryEvery: retryEvery,
		failStop:   failStop,
	}
}

// Lock 阻塞式抢 market 写锁(带退避重试),返回释放函数。maxWait 内抢不到 → ErrAuctionMarketBusy。
func (l *RedisMarketLocker) Lock(ctx context.Context, marketID uint32) (func(), error) {
	key := marketKey(marketID)
	lockCtx, cancel := context.WithTimeout(ctx, l.maxWait)
	defer cancel()
	for {
		res, err := l.locker.TryLock(lockCtx, key, l.ttl)
		if err != nil {
			if lockCtx.Err() != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, marketBusyError(marketID)
			}
			return nil, err
		}
		if res.IsLocked() {
			lease := newMarketLockLease(context.WithoutCancel(ctx), marketID, res, l.ttl, l.failStop)
			lease.start()
			return lease.release, nil
		}
		timer := time.NewTimer(l.retryEvery)
		select {
		case <-lockCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, marketBusyError(marketID)
		case <-timer.C:
		}
	}
}

func marketBusyError(marketID uint32) error {
	return errcode.New(errcode.ErrAuctionMarketBusy, "market %d busy, retry later", marketID)
}

// marketLockLease 管理一次成功持锁的生命周期。Release 先阻止新续租并等待正在执行的
// Extend 完成,随后才释放 token,保证 Extend 与 Release 永不并发读写 TryLockResult。
type marketLockLease struct {
	logCtx   context.Context
	marketID uint32
	result   *redislock.TryLockResult
	ttl      time.Duration
	failStop marketLockFailStop

	stop chan struct{}
	done chan struct{}

	mu        sync.Mutex
	releasing bool
	failed    bool

	releaseOnce sync.Once
	failOnce    sync.Once
}

func newMarketLockLease(
	logCtx context.Context,
	marketID uint32,
	result *redislock.TryLockResult,
	ttl time.Duration,
	failStop marketLockFailStop,
) *marketLockLease {
	return &marketLockLease{
		logCtx:   logCtx,
		marketID: marketID,
		result:   result,
		ttl:      ttl,
		failStop: failStop,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (l *marketLockLease) start() {
	go l.renewLoop()
}

func (l *marketLockLease) renewLoop() {
	defer close(l.done)

	renewEvery := l.ttl / 3
	ticker := time.NewTicker(renewEvery)
	defer ticker.Stop()

	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			if l.isReleasing() {
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), redisCommandTimeout(l.ttl))
			extended, err := l.result.Extend(ctx, l.ttl)
			cancel()
			if err == nil && extended {
				continue
			}

			cause := err
			if cause == nil {
				cause = fmt.Errorf("market lock token expired or ownership changed")
			}
			if l.markFailedUnlessReleasing() {
				// 生产 hook 是 os.Exit(1)：先 fail-stop，避免同步日志输出卡住而延长双写窗口。
				// 测试 hook 会返回，此时再记录可观测错误。
				l.failOnce.Do(func() { l.failStop(l.logCtx, l.marketID, cause) })
				plog.With(l.logCtx).Errorw(
					"msg", "auction_market_lock_renew_failed_fail_stop",
					"market_id", l.marketID,
					"err", cause,
				)
			}
			return
		}
	}
}

func (l *marketLockLease) isReleasing() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.releasing
}

// markFailedUnlessReleasing 把「续租失败」与「正常释放已经开始」的判定放在同一临界区。
func (l *marketLockLease) markFailedUnlessReleasing() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.releasing || l.failed {
		return false
	}
	l.failed = true
	return true
}

func (l *marketLockLease) release() {
	l.releaseOnce.Do(func() {
		l.mu.Lock()
		l.releasing = true
		close(l.stop)
		l.mu.Unlock()

		// 等续租 goroutine 完全退出后再 Release,避免 TryLockResult 的内部状态竞态。
		<-l.done

		ctx, cancel := context.WithTimeout(context.Background(), redisCommandTimeout(l.ttl))
		_, err := l.result.Release(ctx)
		cancel()
		if err != nil {
			plog.With(l.logCtx).Warnw(
				"msg", "auction_market_unlock_failed",
				"market_id", l.marketID,
				"err", err,
			)
		}
	})
}

func redisCommandTimeout(ttl time.Duration) time.Duration {
	timeout := ttl / 3
	if timeout > maxRedisCommandTimeout {
		return maxRedisCommandTimeout
	}
	return timeout
}

func marketKey(marketID uint32) string {
	// 仅用数字 market_id,避免高基数;前缀已在 NewRedisLocker 注入。
	return strconv.FormatUint(uint64(marketID), 10)
}
