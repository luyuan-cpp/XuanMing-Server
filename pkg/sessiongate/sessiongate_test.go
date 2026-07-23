// sessiongate_test.go — 会话现行性只读视图单测(R5 复审 P0-1,自 push internal/data 搬移)。
//
// 用 miniredis 验证与 login 的跨服务契约:key `pandora:sess:<player_id>` hash 字段 jti。
// 覆盖:现行会话命中、无会话(登出/TTL 过期)、jti 空串按无会话处理、Redis 不可达
// 必须返回错误(调用方 fail-closed,不得猜"无会话");MustBuild 的漏配/宽松档语义。
package sessiongate

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/config"
)

func newTestGate(t *testing.T) (*RedisGate, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisGate(rdb), mr
}

func TestRedisGate_CurrentJTI(t *testing.T) {
	gate, mr := newTestGate(t)
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

func TestRedisGate_AuthorityDownReturnsError(t *testing.T) {
	gate, mr := newTestGate(t)
	mr.Close() // 模拟 Redis 不可达
	if _, _, err := gate.CurrentJTI(context.Background(), 7); err == nil {
		t.Fatal("authority down must return error (caller fail-closed), not guess no-session")
	}
}

func TestMustBuild_MissingEndpoint(t *testing.T) {
	// require=false:漏配返回 nil gate(dev 直连联调),不 panic。
	gate, closeFn := MustBuild(config.RedisConf{}, false)
	closeFn()
	if gate != nil {
		t.Fatal("dev profile with no endpoint must return nil gate")
	}

	// require=true:漏配必须 panic(prod fail-fast,禁止无法判定现行性还对客户端面开门)。
	defer func() {
		if recover() == nil {
			t.Fatal("require=true with no endpoint must panic")
		}
	}()
	_, _ = MustBuild(config.RedisConf{}, true)
}

func TestMustBuild_RequirePingFailFast(t *testing.T) {
	// require=true + 端点不可达:启动 Ping 失败必须 panic(对齐 push/svc 强依赖语义)。
	defer func() {
		if recover() == nil {
			t.Fatal("require=true with unreachable redis must panic at startup")
		}
	}()
	_, _ = MustBuild(config.RedisConf{Host: "127.0.0.1:1"}, true)
}

func TestMustBuild_DevProfileSkipsPing(t *testing.T) {
	// require=false + 端点已配但暂不可达:不 ping、不 panic(dev 容忍 Redis 后起),
	// 运行期查询失败仍由调用方 fail-closed。
	gate, closeFn := MustBuild(config.RedisConf{Host: "127.0.0.1:1"}, false)
	defer closeFn()
	if gate == nil {
		t.Fatal("configured endpoint must return a live gate even in dev profile")
	}
	if _, _, err := gate.CurrentJTI(context.Background(), 1); err == nil {
		t.Fatal("unreachable authority must surface an error (caller fail-closed)")
	}
}
