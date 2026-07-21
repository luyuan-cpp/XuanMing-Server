// fixtures_test.go — key / multi_key / fk / bit_index 四类注解的夹具测试。
// 夹具表定义在 proto/pandora/configtest/v1(独立包,生产 Discover 扫不到,
// 角色对齐旧项目 data/ 的 Test.xlsx / TestMultiKey.xlsx)。
package tablegen

import (
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	testpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/configtest/v1"
)

const testPkg = "pandora.configtest.v1"

func fixtureDefs(t *testing.T) []TableDef {
	t.Helper()
	defs, err := DiscoverPackage(testPkg)
	if err != nil {
		t.Fatalf("DiscoverPackage(%s): %v", testPkg, err)
	}
	return defs
}

func fixtureDef(t *testing.T, name string) *TableDef {
	t.Helper()
	defs := fixtureDefs(t)
	for i := range defs {
		if defs[i].Name == name {
			return &defs[i]
		}
	}
	t.Fatalf("夹具表 %q 未发现: %v", name, tableNames(defs))
	return nil
}

// 场景夹具网格(表头「ID/名称」,数据 5 行起)。
func sceneGrid(rows ...[]string) [][]string {
	g := [][]string{{"ID", "名称"}, {}, {}, {}}
	return append(g, rows...)
}

// 副本夹具网格(表头「ID/场景ID/难度/编码」)。
func dungeonGrid(rows ...[]string) [][]string {
	g := [][]string{{"ID", "场景ID", "难度", "编码"}, {}, {}, {}}
	return append(g, rows...)
}

func TestDiscoverFixtures(t *testing.T) {
	scene, dungeon := fixtureDef(t, "test_scene"), fixtureDef(t, "test_dungeon")

	if !dungeon.BitIndex || scene.BitIndex {
		t.Fatalf("bit_index 注解解析错误: dungeon=%v scene=%v", dungeon.BitIndex, scene.BitIndex)
	}
	// 唯一键:scene.scene_name、dungeon.code
	if ks := scene.UniqueKeys(); len(ks) != 1 || ks[0].GoField != "SceneName" || ks[0].GoType != "string" {
		t.Fatalf("scene UniqueKeys=%+v", ks)
	}
	if ks := dungeon.UniqueKeys(); len(ks) != 1 || ks[0].GoField != "Code" {
		t.Fatalf("dungeon UniqueKeys=%+v", ks)
	}
	// 索引:difficulty(multi_key)+ scene_id(fk 隐含反查)
	mk := dungeon.MultiKeys()
	if len(mk) != 2 {
		t.Fatalf("dungeon MultiKeys=%+v", mk)
	}
	// 外键解析:scene_id → test_scene
	fks := dungeon.FKs()
	if len(fks) != 1 || fks[0].GoField != "SceneId" || fks[0].TargetName != "test_scene" ||
		fks[0].TargetGoName != "TestScene" || fks[0].TargetRow != "TestSceneRow" || !fks[0].Required {
		t.Fatalf("dungeon FKs=%+v", fks)
	}
}

func TestBuilderUniqueKeyDup(t *testing.T) {
	scene := fixtureDef(t, "test_scene")
	if _, _, err := scene.Build(sceneGrid(
		[]string{"1", "矿井"},
		[]string{"2", "矿井"}, // 名称唯一键重复
	)); err == nil || !strings.Contains(err.Error(), "唯一键取值") {
		t.Fatalf("唯一键重复应报错: %v", err)
	}
	if _, _, err := scene.Build(sceneGrid(
		[]string{"1", "矿井"},
		[]string{"2", "松林镇"},
	)); err != nil {
		t.Fatalf("合法场景表应通过: %v", err)
	}
}

func TestValidateFKs(t *testing.T) {
	scene, dungeon := fixtureDef(t, "test_scene"), fixtureDef(t, "test_dungeon")
	defs := fixtureDefs(t)

	sceneData, _, err := scene.Build(sceneGrid([]string{"1", "矿井"}, []string{"2", "松林镇"}))
	if err != nil {
		t.Fatal(err)
	}

	build := func(rows ...[]string) proto.Message {
		data, _, err := dungeon.Build(dungeonGrid(rows...))
		if err != nil {
			t.Fatalf("构建副本夹具失败: %v", err)
		}
		return data
	}

	// 合法:同场景两档难度(分层方案的核心诉求)
	ok := build([]string{"10", "1", "1", "MINE_N"}, []string{"11", "1", "2", "MINE_H"})
	if err := ValidateFKs(defs, map[string]proto.Message{"test_scene": sceneData, "test_dungeon": ok}); err != nil {
		t.Fatalf("合法引用应通过: %v", err)
	}

	// 引用不存在的场景
	bad := build([]string{"10", "99", "1", "X"})
	err = ValidateFKs(defs, map[string]proto.Message{"test_scene": sceneData, "test_dungeon": bad})
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("悬空引用应报错: %v", err)
	}

	// 必填外键为 0(场景ID 列 excel_required,单元格 "0" 能过列解析,批级校验必须拦下)
	zero := build([]string{"10", "0", "1", "X"})
	err = ValidateFKs(defs, map[string]proto.Message{"test_scene": sceneData, "test_dungeon": zero})
	if err == nil || !strings.Contains(err.Error(), "必填外键") {
		t.Fatalf("必填外键为 0 应报错: %v", err)
	}

	// 目标表缺席
	err = ValidateFKs(defs, map[string]proto.Message{"test_dungeon": ok})
	if err == nil || !strings.Contains(err.Error(), "不在本批") {
		t.Fatalf("目标表缺席应报错: %v", err)
	}
}

func TestBitStateAssignStable(t *testing.T) {
	s := &BitState{}
	// 首批:1/3/5 → 0/1/2(升序追加)
	live, changed := s.Assign([]uint32{5, 1, 3})
	if !changed || len(live) != 3 || live[0] != (BitEntry{ID: 1, Bit: 0}) || live[2] != (BitEntry{ID: 5, Bit: 2}) {
		t.Fatalf("首批分配错误: %+v", live)
	}
	// 删 3、加 2:已有沿用,2 追加到 bit 3;3 的 bit 1 保留占位不复用
	live, changed = s.Assign([]uint32{1, 5, 2})
	if !changed || len(live) != 3 {
		t.Fatalf("第二批分配错误: %+v", live)
	}
	byID := map[uint32]uint32{}
	for _, e := range live {
		byID[e.ID] = e.Bit
	}
	if byID[1] != 0 || byID[5] != 2 || byID[2] != 3 {
		t.Fatalf("位序不稳定: %+v", byID)
	}
	if s.BitCount() != 4 {
		t.Fatalf("BitCount 应含保留位 = 4,实为 %d", s.BitCount())
	}
	// 同集合重跑:零变更
	if _, changed = s.Assign([]uint32{1, 5, 2}); changed {
		t.Fatal("同集合重跑不应变更状态")
	}
}

func TestBitStateSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	s := &BitState{}
	s.Assign([]uint32{7, 6})
	if err := SaveBitState(path, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBitState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Entries) != 2 || loaded.BitCount() != 2 {
		t.Fatalf("回环失败: %+v", loaded)
	}
	// 缺失文件 = 空状态
	empty, err := LoadBitState(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || len(empty.Entries) != 0 {
		t.Fatalf("缺失状态应为空: %+v err=%v", empty, err)
	}
}

// 编译期防漂移:夹具 pb 类型确实存在(误删 configtest proto 会在此报编译错误)。
var _ = (*testpb.TestDungeonRow)(nil)
