// settlement_test.go — 跨 region 结算回流落点 + 幂等键口径单测(蜂窝扩容 ⑧)。
//
// 覆盖:
//   - SettlementKey:canonical "match:player" 口径(与 player mmr_history uk 同维度)
//   - DistinctSettlementRegions:去重 + 升序 + 空输入
//   - CrossRegionSettlement:多 region true / 单 region(空)false
//   - settlementOwners:nil router 退化 / 双 region 路由器按玩家解析落点
package biz

import (
	"testing"

	"github.com/luyuancpp/pandora/pkg/cellroute"
	battlev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/battle/v1"

	"github.com/luyuancpp/pandora/services/battle/battle_result/internal/conf"
)

// twoRegionRouter 构造一张把逻辑分片前半切给 (region1, cell1)、后半切给 (region2, cell2) 的路由器。
// 于是 player_id%4096 落前半 → region1,落后半 → region2,便于确定性构造跨 region 场景。
func twoRegionRouter(t *testing.T, region1, cell1, region2, cell2 uint32) *cellroute.Router {
	t.Helper()
	entries, regionOfCell, err := cellroute.BuildBalancedEntries([]cellroute.CellSpec{
		{RegionID: region1, CellID: cell1},
		{RegionID: region2, CellID: cell2},
	})
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

// playerInRegion1 返回一个 logical_cell 落前半(region1)的 player_id。
func playerInRegion1() uint64 { return 1 }

// playerInRegion2 返回一个 logical_cell 落后半(region2)的 player_id。
func playerInRegion2() uint64 { return uint64(cellroute.LogicalCellCount)/2 + 1 }

func TestSettlementKey_Canonical(t *testing.T) {
	if got := SettlementKey(100, 7); got != "100:7" {
		t.Fatalf("SettlementKey(100,7) = %q, want %q", got, "100:7")
	}
	// 口径稳定:不同 (match, player) 不碰撞
	if SettlementKey(10, 23) == SettlementKey(102, 3) {
		t.Fatalf("settle keys must not collide across different (match,player)")
	}
}

func TestDistinctSettlementRegions_SortedDedup(t *testing.T) {
	owners := []SettlementOwner{
		{PlayerID: 1, RegionID: 3},
		{PlayerID: 2, RegionID: 1},
		{PlayerID: 3, RegionID: 3},
		{PlayerID: 4, RegionID: 1},
		{PlayerID: 5, RegionID: 2},
	}
	got := DistinctSettlementRegions(owners)
	want := []uint32{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (sorted, deduped)", got, want)
		}
	}
	if DistinctSettlementRegions(nil) != nil {
		t.Fatalf("empty input must return nil")
	}
}

func TestCrossRegionSettlement_TrueOnMultiRegion(t *testing.T) {
	multi := []SettlementOwner{{PlayerID: 1, RegionID: 1}, {PlayerID: 2, RegionID: 2}}
	if !CrossRegionSettlement(multi) {
		t.Fatalf("two distinct regions must be cross-region")
	}
	single := []SettlementOwner{{PlayerID: 1, RegionID: 5}, {PlayerID: 2, RegionID: 5}}
	if CrossRegionSettlement(single) {
		t.Fatalf("single region must not be cross-region")
	}
	if CrossRegionSettlement(nil) {
		t.Fatalf("empty must not be cross-region")
	}
}

func TestSettlementOwners_NilRouterNotOk(t *testing.T) {
	u := NewBattleResultUsecase(newFakeRepo(), nil, nil, nil, conf.BattleConf{BaseMMR: 1500})
	result := &battlev1.BattleResult{
		MatchId: 9001,
		Stats:   []*battlev1.PlayerStats{{PlayerId: playerInRegion1()}, {PlayerId: playerInRegion2()}},
	}
	if _, ok := u.settlementOwners(result); ok {
		t.Fatalf("nil router must yield ok=false (single-Cell behavior unchanged)")
	}
}

func TestSettlementOwners_ResolvesPerPlayerAndCrossRegion(t *testing.T) {
	u := NewBattleResultUsecase(newFakeRepo(), nil, nil, nil, conf.BattleConf{BaseMMR: 1500})
	u.SetCellRouter(twoRegionRouter(t, 1, 10, 2, 20))

	result := &battlev1.BattleResult{
		MatchId: 9002,
		Stats: []*battlev1.PlayerStats{
			{PlayerId: playerInRegion1()},
			{PlayerId: playerInRegion2()},
			{PlayerId: 0}, // 无效 player_id 应被跳过
		},
	}
	owners, ok := u.settlementOwners(result)
	if !ok {
		t.Fatalf("router set + valid players must yield ok=true")
	}
	if len(owners) != 2 {
		t.Fatalf("got %d owners, want 2 (player_id=0 skipped)", len(owners))
	}
	if !CrossRegionSettlement(owners) {
		t.Fatalf("players from region1 + region2 must be cross-region settlement")
	}
	regions := DistinctSettlementRegions(owners)
	if len(regions) != 2 || regions[0] != 1 || regions[1] != 2 {
		t.Fatalf("distinct regions = %v, want [1 2]", regions)
	}
}

func TestSettlementOwners_SingleRegionNotCross(t *testing.T) {
	u := NewBattleResultUsecase(newFakeRepo(), nil, nil, nil, conf.BattleConf{BaseMMR: 1500})
	u.SetCellRouter(twoRegionRouter(t, 1, 10, 2, 20))

	// 两名玩家都落前半 → 同 region1
	result := &battlev1.BattleResult{
		MatchId: 9003,
		Stats:   []*battlev1.PlayerStats{{PlayerId: 1}, {PlayerId: 5}},
	}
	owners, ok := u.settlementOwners(result)
	if !ok {
		t.Fatalf("ok=true expected")
	}
	if CrossRegionSettlement(owners) {
		t.Fatalf("same-region players must not be cross-region")
	}
}
