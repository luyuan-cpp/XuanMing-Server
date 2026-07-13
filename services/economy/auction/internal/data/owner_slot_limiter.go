// 本文件实现单玩家 active/PENDING 拍卖订单配额。Redis SET 是跨 market、跨 MySQL 分片的
// 原子计数索引；MySQL 订单状态仍是成员能否清理的权威依据。
package data

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// OwnerOrderSlot 唯一定位玩家的一张订单。market_id 必须随 order_id 存入成员，才能在
// 分库模式下把惰性清理查询路由到正确 MySQL 分片。
type OwnerOrderSlot struct {
	MarketID uint32
	OrderID  uint64
}

// OwnerSlotLimiter 是单玩家订单配额索引。Reserve 必须在多实例间原子执行。
type OwnerSlotLimiter interface {
	Reserve(ctx context.Context, ownerID uint64, slot OwnerOrderSlot, maxSlots int) (bool, error)
	// Sync 按 slots 顺序把 MySQL 已有 PENDING/活跃订单补入配额 SET。整个批次用单个
	// Lua 脚本原子执行；SET 永不超过 maxSlots。若至少一个新成员装不下，返回 false，
	// 已装入的前缀保留以便把超额 legacy 玩家稳定挡在上限外。slots 最多 maxSlots+1。
	Sync(ctx context.Context, ownerID uint64, slots []OwnerOrderSlot, maxSlots int) (bool, error)
	Release(ctx context.Context, ownerID uint64, slot OwnerOrderSlot) error
	// List 最多返回 limit 个成员，供配额满时按 MySQL 权威状态做有界惰性清理。
	List(ctx context.Context, ownerID uint64, limit int) ([]OwnerOrderSlot, error)
}

type RedisOwnerSlotLimiter struct{ rdb redis.UniversalClient }

func NewRedisOwnerSlotLimiter(rdb redis.UniversalClient) *RedisOwnerSlotLimiter {
	return &RedisOwnerSlotLimiter{rdb: rdb}
}

func ownerSlotKey(ownerID uint64) string {
	// ownerID hashtag 让该玩家的配额操作固定在一个 Redis Cluster slot。
	return fmt.Sprintf("pandora:auction:owner-slots:{%d}", ownerID)
}

func ownerSlotMember(slot OwnerOrderSlot) string {
	return fmt.Sprintf("%010d:%020d", slot.MarketID, slot.OrderID)
}

func parseOwnerSlotMember(member string) (OwnerOrderSlot, error) {
	parts := strings.Split(member, ":")
	if len(parts) != 2 {
		return OwnerOrderSlot{}, fmt.Errorf("invalid owner slot member %q", member)
	}
	marketID, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil || marketID == 0 {
		return OwnerOrderSlot{}, fmt.Errorf("invalid owner slot market %q", member)
	}
	orderID, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || orderID == 0 {
		return OwnerOrderSlot{}, fmt.Errorf("invalid owner slot order %q", member)
	}
	return OwnerOrderSlot{MarketID: uint32(marketID), OrderID: orderID}, nil
}

// reserveOwnerSlotScript：已有成员幂等成功；否则只有 SCARD < hard max 才 SADD。
// SCARD/SADD 在同一 Lua 脚本内原子执行，多实例并发不会突破上限。
var reserveOwnerSlotScript = redis.NewScript(`
if redis.call('SISMEMBER', KEYS[1], ARGV[1]) == 1 then
  return 1
end
if redis.call('SCARD', KEYS[1]) >= tonumber(ARGV[2]) then
  return 0
end
redis.call('SADD', KEYS[1], ARGV[1])
return 1`)

// syncOwnerSlotsScript 把权威库读出的 legacy 活跃成员按传入顺序补入同一个 SET。
// 允许保留成功前缀：当权威活跃数已超过 max 时，SET 会被填满但绝不会越界，后续新写
// 因此稳定失败。SISMEMBER 使重试和并发预热幂等。
var syncOwnerSlotsScript = redis.NewScript(`
local max_slots = tonumber(ARGV[1])
for i = 2, #ARGV do
  if redis.call('SISMEMBER', KEYS[1], ARGV[i]) == 0 then
    if redis.call('SCARD', KEYS[1]) >= max_slots then
      return 0
    end
    redis.call('SADD', KEYS[1], ARGV[i])
  end
end
return 1`)

func (l *RedisOwnerSlotLimiter) Reserve(
	ctx context.Context, ownerID uint64, slot OwnerOrderSlot, maxSlots int,
) (bool, error) {
	if ownerID == 0 || slot.MarketID == 0 || slot.OrderID == 0 || maxSlots <= 0 {
		return false, errcode.New(errcode.ErrInternal, "invalid owner slot reserve arguments")
	}
	result, err := reserveOwnerSlotScript.Run(ctx, l.rdb, []string{ownerSlotKey(ownerID)},
		ownerSlotMember(slot), maxSlots).Int64()
	if err != nil {
		return false, errcode.New(errcode.ErrInternal,
			"reserve owner slot owner=%d market=%d order=%d: %v", ownerID, slot.MarketID, slot.OrderID, err)
	}
	return result == 1, nil
}

func (l *RedisOwnerSlotLimiter) Sync(
	ctx context.Context, ownerID uint64, slots []OwnerOrderSlot, maxSlots int,
) (bool, error) {
	if ownerID == 0 || maxSlots <= 0 || len(slots) > maxSlots+1 {
		return false, errcode.New(errcode.ErrInternal, "invalid owner slot sync arguments")
	}
	args := make([]any, 1, len(slots)+1)
	args[0] = maxSlots
	for _, slot := range slots {
		if slot.MarketID == 0 || slot.OrderID == 0 {
			return false, errcode.New(errcode.ErrInternal, "invalid owner slot sync member")
		}
		args = append(args, ownerSlotMember(slot))
	}
	result, err := syncOwnerSlotsScript.Run(ctx, l.rdb, []string{ownerSlotKey(ownerID)}, args...).Int64()
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "sync owner slots owner=%d: %v", ownerID, err)
	}
	return result == 1, nil
}

func (l *RedisOwnerSlotLimiter) Release(ctx context.Context, ownerID uint64, slot OwnerOrderSlot) error {
	if err := l.rdb.SRem(ctx, ownerSlotKey(ownerID), ownerSlotMember(slot)).Err(); err != nil {
		return errcode.New(errcode.ErrInternal,
			"release owner slot owner=%d market=%d order=%d: %v", ownerID, slot.MarketID, slot.OrderID, err)
	}
	return nil
}

func (l *RedisOwnerSlotLimiter) List(ctx context.Context, ownerID uint64, limit int) ([]OwnerOrderSlot, error) {
	if ownerID == 0 || limit <= 0 {
		return nil, errcode.New(errcode.ErrInternal, "invalid owner slot list arguments")
	}
	out := make([]OwnerOrderSlot, 0, limit)
	var cursor uint64
	for len(out) < limit {
		members, next, err := l.rdb.SScan(ctx, ownerSlotKey(ownerID), cursor, "*", int64(limit-len(out))).Result()
		if err != nil {
			return nil, errcode.New(errcode.ErrInternal, "list owner slots owner=%d: %v", ownerID, err)
		}
		for _, member := range members {
			slot, perr := parseOwnerSlotMember(member)
			if perr != nil {
				return nil, errcode.New(errcode.ErrInternal, "list owner slots owner=%d: %v", ownerID, perr)
			}
			out = append(out, slot)
			if len(out) == limit {
				break
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return out, nil
}
