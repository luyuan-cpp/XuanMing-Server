// session_test.go — SessionCurrent 会话现行性中间件单测(R5 复审 P0-1,INC-20260722-004)。
//
// 覆盖判定矩阵:无证据放行(内部面)、证据残缺 fail-closed、gate 未装配按档位、
// 权威不可达 fail-closed、登出/过期 → ErrUnauthorized、顶号 → ErrSessionSuperseded
// (gRPC ABORTED,与自然过期可判别,防被顶设备自动反顶),现行会话放行。
package middleware

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/sessiongate"
)

// fakeSessionGate 是可编程的会话权威假件。
type fakeSessionGate struct {
	jti   string
	found bool
	err   error
	calls int
}

func (f *fakeSessionGate) CurrentJTI(_ context.Context, _ uint64) (string, bool, error) {
	f.calls++
	return f.jti, f.found, f.err
}

// sessionPayload 构造 Envoy forward_payload_header 形态的 base64url JWT payload。
func sessionPayload(sub string, jti string) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf(`{"sub":%q,"jti":%q,"exp":4102444800}`, sub, jti)))
}

// runSession 跑一次中间件,返回 (下游是否被调用, 错误)。
// gate 用接口类型:传 nil 字面量即"未装配"(避免 typed-nil 接口陷阱)。
func runSession(ctx context.Context, gate sessiongate.Gate, require bool) (bool, error) {
	invoked := false
	handler := func(ctx context.Context, req any) (any, error) {
		invoked = true
		return nil, nil
	}
	_, err := SessionCurrent(gate, require)(handler)(ctx, nil)
	return invoked, err
}

func grpcCodeOf(t *testing.T, err error) codes.Code {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected grpc status error, got %v", err)
	}
	return st.Code()
}

func TestSessionCurrent_NoEvidencePasses(t *testing.T) {
	// 无 payload 头 = 非客户端面(内部服务/DS 回调/dev 直连):require 档也放行,
	// 否则会切断全部内部调用(客户端面必带该头,jwt_authn 全路由 require)。
	gate := &fakeSessionGate{}
	for _, require := range []bool{false, true} {
		invoked, err := runSession(ctxWith(map[string]string{MetadataKeyPlayerID: "1001"}), gate, require)
		if err != nil || !invoked {
			t.Fatalf("require=%v: no-evidence request must pass, invoked=%v err=%v", require, invoked, err)
		}
	}
	if gate.calls != 0 {
		t.Fatalf("no-evidence request must not query authority, calls=%d", gate.calls)
	}
	// 无 transport(进程内直调/单测):同样放行。
	if invoked, err := runSession(context.Background(), gate, true); err != nil || !invoked {
		t.Fatalf("no-transport request must pass, invoked=%v err=%v", invoked, err)
	}
}

func TestSessionCurrent_MalformedEvidenceRejected(t *testing.T) {
	gate := &fakeSessionGate{jti: "cur", found: true}

	// payload 不可解(正常网关路径不可能出现)→ UNAUTHENTICATED。
	_, err := runSession(ctxWith(map[string]string{
		MetadataKeyPlayerID:   "1001",
		MetadataKeyJWTPayload: "!!not-base64url!!",
	}), gate, false)
	if grpcCodeOf(t, err) != codes.Unauthenticated {
		t.Fatalf("malformed payload must be Unauthenticated, got %v", err)
	}

	// payload 可解但缺 player_id 头(身份不可绑定)→ UNAUTHENTICATED。
	_, err = runSession(ctxWith(map[string]string{
		MetadataKeyJWTPayload: sessionPayload("1001", "jti-a"),
	}), gate, false)
	if grpcCodeOf(t, err) != codes.Unauthenticated {
		t.Fatalf("missing player identity must be Unauthenticated, got %v", err)
	}
}

func TestSessionCurrent_NilGateByProfile(t *testing.T) {
	ctx := ctxWith(map[string]string{
		MetadataKeyPlayerID:   "1001",
		MetadataKeyJWTPayload: sessionPayload("1001", "jti-a"),
	})
	// 宽松档:gate 未装配放行(dev 无 Redis 直连)。
	if invoked, err := runSession(ctx, nil, false); err != nil || !invoked {
		t.Fatalf("dev profile nil gate must pass, invoked=%v err=%v", invoked, err)
	}
	// 强制档:gate 未装配 fail-closed(UNAVAILABLE)。
	_, err := runSession(ctx, nil, true)
	if grpcCodeOf(t, err) != codes.Unavailable {
		t.Fatalf("require profile nil gate must be Unavailable, got %v", err)
	}
}

func TestSessionCurrent_AuthorityDownFailClosed(t *testing.T) {
	// 与真实 RedisGate 同款错误形态:ErrUnavailable(→ gRPC UNAVAILABLE,客户端退避重试)。
	gate := &fakeSessionGate{err: errcode.New(errcode.ErrUnavailable, "session authority unavailable")}
	_, err := runSession(ctxWith(map[string]string{
		MetadataKeyPlayerID:   "1001",
		MetadataKeyJWTPayload: sessionPayload("1001", "jti-a"),
	}), gate, false)
	if grpcCodeOf(t, err) != codes.Unavailable {
		t.Fatalf("authority down must fail-closed as Unavailable, got %v", err)
	}
}

func TestSessionCurrent_LoggedOutUnauthenticated(t *testing.T) {
	gate := &fakeSessionGate{found: false}
	_, err := runSession(ctxWith(map[string]string{
		MetadataKeyPlayerID:   "1001",
		MetadataKeyJWTPayload: sessionPayload("1001", "jti-a"),
	}), gate, true)
	// 登出/过期:UNAUTHENTICATED(客户端允许自动换新会话——单设备场景无反顶风险)。
	if grpcCodeOf(t, err) != codes.Unauthenticated {
		t.Fatalf("logged-out must be Unauthenticated, got %v", err)
	}
}

func TestSessionCurrent_SupersededAborted(t *testing.T) {
	gate := &fakeSessionGate{jti: "jti-new", found: true}
	_, err := runSession(ctxWith(map[string]string{
		MetadataKeyPlayerID:   "1001",
		MetadataKeyJWTPayload: sessionPayload("1001", "jti-old"),
	}), gate, true)
	// 顶号:ABORTED(专属可判别,被顶设备只能转交互登录,不得自动完整 Login 反顶)。
	if grpcCodeOf(t, err) != codes.Aborted {
		t.Fatalf("superseded must be Aborted, got %v", err)
	}
}

func TestSessionCurrent_CurrentSessionPasses(t *testing.T) {
	gate := &fakeSessionGate{jti: "jti-a", found: true}
	invoked, err := runSession(ctxWith(map[string]string{
		MetadataKeyPlayerID:   "1001",
		MetadataKeyJWTPayload: sessionPayload("1001", "jti-a"),
	}), gate, true)
	if err != nil || !invoked {
		t.Fatalf("current session must pass, invoked=%v err=%v", invoked, err)
	}
	if gate.calls != 1 {
		t.Fatalf("expected exactly one authority query, got %d", gate.calls)
	}
}
