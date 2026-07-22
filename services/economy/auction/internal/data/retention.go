// retention.go — auction 保留期清理数据层(2026-07-21,CLAUDE.md §9 不变量 24 落地)。
//
// 背景:auction_orders 终态行(FILLED/CANCELED/EXPIRED)、auction_matches 已结算行、
// auction_idempotency_keys 映射行均只增不删,随挂单/成交量无界线性增长。
// 本文件按分片(DBRouter.All())批删超保留期的行:
//
//	auction_orders           终态且 release_pending=0 / match_pending=0(escrow 已释放、
//	                         无续跑意图)且 updated_at_ms 超期。uk_owner_idem 幂等键随行
//	                         删除:客户端挂单幂等键是每次请求生成的,重试窗口分钟级,90 天
//	                         后同 key 重放不可能是同一笔业务请求。
//	auction_matches          settlement_status=COMPLETED 且 event_pending=0 且 matched_at_ms
//	                         超期(结算/事件补偿只扫 PENDING 行,删已完成行不影响补偿闭环;
//	                         inventory 侧结算幂等流水同为 90 天保留,窗口一致)。
//	auction_idempotency_keys created_at_ms 超期(canonical 映射只需覆盖挂单重试窗口;
//	                         与 orders 分片不同——keys 在 owner 分片,orders 在 market 分片,
//	                         无法 join 删,按创建时间独立清理)。
//
// 不清理:auction_owner_guards(每 owner 一行,被玩家数有界)、auction_shard_topology(单行)。
// 多副本:各副本独立逐分片跑,DELETE 幂等无需锁;单批 limit 有界。
package data

import (
	"context"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// PurgeRetention 对所有分片跑一轮保留期清理(每分片每表至多一批 limit 行)。
// 返回三类各自的总删除行数;任一分片/表失败立即返回错误(下一轮重试,不破坏幂等)。
func (r *MySQLAuctionRepo) PurgeRetention(ctx context.Context, cutoffMs int64, limit int) (orders, matches, idemKeys int64, err error) {
	for _, db := range r.r.All() {
		res, oerr := db.ExecContext(ctx,
			`DELETE FROM auction_orders
			 WHERE status IN (?, ?, ?) AND release_pending = 0 AND match_pending = 0 AND updated_at_ms < ?
			 LIMIT ?`,
			StatusFilled, StatusCanceled, StatusExpired, cutoffMs, limit)
		if oerr != nil {
			return orders, matches, idemKeys, errcode.New(errcode.ErrInternal, "purge terminal orders: %v", oerr)
		}
		n, _ := res.RowsAffected()
		orders += n

		res, merr := db.ExecContext(ctx,
			`DELETE FROM auction_matches
			 WHERE settlement_status = ? AND event_pending = 0 AND matched_at_ms < ?
			 LIMIT ?`, SettlementCompleted, cutoffMs, limit)
		if merr != nil {
			return orders, matches, idemKeys, errcode.New(errcode.ErrInternal, "purge settled matches: %v", merr)
		}
		n, _ = res.RowsAffected()
		matches += n

		res, kerr := db.ExecContext(ctx,
			`DELETE FROM auction_idempotency_keys WHERE created_at_ms < ? LIMIT ?`, cutoffMs, limit)
		if kerr != nil {
			return orders, matches, idemKeys, errcode.New(errcode.ErrInternal, "purge idempotency keys: %v", kerr)
		}
		n, _ = res.RowsAffected()
		idemKeys += n
	}
	return orders, matches, idemKeys, nil
}
