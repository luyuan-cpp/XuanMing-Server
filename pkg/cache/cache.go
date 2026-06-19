// Package cache 提供基于 Redis 的 cache-aside 模式封装,带 in-process
// singleflight 去重以避免缓存击穿。
//
// 直接复用自 mmorpg/go/shared/cache/。
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// singleflightGroup 在进程内对 key 维度的并发 dbLoader 去重,
// 不依赖 golang.org/x/sync/singleflight,降低 pkg 依赖。
var (
	sfMu    sync.Mutex
	sfCalls = map[string]*sfCall{}
)

type sfCall struct {
	wg  sync.WaitGroup
	val any
	err error
}

func singleflightDo(key string, fn func() (any, error)) (any, error) {
	sfMu.Lock()
	if c, ok := sfCalls[key]; ok {
		sfMu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &sfCall{}
	c.wg.Add(1)
	sfCalls[key] = c
	sfMu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	sfMu.Lock()
	delete(sfCalls, key)
	sfMu.Unlock()

	return c.val, c.err
}

// LoadOrCache 实现 cache-aside 模式 + 进程内去重。
//   - 先查 Redis,命中返回。
//   - miss 时通过 singleflight 调 dbLoader,结果写回 Redis(TTL 内)。
//
// T 是业务层数据类型,要求可 json 序列化。
func LoadOrCache[T any](
	ctx context.Context,
	rdb redis.UniversalClient,
	cacheKey string,
	sfKey string,
	ttl time.Duration,
	dbLoader func(ctx context.Context) (T, error),
) (T, error) {
	var zero T

	// 1. 先查 Redis
	data, err := rdb.Get(ctx, cacheKey).Bytes()
	if err == nil {
		var result T
		if err := json.Unmarshal(data, &result); err != nil {
			return zero, fmt.Errorf("cache unmarshal %s: %w", cacheKey, err)
		}
		return result, nil
	}
	if err != redis.Nil {
		return zero, fmt.Errorf("redis get %s: %w", cacheKey, err)
	}

	// 2. cache miss → singleflight → DB
	raw, err := singleflightDo(sfKey, func() (any, error) {
		value, err := dbLoader(ctx)
		if err != nil {
			return nil, err
		}
		// 写回缓存(失败不影响业务)
		if bs, jsonErr := json.Marshal(value); jsonErr == nil {
			rdb.Set(ctx, cacheKey, bs, ttl)
		}
		return value, nil
	})
	if err != nil {
		return zero, err
	}
	return raw.(T), nil
}
