// Package biz — owner 权威用例(owner-authority.md §3)。
//
// 职责:入参形状校验(operation UUIDv4 / 目标身份完整性 / 类型合法性)→ 委托 OwnerRepo
// (CAS / 屏障 / 幂等全部在数据层事务内)。fence 常量单一来源 pkg/placement:
// skew margin 是 admit_not_before 的余量项,lease 秒数钳制 ≤ DSFenceLeaseMaxSeconds。
package biz

import (
	"context"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"

	"github.com/luyuancpp/pandora/services/runtime/owner/internal/conf"
	"github.com/luyuancpp/pandora/services/runtime/owner/internal/data"
)

// OwnerUsecase owner 权威用例。
type OwnerUsecase struct {
	repo data.OwnerRepo
	cfg  conf.OwnerConf
}

// NewOwnerUsecase 构造。
func NewOwnerUsecase(repo data.OwnerRepo, cfg conf.OwnerConf) *OwnerUsecase {
	return &OwnerUsecase{repo: repo, cfg: cfg}
}

// Query 读当前 owner 记录(调用方查询失败一律按 UNKNOWN 处理,§9.22)。
func (u *OwnerUsecase) Query(ctx context.Context, playerID uint64) (data.OwnerRecord, error) {
	if playerID == 0 {
		return data.OwnerRecord{}, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	return u.repo.Query(ctx, playerID)
}

// BeginTransition 发起 owner 迁移(CAS;幂等键 = (player, operation))。
func (u *OwnerUsecase) BeginTransition(ctx context.Context, playerID, expectEpoch uint64, operationID string, ownerType int8, target data.OwnerTarget) (data.OwnerRecord, error) {
	if playerID == 0 {
		return data.OwnerRecord{}, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if !placement.ValidOperationID(operationID) {
		return data.OwnerRecord{}, errcode.New(errcode.ErrOwnerInvalidOperation, "operation_id must be canonical UUIDv4")
	}
	if ownerType != data.OwnerTypeHub && ownerType != data.OwnerTypeBattle {
		return data.OwnerRecord{}, errcode.New(errcode.ErrOwnerInvalidOperation, "owner_type must be HUB or BATTLE")
	}
	if !target.Complete() {
		return data.OwnerRecord{}, errcode.New(errcode.ErrOwnerInvalidOperation, "target identity incomplete")
	}
	return u.repo.BeginTransition(ctx, playerID, expectEpoch, operationID, ownerType, target,
		time.Duration(placement.DSFenceSkewMarginSeconds)*time.Second)
}

// Admit 准入提交(屏障 + exact 身份;幂等重放)。
func (u *OwnerUsecase) Admit(ctx context.Context, playerID, ownerEpoch uint64, operationID string, target data.OwnerTarget) (data.OwnerRecord, int64, error) {
	if playerID == 0 || ownerEpoch == 0 {
		return data.OwnerRecord{}, 0, errcode.New(errcode.ErrInvalidArg, "player_id/owner_epoch required")
	}
	if !placement.ValidOperationID(operationID) {
		return data.OwnerRecord{}, 0, errcode.New(errcode.ErrOwnerInvalidOperation, "operation_id must be canonical UUIDv4")
	}
	if !target.Complete() {
		return data.OwnerRecord{}, 0, errcode.New(errcode.ErrOwnerInvalidOperation, "target identity incomplete")
	}
	return u.repo.Admit(ctx, playerID, ownerEpoch, operationID, target)
}

// RenewInstanceLease 实例租约续期(allocator 心跳代写;秒数钳制到协议上限)。
func (u *OwnerUsecase) RenewInstanceLease(ctx context.Context, target data.OwnerTarget, leaseSeconds uint32) (int64, error) {
	// 续租要求 pod + uid;instance_epoch 允许 0(hub 凭据不携带实例纪元,uid 全局唯一已足,
	// 纪元守卫在数据层只对"双方都非零且不同"拒)。分配 ID 是玩家维度信息,租约是实例级。
	if target.PodName == "" || target.InstanceUID == "" {
		return 0, errcode.New(errcode.ErrOwnerInvalidOperation, "instance identity incomplete")
	}
	if leaseSeconds == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "lease_seconds required")
	}
	if leaseSeconds > placement.DSFenceLeaseMaxSeconds {
		// 协议上限硬钳制(§8 fence 契约):配置/调用方无法放大脑裂窗口。
		leaseSeconds = placement.DSFenceLeaseMaxSeconds
	}
	return u.repo.RenewInstanceLease(ctx, target, time.Duration(leaseSeconds)*time.Second)
}

// Release 显式释放(登出/终局;迟到调用幂等 no-op)。
func (u *OwnerUsecase) Release(ctx context.Context, playerID, ownerEpoch uint64, operationID string) (data.OwnerRecord, error) {
	if playerID == 0 {
		return data.OwnerRecord{}, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if !placement.ValidOperationID(operationID) {
		return data.OwnerRecord{}, errcode.New(errcode.ErrOwnerInvalidOperation, "operation_id must be canonical UUIDv4")
	}
	return u.repo.Release(ctx, playerID, ownerEpoch, operationID)
}

// RunTransitionLogSweep 周期清理审计流水(§9.24)。
func (u *OwnerUsecase) RunTransitionLogSweep(ctx context.Context, batch int) (int64, error) {
	retention := time.Duration(u.cfg.LogRetentionDays) * 24 * time.Hour
	return u.repo.SweepTransitionLog(ctx, retention, batch)
}
