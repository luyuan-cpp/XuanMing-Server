// owner_authority.go — owner 迁移弱依赖接线(owner-authority.md migrate ①/③④,2026-07-22)。
//
// migrate 语义(全部弱依赖,路由决策不变,§9.23 行为切换属 contract 阶段):
//   - ① hub 归属定案统一出口(签票点)弱 Begin(HUB):分配/恢复/转移/Battle 回流全路径
//     写进 owner 权威(E+1/PENDING/屏障);失败仅告警,分配照常;
//   - ③ 授权心跳 census 首见玩家代提交 Admit(migrate 近似:census 来自绑定 exact 实例
//     身份的授权心跳,是"该实例正在服务该玩家"的证据;contract 阶段 Admit 移交 DS
//     Admission 链原生提交,本近似退役)。
package biz

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	plog "github.com/luyuancpp/pandora/pkg/log"

	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/data"
)

// owner 类型常量(对齐 owner.proto OwnerType;biz 不依赖生成代码)。
const (
	ownerTypeHub    int8 = 1
	ownerTypeBattle int8 = 2
)

// owner 阶段常量(对齐 owner.proto OwnerPhase)。
const (
	ownerPhasePending  int8 = 1
	ownerPhaseAdmitted int8 = 2
)

// OwnerAuthority 是 owner 权威的 migrate 调用面(Query/Begin/Admit;弱依赖)。
// 由 data.GrpcOwnerLeaseRenewer 实现(与租约续写共用连接);可为 nil(未启用)。
type OwnerAuthority interface {
	QueryOwner(ctx context.Context, playerID uint64) (data.OwnerRecordView, error)
	BeginTransition(ctx context.Context, playerID, expectEpoch uint64, operationID string, ownerType int8, target data.OwnerTargetView) error
	Admit(ctx context.Context, playerID, ownerEpoch uint64, operationID string, target data.OwnerTargetView) (int64, error)
}

// SetOwnerAuthority 注入 owner 权威调用面(nil-safe)。
func (u *HubUsecase) SetOwnerAuthority(a OwnerAuthority) {
	u.ownerAuth = a
}

// decideOwnerBegin 判定是否需要发起迁移(§9.23 幂等 no-op 规则):
// 记录已指向同一实例(类型同 && pod+uid 同)且处于 PENDING/ADMITTED → 跳过
// (同目标重连/重复交付不再推进 epoch);否则以当前 epoch 为 CAS 期望发起。
func decideOwnerBegin(rec data.OwnerRecordView, ownerType int8, target data.OwnerTargetView) (skip bool, expectEpoch uint64) {
	if rec.OwnerType == ownerType && rec.PodName == target.PodName && rec.InstanceUID == target.InstanceUID &&
		(rec.Phase == ownerPhasePending || rec.Phase == ownerPhaseAdmitted) {
		return true, 0
	}
	return false, rec.OwnerEpoch
}

// ownerBeginPlayersWeak 批量弱 Begin:整批共享预算 ctx(防 owner 卡顿拖慢调用链),
// 每玩家 Query→decide→Begin;任何失败仅告警(migrate 弱依赖,旧路由门照跑)。
func ownerBeginPlayersWeak(ctx context.Context, auth OwnerAuthority, players []uint64,
	ownerType int8, target data.OwnerTargetView, budget time.Duration) {
	if auth == nil || len(players) == 0 {
		return
	}
	budgetCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for _, playerID := range players {
		if budgetCtx.Err() != nil {
			plog.With(ctx).Warnw("msg", "owner_begin_budget_exhausted",
				"remaining_players", len(players), "hint", "migrate 弱依赖,跳过剩余玩家")
			return
		}
		rec, err := auth.QueryOwner(budgetCtx, playerID)
		if err != nil {
			plog.With(ctx).Warnw("msg", "owner_begin_query_failed_weak", "player_id", playerID, "err", err)
			continue
		}
		skip, expectEpoch := decideOwnerBegin(rec, ownerType, target)
		if skip {
			continue
		}
		if err := auth.BeginTransition(budgetCtx, playerID, expectEpoch, uuid.NewString(), ownerType, target); err != nil {
			plog.With(ctx).Warnw("msg", "owner_begin_failed_weak", "player_id", playerID, "err", err)
		}
	}
}

// ownerAdmitCensusWeak census 首见玩家代提交 Admit(migrate 近似;弱依赖)。
//
// admitted 缓存 key = instanceUID|playerID(进程内 best-effort:重启后重查一轮即收敛);
// 仅当记录确实指向本实例(pod+uid 同 && 类型同 && PENDING)才 Admit,目标取记录自身字段
// (Admit 的 exact 全等校验由 owner 侧执行;pod/uid 是本调用方独立断言的部分)。
// 屏障未开(retryAfter>0)→ 本轮跳过,下次心跳重试;其余失败告警跳过。
func ownerAdmitCensusWeak(ctx context.Context, auth OwnerAuthority, admitted *sync.Map,
	players []uint64, ownerType int8, selfPod, selfUID string, budget time.Duration) {
	if auth == nil || len(players) == 0 {
		return
	}
	budgetCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for _, playerID := range players {
		key := selfUID + "|" + fmt.Sprintf("%d", playerID)
		if _, ok := admitted.Load(key); ok {
			continue
		}
		if budgetCtx.Err() != nil {
			return // 预算耗尽:剩余玩家下次心跳继续(census 每 ~5s 一轮,自然收敛)。
		}
		rec, err := auth.QueryOwner(budgetCtx, playerID)
		if err != nil {
			plog.With(ctx).Warnw("msg", "owner_admit_query_failed_weak", "player_id", playerID, "err", err)
			continue
		}
		if rec.OwnerType != ownerType || rec.PodName != selfPod || rec.InstanceUID != selfUID {
			continue // 记录不指向本实例(迁移中/漂移),不是本实例可断言的准入。
		}
		if rec.Phase == ownerPhaseAdmitted {
			admitted.Store(key, struct{}{})
			continue
		}
		if rec.Phase != ownerPhasePending {
			continue
		}
		target := data.OwnerTargetView{
			PodName: rec.PodName, InstanceUID: rec.InstanceUID, InstanceEpoch: rec.InstanceEpoch,
			AssignmentOrAllocationID: rec.AssignmentOrAllocationID, ReleaseTrack: rec.ReleaseTrack,
		}
		retryAfter, aerr := auth.Admit(budgetCtx, playerID, rec.OwnerEpoch, rec.OperationID, target)
		switch {
		case aerr == nil:
			admitted.Store(key, struct{}{})
		case retryAfter > 0:
			// 屏障未开:预期中的 WAIT,下次心跳重试,不告警刷屏。
		default:
			plog.With(ctx).Warnw("msg", "owner_admit_failed_weak", "player_id", playerID, "err", aerr)
		}
	}
}
