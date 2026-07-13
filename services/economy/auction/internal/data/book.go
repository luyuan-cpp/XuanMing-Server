// 本文件实现 Redis ZSET 订单簿兼容缓存(MySQL 为撮合候选与订单状态权威,2026-07-12)。
//
// key:pandora:auction:book:{<market_id>}:ask / :bid
//   - hashtag {<market_id>} 把同一市场的买 / 卖盘锁到同一 Redis Cluster slot,
//     保持旧版本 key/member/score 语义不变(对齐 decision-revisit-auction-engine §3)。
//
// 兼容缓存继续使用旧价格-时间编码(float64 精度对整数价足够):
//   - 卖盘(ask):score = price;ZRANGE 0.. 升序 → 最低价在前。
//   - 买盘(bid):score = -price;ZRANGE 0.. 升序 → 最高价在前。
//   - member = 零padded 20 位 order_id;雪花 order_id 时序递增 → 同价按 member 字典序 = 最早在前。
//     旧版本仍可按原方式读取；新版本不从该缓存选择撮合候选。
package data

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// BookStore 是订单簿兼容缓存的最小写接口。缓存写失败不得影响 MySQL 权威撮合；保留该缓存
// 是为了滚动/回滚期间仍在运行的旧版本实例可以观察新挂单。
type BookStore interface {
	// Add 把挂单加入对应方向的簿。
	Add(ctx context.Context, marketID uint32, side Side, orderID uint64, price int64) error
	// Remove 从簿移除挂单(成交满 / 撤单 / 过期)。
	Remove(ctx context.Context, marketID uint32, side Side, orderID uint64) error
}

// RedisBookStore 是基于 go-redis ZSET 的订单簿。
type RedisBookStore struct {
	rdb redis.UniversalClient
}

// NewRedisBookStore 构造。
func NewRedisBookStore(rdb redis.UniversalClient) *RedisBookStore { return &RedisBookStore{rdb: rdb} }

func bookKey(marketID uint32, side Side) string {
	s := "ask"
	if side == SideBuy {
		s = "bid"
	}
	return fmt.Sprintf("pandora:auction:book:{%d}:%s", marketID, s)
}

func scoreOf(side Side, price int64) float64 {
	if side == SideBuy {
		return -float64(price) // 买盘负分:升序取到最高价
	}
	return float64(price)
}

func memberOf(orderID uint64) string { return fmt.Sprintf("%020d", orderID) }

func (b *RedisBookStore) Add(ctx context.Context, marketID uint32, side Side, orderID uint64, price int64) error {
	if err := b.rdb.ZAdd(ctx, bookKey(marketID, side), redis.Z{
		Score:  scoreOf(side, price),
		Member: memberOf(orderID),
	}).Err(); err != nil {
		return errcode.New(errcode.ErrInternal, "book add market=%d order=%d: %v", marketID, orderID, err)
	}
	return nil
}

func (b *RedisBookStore) Remove(ctx context.Context, marketID uint32, side Side, orderID uint64) error {
	if err := b.rdb.ZRem(ctx, bookKey(marketID, side), memberOf(orderID)).Err(); err != nil {
		return errcode.New(errcode.ErrInternal, "book remove market=%d order=%d: %v", marketID, orderID, err)
	}
	return nil
}
