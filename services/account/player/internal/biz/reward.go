// reward.go — 领奖记录业务逻辑(2026-06-30)。
//
// 客户端经 Envoy 调 ClaimReward / GetRewardClaims 领取 / 查询奖励档位。服务端权威判重
// 幂等(不变量 §2 / §7),领取状态用变长位图记录(pkg/rewardclaim),序列化进
// RewardClaimStorageRecord 落 pandora_player.player_reward_claims(乐观锁 version)。
//
// 两类来源(见 proto RewardClaimStorageRecord 注释):
//   - 永久类(REWARD_SOURCE_TYPE_PERMANENT):按来源名(sign_in / achievement / ……),
//     只增不删。
//   - 活动类(REWARD_SOURCE_TYPE_ACTIVITY):按活动实例 ID,活动下线整条回收复用。
//
// bit 位映射:当前直接用 reward_id(配置 ID)当 bit 位(标识映射,稳定且无需配置表)。
// 待奖励配置表落地后,可换 pkg/rewardclaim 的 dense BitIndexMap(ClaimPermanentByID),
// 把稀疏配置 ID 压成紧凑位 —— 接缝已在 pkg 侧备好,此处替换映射即可,落地格式不变。
//
// §14:响应只回客户端可见最小视图(已领取的 reward_id 列表),不外露 StorageRecord 位图。
package biz

import (
	"context"
	"errors"

	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/rewardclaim"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"
)

// maxRewardClaimRetry 是领奖写入遇乐观锁冲突的最大重试次数(并发领取同一玩家时兜底)。
const maxRewardClaimRetry = 3

// loadRewardRecord 读取并反序列化玩家领奖记录,返回内存 Record + 当前乐观锁版本。
// 未建行 → 空 Record + version 0(后续 Save 按新建处理)。
func (u *PlayerUsecase) loadRewardRecord(ctx context.Context, playerID uint64) (*rewardclaim.Record, int32, error) {
	raw, version, err := u.repo.LoadRewardClaims(ctx, playerID)
	if err != nil {
		return nil, 0, err
	}
	if len(raw) == 0 {
		return rewardclaim.New(), version, nil
	}
	stored := &playerv1.RewardClaimStorageRecord{}
	if uerr := proto.Unmarshal(raw, stored); uerr != nil {
		return nil, 0, errcode.New(errcode.ErrInternal, "decode reward record player=%d: %v", playerID, uerr)
	}
	return rewardclaim.Load(stored.GetPermanent(), stored.GetActivity()), version, nil
}

// saveRewardRecord 序列化并乐观锁写回领奖记录。
func (u *PlayerUsecase) saveRewardRecord(ctx context.Context, playerID uint64, rec *rewardclaim.Record, expectVersion int32) error {
	perm, act := rec.Snapshot()
	raw, err := proto.Marshal(&playerv1.RewardClaimStorageRecord{Permanent: perm, Activity: act})
	if err != nil {
		return errcode.New(errcode.ErrInternal, "encode reward record player=%d: %v", playerID, err)
	}
	return u.repo.SaveRewardClaims(ctx, playerID, raw, expectVersion)
}

// ClaimReward 领取一档奖励(客户端权威领取,幂等)。
// 已领取 → ErrRewardAlreadyClaimed;reward_id 超 bit 上界 → ErrRewardUnknownID。
func (u *PlayerUsecase) ClaimReward(ctx context.Context, playerID uint64, sourceType playerv1.RewardSourceType, source string, activityInstanceID uint64, rewardID uint32) error {
	if playerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if rewardID >= rewardclaim.MaxBitIndex {
		return errcode.New(errcode.ErrRewardUnknownID, "reward_id out of range: %d", rewardID)
	}
	switch sourceType {
	case playerv1.RewardSourceType_REWARD_SOURCE_TYPE_PERMANENT:
		if source == "" {
			return errcode.New(errcode.ErrInvalidArg, "source required for permanent reward")
		}
	case playerv1.RewardSourceType_REWARD_SOURCE_TYPE_ACTIVITY:
		if activityInstanceID == 0 {
			return errcode.New(errcode.ErrInvalidArg, "activity_instance_id required for activity reward")
		}
	default:
		return errcode.New(errcode.ErrInvalidArg, "unknown reward source_type: %d", sourceType)
	}

	var lastErr error
	for attempt := 0; attempt < maxRewardClaimRetry; attempt++ {
		rec, version, err := u.loadRewardRecord(ctx, playerID)
		if err != nil {
			return err
		}

		var claimErr error
		if sourceType == playerv1.RewardSourceType_REWARD_SOURCE_TYPE_PERMANENT {
			claimErr = rec.ClaimPermanent(source, rewardID)
		} else {
			claimErr = rec.ClaimActivity(activityInstanceID, rewardID)
		}
		switch {
		case claimErr == nil:
			// 首次领取,继续落库
		case errors.Is(claimErr, rewardclaim.ErrAlreadyClaimed):
			return errcode.New(errcode.ErrRewardAlreadyClaimed, "reward already claimed: player=%d id=%d", playerID, rewardID)
		case errors.Is(claimErr, rewardclaim.ErrIndexTooLarge):
			return errcode.New(errcode.ErrRewardUnknownID, "reward_id out of range: %d", rewardID)
		default:
			return errcode.New(errcode.ErrInternal, "claim reward player=%d id=%d: %v", playerID, rewardID, claimErr)
		}

		if serr := u.saveRewardRecord(ctx, playerID, rec, version); serr != nil {
			if errcode.As(serr) == errcode.ErrPlayerVersionMismatch {
				lastErr = serr
				continue // 并发冲突,重读重试
			}
			return serr
		}
		return nil
	}
	return errcode.New(errcode.ErrPlayerVersionMismatch, "claim reward player=%d id=%d: too many version conflicts: %v", playerID, rewardID, lastErr)
}

// GetRewardClaims 查询某来源已领取的奖励配置 ID 列表(客户端可见最小视图)。
func (u *PlayerUsecase) GetRewardClaims(ctx context.Context, playerID uint64, sourceType playerv1.RewardSourceType, source string, activityInstanceID uint64) ([]uint32, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	rec, _, err := u.loadRewardRecord(ctx, playerID)
	if err != nil {
		return nil, err
	}
	switch sourceType {
	case playerv1.RewardSourceType_REWARD_SOURCE_TYPE_PERMANENT:
		if source == "" {
			return nil, errcode.New(errcode.ErrInvalidArg, "source required for permanent reward")
		}
		return rec.PermanentClaimedIndices(source), nil
	case playerv1.RewardSourceType_REWARD_SOURCE_TYPE_ACTIVITY:
		if activityInstanceID == 0 {
			return nil, errcode.New(errcode.ErrInvalidArg, "activity_instance_id required for activity reward")
		}
		return rec.ActivityClaimedIndices(activityInstanceID), nil
	default:
		return nil, errcode.New(errcode.ErrInvalidArg, "unknown reward source_type: %d", sourceType)
	}
}
