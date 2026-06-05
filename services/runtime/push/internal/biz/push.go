// Package biz 是 push 服务的业务逻辑层(usecase)。
//
// W3 ④(2026-06-05)真实化:
//   - 删除 W2 RunMockStream(mock tick 退役)
//   - 新增 RunSubscribeStream:按 last_seen_ms 从 OfflineCacheRepo 拉补推帧,
//     再阻塞等 ctx.Done(实际推送由 KafkaConsumer 调 ConnectionManager.SendTo)
//
// 职责分层(对齐 login 服务):
//
//	service/  RPC 入口,只做 proto 与 biz 类型互转、stream 注册
//	biz/      usecase,补推 + 阻塞等推送
//	data/     仓储,redis ZSET 离线缓存(W3 ④)
package biz

import (
	"context"

	plog "github.com/luyuancpp/pandora/pkg/log"

	"github.com/luyuancpp/pandora/services/runtime/push/internal/data"
)

// PushUsecase 是 push 服务用例。
//
// W3 ④:持有 ConnectionManager + OfflineCacheRepo。
// kafka consumer 由 main.go 直接装配并启动,不通过 usecase。
type PushUsecase struct {
	conns   *ConnectionManager
	offline data.OfflineCacheRepo
}

// NewPushUsecase 构造 PushUsecase。
func NewPushUsecase(conns *ConnectionManager, offline data.OfflineCacheRepo) *PushUsecase {
	return &PushUsecase{conns: conns, offline: offline}
}

// Conns 暴露 ConnectionManager,给 service 层 Register/Unregister 用。
func (u *PushUsecase) Conns() *ConnectionManager {
	return u.conns
}

// RunSubscribeStream 跑一个 Subscribe stream 的生命周期(W3 ④)。
//
// 流程:
//  1. 若 sinceMs > 0,从 offline 拉补推帧 → slot.SafeSend;期间任何 ctx 取消或 Send 失败即返回
//  2. 补推完成后,select 等 ctx.Done:
//     - client 断开 / server 关闭 / 顶号 cancel → ctx.Done,返回 nil
//     - 期间 KafkaConsumer 调 cm.SendTo(playerID, frame) 把新消息直接推到 stream
//
// **必须用 slot.SafeSend(不能直接 stream.Send)**:与 KafkaConsumer.SendTo 共享 sendMu 串行化,
// 防止 replay 循环与实时推送并发写同一 ServerStream 撕坏 HTTP/2 帧(Opus W3 ④ 审查 R1)。
//
// 注意:在线期间的新消息**不走 usecase**,直接由 ConnectionManager 路由 stream;
// 这里只负责"补推 + 守门",不耦合 kafka。
func (u *PushUsecase) RunSubscribeStream(
	ctx context.Context,
	slot *StreamSlot,
	playerID uint64,
	sinceMs int64,
) error {
	h := plog.With(ctx)

	// 1. 补推离线帧
	if sinceMs > 0 && playerID > 0 {
		frames, err := u.offline.Range(ctx, playerID, sinceMs)
		if err != nil {
			// 拉补推失败不阻断订阅(降级:玩家可能丢离线消息,但仍能收新消息)
			h.Warnw("msg", "push_offline_range_failed", "player_id", playerID, "since_ms", sinceMs, "err", err)
		}
		for _, f := range frames {
			if err := ctx.Err(); err != nil {
				return nil
			}
			if err := slot.SafeSend(f.Frame); err != nil {
				h.Warnw("msg", "push_offline_replay_send_failed", "player_id", playerID, "ts_ms", f.ScoreMs, "err", err)
				return err
			}
		}
		if n := len(frames); n > 0 {
			h.Infow("msg", "push_offline_replayed", "player_id", playerID, "since_ms", sinceMs, "count", n)
		}
	}

	// 2. 阻塞等 ctx.Done(实际推送由 KafkaConsumer 调 cm.SendTo)
	<-ctx.Done()
	return nil
}
