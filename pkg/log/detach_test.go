// detach_test.go — Detach 的行为契约测试:
// 只复制标准业务日志字段;剥离取消;剥离 Kratos transport(防 2026-07-21 ds_allocator
// Heartbeat 并发写 ReplyHeader 崩溃的同类泄漏)。
package log

import (
	"context"
	"testing"

	"github.com/go-kratos/kratos/v2/transport"
)

type stubTransport struct{}

func (stubTransport) Kind() transport.Kind            { return transport.KindGRPC }
func (stubTransport) Endpoint() string                { return "" }
func (stubTransport) Operation() string               { return "" }
func (stubTransport) RequestHeader() transport.Header { return nil }
func (stubTransport) ReplyHeader() transport.Header   { return nil }

func TestDetachCopiesLogFieldsOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = WithTraceID(ctx, "t-1")
	ctx = WithPlayerID(ctx, 42)
	ctx = WithMatchID(ctx, 7)
	ctx = WithTeamID(ctx, 3)
	ctx = transport.NewServerContext(ctx, stubTransport{})
	ctx = transport.NewClientContext(ctx, stubTransport{})

	detached := Detach(ctx)

	// 标准日志字段全部复制
	if got, _ := detached.Value(CtxKeyTraceID).(string); got != "t-1" {
		t.Fatalf("trace_id = %q, want t-1", got)
	}
	if got, _ := detached.Value(CtxKeyPlayerID).(uint64); got != 42 {
		t.Fatalf("player_id = %d, want 42", got)
	}
	if got, _ := detached.Value(CtxKeyMatchID).(uint64); got != 7 {
		t.Fatalf("match_id = %d, want 7", got)
	}
	if got, _ := detached.Value(CtxKeyTeamID).(uint64); got != 3 {
		t.Fatalf("team_id = %d, want 3", got)
	}

	// 请求级 transport 必须被剥离
	if _, ok := transport.FromServerContext(detached); ok {
		t.Fatal("detached ctx still carries server transport")
	}
	if _, ok := transport.FromClientContext(detached); ok {
		t.Fatal("detached ctx still carries client transport")
	}

	// 取消父 ctx 不影响 detached ctx
	cancel()
	select {
	case <-detached.Done():
		t.Fatal("detached ctx canceled with parent")
	default:
	}
}

func TestDetachSkipsAbsentFields(t *testing.T) {
	detached := Detach(context.Background())
	if v := detached.Value(CtxKeyTraceID); v != nil {
		t.Fatalf("trace_id = %v, want nil", v)
	}
	if err := detached.Err(); err != nil {
		t.Fatalf("detached ctx err = %v, want nil", err)
	}
}
