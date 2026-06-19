// cache.go —— data_service 的 Redis 缓存层(cache-aside,2026-06-16)。
//
// Redis key 模板:pandora:data:player:%d → protobuf bytes(data_service/v1.PlayerData)
//
// 读 miss 回填、写后删除均由 biz 编排;缓存是弱一致旁路:
//   - Get miss / 反序列化失败 → 视为未命中,回落 MySQL,不报错给上层;
//   - Set / Del 失败 → 仅影响命中率,不影响数据正确性(MySQL 才是事实源)。
package data

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	datav1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/data_service/v1"
)

// cacheKey returns "pandora:data:player:playerID"。
func cacheKey(playerID uint64) string {
	return fmt.Sprintf("pandora:data:player:%d", playerID)
}

// PlayerCache 是玩家数据缓存抽象。
type PlayerCache interface {
	// Get 读缓存。未命中(含反序列化失败)→ (nil, false, nil)。
	Get(ctx context.Context, playerID uint64) (*datav1.PlayerData, bool, error)
	// Set 写缓存(TTL=ttl)。
	Set(ctx context.Context, pd *datav1.PlayerData, ttl time.Duration) error
	// Del 删缓存。
	Del(ctx context.Context, playerID uint64) error
}

// RedisPlayerCache 是基于 go-redis/v9 的 PlayerCache 实现。
type RedisPlayerCache struct {
	rdb redis.UniversalClient
}

// NewRedisPlayerCache 构造。
func NewRedisPlayerCache(rdb redis.UniversalClient) *RedisPlayerCache {
	return &RedisPlayerCache{rdb: rdb}
}

func (c *RedisPlayerCache) Get(ctx context.Context, playerID uint64) (*datav1.PlayerData, bool, error) {
	b, err := c.rdb.Get(ctx, cacheKey(playerID)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		// 缓存读失败不阻断业务,交由上层回落 MySQL。
		return nil, false, err
	}
	pd := &datav1.PlayerData{}
	if err := proto.Unmarshal(b, pd); err != nil {
		// 脏缓存当未命中处理。
		return nil, false, nil
	}
	return pd, true, nil
}

func (c *RedisPlayerCache) Set(ctx context.Context, pd *datav1.PlayerData, ttl time.Duration) error {
	b, err := proto.Marshal(pd)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, cacheKey(pd.GetPlayerId()), b, ttl).Err()
}

func (c *RedisPlayerCache) Del(ctx context.Context, playerID uint64) error {
	return c.rdb.Del(ctx, cacheKey(playerID)).Err()
}
