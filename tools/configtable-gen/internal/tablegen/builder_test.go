package tablegen

import (
	"strings"
	"testing"

	configpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
)

// levelDef 从描述符发现 level 表定义(excel.proto 注解驱动,零手写登记)。
func levelDef(t *testing.T) *TableDef {
	t.Helper()
	defs, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for i := range defs {
		if defs[i].Name == "level" {
			return &defs[i]
		}
	}
	t.Fatalf("未发现 level 表,defs=%v", defs)
	return nil
}

func TestDiscoverLevel(t *testing.T) {
	d := levelDef(t)
	if d.GoName != "Level" || d.RowType != "LevelRow" || d.DataType != "LevelTableData" ||
		d.ProtoName != "pandora.config.v1.LevelTableData" || d.KeyType != "uint32" {
		t.Fatalf("def=%+v", d)
	}
	if d.ExcelFile != "关卡/g_关卡.xlsx" {
		t.Fatalf("ExcelFile=%q", d.ExcelFile)
	}
	if len(d.columns) != 7 || d.columns[0].header != "ID" || d.columns[6].header != "匹配列表显示" {
		t.Fatalf("columns=%d", len(d.columns))
	}
}

// sampleGrid 复刻 g_关卡.xlsx 的真实版式:1 表头、2-4 注释、5+ 数据、尾部残留空单元格行。
func sampleGrid() [][]string {
	return [][]string{
		{"ID", "关卡名称", "关卡资源", "GameMode类", "关卡类别", "禁止ui快捷键开关", "匹配列表显示"},
		{"", "", "", "", "", "0:不禁止", "0:不显示(默认)"},
		{"", "", "", "", "", "1:禁止(默认)", "1:显示"},
		{},
		{"1", "登录", "/Game/Level/Login/Lvl_Login.Lvl_Login", "", "1", "", "0"},
		{"2", "选角", "/Game/Level/RoleSelect/Lvl_RoleSelect.Lvl_RoleSelect", "", "2", "", "0"},
		{"6", "MOBA战斗", "/Game/Test/Level/MobaLevel.MobaLevel", "/Script/Pandora.PandoraBattleGameMode", "4", "", "1"},
		{"7", "松林镇副本", "/Game/Test/Level/SonglinTown.SonglinTown", "/Script/Pandora.PandoraPveGameMode", "4", "", "1"},
		{"", "", "", ""}, // 格式残留:全空行(g_关卡 D12-D51 的空字符串单元格)
		{"", "", "", ""},
	}
}

func buildLevel(t *testing.T, grid [][]string) (*configpb.LevelTableData, error) {
	t.Helper()
	msg, rows, err := levelDef(t).Build(grid)
	if err != nil {
		return nil, err
	}
	data, ok := msg.(*configpb.LevelTableData)
	if !ok {
		t.Fatalf("Build 返回类型 %T,应为具体生成类型", msg)
	}
	if rows != len(data.GetRows()) {
		t.Fatalf("行数返回 %d 与容器 %d 不一致", rows, len(data.GetRows()))
	}
	return data, nil
}

func TestBuildLevelHappyPath(t *testing.T) {
	data, err := buildLevel(t, sampleGrid())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(data.Rows) != 4 {
		t.Fatalf("期望 4 行,得到 %d", len(data.Rows))
	}
	r6 := data.Rows[2]
	if r6.Id != 6 || r6.Category != configpb.LevelCategory_LEVEL_CATEGORY_BATTLE || !r6.ShowInMatchList {
		t.Fatalf("id6 解析错误: %+v", r6)
	}
	// 布尔默认值((excel_default) 注解):禁止ui快捷键开关 空 = true;匹配列表显示 填 0 = false
	r1 := data.Rows[0]
	if !r1.DisableUiShortcut || r1.ShowInMatchList {
		t.Fatalf("id1 默认值错误: %+v", r1)
	}
	if r1.GameModeClass != "" {
		t.Fatalf("id1 GameMode类 应为空: %+v", r1)
	}
}

func TestBuildLevelErrors(t *testing.T) {
	mutate := func(f func(g [][]string) [][]string) [][]string { return f(sampleGrid()) }
	cases := map[string]struct {
		grid    [][]string
		wantErr string
	}{
		"表头改名": {mutate(func(g [][]string) [][]string {
			g[0][1] = "名称"
			return g
		}), "表头第 B 列"},
		"表头新列未登记": {mutate(func(g [][]string) [][]string {
			g[0] = append(g[0], "新列")
			return g
		}), "未登记"},
		"主键重复": {mutate(func(g [][]string) [][]string {
			g[5][0] = "1"
			return g
		}), "重复"},
		"ID 非数字": {mutate(func(g [][]string) [][]string {
			g[4][0] = "abc"
			return g
		}), "非负整数"},
		"ID 为 0": {mutate(func(g [][]string) [][]string {
			g[4][0] = "0"
			return g
		}), "正整数"},
		"有内容但 ID 空": {mutate(func(g [][]string) [][]string {
			g[4][0] = ""
			return g
		}), "ID: 必填列为空"},
		"名称为空": {mutate(func(g [][]string) [][]string {
			g[4][1] = ""
			return g
		}), "关卡名称: 必填列为空"},
		"资源路径缺前缀": {mutate(func(g [][]string) [][]string {
			g[4][2] = "Game/x"
			return g
		}), "关卡资源"},
		"GameMode 缺前缀": {mutate(func(g [][]string) [][]string {
			g[4][3] = "Pandora.Foo"
			return g
		}), "GameMode类"},
		"类别越界": {mutate(func(g [][]string) [][]string {
			g[4][4] = "9"
			return g
		}), "枚举"},
		"类别填 0": {mutate(func(g [][]string) [][]string {
			g[4][4] = "0"
			return g
		}), "UNSPECIFIED 不允许"},
		"类别为空": {mutate(func(g [][]string) [][]string {
			g[4][4] = ""
			return g
		}), "关卡类别: 必填列为空"},
		"布尔列非法": {mutate(func(g [][]string) [][]string {
			g[4][6] = "2"
			return g
		}), "布尔列"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := buildLevel(t, tc.grid)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("期望含 %q 的错误,得到: %v", tc.wantErr, err)
			}
		})
	}
}
