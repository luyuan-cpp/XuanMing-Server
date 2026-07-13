package errcode

import (
	"errors"
	"fmt"
	"testing"
)

// mysqlLikeErr 模拟 *mysql.MySQLError(带 Number 字段),用于验证 NewCause 包裹后
// errors.As 仍能沿 Unwrap 链检出底层驱动错误(guild/group 数据层 1213/1205 死锁重试依赖此)。
type mysqlLikeErr struct {
	Number  uint16
	Message string
}

func (e *mysqlLikeErr) Error() string {
	return fmt.Sprintf("Error %d: %s", e.Number, e.Message)
}

func TestNewCause_UnwrapExposesCause(t *testing.T) {
	cause := &mysqlLikeErr{Number: 1213, Message: "Deadlock found"}
	err := NewCause(ErrInternal, cause, "commit tx: %v", cause)

	// Unwrap 应返回底层原因。
	if got := errors.Unwrap(err); got != error(cause) {
		t.Fatalf("Unwrap() = %v, want %v", got, cause)
	}
	// errors.As 应沿链检出底层驱动错误类型。
	var me *mysqlLikeErr
	if !errors.As(err, &me) {
		t.Fatalf("errors.As 未能从包裹错误检出 *mysqlLikeErr")
	}
	if me.Number != 1213 {
		t.Fatalf("检出的 Number = %d, want 1213", me.Number)
	}
}

func TestNewCause_CodeUnchangedByCause(t *testing.T) {
	cause := &mysqlLikeErr{Number: 1205, Message: "Lock wait timeout"}
	err := NewCause(ErrInternal, cause, "lock guild")
	// cause 不改变对外错误码语义:仍是 ErrInternal。
	if As(err) != ErrInternal {
		t.Fatalf("As(err) = %d, want %d", As(err), ErrInternal)
	}
}

func TestAs_FallbackThroughWrappedChain(t *testing.T) {
	// 业务错误被 fmt.Errorf %w 再包一层时,As 应沿链回溯拿到原始 Code。
	base := New(ErrGuildRequestLimit, "pending limit")
	wrapped := fmt.Errorf("service layer: %w", base)
	if As(wrapped) != ErrGuildRequestLimit {
		t.Fatalf("As(wrapped) = %d, want %d", As(wrapped), ErrGuildRequestLimit)
	}
}

func TestUnwrap_NilCauseIsBackwardCompatible(t *testing.T) {
	// 不带 cause 的普通 New:Unwrap 返回 nil,行为与未包裹一致。
	err := New(ErrInternal, "plain")
	if errors.Unwrap(err) != nil {
		t.Fatalf("Unwrap(plain) = %v, want nil", errors.Unwrap(err))
	}
	if As(err) != ErrInternal {
		t.Fatalf("As(plain) = %d, want %d", As(err), ErrInternal)
	}
}
