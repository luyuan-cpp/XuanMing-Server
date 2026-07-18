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
	claims := `{"sub":"42","jti":"session-jti-1","exp":1789000000,"iss":"pandora-login"}`

	t.Run("Envoy 标准 base64url 无 padding", func(t *testing.T) {
		payload := base64.RawURLEncoding.EncodeToString([]byte(claims))
		if got := ParseJWTPayloadJTI(payload); got != "session-jti-1" {
			t.Fatalf("want session-jti-1, got %q", got)
		}
	})

	t.Run("兼容带 padding 的 base64url", func(t *testing.T) {
		payload := base64.URLEncoding.EncodeToString([]byte(claims))
		if got := ParseJWTPayloadJTI(payload); got != "session-jti-1" {
			t.Fatalf("want session-jti-1, got %q", got)
		}
	})

	t.Run("空头返回空", func(t *testing.T) {
		if got := ParseJWTPayloadJTI(""); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})

	t.Run("非法 base64 返回空", func(t *testing.T) {
		if got := ParseJWTPayloadJTI("!!!not-base64!!!"); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})

	t.Run("非法 JSON 返回空", func(t *testing.T) {
		payload := base64.RawURLEncoding.EncodeToString([]byte("not-json"))
		if got := ParseJWTPayloadJTI(payload); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})

	t.Run("缺 jti claim 返回空", func(t *testing.T) {
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"42"}`))
		if got := ParseJWTPayloadJTI(payload); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
}
