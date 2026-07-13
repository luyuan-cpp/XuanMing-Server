package data

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	hubv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/hub/v1"
)

type stubHubAuthRedis struct {
	value string
	err   error
	key   string
}

func (s *stubHubAuthRedis) Get(_ context.Context, key string) *redis.StringCmd {
	s.key = key
	return redis.NewStringResult(s.value, s.err)
}

func TestRedisHubAuthReader(t *testing.T) {
	rec := &hubv1.HubShardAuthStorageRecord{
		PodName:       "hub-1",
		InstanceUid:   "uid-1",
		ProtocolEpoch: 2,
		Phase:         hubv1.HubAuthPhase_HUB_AUTH_PHASE_ACTIVE,
	}
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	t.Run("读取并解码", func(t *testing.T) {
		rdb := &stubHubAuthRedis{value: string(raw)}
		reader := &RedisHubAuthReader{rdb: rdb}
		got, found, err := reader.GetHubAuth(context.Background(), "hub-1")
		if err != nil || !found {
			t.Fatalf("GetHubAuth: found=%v err=%v", found, err)
		}
		if rdb.key != "pandora:hub:auth:{hub-1}" {
			t.Fatalf("key=%q", rdb.key)
		}
		if !proto.Equal(got, rec) {
			t.Fatalf("record mismatch: got=%v want=%v", got, rec)
		}
	})

	t.Run("key miss", func(t *testing.T) {
		reader := &RedisHubAuthReader{rdb: &stubHubAuthRedis{err: redis.Nil}}
		got, found, err := reader.GetHubAuth(context.Background(), "hub-1")
		if err != nil || found || got != nil {
			t.Fatalf("miss must be distinct: got=%v found=%v err=%v", got, found, err)
		}
	})

	t.Run("Redis 故障", func(t *testing.T) {
		boom := errors.New("redis unavailable")
		reader := &RedisHubAuthReader{rdb: &stubHubAuthRedis{err: boom}}
		if _, _, err := reader.GetHubAuth(context.Background(), "hub-1"); !errors.Is(err, boom) {
			t.Fatalf("want wrapped redis error, got %v", err)
		}
	})

	t.Run("坏 proto", func(t *testing.T) {
		reader := &RedisHubAuthReader{rdb: &stubHubAuthRedis{value: "\xff\xff"}}
		if _, _, err := reader.GetHubAuth(context.Background(), "hub-1"); err == nil {
			t.Fatal("corrupt auth record must fail")
		}
	})
}
