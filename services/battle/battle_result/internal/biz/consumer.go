// consumer.go — kafka 消费 handler(W4 ③,2026-06-06)。
//
// battle_result 订阅两个 topic,各用一个 kafkax.KeyOrderedConsumer(cmd 层装配),
// handler 在此定义:解 proto → 调 usecase。decode 失败返 ErrBattleResultDecode
// (W1-D2 简化版 kafkax 当前 log+ack,W2+ 接 retry/DLQ 时此 error 才进重试队列)。
package biz

import (
	"context"

	"github.com/IBM/sarama"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/kafkax"
	plog "github.com/luyuancpp/pandora/pkg/log"
	battlev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/battle/v1"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

// BattleResultHandler 返回 pandora.battle.result 的消费 handler(幂等落库 + MMR)。
func (u *BattleResultUsecase) BattleResultHandler() kafkax.Handler {
	return func(ctx context.Context, msg *sarama.ConsumerMessage) error {
		result := &battlev1.BattleResult{}
		if err := proto.Unmarshal(msg.Value, result); err != nil {
			return errcode.New(errcode.ErrBattleResultDecode, "decode battle.result offset=%d: %v", msg.Offset, err)
		}
		_, err := u.ReportResult(ctx, result)
		return err
	}
}

// DSLifecycleHandler 返回 pandora.ds.lifecycle 的消费 handler。
// 只处理 ABANDONED(W4 ③ 补偿,不变量 §4),其余阶段忽略。
func (u *BattleResultUsecase) DSLifecycleHandler() kafkax.Handler {
	return func(ctx context.Context, msg *sarama.ConsumerMessage) error {
		evt := &dsv1.DSLifecycleEvent{}
		if err := proto.Unmarshal(msg.Value, evt); err != nil {
			return errcode.New(errcode.ErrBattleResultDecode, "decode ds.lifecycle offset=%d: %v", msg.Offset, err)
		}
		if evt.GetPhase() != dsv1.DSLifecyclePhase_DS_LIFECYCLE_PHASE_ABANDONED {
			plog.With(ctx).Debugw("msg", "ds_lifecycle_ignored", "phase", evt.GetPhase().String(), "match_id", evt.GetMatchId())
			return nil
		}
		return u.HandleAbandoned(ctx, evt.GetMatchId(), evt.GetPlayerIds(), evt.GetMapId(), evt.GetGameMode(), evt.GetTsMs())
	}
}
