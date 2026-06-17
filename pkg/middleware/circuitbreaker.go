// Package middleware — CircuitBreaker middleware(第 4 层:自动熔断)
//
// 用 Kratos 自带的 circuitbreaker middleware(底层 go-kratos/aegis SRE breaker):
// 按调用成功/失败比例自动「断开 → 半开 → 闭合」,下游故障时快速失败,
// 避免雪崩(调用方线程 / 连接被卡死的下游拖垮)。
//
// ⚠️ 熔断是「客户端侧」防护:挂在调用方(grpcclient)的 client middleware 链上,
// 保护「我」不被「我依赖的下游」拖死。这与限流(server 侧,保护我不被上游打爆)对称。
//
// 熔断触发时 Kratos 返回 circuitbreaker.ErrNotAllowed(reason "CIRCUITBREAKER"),
// 对应 errcode.ErrUnavailable,客户端按可重试处理(可切换实例 / 退避重试)。
package middleware

import (
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/middleware/circuitbreaker"
)

// CircuitBreaker 返回 SRE 自适应熔断 middleware(client 侧)。
//
// 已内置进 pkg/grpcclient 的默认 client 链;按 endpoint+operation 维度各自统计。
func CircuitBreaker() middleware.Middleware {
	return circuitbreaker.Client()
}
