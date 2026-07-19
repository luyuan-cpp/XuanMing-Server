// auth_test.go — ParseJWTPayloadJTI(Envoy forward_payload_header 解析,2026-07-18)。
//
// x-pandora-jwt-payload 是 Envoy jwt_authn 验签成功后重写的 base64url JSON;
// SelectRole 会话现行性门从中取 jti。任何解码/结构异常都必须返回 ""(按头缺失处理,
// 由业务侧 profile 决定 fail-closed 还是放行),不得 panic 或误返部分内容。
package middleware

import (
	"encoding/base64"
	"testing"
)

func TestParseJWTPayloadJTI(t *testing.T) {
	// claims 模拟 Envoy 验签后转发的 payload JSON,jti 为唯一需要断言的目标字段。
	claims := `{"sub":"42","jti":"session-jti-1","exp":1789000000,"iss":"pandora-login"}`

	t.Run("Envoy 标准 base64url 无 padding", func(t *testing.T) {
		// payload 模拟 Envoy forward_payload_header 的标准无 padding 编码。
		payload := base64.RawURLEncoding.EncodeToString([]byte(claims))
		// got 是解析出的 jti,必须与 payload 中的当前会话代一致。
		if got := ParseJWTPayloadJTI(payload); got != "session-jti-1" {
			t.Fatalf("want session-jti-1, got %q", got)
		}
	})

	t.Run("兼容带 padding 的 base64url", func(t *testing.T) {
		// payload 使用带 padding 的兼容编码,验证第二条解码分支不会丢失 jti。
		payload := base64.URLEncoding.EncodeToString([]byte(claims))
		// got 仍应是同一个会话 jti,证明兼容分支与标准分支语义一致。
		if got := ParseJWTPayloadJTI(payload); got != "session-jti-1" {
			t.Fatalf("want session-jti-1, got %q", got)
		}
	})

	t.Run("空头返回空", func(t *testing.T) {
		// got 必须为空,上层才能把缺失可信头按 profile 做 fail-closed 或本地兼容。
		if got := ParseJWTPayloadJTI(""); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})

	t.Run("非法 base64 返回空", func(t *testing.T) {
		// got 不能包含任何部分解析结果,非法编码统一收敛为空。
		if got := ParseJWTPayloadJTI("!!!not-base64!!!"); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})

	t.Run("非法 JSON 返回空", func(t *testing.T) {
		// payload 是编码合法但内容不是 JSON 的负例,用于隔离 JSON 解析失败路径。
		payload := base64.RawURLEncoding.EncodeToString([]byte("not-json"))
		// got 必须为空,避免格式合法的 header 绕过 claim 结构校验。
		if got := ParseJWTPayloadJTI(payload); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})

	t.Run("缺 jti claim 返回空", func(t *testing.T) {
		// payload 保留合法 JSON 与其他 claim,只缺少会话现行性所需的 jti。
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"42"}`))
		// got 使用结构体字段零值返回空,不得拿 sub 等其他 claim 冒充会话代。
		if got := ParseJWTPayloadJTI(payload); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
}
