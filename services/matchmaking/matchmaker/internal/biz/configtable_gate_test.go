// configtable_gate_test.go — StartMatch 关卡表准入门(不变量 §9.15 接线)测试。
// 用真实 pkg/configtable.Store 从临时目录加载批次,覆盖启用 / 未启用 / 热更三种形态。
package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/luyuancpp/pandora/pkg/configtable"
	"github.com/luyuancpp/pandora/pkg/errcode"
	configpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"

	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/conf"
)

// writeLevelBatch 在临时目录写一批完整配置表产物(与 tools/configtable-gen 同一 JSON 口径)。
func writeLevelBatch(t *testing.T, dir string, version uint64, rows []*configpb.LevelRow) {
	t.Helper()
	levelRaw, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true}.
		Marshal(&configpb.LevelTableData{Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	playerLevelExpRows := []*configpb.PlayerLevelExpRow{
		{Id: 1, Level: 1, UpgradeExp: 100, CumulativeExp: 0},
		{Id: 2, Level: 2, UpgradeExp: 0, CumulativeExp: 100},
	}
	playerLevelExpRaw, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true}.
		Marshal(&configpb.PlayerLevelExpTableData{Rows: playerLevelExpRows})
	if err != nil {
		t.Fatal(err)
	}
	levelSum := sha256.Sum256(levelRaw)
	playerLevelExpSum := sha256.Sum256(playerLevelExpRaw)
	manifest := map[string]any{
		"version":   version,
		"generator": "test",
		"tables": []map[string]any{
			{
				"name": "level", "file": "level.json",
				"proto":    "pandora.config.v1.LevelTableData",
				"checksum": "sha256:" + hex.EncodeToString(levelSum[:]),
				"rows":     len(rows),
			},
			{
				"name": "player_level_exp", "file": "player_level_exp.json",
				"proto":    "pandora.config.v1.PlayerLevelExpTableData",
				"checksum": "sha256:" + hex.EncodeToString(playerLevelExpSum[:]),
				"rows":     len(playerLevelExpRows),
			},
		},
	}
	mraw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "level.json"), levelRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "player_level_exp.json"), playerLevelExpRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, configtable.ManifestFileName), mraw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func levelRows(includeMap6 bool) []*configpb.LevelRow {
	rows := []*configpb.LevelRow{
		{Id: 1, Name: "登录", AssetPath: "/Game/L/Login.Login",
			Category: configpb.LevelCategory_LEVEL_CATEGORY_LOGIN},
		{Id: 7, Name: "松林镇副本", AssetPath: "/Game/L/SonglinTown.SonglinTown",
			Category: configpb.LevelCategory_LEVEL_CATEGORY_BATTLE},
	}
	if includeMap6 {
		rows = append(rows, &configpb.LevelRow{Id: 6, Name: "MOBA战斗",
			AssetPath: "/Game/L/MobaLevel.MobaLevel",
			Category:  configpb.LevelCategory_LEVEL_CATEGORY_BATTLE})
	}
	return rows
}

func TestStartMatch_MapGate(t *testing.T) {
	f := newFixtureWith(t, 8000, func(c *conf.MatchConf) { c.MapId = 6 })

	dir := t.TempDir()
	writeLevelBatch(t, dir, 100, levelRows(true))
	store := configtable.NewStore()
	if _, err := store.Load(dir, 0); err != nil {
		t.Fatal(err)
	}
	f.uc.SetConfigTables(store)
	ctx := context.Background()

	// 战斗类关卡放行
	if _, err := f.uc.StartMatch(ctx, 8101, 8101, 1001, 6); err != nil {
		t.Fatalf("map 6 应放行: %v", err)
	}
	// map_id=0 → 兜底 cfg.MapId=6,放行
	if _, err := f.uc.StartMatch(ctx, 8102, 8102, 1002, 0); err != nil {
		t.Fatalf("map 0(默认 6)应放行: %v", err)
	}
	// 非战斗类关卡(登录)拒绝
	if _, err := f.uc.StartMatch(ctx, 8103, 8103, 1003, 1); errcode.As(err) != errcode.ErrMatchInvalidMap {
		t.Fatalf("map 1(登录)应拒绝 ErrMatchInvalidMap: %v", err)
	}
	// 表里不存在的 map 拒绝
	if _, err := f.uc.StartMatch(ctx, 8104, 8104, 1004, 999); errcode.As(err) != errcode.ErrMatchInvalidMap {
		t.Fatalf("map 999 应拒绝 ErrMatchInvalidMap: %v", err)
	}
}

// TestStartMatch_MapGateHotReload 热更后新批次立即生效:删掉 map 6 → 后续 StartMatch 被拒。
func TestStartMatch_MapGateHotReload(t *testing.T) {
	f := newFixtureWith(t, 8200, func(c *conf.MatchConf) { c.MapId = 7 })

	v1 := t.TempDir()
	writeLevelBatch(t, v1, 100, levelRows(true))
	store := configtable.NewStore()
	if _, err := store.Load(v1, 0); err != nil {
		t.Fatal(err)
	}
	f.uc.SetConfigTables(store)
	ctx := context.Background()

	if _, err := f.uc.StartMatch(ctx, 8201, 8201, 2001, 6); err != nil {
		t.Fatalf("热更前 map 6 应放行: %v", err)
	}

	v2 := t.TempDir()
	writeLevelBatch(t, v2, 200, levelRows(false)) // 新批次删掉 map 6
	res, err := store.Load(v2, 0)
	if err != nil || !res.Reloaded {
		t.Fatalf("热更失败: res=%+v err=%v", res, err)
	}
	if _, err := f.uc.StartMatch(ctx, 8202, 8202, 2002, 6); errcode.As(err) != errcode.ErrMatchInvalidMap {
		t.Fatalf("热更后 map 6 应被拒: %v", err)
	}
	// 默认副本 7 仍在表内,map 0 继续放行
	if _, err := f.uc.StartMatch(ctx, 8203, 8203, 2003, 0); err != nil {
		t.Fatalf("热更后 map 0(默认 7)应放行: %v", err)
	}
}

// TestStartMatch_MapGateDisabled 未启用配置表(tables=nil)保持历史行为:任意 map_id 放行。
func TestStartMatch_MapGateDisabled(t *testing.T) {
	f := newFixture(t, 8300)
	if _, err := f.uc.StartMatch(context.Background(), 8301, 8301, 3001, 424242); err != nil {
		t.Fatalf("未启用配置表时不应校验 map_id: %v", err)
	}
}

// TestTeamSizeForMap 按 map_id 读关卡表一方人数:表填正值按表,未填 / 未知 map / 未启用回退全局 cfg.TeamSize。
func TestTeamSizeForMap(t *testing.T) {
	f := newFixtureWith(t, 8400, func(c *conf.MatchConf) {
		c.TeamSize = 5
		c.MapId = 6 // map_id==0 的默认副本兜底
	})

	// 未启用配置表(tables=nil)→ 回退全局 5。
	if got := f.uc.teamSizeForMap(context.Background(), 7); got != 5 {
		t.Fatalf("tables=nil 应回退全局 5,得 %d", got)
	}

	dir := t.TempDir()
	rows := []*configpb.LevelRow{
		{Id: 6, Name: "MOBA战斗", AssetPath: "/Game/L/MobaLevel.MobaLevel",
			Category: configpb.LevelCategory_LEVEL_CATEGORY_BATTLE, TeamSize: 5},
		{Id: 7, Name: "松林镇副本", AssetPath: "/Game/L/SonglinTown.SonglinTown",
			Category: configpb.LevelCategory_LEVEL_CATEGORY_BATTLE, TeamSize: 1, AllowExit: true},
		{Id: 8, Name: "未填人数副本", AssetPath: "/Game/L/X.X",
			Category: configpb.LevelCategory_LEVEL_CATEGORY_BATTLE}, // TeamSize 留 0
	}
	writeLevelBatch(t, dir, 100, rows)
	store := configtable.NewStore()
	if _, err := store.Load(dir, 0); err != nil {
		t.Fatal(err)
	}
	f.uc.SetConfigTables(store)

	cases := []struct {
		name  string
		mapID uint32
		want  int
	}{
		{"表填 1v1", 7, 1},
		{"表填 5v5", 6, 5},
		{"map_id=0 兜底默认副本 6", 0, 5},
		{"表内未填人数回退全局", 8, 5},
		{"表内不存在的 map 回退全局", 999, 5},
	}
	for _, c := range cases {
		if got := f.uc.teamSizeForMap(context.Background(), c.mapID); got != c.want {
			t.Fatalf("%s: teamSizeForMap(%d)=%d, 期望 %d", c.name, c.mapID, got, c.want)
		}
	}
}
