// reward_test.go — 领奖业务链路单测(2026-06-30)。
//
// 用内存版 fakeRepo 验证 ClaimReward 幂等 / 来源隔离 / 活动隔离 / 查询视图,无需真 DB。
package biz

import (
	"bytes"
	"context"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

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

// TestClaimReward_PreservesUnknownFields 金丝雀共存窗口回写保护回归(不变量 §17 /
// zero-downtime-update.md §7.3):存量记录携带本副本 proto 不认识的新字段(unknown fields,
// 模拟新副本已写入的未来字段)时,ClaimReward 的 read-modify-write 必须原样带回,
// 不得因重建 message 丢弃。修复前(saveRewardRecord 重建 RewardClaimStorageRecord)本测试失败。
func TestClaimReward_PreservesUnknownFields(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	ctx := context.Background()

	// 未来字段模拟:field number 15(当前 message 只用 1/2)/ wire type varint / 值 42
	// → tag 字节 (15<<3)|0 = 0x78,值 0x2A。
	futureField := protoreflect.RawFields{0x78, 0x2A}

	// 先建一条已领 permSrc/3 的存量记录,并挂上未来字段,直接落进 fake 存储。
	seed := &playerv1.RewardClaimStorageRecord{Permanent: map[string][]byte{permSrc: {0x08}}} // bit 3
	seed.ProtoReflect().SetUnknown(futureField)
	raw, err := proto.Marshal(seed)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	repo.rewardRec[playerID] = raw
	repo.rewardVer[playerID] = 1

	// 旧副本视角处理一次领奖(read-modify-write)。
	if err := uc.ClaimReward(ctx, playerID, playerv1.RewardSourceType_REWARD_SOURCE_TYPE_PERMANENT, permSrc, 0, 7); err != nil {
		t.Fatalf("claim: %v", err)
	}

	saved := &playerv1.RewardClaimStorageRecord{}
	if err := proto.Unmarshal(repo.rewardRec[playerID], saved); err != nil {
		t.Fatalf("unmarshal saved: %v", err)
	}
	// 未来字段必须原样保留。
	if got := saved.ProtoReflect().GetUnknown(); !bytes.Equal(got, futureField) {
		t.Fatalf("unknown fields lost or mutated: got %x, want %x", got, futureField)
	}
	// 本副本的业务修改也要生效:原有 bit 3 + 新领 bit 7。
	ids, err := uc.GetRewardClaims(ctx, playerID, playerv1.RewardSourceType_REWARD_SOURCE_TYPE_PERMANENT, permSrc, 0)
	if err != nil {
		t.Fatalf("get claims: %v", err)
	}
	if len(ids) != 2 || ids[0] != 3 || ids[1] != 7 {
		t.Fatalf("expected [3 7], got %v", ids)
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
