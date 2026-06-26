// profile_sharding_test.go — 玩家档案 owner cell 锚定单测(2026-06-26)。
//
// 覆盖:ProfileShardKey canonical 口径(= player_id,与 nickname/hero 无关)、profileOwner nil-router
// 退化 / 零 player_id / 正常解析、同一玩家档案 owner 落点稳定。验证 router 为 nil(单 Cell)时
// 行为不变,注入后档案锚定到玩家 owner cell(§4.2 line 142)。
package biz

import (
	"testing"

	"github.com/luyuancpp/pandora/pkg/cellroute"
)

// twoRegionProfileRouter 造一张前半 region1 / 后半 region2 的均衡路由表。
func twoRegionProfileRouter(t *testing.T) *cellroute.Router {
	t.Helper()
	specs := []cellroute.CellSpec{
		{RegionID: 1, CellID: 1},
		{RegionID: 2, CellID: 2},
	}
	entries, regionOfCell, err := cellroute.BuildBalancedEntries(specs)
	if err != nil {
		t.Fatalf("BuildBalancedEntries: %v", err)
	}
	tbl, err := cellroute.NewStaticTable(entries, regionOfCell)
	if err != nil {
		t.Fatalf("NewStaticTable: %v", err)
	}
	r, err := cellroute.NewRouter(tbl)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return r
}

func profilePlayerRegion1() uint64 { return 1 }
func profilePlayerRegion2() uint64 { return uint64(cellroute.LogicalCellCount/2 + 1) }

func TestProfileShardKey_IsPlayerID(t *testing.T) {
	if got := ProfileShardKey(123); got != "123" {
		t.Fatalf("ProfileShardKey(123) = %q, want \"123\"", got)
	}
}

func TestProfileShardKey_IndependentOfProfileFields(t *testing.T) {
	// 同一玩家恒锚定同 owner cell,与 nickname / hero / 配置无关。
	if ProfileShardKey(42) != ProfileShardKey(42) {
		t.Fatal("same player should yield same shard key")
	}
	if ProfileShardKey(42) == ProfileShardKey(43) {
		t.Fatal("different players should yield different shard keys")
	}
}

func TestProfileOwner_NilRouter(t *testing.T) {
	uc := newUC(newFakeRepo())
	if _, ok := uc.profileOwner(1); ok {
		t.Fatal("nil router should yield ok=false")
	}
}

func TestProfileOwner_ZeroPlayer(t *testing.T) {
	uc := newUC(newFakeRepo())
	uc.SetCellRouter(twoRegionProfileRouter(t))
	if _, ok := uc.profileOwner(0); ok {
		t.Fatal("player_id=0 should yield ok=false")
	}
}

func TestProfileOwner_Resolves(t *testing.T) {
	uc := newUC(newFakeRepo())
	uc.SetCellRouter(twoRegionProfileRouter(t))
	o1, ok := uc.profileOwner(profilePlayerRegion1())
	if !ok {
		t.Fatal("router should resolve region1 player")
	}
	o2, ok := uc.profileOwner(profilePlayerRegion2())
	if !ok {
		t.Fatal("router should resolve region2 player")
	}
	if o1.RegionID == o2.RegionID {
		t.Fatalf("region1/region2 players should resolve to different regions, got %d/%d", o1.RegionID, o2.RegionID)
	}
}

func TestProfileOwner_SamePlayerStable(t *testing.T) {
	uc := newUC(newFakeRepo())
	uc.SetCellRouter(twoRegionProfileRouter(t))
	pid := profilePlayerRegion2()
	a, ok := uc.profileOwner(pid)
	if !ok {
		t.Fatal("router should resolve")
	}
	b, _ := uc.profileOwner(pid)
	if a != b {
		t.Fatalf("same player should map to stable owner, got %+v vs %+v", a, b)
	}
}
