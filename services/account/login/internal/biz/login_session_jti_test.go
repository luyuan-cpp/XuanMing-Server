// login_session_jti_test.go — RequireCurrentSessionJTI(SelectRole 会话现行性门,2026-07-18)。
//
// 该门封 battle-reconnect.md 已知边界「SelectRole 请求体无 token,顶号后旧设备仍可拿 hub 票」:
// jti 来自 Envoy jwt_authn 验签后重写的 x-pandora-jwt-payload 头,与 IssueDSTicket 的
// 请求体 token 走同一 requireCurrentSession 判定。
package biz

import (
	"context"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// jtiSessionRepo 是可控现行性判定的 SessionRepo fake。
type jtiSessionRepo struct {
	cur   string
	found bool
	err   error
}

func (f *jtiSessionRepo) Set(_ context.Context, _ uint64, _, _, _ string, _ time.Duration) error {
	return nil
}
func (f *jtiSessionRepo) Delete(_ context.Context, _ uint64) error { return nil }
func (f *jtiSessionRepo) GetJTI(_ context.Context, _ uint64) (string, bool, error) {
	return f.cur, f.found, f.err
}
func (f *jtiSessionRepo) DeleteIfJTI(_ context.Context, _ uint64, _ string) (bool, error) {
	return true, nil
}

func newSessionJTIUsecase(t *testing.T, sessions *jtiSessionRepo, strict bool) *LoginUsecase {
	t.Helper()
	signer, verifier := newTicketTestPair(t)
	var uc *LoginUsecase
	if sessions == nil {
		uc = NewLoginUsecase(nil, nil, nil, nil, nil, nil, "127.0.0.1:7777", "cn",
			signer, verifier, nil, false, false, nil, false)
	} else {
		uc = NewLoginUsecase(nil, sessions, nil, nil, nil, nil, "127.0.0.1:7777", "cn",
			signer, verifier, nil, false, false, nil, false)
	}
	uc.SetRequireHubAssignmentBinding(strict)
	return uc
}

func TestRequireCurrentSessionJTI(t *testing.T) {
	ctx := context.Background()

	t.Run("sessions 未配(dev 裸跑)直通", func(t *testing.T) {
		uc := newSessionJTIUsecase(t, nil, true)
		if err := uc.RequireCurrentSessionJTI(ctx, 42, "any"); err != nil {
			t.Fatalf("nil sessions must pass, got %v", err)
		}
	})

	t.Run("jti 缺失:B1 严格档 fail-closed 拒绝", func(t *testing.T) {
		uc := newSessionJTIUsecase(t, &jtiSessionRepo{cur: "cur", found: true}, true)
		err := uc.RequireCurrentSessionJTI(ctx, 42, "")
		if errcode.As(err) != errcode.ErrUnauthorized {
			t.Fatalf("empty jti under strict profile must be Unauthorized, got %v", err)
		}
	})

	t.Run("jti 缺失:local/off 保留历史放行", func(t *testing.T) {
		uc := newSessionJTIUsecase(t, &jtiSessionRepo{cur: "cur", found: true}, false)
		if err := uc.RequireCurrentSessionJTI(ctx, 42, ""); err != nil {
			t.Fatalf("empty jti under dev profile must pass, got %v", err)
		}
	})

	t.Run("顶号后旧 jti 拒绝(核心负例)", func(t *testing.T) {
		uc := newSessionJTIUsecase(t, &jtiSessionRepo{cur: "new-generation", found: true}, true)
		err := uc.RequireCurrentSessionJTI(ctx, 42, "old-generation")
		if errcode.As(err) != errcode.ErrUnauthorized {
			t.Fatalf("superseded jti must be Unauthorized, got %v", err)
		}
	})

	t.Run("当前代 jti 放行", func(t *testing.T) {
		uc := newSessionJTIUsecase(t, &jtiSessionRepo{cur: "current", found: true}, true)
		if err := uc.RequireCurrentSessionJTI(ctx, 42, "current"); err != nil {
			t.Fatalf("current jti must pass, got %v", err)
		}
	})

	t.Run("session 已过期/登出拒绝", func(t *testing.T) {
		uc := newSessionJTIUsecase(t, &jtiSessionRepo{found: false}, true)
		err := uc.RequireCurrentSessionJTI(ctx, 42, "whatever")
		if errcode.As(err) != errcode.ErrUnauthorized {
			t.Fatalf("missing session must be Unauthorized, got %v", err)
		}
	})

	t.Run("Redis 故障可重试 Unavailable(fail-closed 不放行)", func(t *testing.T) {
		uc := newSessionJTIUsecase(t, &jtiSessionRepo{err: context.DeadlineExceeded}, true)
		err := uc.RequireCurrentSessionJTI(ctx, 42, "whatever")
		if errcode.As(err) != errcode.ErrUnavailable {
			t.Fatalf("session authority failure must be Unavailable, got %v", err)
		}
	})
}
