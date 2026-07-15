package internalrpcauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisReplayStore struct {
	client redis.UniversalClient
	prefix string
}

func NewRedisReplayStore(client redis.UniversalClient, prefix string) (*RedisReplayStore, error) {
	if client == nil {
		return nil, errors.New("internal RPC Redis replay client is required")
	}
	if prefix == "" {
		prefix = "pandora:internal-rpc:nonce:"
	}
	return &RedisReplayStore{client: client, prefix: prefix}, nil
}

func (s *RedisReplayStore) Consume(ctx context.Context, nonceKey string, ttl time.Duration) (bool, error) {
	if s == nil || s.client == nil || nonceKey == "" || ttl <= 0 {
		return false, errors.New("internal RPC Redis replay store unavailable")
	}
	// Hash the caller+nonce so attacker-controlled metadata never becomes a
	// Redis key fragment and logs/diagnostics cannot expose a reusable nonce.
	digest := sha256.Sum256([]byte(nonceKey))
	return s.client.SetNX(ctx, s.prefix+hex.EncodeToString(digest[:]), "1", ttl).Result()
}
