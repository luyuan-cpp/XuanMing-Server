// hub_auth.go 提供 Hub DS Redis 授权权威的只读访问。
//
// player_locator 只读取 `pandora:hub:auth:{pod}` 做副作用前鉴权，不参与 stage/promote，
// 更不会回写授权记录。这样授权状态的唯一写者仍是 hub_allocator，未知 proto 字段也不会
// 因旧副本 read-modify-write 被丢弃（CLAUDE.md 不变量 17）。
package data

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

// HubAuthReader 是 Hub 授权权威的只读端口。
type HubAuthReader interface {
	GetHubAuth(ctx context.Context, pod string) (*hubv1.HubShardAuthStorageRecord, bool, error)
}

// RedisHubAuthReader 从与 hub_allocator 共用的 Redis 读取授权记录。
type RedisHubAuthReader struct {
	rdb hubAuthRedis
}

// hubAuthRedis 只暴露只读仓实际需要的 GET，便于精确测试 Redis miss/failure，避免测试
// 引入新的模拟 Redis 依赖。
type hubAuthRedis interface {
	Get(ctx context.Context, key string) *redis.StringCmd
}

// NewRedisHubAuthReader 构造 Hub 授权只读仓。
func NewRedisHubAuthReader(rdb redis.UniversalClient) *RedisHubAuthReader {
	return &RedisHubAuthReader{rdb: rdb}
}

func hubAuthKey(pod string) string {
	return fmt.Sprintf("pandora:hub:auth:{%s}", pod)
}

// GetHubAuth 读取并严格解码授权记录。key miss 与 Redis/数据损坏分开返回，供 service
// 分别映射为“凭据未激活”和“授权权威不可用”。
func (r *RedisHubAuthReader) GetHubAuth(ctx context.Context, pod string) (*hubv1.HubShardAuthStorageRecord, bool, error) {
	if r == nil || r.rdb == nil {
		return nil, false, fmt.Errorf("hub auth redis reader is not initialized")
	}
	if pod == "" {
		return nil, false, fmt.Errorf("hub auth pod is empty")
	}
	raw, err := r.rdb.Get(ctx, hubAuthKey(pod)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get hub auth %q: %w", pod, err)
	}
	rec := &hubv1.HubShardAuthStorageRecord{}
	if err := proto.Unmarshal(raw, rec); err != nil {
		return nil, false, fmt.Errorf("unmarshal hub auth %q: %w", pod, err)
	}
	return rec, true, nil
}
