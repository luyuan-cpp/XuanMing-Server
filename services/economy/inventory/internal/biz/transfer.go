// transfer.go — 邮件 transfer 附件实例托管用例(2026-07-22,bag-domain.md §7.1)。
//
// 既存装备实例"只改归属"的托管转移链(拍卖成交到账 / 玩家转赠 / 活动补发已鉴定物):
// 本层做请求形状校验(非空 / 上限 / 无重复 / 幂等键形状),事务性搬移与幂等回放在
// data/inventory_transfer.go。三 RPC 均为系统接口(service 层拒带玩家 JWT 的调用)。
package biz

import (
	"context"
	"fmt"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"

	"github.com/luyuancpp/pandora/services/economy/inventory/internal/data"
)

// maxTransferBatch 单次托管/领取/释放的实例数上限(mail 单封附件上限 16 的宽裕倍;
// 防超大批量长事务锁表)。
const maxTransferBatch = 64

// validateTransferIDs 校验实例 ID 列表形状(非空 / 上限 / 无零值 / 无重复)。
func validateTransferIDs(instanceIDs []uint64) error {
	if len(instanceIDs) == 0 {
		return errcode.New(errcode.ErrInvalidArg, "instance_ids required")
	}
	if len(instanceIDs) > maxTransferBatch {
		return errcode.New(errcode.ErrInvalidArg, "instance_ids %d exceed max %d", len(instanceIDs), maxTransferBatch)
	}
	seen := make(map[uint64]bool, len(instanceIDs))
	for _, id := range instanceIDs {
		if id == 0 {
			return errcode.New(errcode.ErrInvalidArg, "instance_id required")
		}
		if seen[id] {
			return errcode.New(errcode.ErrInvalidArg, "duplicate instance_id %d", id)
		}
		seen[id] = true
	}
	return nil
}

// validateIdemKey 校验幂等键形状(非空且不超 inventory_ledger 列宽 64)。
func validateIdemKey(key string) error {
	if key == "" || len(key) > 64 {
		return errcode.New(errcode.ErrInvalidArg, "invalid idempotency key")
	}
	return nil
}

// EscrowOutInstances 从源玩家扣出实例并托管(发 transfer 邮件前的 saga 第一步)。
// source==to 合法(活动补发 / 切代 salvage 把玩家自己的已鉴定物经邮件送回)。
func (u *InventoryUsecase) EscrowOutInstances(ctx context.Context, sourcePlayerID, toPlayerID uint64, instanceIDs []uint64, escrowKey string) ([]data.EscrowedInstance, error) {
	if sourcePlayerID == 0 || toPlayerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "source/to player_id required")
	}
	if err := validateTransferIDs(instanceIDs); err != nil {
		return nil, err
	}
	if err := validateIdemKey(escrowKey); err != nil {
		return nil, err
	}
	detail := fmt.Sprintf("escrow_out to=%d n=%d", toPlayerID, len(instanceIDs))
	rows, already, err := u.repo.EscrowOutInstances(ctx, sourcePlayerID, toPlayerID, instanceIDs, escrowKey, detail)
	if err != nil {
		return nil, err
	}
	if already {
		plog.With(ctx).Infow("msg", "escrow_out_idempotent_hit",
			"source_player_id", sourcePlayerID, "escrow_key", escrowKey, "count", len(rows))
	}
	return rows, nil
}

// ClaimTransferInstances 领取托管实例(mail ClaimMail 专用)。
func (u *InventoryUsecase) ClaimTransferInstances(ctx context.Context, toPlayerID uint64, items []data.TransferClaimItem, idempotencyKey string) error {
	if toPlayerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "to_player_id required")
	}
	ids := make([]uint64, 0, len(items))
	for _, it := range items {
		if it.ItemConfigID == 0 {
			return errcode.New(errcode.ErrInvalidArg, "item_config_id required instance=%d", it.InstanceID)
		}
		ids = append(ids, it.InstanceID)
	}
	if err := validateTransferIDs(ids); err != nil {
		return err
	}
	if err := validateIdemKey(idempotencyKey); err != nil {
		return err
	}
	detail := fmt.Sprintf("transfer_claim n=%d", len(items))
	already, err := u.repo.ClaimTransferInstances(ctx, toPlayerID, items, u.cfg.Capacity, idempotencyKey, detail)
	if err != nil {
		return err
	}
	if already {
		plog.With(ctx).Infow("msg", "transfer_claim_idempotent_hit",
			"to_player_id", toPlayerID, "idempotency_key", idempotencyKey, "count", len(items))
	}
	return nil
}

// ReleaseTransferEscrow 托管释放回源玩家(发信 saga 失败补偿;行缺失 no-op 幂等)。
func (u *InventoryUsecase) ReleaseTransferEscrow(ctx context.Context, instanceIDs []uint64) error {
	if err := validateTransferIDs(instanceIDs); err != nil {
		return err
	}
	released, err := u.repo.ReleaseTransferEscrow(ctx, instanceIDs)
	if err != nil {
		return err
	}
	if released != len(instanceIDs) {
		// 缺行 = 已被领取或已释放,no-op 属预期;打观测日志便于审计核对,不算错。
		plog.With(ctx).Infow("msg", "release_transfer_escrow_partial",
			"requested", len(instanceIDs), "released", released)
	}
	return nil
}

// ConsumeTransferEscrow 消托管行不物化(bag phase 2 DS 领取链;mail.MarkMailClaimed 调)。
func (u *InventoryUsecase) ConsumeTransferEscrow(ctx context.Context, toPlayerID uint64, instanceIDs []uint64) error {
	if toPlayerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "to_player_id required")
	}
	if err := validateTransferIDs(instanceIDs); err != nil {
		return err
	}
	consumed, err := u.repo.ConsumeTransferEscrow(ctx, toPlayerID, instanceIDs)
	if err != nil {
		return err
	}
	if consumed != len(instanceIDs) {
		// 缺行 = 重放已消费,幂等预期;观测日志留审计线索。
		plog.With(ctx).Infow("msg", "consume_transfer_escrow_partial",
			"to_player_id", toPlayerID, "requested", len(instanceIDs), "consumed", consumed)
	}
	return nil
}
