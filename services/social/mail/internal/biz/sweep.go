// sweep.go — mail 服务过期清理(2026-07-21,docs/design/mail.md §2.4 落地)。
//
// 背景:邮件表只增不减会无界增长(写扩散的 player_mail 与领取写扩散的 player_mail_claim
// 尤甚)。前提是一切邮件生命有限(end_ms/expire_ms 为 0 时发送侧已补默认 TTL),本文件
// 周期批量回收:
//
//	player_mail         过期 + 缓冲期后:已领/无附件直删;带未领附件的先归档再删
//	                    (mail.md §7.4 "过期附件清理后必有补偿或归档,不静默丢失")
//	sys_mail/guild_mail 失效(end_ms 过)+ 缓冲期后直删(领取幂等由 inventory 键兜底)
//	player_mail_claim   邮件本体按最长寿命清完后,按雪花 mail_id cutoff 范围删
//	player_mail_archive 超归档保留期后删(归档表自身有界)
//
// 多副本:各副本独立跑,无锁(对齐 leaderboard 发奖补扫模式)——删除/INSERT IGNORE 幂等,
// 并发只多花几次空批,不破坏正确性。每轮每表单批 limit 有界,积压跨轮摊平,不长事务锁表。
package biz

import (
	"context"

	"google.golang.org/protobuf/proto"

	plog "github.com/luyuancpp/pandora/pkg/log"
	"github.com/luyuancpp/pandora/pkg/snowflake"
	mailv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/mail/v1"

	"github.com/luyuancpp/pandora/services/social/mail/internal/data"
)

const dayMs = int64(86400_000)

// SweepExpired 跑一轮清理,每表至多一批(cfg.SweepBatch),返回后由调用方 ticker 驱动下一轮。
// 任一步失败只记日志继续后面的表:清理彼此独立,幂等,下一轮自然重试。
func (u *MailUsecase) SweepExpired(ctx context.Context, nowMs int64) {
	log := plog.With(ctx)

	// 个人邮件:过期 + 缓冲期后,按 payload 是否有未领附件分流(归档 or 直删)
	expireBefore := nowMs - int64(u.cfg.ExpiredRetentionDays)*dayMs
	rows, err := u.repo.ListExpiredPersonal(ctx, expireBefore, u.cfg.SweepBatch)
	if err != nil {
		log.Warnw("msg", "mail_sweep_list_expired_failed", "err", err)
	} else if len(rows) > 0 {
		archive, deleteIDs := partitionExpired(rows)
		if err := u.repo.ArchiveAndDeletePersonal(ctx, archive, deleteIDs); err != nil {
			log.Warnw("msg", "mail_sweep_personal_failed", "err", err)
		} else {
			log.Infow("msg", "mail_sweep_personal", "deleted", len(deleteIDs), "archived", len(archive))
		}
	}

	// 系统/公会邮件:失效 + 缓冲期后直删(游标玩家侧天然跳过,不参与拉取)
	endBefore := nowMs - int64(u.cfg.ExpiredRetentionDays)*dayMs
	if n, err := u.repo.DeleteSysMailEndedBefore(ctx, endBefore, u.cfg.SweepBatch); err != nil {
		log.Warnw("msg", "mail_sweep_sys_failed", "err", err)
	} else if n > 0 {
		log.Infow("msg", "mail_sweep_sys", "deleted", n)
	}
	if n, err := u.repo.DeleteGuildMailEndedBefore(ctx, endBefore, u.cfg.SweepBatch); err != nil {
		log.Warnw("msg", "mail_sweep_guild_failed", "err", err)
	} else if n > 0 {
		log.Infow("msg", "mail_sweep_guild", "deleted", n)
	}

	// 领取记录:雪花 mail_id 时间段单调,mail_id < MinIDAt(cutoff) ⇔ 邮件创建早于 cutoff。
	// ClaimRetentionDays 大于一切邮件最长寿命 → 被删 claim 的邮件本体必已清;即便运营
	// 显式设了超长 end_ms 的例外邮件,提前删 claim 也只影响 UI 已领标记,重复发奖被
	// inventory 幂等键(mail:{mail_id}:{player_id})兜住,不会重发。
	claimCutoffSec := (nowMs - int64(u.cfg.ClaimRetentionDays)*dayMs) / 1000
	if maxID := snowflake.MinIDAt(claimCutoffSec); maxID > 0 {
		if n, err := u.repo.DeleteClaimsBefore(ctx, maxID, u.cfg.SweepBatch); err != nil {
			log.Warnw("msg", "mail_sweep_claims_failed", "err", err)
		} else if n > 0 {
			log.Infow("msg", "mail_sweep_claims", "deleted", n)
		}
	}

	// 归档表:超保留期后清除,归档表自身有界
	if n, err := u.repo.PurgeArchiveBefore(ctx, u.cfg.ArchiveRetentionDays, u.cfg.SweepBatch); err != nil {
		log.Warnw("msg", "mail_sweep_archive_failed", "err", err)
	} else if n > 0 {
		log.Infow("msg", "mail_sweep_archive", "purged", n)
	}
}

// partitionExpired 把过期个人邮件分流:带未领附件的进归档(留补偿凭据),
// 已领(status=claimed)或无附件的直删。deleteIDs 含归档行(归档后本体也要删)。
// payload 解码失败的行保守归档(内容未知,宁多存不误删)。
func partitionExpired(rows []data.ExpiredPersonalRow) (archive []data.ExpiredPersonalRow, deleteIDs []uint64) {
	deleteIDs = make([]uint64, 0, len(rows))
	for _, m := range rows {
		deleteIDs = append(deleteIDs, m.MailID)
		if m.Status == data.StatusClaimed {
			continue
		}
		rec := &mailv1.MailContentStorageRecord{}
		if err := proto.Unmarshal(m.Payload, rec); err != nil || len(rec.GetAttachments()) > 0 {
			archive = append(archive, m)
		}
	}
	return archive, deleteIDs
}
