// bag_migrate_test.go — 存量迁移用例纯单测(fake 双仓;D5)。
package biz

import (
	"context"
	"testing"

	"github.com/go-kratos/kratos/v2/log"

	"github.com/luyuancpp/pandora/pkg/errcode"
	bagv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/bag/v1"

	"github.com/luyuancpp/pandora/services/economy/inventory/internal/conf"
	"github.com/luyuancpp/pandora/services/economy/inventory/internal/data"
)

type fakeLegacySource struct {
	players  []uint64 // 升序
	stock    map[uint64][]*bagv1.BagItem
	failLoad map[uint64]bool
}

func (f *fakeLegacySource) ListLegacyBagPlayers(_ context.Context, after uint64, limit int) ([]uint64, error) {
	var out []uint64
	for _, p := range f.players {
		if p > after {
			out = append(out, p)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeLegacySource) LoadLegacyBagStock(_ context.Context, playerID uint64) ([]*bagv1.BagItem, error) {
	if f.failLoad[playerID] {
		return nil, errcode.New(errcode.ErrInvalidState, "legacy bound instance blocks migration player=%d", playerID)
	}
	return f.stock[playerID], nil
}

type fakeBagSeeder struct {
	already    map[uint64]bool // 幂等闸已存在
	failSeed   map[uint64]bool
	failVerify map[uint64]bool
	seeded     map[uint64]int // player → 落位物品数
	verified   map[uint64]bool
}

func (f *fakeBagSeeder) SeedLegacyWarehouse(_ context.Context, playerID uint64, items []*bagv1.BagItem, _ data.BagMaxStack) (bool, error) {
	if f.failSeed[playerID] {
		return false, errcode.New(errcode.ErrInternal, "seed failed player=%d", playerID)
	}
	if f.already[playerID] {
		return false, nil
	}
	if f.seeded == nil {
		f.seeded = map[uint64]int{}
	}
	f.seeded[playerID] = len(items)
	return true, nil
}

func (f *fakeBagSeeder) VerifyLegacyWarehouse(_ context.Context, playerID uint64, _ []*bagv1.BagItem) error {
	if f.failVerify[playerID] {
		return errcode.New(errcode.ErrInvalidState, "totals drift player=%d", playerID)
	}
	if f.verified == nil {
		f.verified = map[uint64]bool{}
	}
	f.verified[playerID] = true
	return nil
}

func migTestConf() conf.BagConf {
	return conf.BagConf{MigrationBatch: 2, DefaultMaxStack: 99}
}

// TestBagMigrationRunOnce 游标翻批 + 幂等 skip + 单玩家失败不阻断 + 统计正确。
func TestBagMigrationRunOnce(t *testing.T) {
	legacy := &fakeLegacySource{
		players: []uint64{101, 102, 103, 104, 105},
		stock: map[uint64][]*bagv1.BagItem{
			101: {{ItemConfigId: 1, Count: 3}},
			102: {{ItemConfigId: 1, Count: 1}},
			103: {{ItemConfigId: 2, Count: 7}, {ItemConfigId: 9, Count: 1, InstanceId: 55}},
			105: {{ItemConfigId: 3, Count: 2}},
		},
		failLoad: map[uint64]bool{104: true}, // bound 实例 fail-closed 形态
	}
	seeder := &fakeBagSeeder{already: map[uint64]bool{102: true}}
	uc := NewBagMigrationUsecase(legacy, seeder, migTestConf(), log.DefaultLogger)

	sum, err := uc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sum.Scanned != 5 || sum.Migrated != 3 || sum.Skipped != 1 || sum.Failed != 1 {
		t.Fatalf("统计不符: %+v(want Scanned=5 Migrated=3 Skipped=1 Failed=1)", sum)
	}
	if len(seeder.seeded) != 3 || seeder.seeded[103] != 2 {
		t.Fatalf("落位内容不符: %+v", seeder.seeded)
	}
	// 对账只对真实迁移的玩家执行(skip 玩家早已对过账,失败玩家没有落位)。
	if len(seeder.verified) != 3 || !seeder.verified[101] || !seeder.verified[103] || !seeder.verified[105] {
		t.Fatalf("对账覆盖不符: %+v", seeder.verified)
	}
}

// TestBagMigrationVerifyFailureCountsFailed 对账失败按玩家计失败(资产已落位,人工排障)。
func TestBagMigrationVerifyFailureCountsFailed(t *testing.T) {
	legacy := &fakeLegacySource{
		players: []uint64{201},
		stock:   map[uint64][]*bagv1.BagItem{201: {{ItemConfigId: 1, Count: 1}}},
	}
	seeder := &fakeBagSeeder{failVerify: map[uint64]bool{201: true}}
	uc := NewBagMigrationUsecase(legacy, seeder, migTestConf(), log.DefaultLogger)

	sum, err := uc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sum.Failed != 1 || sum.Migrated != 0 {
		t.Fatalf("对账失败应计 Failed: %+v", sum)
	}
}

// TestBagMigrationCtxCancel 取消即停(可中断,重启断点续跑)。
func TestBagMigrationCtxCancel(t *testing.T) {
	legacy := &fakeLegacySource{players: []uint64{301}}
	uc := NewBagMigrationUsecase(legacy, &fakeBagSeeder{}, migTestConf(), log.DefaultLogger)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := uc.RunOnce(ctx); err == nil {
		t.Fatal("已取消的 ctx 应返回错误")
	}
}
