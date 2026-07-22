package configtable

import (
	"strings"
	"testing"

	configpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
)

func testPlayerLevelTable(t *testing.T, rows ...*configpb.PlayerLevelExpRow) *PlayerLevelExpTable {
	t.Helper()
	table, err := newPlayerLevelExpTable(&configpb.PlayerLevelExpTableData{Rows: rows})
	if err != nil {
		t.Fatalf("new table: %v", err)
	}
	return table
}

func validPlayerLevelRows() []*configpb.PlayerLevelExpRow {
	return []*configpb.PlayerLevelExpRow{
		{Id: 1, Level: 1, UpgradeExp: 100, CumulativeExp: 0},
		{Id: 2, Level: 2, UpgradeExp: 200, CumulativeExp: 100},
		{Id: 3, Level: 3, UpgradeExp: 0, CumulativeExp: 300},
	}
}

func TestPlayerLevelExpCurve(t *testing.T) {
	table := testPlayerLevelTable(t, validPlayerLevelRows()...)
	if err := table.ValidateCurve(); err != nil {
		t.Fatalf("valid curve rejected: %v", err)
	}
	curve := table.ExperienceCurve()
	if len(curve) != 2 || curve[0] != 100 || curve[1] != 200 || table.MaxLevel() != 3 {
		t.Fatalf("curve=%v max=%d", curve, table.MaxLevel())
	}
}

func TestPlayerLevelExpCurveRejectsBrokenWholeTable(t *testing.T) {
	cases := map[string]struct {
		mutate func([]*configpb.PlayerLevelExpRow) []*configpb.PlayerLevelExpRow
		want   string
	}{
		"只有一级": {func(rows []*configpb.PlayerLevelExpRow) []*configpb.PlayerLevelExpRow {
			return rows[:1]
		}, "至少需要 Lv1-Lv2"},
		"等级断层": {func(rows []*configpb.PlayerLevelExpRow) []*configpb.PlayerLevelExpRow {
			return append(rows[:1], rows[2:]...)
		}, "缺少 Lv2"},
		"非末级零经验": {func(rows []*configpb.PlayerLevelExpRow) []*configpb.PlayerLevelExpRow {
			rows[0].UpgradeExp = 0
			return rows
		}, "必须大于 0"},
		"末级仍可升级": {func(rows []*configpb.PlayerLevelExpRow) []*configpb.PlayerLevelExpRow {
			rows[2].UpgradeExp = 1
			return rows
		}, "必须为 0"},
		"累计经验错误": {func(rows []*configpb.PlayerLevelExpRow) []*configpb.PlayerLevelExpRow {
			rows[1].CumulativeExp = 99
			return rows
		}, "累计经验"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rows := validPlayerLevelRows()
			table := testPlayerLevelTable(t, tc.mutate(rows)...)
			if err := table.ValidateCurve(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestPlayerLevelExpRowRequiresIDEqualsLevel(t *testing.T) {
	_, err := newPlayerLevelExpTable(&configpb.PlayerLevelExpTableData{Rows: []*configpb.PlayerLevelExpRow{
		{Id: 1, Level: 2, UpgradeExp: 0},
	}})
	if err == nil || !strings.Contains(err.Error(), "必须与等级") {
		t.Fatalf("ID/level mismatch should fail, got %v", err)
	}
}
