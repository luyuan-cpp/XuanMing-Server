// consumer.go — kafka 消费 handler(W4 ④,2026-06-06)。
//
// player 订阅 pandora.player.update(battle_result 结算后发),解 proto → 幂等 UpdateMMR
// (idempotency_key=match_id,不变量 §2)。decode 失败用 kafkax.Poison 包装(毒丸消息)
// → 消费者直接投 DLQ;业务瞬时错误走重试→耗尽进 DLQ(不丢 MMR 更新)。
package biz

import (
	"context"
	"strconv"

	"github.com/IBM/sarama"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"
)

// PlayerUpdateHandler 返回 pandora.player.update 的消费 handler(幂等 UpdateMMR)。
//
// 域内多事件类型(push.proto):本消费者只处理 event_type=0(旧 PlayerUpdateEvent,MMR)。
// event_type≠0(如 EXPERIENCE=1,由 player 自己出箱生产、push 转发客户端)不属本消费者,
// 必须按 header 跳过 —— 否则按 PlayerUpdateEvent 反序列化会因 wire type 冲突进 DLQ。
func (u *PlayerUsecase) PlayerUpdateHandler() kafkax.Handler {
	return func(ctx context.Context, msg *sarama.ConsumerMessage) error {
		if et := headerEventType(msg.Headers); et != uint32(playerv1.PlayerPushEventType_PLAYER_PUSH_EVENT_TYPE_LEGACY_UPDATE) {
			return nil
		}
		evt := &playerv1.PlayerUpdateEvent{}
		if err := proto.Unmarshal(msg.Value, evt); err != nil {
			return kafkax.Poison(errcode.New(errcode.ErrInvalidArg, "decode player.update offset=%d: %v", msg.Offset, err))
		}
		if evt.GetPlayerId() == 0 {
			plog.With(ctx).Warnw("msg", "player_update_missing_player_id", "offset", msg.Offset)
			return nil
		}
		if evt.GetMatchId() == 0 {
			// 幂等键缺失:无法保证不变量 §2,丢弃(battle_result 正常路径必带 match_id)
			plog.With(ctx).Warnw("msg", "player_update_missing_match_id",
				"player_id", evt.GetPlayerId(), "offset", msg.Offset)
			return nil
		}
		key := strconv.FormatUint(evt.GetMatchId(), 10)
		_, _, err := u.UpdateMMR(ctx, evt.GetPlayerId(), evt.GetMmrDelta(), evt.GetReason(), key)
		return err
	}
}

// headerEventType 从 kafka headers 解析 event_type(缺失 / 解析失败 → 0,兼容旧 producer;
// 语义对齐 push 服务 consumer 的 headerUint32)。
func headerEventType(headers []*sarama.RecordHeader) uint32 {
	for _, h := range headers {
		if h == nil || string(h.Key) != kafkax.HeaderEventType {
			continue
		}
		v, err := strconv.ParseUint(string(h.Value), 10, 32)
		if err != nil {
			return 0
		}
		return uint32(v)
	}
	return 0
}
