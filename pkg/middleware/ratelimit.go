// Package middleware — RateLimit middleware(第 4 层:自动过载保护)
//
// 用 Kratos 自带的 ratelimit middleware(底层 go-kratos/aegis BBR 自适应限流):
// 基于 CPU / inflight / RT 动态判断是否过载,过载时拒绝新请求,保护服务不被打爆。
//
// 跟 Kill-Switch 的区别:
//   - Kill-Switch 是「人工临时关某个 RPC」(手动、精确、修 bug 用)。
//   - RateLimit 是「系统自动在过载时丢请求」(自动、全局、保命用)。
//   两者互补,都挂在 server 默认链上。
//
// 命中限流时 Kratos 返回 ratelimit.ErrLimitExceeded(reason "RATELIMIT"),
// 客户端按可重试处理(对应 errcode.ErrRateLimited)。
package middleware

import (
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/middleware/ratelimit"
)

// RateLimit 返回 BBR 自适应限流 middleware(server 侧)。
//
// 无需配阈值:BBR 根据运行时负载自适应。建议挂在默认链靠外层
// (Recovery 之后、业务之前),让被限流的请求尽早拒绝、少占资源。
func RateLimit() middleware.Middleware {
	return ratelimit.Server()
}
