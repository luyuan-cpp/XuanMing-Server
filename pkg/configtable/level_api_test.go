package configtable

import (
	"testing"

	configpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
)

// level_api_test.go — 生成的表访问 API(level_table.gen.go)与手写伴生钩子(level.go)测试。

func mustLevelTable(t *testing.T, rows ...*configpb.LevelRow) *LevelTable {
	t.Helper()
	tbl, err := newLevelTable(&configpb.LevelTableData{Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

func battleRow(id uint32, name string) *configpb.LevelRow {
	return &configpb.LevelRow{Id: id, Name: name, AssetPath: "/Game/L/x.x",
		Category: configpb.LevelCategory_LEVEL_CATEGORY_BATTLE}
}

func TestGeneratedAPI(t *testing.T) {
	tbl := mustLevelTable(t, battleRow(6, "MOBA战斗"), battleRow(7, "松林镇副本"), battleRow(9, "备用"))

	// ByIDs:缺失键跳过,结果按入参序
	got := tbl.ByIDs([]uint32{7, 999, 6})
	if len(got) != 2 || got[0].GetId() != 7 || got[1].GetId() != 6 {
		t.Fatalf("ByIDs=%v", got)
	}
	// Where / First
	if rows := tbl.Where(func(r *configpb.LevelRow) bool { return r.GetId() > 6 }); len(rows) != 2 {
		t.Fatalf("Where=%v", rows)
	}
	if row, ok := tbl.First(func(r *configpb.LevelRow) bool { return r.GetName() == "备用" }); !ok || row.GetId() != 9 {
		t.Fatalf("First=%v %v", row, ok)
	}
	if _, ok := tbl.First(func(r *configpb.LevelRow) bool { return false }); ok {
		t.Fatal("First 无命中应 false")
	}
	// RandOne:非空表必命中且行属于本表
	row, ok := tbl.RandOne()
	if !ok || !tbl.Exists(row.GetId()) {
		t.Fatalf("RandOne=%v %v", row, ok)
	}
	// 空表:RandOne false / 其余 API 零值安全
	empty := mustLevelTable(t)
	if _, ok := empty.RandOne(); ok {
		t.Fatal("空表 RandOne 应 false")
	}
	if empty.Count() != 0 || len(empty.ByIDs([]uint32{1})) != 0 {
		t.Fatal("空表 API 应零值安全")
	}
}

// TestValidateHookWired 生成的 newLevelTable 必须调用手写 validateLevelRow(钩子接线守护)。
func TestValidateHookWired(t *testing.T) {
	bad := battleRow(6, "MOBA战斗")
	bad.AssetPath = ""
	if _, err := newLevelTable(&configpb.LevelTableData{Rows: []*configpb.LevelRow{bad}}); err == nil {
		t.Fatal("asset_path 为空应被伴生钩子拦下")
	}
	bad2 := battleRow(7, "x")
	bad2.Category = configpb.LevelCategory_LEVEL_CATEGORY_UNSPECIFIED
	if _, err := newLevelTable(&configpb.LevelTableData{Rows: []*configpb.LevelRow{bad2}}); err == nil {
		t.Fatal("category 未填应被伴生钩子拦下")
	}
}
