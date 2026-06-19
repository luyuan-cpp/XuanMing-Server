// 本文件实现 Redis ZSET 订单簿:撮合引擎的活跃挂单索引(MySQL 为权威,2026-06-19)。
//
// key:pandora:auction:book:{<market_id>}:ask / :bid
//   - hashtag {<market_id>} 把同一市场的买 / 卖盘锁到同一 Redis Cluster slot,
//     撮合一次会同时碰两侧,避免 CROSSSLOT(对齐 decision-revisit-auction-engine §3)。
//
// 价格-时间优先(用 ZSET score + member 编码,float64 精度对整数价足够):
//   - 卖盘(ask):score = price;ZRANGE 0.. 升序 → 最低价在前。
//   - 买盘(bid):score = -price;ZRANGE 0.. 升序 → 最高价在前。
//   - member = 零padded 20 位 order_id;雪花 order_id 时序递增 → 同价按 member 字典序 = 最早在前。
//     故两侧「最优可成交单」都是 ZRANGE 第 0 个元素(最优价 + 最早时间)。
package data

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// BookStore 是订单簿抽象(Redis ZSET 实现)。
type BookStore interface {
	// Add 把挂单加入对应方向的簿。
	Add(ctx context.Context, marketID uint32, side Side, orderID uint64, price int64) error
	// Remove 从簿移除挂单(成交满 / 撤单 / 过期)。
	Remove(ctx context.Context, marketID uint32, side Side, orderID uint64) error
	// Best 取某方向最优可成交单(最优价 + 最早时间);簿空 → ok=false。
	Best(ctx context.Context, marketID uint32, side Side) (orderID uint64, price int64, ok bool, err error)
	// List 按最优在前列某方向最多 limit 个挂单 order_id。
	List(ctx context.Context, marketID uint32, side Side, limit int64) ([]uint64, error)
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

func priceFromScore(side Side, score float64) int64 {
	if side == SideBuy {
		return int64(-score)
	}
	return int64(score)
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

func (b *RedisBookStore) Best(ctx context.Context, marketID uint32, side Side) (uint64, int64, bool, error) {
	zs, err := b.rdb.ZRangeWithScores(ctx, bookKey(marketID, side), 0, 0).Result()
	if err != nil {
		return 0, 0, false, errcode.New(errcode.ErrInternal, "book best market=%d side=%d: %v", marketID, side, err)
	}
	if len(zs) == 0 {
		return 0, 0, false, nil
	}
	member, _ := zs[0].Member.(string)
	orderID, perr := strconv.ParseUint(member, 10, 64)
	if perr != nil {
		return 0, 0, false, errcode.New(errcode.ErrInternal, "book bad member %q: %v", member, perr)
	}
	return orderID, priceFromScore(side, zs[0].Score), true, nil
}

func (b *RedisBookStore) List(ctx context.Context, marketID uint32, side Side, limit int64) ([]uint64, error) {
	if limit <= 0 {
		return nil, nil
	}
	members, err := b.rdb.ZRange(ctx, bookKey(marketID, side), 0, limit-1).Result()
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "book list market=%d side=%d: %v", marketID, side, err)
	}
	out := make([]uint64, 0, len(members))
	for _, m := range members {
		id, perr := strconv.ParseUint(m, 10, 64)
		if perr != nil {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}
