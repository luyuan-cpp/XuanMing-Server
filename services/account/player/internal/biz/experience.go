// experience.go — 玩家等级经验入账 + 经验推送出箱发布器(实时成长,2026-07-20)。
//
// 设计(docs/design/realtime-progression.md §4.2):
//   - AddExperience 是**系统 RPC 的业务核心**:幂等入账(uk player_id+idempotency_key)、
//     按等级经验曲线循环进位(连升多级)、最高等级封顶、满级 no-op。
//   - 入账与经验推送出箱同一 MySQL 事务(不变量 §4);后台 RunPushOutboxPublisher 轮询
//     出箱表逐条投 kafka pandora.player.experience(event_type=EXPERIENCE header),
//     push 服务透明转发给客户端刷新经验条 / 播升级表现。
//   - 经验来源(battle_result progress 出箱 / 任务完成点 / GM)只报 delta,
//     等级曲线唯一权威在本服务(DS / 调用方不可信,不变量 §6 同构)。
//   - 多副本发布器无需 claim/fencing:事件是入账后的**全量权威快照**,客户端按
//     (level, exp_in_level) 单调不回退去重,重复投递 / 旧快照晚到都无副作用
//     (at-least-once + 快照语义,§16.2)。
package biz

import (
	"context"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"

	"github.com/luyuancpp/pandora/services/account/player/internal/data"
)

// ExperiencePusher 把玩家推送出箱行投递到 kafka pandora.player.experience
// (key=player_id 保序,event_type 走 kafka header,push 服务透传)。
// 投递失败返回 error → 出箱行保留下轮重试(at-least-once,不变量 §4)。
type ExperiencePusher interface {
	PushPlayerEvent(ctx context.Context, playerID uint64, eventType uint32, payload []byte) error
}

// SetExperiencePusher 注入推送出箱发布器的 kafka producer 适配(nil-safe:
// brokers 未配 / producer 构造失败时不注入,出箱积压不丢,恢复后重启补发)。
func (u *PlayerUsecase) SetExperiencePusher(p ExperiencePusher) {
	u.expPusher = p
}

// AddExperience 幂等入账经验并结算等级(实时成长唯一入口,系统调用)。
// 返回 (入账后快照, 是否幂等命中, error)。
//
//	曲线未配置(exp_curve 空)→ ErrPlayerFeatureDisabled(功能关闭,默认行为不变 §14.2)
//	delta 超单次上限 → ErrInvalidArg(防异常调用方一次灌满;上限见 conf.MaxExpPerGrant)
//	满级 → no-op:返回满级快照,不消费幂等键、不出箱
func (u *PlayerUsecase) AddExperience(ctx context.Context, playerID uint64, delta uint64, reason, idempotencyKey string) (data.ExpState, bool, error) {
	if playerID == 0 {
		return data.ExpState{}, false, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if idempotencyKey == "" {
		return data.ExpState{}, false, errcode.New(errcode.ErrInvalidArg, "idempotency_key required")
	}
	if delta == 0 {
		return data.ExpState{}, false, errcode.New(errcode.ErrInvalidArg, "exp_delta must be positive")
	}
	if maxGrant := u.cfg.MaxExpPerGrantOrDefault(); delta > maxGrant {
		return data.ExpState{}, false, errcode.New(errcode.ErrInvalidArg,
			"exp_delta %d exceeds max_exp_per_grant %d", delta, maxGrant)
	}
	curve := u.cfg.ExpCurve
	if len(curve) == 0 {
		return data.ExpState{}, false, errcode.New(errcode.ErrPlayerFeatureDisabled, "experience disabled (exp_curve empty)")
	}
	if err := u.repo.EnsureProfile(ctx, playerID, u.defaultNickname(playerID), u.cfg.BaseMMR); err != nil {
		return data.ExpState{}, false, err
	}

	st, already, err := u.repo.ApplyExperience(ctx, data.ExpApply{
		PlayerID:       playerID,
		Delta:          delta,
		Reason:         reason,
		IdempotencyKey: idempotencyKey,
		Curve:          curve,
	})
	if err != nil {
		return data.ExpState{}, false, err
	}
	if already {
		plog.With(ctx).Infow("msg", "add_experience_idempotent_hit",
			"player_id", playerID, "idempotency_key", idempotencyKey,
			"level", st.Level, "exp_in_level", st.ExpInLevel)
		return st, true, nil
	}
	plog.With(ctx).Infow("msg", "add_experience_applied",
		"player_id", playerID, "delta", delta, "reason", reason,
		"level", st.Level, "exp_in_level", st.ExpInLevel,
		"levels_gained", st.LevelsGained, "is_max", st.IsMaxLevel)
	return st, false, nil
}

// DecorateExperience 用等级曲线给档案补经验派生字段(GetProfile 出参装饰):
// 满级 → is_max_level=true 且级内经验按 0 展示(权威列已保证满级恒 0,此处防御性夹紧)。
// 曲线未配置(功能关闭)→ 不标满级,exp 原样(默认 0),行为与历史一致。
func (u *PlayerUsecase) DecorateExperience(level int32, expInLevel uint64) (uint64, bool) {
	if len(u.cfg.ExpCurve) == 0 {
		return expInLevel, false
	}
	maxLevel := int32(len(u.cfg.ExpCurve)) + 1
	if level >= maxLevel {
		return 0, true
	}
	return expInLevel, false
}

// RunPushOutboxPublisher 启动后台玩家推送出箱发布循环,直到 ctx 取消
// (发布器纪律对齐 battle_result.RunOutboxPublisher:FIFO、失败中断本轮保序、成功才删行)。
func (u *PlayerUsecase) RunPushOutboxPublisher(ctx context.Context) {
	if u.expPusher == nil {
		plog.With(ctx).Infow("msg", "push_outbox_publisher_disabled",
			"hint", "kafka producer 未注入 → 经验推送出箱积压不丢,producer 可用后重启补发")
		return
	}
	interval := u.cfg.PushOutboxIntervalOrDefault()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	plog.With(ctx).Infow("msg", "push_outbox_publisher_started",
		"interval", interval.String(), "batch", u.cfg.PushOutboxBatchOrDefault())
	for {
		select {
		case <-ctx.Done():
			plog.With(ctx).Infow("msg", "push_outbox_publisher_stopped")
			return
		case <-ticker.C:
			// 排空循环:满批说明还有积压,立即继续下一批,不等下个 tick
			// (否则吞吐被钉死在 batch/interval,持续流量下积压只增不减)。
			for ctx.Err() == nil {
				n, err := u.publishPushOutboxBatch(ctx)
				if err != nil {
					plog.With(ctx).Warnw("msg", "push_outbox_publish_batch_failed", "published", n, "err", err)
					break
				}
				if n < u.cfg.PushOutboxBatchOrDefault() {
					break // 未满批 = 已清空
				}
			}
		}
	}
}

// RunExpHistoryJanitor 启动 exp_history 幂等收据的后台清理循环,直到 ctx 取消。
//
// 设计(realtime-progression.md §4.2):留存期默认 7 天(覆盖 progress 出箱最长重试窗,
// 出箱重试退避上限 5min,量级远小于留存期),到期分批删除防表无限增长。
// 多副本并发 DELETE ... LIMIT 安全(各删各的行);表按 PK 升序扫,老行在前,无需新索引。
func (u *PlayerUsecase) RunExpHistoryJanitor(ctx context.Context) {
	const (
		sweepInterval = time.Hour
		purgeBatch    = 1000
	)
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-u.cfg.ExpHistoryRetentionOrDefault())
			var total int64
			for ctx.Err() == nil {
				n, err := u.repo.PurgeExpHistory(ctx, cutoff, purgeBatch)
				if err != nil {
					plog.With(ctx).Warnw("msg", "exp_history_purge_failed", "purged", total, "err", err)
					break
				}
				total += n
				if n < purgeBatch {
					break
				}
			}
			if total > 0 {
				plog.With(ctx).Infow("msg", "exp_history_purged", "rows", total)
			}
		}
	}
}

// publishPushOutboxBatch 取一批推送出箱记录投递,返回本轮成功投递并删除的条数。
// 投递失败立即中断本轮(保留出箱行下轮重试),保证同玩家事件按 id 顺序投递(不变量 §9)。
func (u *PlayerUsecase) publishPushOutboxBatch(ctx context.Context) (int, error) {
	if u.expPusher == nil {
		return 0, nil
	}
	recs, err := u.repo.FetchPushOutbox(ctx, u.cfg.PushOutboxBatchOrDefault())
	if err != nil {
		return 0, err
	}
	published := 0
	for _, r := range recs {
		if perr := u.expPusher.PushPlayerEvent(ctx, r.PlayerID, r.EventType, r.Payload); perr != nil {
			return published, perr // 本轮中断,保留出箱行下轮重试
		}
		if derr := u.repo.DeletePushOutbox(ctx, r.ID); derr != nil {
			return published, derr
		}
		published++
	}
	if published > 0 {
		plog.With(ctx).Infow("msg", "push_outbox_published", "count", published)
	}
	return published, nil
}
