// sweep.go — inventory 保留期清理(2026-07-21,CLAUDE.md §9 不变量 24 落地)。
//
// 背景:inventory_ledger 每笔发放/使用/出售写 1 行、拍卖成交/玩家交易写 2 行(买卖双方),
// auction_escrow 每笔冻结挂单写 1 行;两表只增不删会随玩家活跃度无界线性增长。
// 本文件周期批量回收:
//
//	inventory_ledger         created_at 超保留期(默认 90 天)后删。幂等键只需覆盖对应操作
//	                         的重试窗口(分钟级),90 天 ≫ 任何重试;唯一长横距回放源是邮件
//	                         领奖,已由 mail 发送侧把邮件寿命钳到 claim_retention_days 内 +
//	                         claim 行存活 ≥ 可领窗口闭环,不依赖本流水永久兜底。
//	auction_escrow (closed)  关闭(退还/完结)且 updated_at 超保留期后删;active 行永不清理
//	                         (EnsureAuctionEscrow 核对 OPEN/PARTIAL 遗留订单依赖其存在)。
//	                         删后迟到 ReleaseEscrow 命中 ErrNoRows → already no-op,fail-safe。
//
// player_items 的 count=0 行**故意不清**:行数被 uk(player_id,item_config_id) 有界
// (玩家数 × 道具配置数),不属于无界增长;删 0 行还会把「数量不足」(ErrInventoryInsufficient)
// 漂成「道具不存在」(ErrInventoryItemNotFound),得不偿失。
//
// 多副本:各副本独立跑,无锁(对齐 mail sweep / leaderboard 发奖补扫模式)——DELETE 幂等,
// 并发只多花几次空批,不破坏正确性。每轮每表单批 limit 有界,积压跨轮摊平,不长事务锁表。
package biz

import (
	"context"

	plog "github.com/luyuancpp/pandora/pkg/log"
)

// SweepRetention 跑一轮保留期清理,每表至多一批(cfg.SweepBatch),返回后由调用方 ticker
// 驱动下一轮。任一表失败只记日志继续下一表:清理彼此独立,幂等,下一轮自然重试。
func (u *InventoryUsecase) SweepRetention(ctx context.Context) {
	log := plog.With(ctx)

	if n, err := u.repo.DeleteLedgerBefore(ctx, u.cfg.LedgerRetentionDays, u.cfg.SweepBatch); err != nil {
		log.Warnw("msg", "inventory_sweep_ledger_failed", "err", err)
	} else if n > 0 {
		log.Infow("msg", "inventory_sweep_ledger", "deleted", n, "retention_days", u.cfg.LedgerRetentionDays)
	}

	if n, err := u.repo.DeleteClosedEscrowBefore(ctx, u.cfg.EscrowRetentionDays, u.cfg.SweepBatch); err != nil {
		log.Warnw("msg", "inventory_sweep_escrow_failed", "err", err)
	} else if n > 0 {
		log.Infow("msg", "inventory_sweep_escrow", "deleted", n, "retention_days", u.cfg.EscrowRetentionDays)
	}
}
