// session_gate_test.go — RedisSessionGate 会话现行性只读视图单测(P0,INC-20260722-004)。
//
// 用 miniredis 验证与 login 的跨服务契约:key `pandora:sess:<player_id>` hash 字段 jti。
// 覆盖:现行会话命中、无会话(登出/TTL 过期)、jti 空串按无会话处理、Redis 不可达
// 必须返回错误(调用方 fail-closed,不得猜"无会话")。
package data

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestSessionGate(t *testing.T) (*RedisSessionGate, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisSessionGate(rdb), mr
}

func TestSessionGate_CurrentJTI(t *testing.T) {
	gate, mr := newTestSessionGate(t)
	ctx := context.Background()

	// 无会话(从未登录/已登出):found=false 且无错误。
	jti, found, err := gate.CurrentJTI(ctx, 7)
	if err != nil || found || jti != "" {
		t.Fatalf("no session: jti=%q found=%v err=%v", jti, found, err)
	}

	// login 写入会话(契约:pandora:sess:<pid> hash 字段 jti)。
	mr.HSet("pandora:sess:7", "jti", "jti-current")
	jti, found, err = gate.CurrentJTI(ctx, 7)
	if err != nil || !found || jti != "jti-current" {
		t.Fatalf("current session: jti=%q found=%v err=%v", jti, found, err)
	}

	// jti 空串视同无会话(防御脏数据,不得当作"任意 jti 均现行")。
	mr.HSet("pandora:sess:8", "jti", "")
	jti, found, err = gate.CurrentJTI(ctx, 8)
	if err != nil || found || jti != "" {
		t.Fatalf("empty jti: jti=%q found=%v err=%v", jti, found, err)
	}
}

func TestSessionGate_AuthorityDownReturnsError(t *testing.T) {
	gate, mr := newTestSessionGate(t)
	mr.Close() // 模拟 Redis 不可达
	if _, _, err := gate.CurrentJTI(context.Background(), 7); err == nil {
		t.Fatal("authority down must return error (caller fail-closed), not guess no-session")
	}
}
