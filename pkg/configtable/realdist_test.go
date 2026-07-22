package configtable

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadRealDistIfPresent 端到端冒烟:直接加载仓库 configtable/dist 的真实产物
// (tools/configtable-gen 生成)。产物尚未生成的环境跳过。
func TestLoadRealDistIfPresent(t *testing.T) {
	dist := filepath.Join("..", "..", "configtable", "dist")
	if _, err := os.Stat(filepath.Join(dist, ManifestFileName)); err != nil {
		t.Skipf("真实 dist 不存在,跳过: %v", err)
	}
	s := NewStore()
	res, err := s.Load(dist, 0)
	if err != nil {
		t.Fatalf("加载真实 dist 失败: %v", err)
	}
	for _, w := range res.Warnings {
		t.Logf("告警: %s", w)
	}
	tb := s.Tables()
	if tb.Level.Count() == 0 {
		t.Fatal("关卡表为空")
	}
	// 与 g_关卡.xlsx 的稳定事实对齐:6=MOBA战斗、7=松林镇副本均为战斗类;1=登录不是。
	if !tb.Level.IsBattleLevel(6) || !tb.Level.IsBattleLevel(7) {
		t.Fatal("6/7 应为战斗关卡")
	}
	if tb.Level.IsBattleLevel(1) {
		t.Fatal("1(登录)不应为战斗关卡")
	}
	if err := tb.PlayerLevelExp.ValidateCurve(); err != nil {
		t.Fatalf("真实玩家等级经验表不合法: %v", err)
	}
	if tb.PlayerLevelExp.Count() != 15 || tb.PlayerLevelExp.MaxLevel() != 15 {
		t.Fatalf("玩家等级经验表等级数=%d max=%d, want 15/15",
			tb.PlayerLevelExp.Count(), tb.PlayerLevelExp.MaxLevel())
	}
	curve := tb.PlayerLevelExp.ExperienceCurve()
	if len(curve) != 14 || curve[0] != 1000 || curve[7] != 6600 || curve[13] != 11400 {
		t.Fatalf("真实曲线关键值错误: %v", curve)
	}
	last, ok := tb.PlayerLevelExp.ByID(15)
	if !ok || last.GetUpgradeExp() != 0 || last.GetCumulativeExp() != 86800 {
		t.Fatalf("Lv15 终点错误: row=%+v ok=%v", last, ok)
	}
}
