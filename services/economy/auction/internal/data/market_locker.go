// market_locker.go 实现 biz.MarketLocker:用 Redis 单写者 token 保证「跨实例 per-market 单写者」
// (限制#2:多实例一致性,不变量 §10「Redis lock TTL ≤ 30s」)。
//
// 进程内 striped lock 只在单实例内串行;多实例部署时同一 market 可能落到不同实例并发撮合 →
// 订单簿(Redis ZSET)与权威库(MySQL)被并发改 → 超卖。本锁让任一时刻同一 market 全局只有
// 一个实例持锁撮合。推荐再叠一致性哈希路由(同一 market 固定落同一实例)把锁竞争降到最低。
package data

import (
	"context"
	"strconv"
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
}

// NewRedisMarketLocker 构造。ttl/maxWait/retryEvery ≤ 0 时取安全默认。
func NewRedisMarketLocker(rdb redis.UniversalClient, ttl, maxWait, retryEvery time.Duration) *RedisMarketLocker {
	if ttl <= 0 || ttl > 30*time.Second {
		ttl = 30 * time.Second // 不变量 §10:Redis lock TTL ≤ 30s
	}
	if maxWait <= 0 {
		maxWait = 3 * time.Second
	}
	if retryEvery <= 0 {
		retryEvery = 20 * time.Millisecond
	}
	return &RedisMarketLocker{
		locker:     redislock.NewRedisLocker(rdb, "pandora:auction:market:"),
		ttl:        ttl,
		maxWait:    maxWait,
		retryEvery: retryEvery,
	}
}

// Lock 阻塞式抢 market 写锁(带退避重试),返回释放函数。maxWait 内抢不到 → ErrAuctionMarketBusy。
func (l *RedisMarketLocker) Lock(ctx context.Context, marketID uint32) (func(), error) {
	key := marketKey(marketID)
	deadline := time.Now().Add(l.maxWait)
	for {
		res, err := l.locker.TryLock(ctx, key, l.ttl)
		if err != nil {
			return nil, err
		}
		if res.IsLocked() {
			return func() {
				if _, rerr := res.Release(context.WithoutCancel(ctx)); rerr != nil {
					plog.With(ctx).Warnw("msg", "auction_market_unlock_failed", "market_id", marketID, "err", rerr)
				}
			}, nil
		}
		if time.Now().After(deadline) {
			return nil, errcode.New(errcode.ErrAuctionMarketBusy, "market %d busy, retry later", marketID)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(l.retryEvery):
		}
	}
}

func marketKey(marketID uint32) string {
	// 仅用数字 market_id,避免高基数;前缀已在 NewRedisLocker 注入。
	return strconv.FormatUint(uint64(marketID), 10)
}
