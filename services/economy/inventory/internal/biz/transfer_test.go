// transfer_test.go — 邮件 transfer 实例托管用例单测(2026-07-22,bag-domain.md §7.1)。
// 用 fakeRepo 复刻托管搬移语义,覆盖:形状校验、扣出→领取全链只改归属、幂等回放、
// 越权/漂移拒、容量拒、释放回源。事务级/字节级断言见 data 层 MySQL 集成测试。
package biz

import (
	"context"
	"errors"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/conf"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/data"
)

func newTransferUC(repo data.InventoryRepo, capacity int32) *InventoryUsecase {
	return NewInventoryUsecase(repo, conf.InventoryConf{Capacity: capacity})
}

func seedFakeInstance(repo *fakeRepo, playerID, instanceID uint64, configID uint32, identified, bound bool, attrs []data.ItemAttribute) {
	repo.instMap(playerID)[instanceID] = &data.ItemInstance{
		InstanceID: instanceID, ItemConfigID: configID,
		Identified: identified, Bound: bound, Attributes: attrs, SlotIndex: 0,
	}
}

func wantCode(t *testing.T, err error, code errcode.Code) {
	t.Helper()
	var ec *errcode.Error
	if !errors.As(err, &ec) || ec.Code != code {
		t.Fatalf("want code %d, got %v", code, err)
	}
}

// TestTransferChainOnlyChangesOwnership 扣出→领取全链:实例身份与鉴定态/词条原样,只改归属。
func TestTransferChainOnlyChangesOwnership(t *testing.T) {
	repo := newFakeRepo()
	uc := newTransferUC(repo, 10)
	ctx := context.Background()
	attrs := []data.ItemAttribute{{AttrID: 1, Value: 42}}
	seedFakeInstance(repo, 100, 9001, 5001, true, false, attrs)

	rows, err := uc.EscrowOutInstances(ctx, 100, 200, []uint64{9001}, "gift:1")
	if err != nil {
		t.Fatalf("escrow out: %v", err)
	}
	if len(rows) != 1 || rows[0].InstanceID != 9001 || !rows[0].Identified || len(rows[0].Attributes) != 1 {
		t.Fatalf("托管快照错: %+v", rows)
	}
	if _, ok := repo.instances[100][9001]; ok {
		t.Fatal("扣出后实例仍在源玩家背包")
	}

	if err := uc.ClaimTransferInstances(ctx, 200, []data.TransferClaimItem{{InstanceID: 9001, ItemConfigID: 5001}}, "xfer:1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, ok := repo.instances[200][9001]
	if !ok || got.InstanceID != 9001 || !got.Identified || len(got.Attributes) != 1 || got.ItemConfigID != 5001 {
		t.Fatalf("领取后实例数据漂移: %+v", got)
	}
	if _, escrowed := repo.xferEscrow[9001]; escrowed {
		t.Fatal("领取后托管行未删除")
	}

	// 领取幂等回放:同 key 直接成功。
	if err := uc.ClaimTransferInstances(ctx, 200, []data.TransferClaimItem{{InstanceID: 9001, ItemConfigID: 5001}}, "xfer:1"); err != nil {
		t.Fatalf("claim replay: %v", err)
	}
}

// TestTransferSelfSendAllowed source==to 合法(活动补发/切代 salvage 把玩家自己的物品经邮件送回)。
func TestTransferSelfSendAllowed(t *testing.T) {
	repo := newFakeRepo()
	uc := newTransferUC(repo, 10)
	ctx := context.Background()
	seedFakeInstance(repo, 100, 9002, 5001, false, false, nil)

	if _, err := uc.EscrowOutInstances(ctx, 100, 100, []uint64{9002}, "salvage:1"); err != nil {
		t.Fatalf("self escrow out: %v", err)
	}
	if err := uc.ClaimTransferInstances(ctx, 100, []data.TransferClaimItem{{InstanceID: 9002, ItemConfigID: 5001}}, "xfer:s1"); err != nil {
		t.Fatalf("self claim: %v", err)
	}
	if _, ok := repo.instances[100][9002]; !ok {
		t.Fatal("self 转移后实例丢失")
	}
}

// TestTransferEscrowOutRejects 扣出侧:绑定实例 / 不存在 / 形状非法拒。
func TestTransferEscrowOutRejects(t *testing.T) {
	repo := newFakeRepo()
	uc := newTransferUC(repo, 10)
	ctx := context.Background()
	seedFakeInstance(repo, 100, 9003, 5001, false, true, nil) // bound

	_, err := uc.EscrowOutInstances(ctx, 100, 200, []uint64{9003}, "gift:b")
	wantCode(t, err, errcode.ErrInventoryInstanceBound)

	_, err = uc.EscrowOutInstances(ctx, 100, 200, []uint64{77777}, "gift:m")
	wantCode(t, err, errcode.ErrInventoryItemNotFound)

	_, err = uc.EscrowOutInstances(ctx, 100, 200, nil, "gift:e")
	wantCode(t, err, errcode.ErrInvalidArg)
	_, err = uc.EscrowOutInstances(ctx, 100, 200, []uint64{1, 1}, "gift:d")
	wantCode(t, err, errcode.ErrInvalidArg)
	_, err = uc.EscrowOutInstances(ctx, 0, 200, []uint64{1}, "gift:z")
	wantCode(t, err, errcode.ErrInvalidArg)
	_, err = uc.EscrowOutInstances(ctx, 100, 200, []uint64{1}, "")
	wantCode(t, err, errcode.ErrInvalidArg)
}

// TestTransferClaimRejects 领取侧:越权 / config 漂移 / 未托管 / 容量满拒,托管行保持。
func TestTransferClaimRejects(t *testing.T) {
	repo := newFakeRepo()
	uc := newTransferUC(repo, 1)
	ctx := context.Background()
	seedFakeInstance(repo, 100, 9004, 5001, false, false, nil)
	if _, err := uc.EscrowOutInstances(ctx, 100, 200, []uint64{9004}, "gift:c"); err != nil {
		t.Fatalf("escrow out: %v", err)
	}

	err := uc.ClaimTransferInstances(ctx, 999, []data.TransferClaimItem{{InstanceID: 9004, ItemConfigID: 5001}}, "xfer:w1")
	wantCode(t, err, errcode.ErrInventoryItemNotFound)
	err = uc.ClaimTransferInstances(ctx, 200, []data.TransferClaimItem{{InstanceID: 9004, ItemConfigID: 8888}}, "xfer:w2")
	wantCode(t, err, errcode.ErrInventoryItemNotFound)
	err = uc.ClaimTransferInstances(ctx, 200, []data.TransferClaimItem{{InstanceID: 66666, ItemConfigID: 5001}}, "xfer:w3")
	wantCode(t, err, errcode.ErrInventoryItemNotFound)

	// 容量满:领取人已有 1 件且 capacity=1 → 拒,托管行保持(重领可重试)。
	seedFakeInstance(repo, 200, 9100, 5002, false, false, nil)
	err = uc.ClaimTransferInstances(ctx, 200, []data.TransferClaimItem{{InstanceID: 9004, ItemConfigID: 5001}}, "xfer:cap")
	wantCode(t, err, errcode.ErrInventoryCapacityFull)
	if _, ok := repo.xferEscrow[9004]; !ok {
		t.Fatal("拒后托管行不应消失")
	}
}

// TestTransferReleaseReturnsToSource 释放回源玩家;行缺失 no-op 幂等。
func TestTransferReleaseReturnsToSource(t *testing.T) {
	repo := newFakeRepo()
	uc := newTransferUC(repo, 10)
	ctx := context.Background()
	seedFakeInstance(repo, 100, 9005, 5001, true, false, nil)
	if _, err := uc.EscrowOutInstances(ctx, 100, 200, []uint64{9005}, "gift:r"); err != nil {
		t.Fatalf("escrow out: %v", err)
	}

	if err := uc.ReleaseTransferEscrow(ctx, []uint64{9005}); err != nil {
		t.Fatalf("release: %v", err)
	}
	got, ok := repo.instances[100][9005]
	if !ok || !got.Identified {
		t.Fatalf("释放后实例未回源或数据漂移: %+v", got)
	}
	// 再释放 → no-op。
	if err := uc.ReleaseTransferEscrow(ctx, []uint64{9005}); err != nil {
		t.Fatalf("release replay: %v", err)
	}
}
