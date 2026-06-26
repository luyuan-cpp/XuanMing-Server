// data_sharding_test.go — 玩家数据 blob owner cell 锚定单测(2026-06-26)。
//
// 覆盖:PlayerDataShardKey canonical 口径(= player_id,与 version/data 无关)、playerDataOwner
// nil-router 退化 / 零 player_id / 正常解析 / 同一玩家落点稳定。验证 router 为 nil(单 Cell)时
// 行为不变,注入后 blob 锚定到玩家 owner cell(§4.2 line 142)。
package biz

import (
	"testing"

	"github.com/luyuancpp/pandora/pkg/cellroute"
)

// twoRegionDataRouter 造一张前半 region1 / 后半 region2 的均衡路由表。
func twoRegionDataRouter(t *testing.T) *cellroute.Router {
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

func dataPlayerRegion1() uint64 { return 1 }
func dataPlayerRegion2() uint64 { return uint64(cellroute.LogicalCellCount/2 + 1) }

func TestPlayerDataShardKey_IsPlayerID(t *testing.T) {
	if got := PlayerDataShardKey(456); got != "456" {
		t.Fatalf("PlayerDataShardKey(456) = %q, want \"456\"", got)
	}
}

func TestPlayerDataShardKey_IndependentOfPayload(t *testing.T) {
	// 同一玩家恒锚定同 owner cell,与 version / data 内容无关。
	if PlayerDataShardKey(42) != PlayerDataShardKey(42) {
		t.Fatal("same player should yield same shard key")
	}
	if PlayerDataShardKey(42) == PlayerDataShardKey(43) {
		t.Fatal("different players should yield different shard keys")
	}
}

func TestPlayerDataOwner_NilRouter(t *testing.T) {
	uc := newUC(newFakeStore(), newFakeCache())
	if _, ok := uc.playerDataOwner(1); ok {
		t.Fatal("nil router should yield ok=false")
	}
}

func TestPlayerDataOwner_ZeroPlayer(t *testing.T) {
	uc := newUC(newFakeStore(), newFakeCache())
	uc.SetCellRouter(twoRegionDataRouter(t))
	if _, ok := uc.playerDataOwner(0); ok {
		t.Fatal("player_id=0 should yield ok=false")
	}
}

func TestPlayerDataOwner_Resolves(t *testing.T) {
	uc := newUC(newFakeStore(), newFakeCache())
	uc.SetCellRouter(twoRegionDataRouter(t))
	o1, ok := uc.playerDataOwner(dataPlayerRegion1())
	if !ok {
		t.Fatal("router should resolve region1 player")
	}
	o2, ok := uc.playerDataOwner(dataPlayerRegion2())
	if !ok {
		t.Fatal("router should resolve region2 player")
	}
	if o1.RegionID == o2.RegionID {
		t.Fatalf("region1/region2 players should resolve to different regions, got %d/%d", o1.RegionID, o2.RegionID)
	}
}

func TestPlayerDataOwner_SamePlayerStable(t *testing.T) {
	uc := newUC(newFakeStore(), newFakeCache())
	uc.SetCellRouter(twoRegionDataRouter(t))
	pid := dataPlayerRegion2()
	a, ok := uc.playerDataOwner(pid)
	if !ok {
		t.Fatal("router should resolve")
	}
	b, _ := uc.playerDataOwner(pid)
	if a != b {
		t.Fatalf("same player should map to stable owner, got %+v vs %+v", a, b)
	}
}
