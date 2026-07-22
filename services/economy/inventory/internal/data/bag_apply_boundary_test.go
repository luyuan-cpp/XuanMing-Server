package data

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	bagv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/bag/v1"
)

func boundaryStack(configID, count, slot uint32) *bagv1.BagItem {
	return &bagv1.BagItem{ItemConfigId: configID, Count: count, Slot: slot}
}

func boundaryInstance(configID uint32, instanceID uint64, slot uint32) *bagv1.BagItem {
	return &bagv1.BagItem{
		ItemConfigId: configID,
		Count:        1,
		InstanceId:   instanceID,
		Slot:         slot,
	}
}

func requireBagCode(t *testing.T, err error, want errcode.Code) {
	t.Helper()
	if got := errcode.As(err); got != want {
		t.Fatalf("错误码不符: got=%d want=%d err=%v", got, want, err)
	}
}

// TestSectionAddItemsCapacityBoundaries 固化“满格”的精确定义：容量按条目数计算，
// 已有堆叠合并不占新格；新增堆叠或实例必须有空格。
func TestSectionAddItemsCapacityBoundaries(t *testing.T) {
	t.Run("恰好填满并复用最低空槽", func(t *testing.T) {
		sec := &bagv1.BagSection{
			BagType: BagWarehouseType,
			Items: []*bagv1.BagItem{
				boundaryStack(10001, 3, 0),
				boundaryInstance(20001, 9001, 2),
			},
		}

		if err := sectionAddItems(sec, []*bagv1.BagItem{boundaryStack(10002, 1, 99)}, 3, stackLimit(99)); err != nil {
			t.Fatalf("恰好填满应成功: %v", err)
		}
		if len(sec.GetItems()) != 3 {
			t.Fatalf("填满后格数不符: got=%d want=3", len(sec.GetItems()))
		}
		if got := sec.GetItems()[2].GetSlot(); got != 1 {
			t.Fatalf("应复用最低空槽 1: got=%d", got)
		}
	})

	t.Run("满格仍可合并但不能开新格", func(t *testing.T) {
		sec := &bagv1.BagSection{
			BagType: BagWarehouseType,
			Items: []*bagv1.BagItem{
				boundaryStack(10001, 3, 0),
				boundaryStack(10002, 4, 1),
			},
		}

		if err := sectionAddItems(sec, []*bagv1.BagItem{boundaryStack(10001, 2, 0)}, 2, stackLimit(99)); err != nil {
			t.Fatalf("满格合并已有堆叠不应失败: %v", err)
		}
		if got := sec.GetItems()[0].GetCount(); got != 5 {
			t.Fatalf("合并后数量不符: got=%d want=5", got)
		}

		err := sectionAddItems(sec, []*bagv1.BagItem{boundaryStack(10003, 1, 0)}, 2, stackLimit(99))
		requireBagCode(t, err, errcode.ErrBagCapacityFull)
		if len(sec.GetItems()) != 2 || sec.GetItems()[0].GetCount() != 5 {
			t.Fatalf("拒绝新格后不得改动既有内容: %+v", sec.GetItems())
		}
	})

	t.Run("满格拒绝新实例且不占槽", func(t *testing.T) {
		sec := &bagv1.BagSection{
			BagType: BagWarehouseType,
			Items:   []*bagv1.BagItem{boundaryStack(10001, 1, 0)},
		}

		err := sectionAddItems(sec, []*bagv1.BagItem{boundaryInstance(20001, 9001, 0)}, 1, stackLimit(99))
		requireBagCode(t, err, errcode.ErrBagCapacityFull)
		if len(sec.GetItems()) != 1 || sec.GetItems()[0].GetItemConfigId() != 10001 {
			t.Fatalf("拒绝实例后背包内容被改动: %+v", sec.GetItems())
		}
	})
}

// TestSectionAddItemsCapacityResize 固化运行期容量调整边界：缩容不删除既有资产，
// 但在占用量重新低于容量前只能合并已有堆叠；扩容后立即允许新增格。
func TestSectionAddItemsCapacityResize(t *testing.T) {
	sec := &bagv1.BagSection{
		BagType: BagWarehouseType,
		Items: []*bagv1.BagItem{
			boundaryStack(10001, 3, 0),
			boundaryStack(10002, 4, 1),
			boundaryInstance(20001, 9001, 2),
		},
	}

	if err := sectionAddItems(sec, []*bagv1.BagItem{boundaryStack(10001, 2, 0)}, 2, stackLimit(99)); err != nil {
		t.Fatalf("缩容后合并已有堆叠应保留资产并成功: %v", err)
	}
	if got := sec.GetItems()[0].GetCount(); got != 5 {
		t.Fatalf("缩容后合并数量不符: got=%d want=5", got)
	}
	if err := sectionAddItems(sec, []*bagv1.BagItem{boundaryStack(10003, 1, 0)}, 2, stackLimit(99)); errcode.As(err) != errcode.ErrBagCapacityFull {
		t.Fatalf("占用量高于缩容值时新增格应拒绝: %v", err)
	}
	if len(sec.GetItems()) != 3 {
		t.Fatalf("缩容不得删除既有资产: got=%d want=3", len(sec.GetItems()))
	}

	if err := sectionAddItems(sec, []*bagv1.BagItem{boundaryStack(10003, 1, 0)}, 4, stackLimit(99)); err != nil {
		t.Fatalf("扩容后新增格应成功: %v", err)
	}
	if len(sec.GetItems()) != 4 || sec.GetItems()[3].GetSlot() != 3 {
		t.Fatalf("扩容后新增格不符: %+v", sec.GetItems())
	}
}

// TestSectionAddItemsRejectsDuplicateInstanceAtCapacityBoundary 确保重复实例错误不会
// 被“背包已满”掩盖，调用方可以稳定区分重放/坏数据与真实容量不足。
func TestSectionAddItemsRejectsDuplicateInstanceAtCapacityBoundary(t *testing.T) {
	sec := &bagv1.BagSection{
		BagType: BagWarehouseType,
		Items:   []*bagv1.BagItem{boundaryInstance(20001, 9001, 0)},
	}

	err := sectionAddItems(sec, []*bagv1.BagItem{boundaryInstance(20001, 9001, 0)}, 1, stackLimit(99))
	requireBagCode(t, err, errcode.ErrInvalidArg)
	if len(sec.GetItems()) != 1 || sec.GetItems()[0].GetInstanceId() != 9001 {
		t.Fatalf("重复实例拒绝后内容被改动: %+v", sec.GetItems())
	}
}

// TestBagRepoMySQLCapacityFailureRollsBackEarlierSameEntryMerge 验证同一 journal entry
// 内先合并已有堆、后因没有新格失败时，MySQL 事务不会持久化前半段修改。
func TestBagRepoMySQLCapacityFailureRollsBackEarlierSameEntryMerge(t *testing.T) {
	f := openBagMySQLFixture(t)
	repo := NewMySQLBagRepo(f.db)
	ctx := context.Background()
	const playerID = uint64(207)
	oneSlot := func(bagType uint32) uint32 {
		if bagType == BagWarehouseType {
			return 1
		}
		return 0
	}

	if _, err := repo.AppendJournal(ctx, playerID, 1, []*bagv1.BagJournalEntry{
		mkClaimEntry(1, "capacity-seed", BagWarehouseType, 0, stackItem(10001, 3)),
	}, oneSlot, stackLimit(99), 0); err != nil {
		t.Fatalf("seed full one-slot bag: %v", err)
	}

	_, err := repo.AppendJournal(ctx, playerID, 1, []*bagv1.BagJournalEntry{
		mkClaimEntry(2, "capacity-batch", BagWarehouseType, 0,
			stackItem(10001, 2), // 先原地合并
			stackItem(10002, 1), // 后开新格失败
		),
	}, oneSlot, stackLimit(99), 0)
	requireBagCode(t, err, errcode.ErrBagCapacityFull)

	if got := queryWarehouseCount(t, repo, playerID, 10001); got != 3 {
		t.Fatalf("容量失败后前置合并被部分提交: got=%d want=3", got)
	}
	if got := queryWarehouseCount(t, repo, playerID, 10002); got != 0 {
		t.Fatalf("容量失败后新格被部分提交: got=%d want=0", got)
	}
	if _, _, lastSeq, loadErr := repo.LoadBag(ctx, playerID, 1); loadErr != nil || lastSeq != 1 {
		t.Fatalf("容量失败后 journal 水位变化: last=%d want=1 err=%v", lastSeq, loadErr)
	}
}
