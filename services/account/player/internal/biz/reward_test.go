// reward_test.go — 领奖业务链路单测(2026-06-30)。
//
// 用内存版 fakeRepo 验证 ClaimReward 幂等 / 来源隔离 / 活动隔离 / 查询视图,无需真 DB。
package biz

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"
)

const (
	permSrc  = "sign_in"
	actInst  = uint64(202606)
	playerID = uint64(1001)
)

func TestClaimReward_PermanentIdempotent(t *testing.T) {
	uc := newUC(newFakeRepo())
	ctx := context.Background()

	if err := uc.ClaimReward(ctx, playerID, playerv1.RewardSourceType_REWARD_SOURCE_TYPE_PERMANENT, permSrc, 0, 7); err != nil {
		t.Fatalf("first claim should succeed: %v", err)
	}
	err := uc.ClaimReward(ctx, playerID, playerv1.RewardSourceType_REWARD_SOURCE_TYPE_PERMANENT, permSrc, 0, 7)
	if errcode.As(err) != errcode.ErrRewardAlreadyClaimed {
		t.Fatalf("second claim should be ErrRewardAlreadyClaimed, got %v", err)
	}

	ids, err := uc.GetRewardClaims(ctx, playerID, playerv1.RewardSourceType_REWARD_SOURCE_TYPE_PERMANENT, permSrc, 0)
	if err != nil {
		t.Fatalf("get claims: %v", err)
	}
	if len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("expected [7], got %v", ids)
	}
}

func TestClaimReward_ActivityIsolatedFromPermanent(t *testing.T) {
	uc := newUC(newFakeRepo())
	ctx := context.Background()

	if err := uc.ClaimReward(ctx, playerID, playerv1.RewardSourceType_REWARD_SOURCE_TYPE_PERMANENT, permSrc, 0, 3); err != nil {
		t.Fatalf("perm claim: %v", err)
	}
	// 同 bit 位在活动来源应仍可领取(互不影响)。
	if err := uc.ClaimReward(ctx, playerID, playerv1.RewardSourceType_REWARD_SOURCE_TYPE_ACTIVITY, "", actInst, 3); err != nil {
		t.Fatalf("activity claim same index should succeed: %v", err)
	}

	actIDs, _ := uc.GetRewardClaims(ctx, playerID, playerv1.RewardSourceType_REWARD_SOURCE_TYPE_ACTIVITY, "", actInst)
	if len(actIDs) != 1 || actIDs[0] != 3 {
		t.Fatalf("expected activity [3], got %v", actIDs)
	}
}

func TestClaimReward_Validation(t *testing.T) {
	uc := newUC(newFakeRepo())
	ctx := context.Background()

	// 永久来源缺 source。
	if err := uc.ClaimReward(ctx, playerID, playerv1.RewardSourceType_REWARD_SOURCE_TYPE_PERMANENT, "", 0, 1); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("missing source should be ErrInvalidArg, got %v", err)
	}
	// 活动来源缺实例 ID。
	if err := uc.ClaimReward(ctx, playerID, playerv1.RewardSourceType_REWARD_SOURCE_TYPE_ACTIVITY, "", 0, 1); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("missing activity instance should be ErrInvalidArg, got %v", err)
	}
}
