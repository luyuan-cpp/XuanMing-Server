// grpc.go — Pandora 错误码 → gRPC 标准状态码映射(R4 复审 P1-1)。
//
// 背景:*Error 不实现 GRPCStatus(),raw gRPC 路径(如 push Subscribe server stream,
// 不经 Kratos unary 中间件链)把它原样返回时,客户端一律收到 UNKNOWN——UE 无法区分
// 「会话已失效(换新 session 才能恢复)」与「依赖故障(退避重连即可)」,过期会话
// 每秒重连形成风暴。流/raw 路径的最终返回值必须经 ToGRPCError 显式转换。
//
// 刻意做成**显式转换助手**而不是给 *Error 加 GRPCStatus() 方法:后者会静默改变
// 全部服务 unary 路径的线上错误形态(Kratos errors.FromError 同样识别该接口),
// 影响面无法逐点验证;显式转换按调用点渐进接入(§15 最小复杂度)。
package errcode

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GRPCCode 把公共错误码段映射到 gRPC 标准码。业务段错误码(≥1000)不逐一映射,
// 归入 Unknown 并保留 "errcode=%d" 消息文本供客户端解析业务语义。
func GRPCCode(c Code) codes.Code {
	switch c {
	case OK:
		return codes.OK
	case ErrTimeout:
		return codes.DeadlineExceeded
	case ErrInvalidArg:
		return codes.InvalidArgument
	case ErrNotFound:
		return codes.NotFound
	case ErrAlreadyExists:
		return codes.AlreadyExists
	case ErrPermissionDeny:
		return codes.PermissionDenied
	case ErrUnauthorized:
		return codes.Unauthenticated
	case ErrRateLimited:
		return codes.ResourceExhausted
	case ErrUnavailable, ErrServiceDisabled:
		return codes.Unavailable
	case ErrCanceled:
		return codes.Canceled
	default:
		return codes.Unknown
	}
}

// ToGRPCError 把错误转换为携带标准 gRPC 状态码的错误(消息保留 "errcode=%d ..." 原文)。
// 已携带 gRPC 状态的错误(status 错误、Kratos 错误等实现 GRPCStatus 者)原样透传;
// 非 *Error 的普通错误同样原样返回(交由 gRPC 默认 Unknown 处理,不伪造语义)。
func ToGRPCError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	var e *Error
	if errors.As(err, &e) {
		return status.Error(GRPCCode(e.Code), e.Error())
	}
	return err
}
