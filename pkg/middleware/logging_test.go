// logging_test.go — access-log 中间件对 in-band 服务端故障的分级回归测试。
//
// 背景:各 service handler 普遍以 `&Resp{Code: toProtoCode(err)}, nil` 返回错误
// (transport error 恒为 nil)。中间件旧实现只按 transport error 判 rpc_failed,于是
// MySQL/Redis/CAS/依赖超时等被返回成 in-band ErrInternal/ErrUnavailable 的故障都被记成
// rpc_ok(DEBUG),线上 info 级静默。本文件锁死修复:server hop 上 in-band 服务端故障码
// 升 ERROR(rpc_inband_error),预期业务拒绝码与成功仍是 rpc_ok(DEBUG),client hop 不升级。
package middleware

import (
	"context"
	"testing"

	klog "github.com/go-kratos/kratos/v2/log"
	"github.com/go-kratos/kratos/v2/transport"

	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
)

// captureLogger 是记录每条日志级别 + keyvals 的 klog.Logger,用于断言中间件实际打的级别。
type captureLogger struct {
	records []captureRecord
}

type captureRecord struct {
	level klog.Level
	kv    []any
}

func (c *captureLogger) Log(level klog.Level, keyvals ...any) error {
	c.records = append(c.records, captureRecord{level: level, kv: append([]any(nil), keyvals...)})
	return nil
}

// msgAndLevel 返回最后一条含 "msg" 键的记录的 msg 值与级别。
func (c *captureLogger) msgAndLevel() (string, klog.Level, bool) {
	for i := len(c.records) - 1; i >= 0; i-- {
		r := c.records[i]
		for j := 0; j+1 < len(r.kv); j += 2 {
			if k, ok := r.kv[j].(string); ok && k == "msg" {
				if v, ok := r.kv[j+1].(string); ok {
					return v, r.level, true
				}
			}
		}
	}
	return "", 0, false
}

// codeResp 是携带统一 commonv1.ErrCode 的假响应(等价于生成的 proto 响应)。
type codeResp struct{ code commonv1.ErrCode }

func (r codeResp) GetCode() commonv1.ErrCode { return r.code }

func withCapturedLogger(t *testing.T) *captureLogger {
	t.Helper()
	// plog.With(ctx) 从 klog.DefaultLogger(导出变量)派生,而非 SetLogger 的全局 logger,
	// 故这里覆盖 DefaultLogger 才能截获中间件实际打的日志。
	prev := klog.DefaultLogger
	cap := &captureLogger{}
	klog.DefaultLogger = cap
	t.Cleanup(func() { klog.DefaultLogger = prev })
	return cap
}

func runLogging(ctx context.Context, resp any, err error) {
	handler := func(context.Context, any) (any, error) { return resp, err }
	_, _ = Logging()(handler)(ctx, struct{}{})
}

func TestLogging_InbandServerFault_ServerHop_ElevatesToError(t *testing.T) {
	cap := withCapturedLogger(t)
	ctx := transport.NewServerContext(context.Background(), newMockTransport())

	runLogging(ctx, codeResp{code: commonv1.ErrCode_ERR_INTERNAL}, nil)

	msg, level, ok := cap.msgAndLevel()
	if !ok {
		t.Fatal("no log record captured")
	}
	if msg != "rpc_inband_error" {
		t.Fatalf("expected rpc_inband_error, got %q", msg)
	}
	if level != klog.LevelError {
		t.Fatalf("expected ERROR level, got %v", level)
	}
}

func TestLogging_InbandBusinessReject_ServerHop_StaysRpcOk(t *testing.T) {
	cap := withCapturedLogger(t)
	ctx := transport.NewServerContext(context.Background(), newMockTransport())

	// ErrNotFound 是预期业务拒绝,不是服务端故障 → 仍应是 rpc_ok(DEBUG),不升 ERROR。
	runLogging(ctx, codeResp{code: commonv1.ErrCode_ERR_NOT_FOUND}, nil)

	msg, level, ok := cap.msgAndLevel()
	if !ok {
		t.Fatal("no log record captured")
	}
	if msg != "rpc_ok" {
		t.Fatalf("expected rpc_ok for business reject, got %q", msg)
	}
	if level != klog.LevelDebug {
		t.Fatalf("expected DEBUG level, got %v", level)
	}
}

func TestLogging_InbandServerFault_ClientHop_NotElevated(t *testing.T) {
	cap := withCapturedLogger(t)
	// client hop:resp 是下游响应,其内部故障归下游服务记,本侧不升 ERROR。
	ctx := transport.NewClientContext(context.Background(), newMockTransport())

	runLogging(ctx, codeResp{code: commonv1.ErrCode_ERR_UNAVAILABLE}, nil)

	msg, _, ok := cap.msgAndLevel()
	if !ok {
		t.Fatal("no log record captured")
	}
	if msg == "rpc_inband_error" {
		t.Fatalf("client hop must not elevate in-band fault to rpc_inband_error")
	}
}

func TestLogging_SuccessOk_StaysDebug(t *testing.T) {
	cap := withCapturedLogger(t)
	ctx := transport.NewServerContext(context.Background(), newMockTransport())

	runLogging(ctx, codeResp{code: commonv1.ErrCode_OK}, nil)

	msg, level, ok := cap.msgAndLevel()
	if !ok {
		t.Fatal("no log record captured")
	}
	if msg != "rpc_ok" || level != klog.LevelDebug {
		t.Fatalf("expected rpc_ok DEBUG, got %q %v", msg, level)
	}
}

// inbandServerFault 单元覆盖:内部类为 true,业务类/无 Code 接口为 false。
func TestInbandServerFault(t *testing.T) {
	if _, fault := inbandServerFault(codeResp{code: commonv1.ErrCode_ERR_INTERNAL}); !fault {
		t.Error("ERR_INTERNAL should be a server fault")
	}
	if _, fault := inbandServerFault(codeResp{code: commonv1.ErrCode_ERR_UNAVAILABLE}); !fault {
		t.Error("ERR_UNAVAILABLE should be a server fault")
	}
	if _, fault := inbandServerFault(codeResp{code: commonv1.ErrCode_ERR_NOT_FOUND}); fault {
		t.Error("ERR_NOT_FOUND is a business reject, not a server fault")
	}
	if _, fault := inbandServerFault(codeResp{code: commonv1.ErrCode_OK}); fault {
		t.Error("OK is not a fault")
	}
	if _, fault := inbandServerFault(struct{}{}); fault {
		t.Error("a response without GetCode() must not be treated as a fault")
	}
}
