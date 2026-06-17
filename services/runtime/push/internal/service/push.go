// Package service 是 push 服务的 RPC 入口层。
//
// 职责:
//   - 实现 pushv1.PushServiceServer 接口
//   - Subscribe:校验 session(W2 mock 跳过)→ 注册 stream → 跑 mock 推送循环 → 退出时反注册
//
// 不变量(docs/design/protocol-ordering-rules.md 原则 3):
//   - Subscribe 是"已受理 + 长连"型,不是立即完成型 RPC
//   - 客户端拿到 stream 后,等待 server 推 PushFrame;直到 client 主动关闭或 server 断开
package service

import (
	"context"

	plog "github.com/luyuancpp/pandora/pkg/log"
	pmw "github.com/luyuancpp/pandora/pkg/middleware"
	pushv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/push/v1"

	"github.com/luyuancpp/pandora/services/runtime/push/internal/biz"
)

// PushService 实现 pushv1.PushServiceServer。
//
// 内嵌 UnimplementedPushServiceServer 以满足 grpc 向前兼容约束。
type PushService struct {
	pushv1.UnimplementedPushServiceServer

	uc *biz.PushUsecase
}

// NewPushService 注入 PushUsecase。
func NewPushService(uc *biz.PushUsecase) *PushService {
	return &PushService{uc: uc}
}

// Subscribe 处理客户端长连接订阅(server stream)。
//
// W3 ① 流程(2026-06-05):
//  1. Envoy jwt_authn filter 已校验 JWT 并把 sub 提到 x-pandora-player-id 头
//  2. 本方法从 ctx 取 player_id;0 表示匿名(直连 :50014 联调时正常)
//  3. 注册 stream 到 ConnectionManager(顶号语义:旧 stream 会被 close),拿到 *StreamSlot
//  4. defer 反注册
//
// ⚠️ player_id 提取(2026-06-08 修复):Subscribe 是 server stream,Kratos v2 的 unary
// middleware 链(pmw.AuthOptional)**对 stream 不生效**,因此不能依赖 ctx.Value 拿
// 中间件注入的 player_id(那样恒为 0,kafka 业务推送无法路由到本 stream)。改用
// pmw.PlayerIDFromContext:它在 stream 路径直接从 Kratos transport 的 x-pandora-player-id
// 头(Envoy jwt_authn 注入)读取真实 player_id。
//
// W3 ④ 真实化(2026-06-05):
//   - 走 uc.RunSubscribeStream(slot, ...):按 req.LastSeenMs 补推 redis ZSET 离线帧,然后阻塞等 ctx.Done
//   - 实际新消息由 main.go 装配的 KafkaConsumer 调 cm.SendTo 直接推到 stream
//   - mock tick 已退役
//
// W3 ④ 二次修复(Opus 审查 R1):replay 补推与 KafkaConsumer.SendTo 共享 slot.sendMu 串行化,
// 防止两个 goroutine 并发 stream.Send 撕坏 HTTP/2 帧。
func (s *PushService) Subscribe(req *pushv1.SubscribeRequest, stream pushv1.PushService_SubscribeServer) error {
	ctx := stream.Context()

	// server stream 不跑 unary 中间件链,KillSwitch 不会自动生效,这里手动查一次开关。
	// 命中 Subscribe 关停规则时拒绝建连(返回 ErrServiceDisabled),修好后删规则即恢复。
	if err := pmw.KillSwitchStreamCheck(ctx); err != nil {
		return err
	}

	// server stream 不跑 unary 中间件链,直接从 transport header 取 Envoy 注入的 player_id。
	playerID := pmw.PlayerIDFromContext(ctx)
	if playerID > 0 {
		ctx = plog.WithPlayerID(ctx, playerID)
	}
	h := plog.With(ctx)

	// 注册到内存索引(KafkaConsumer 路由 SendTo 时按 player_id 找到这个 slot)
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	slot := s.uc.Conns().Register(playerID, stream, cancel)
	defer s.uc.Conns().Unregister(playerID, slot)

	h.Infow(
		"msg", "push_stream_open",
		"player_id", playerID,
		"last_seen_ms", req.GetLastSeenMs(),
		"online_total", s.uc.Conns().Size(),
	)

	return s.uc.RunSubscribeStream(subCtx, slot, playerID, req.GetLastSeenMs())
}
