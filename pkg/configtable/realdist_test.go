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
}
