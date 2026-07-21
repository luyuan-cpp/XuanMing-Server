package configtable

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	configpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
)

// marshalLevel 与生成器一致的序列化口径:proto 原名 + 枚举数字。
func marshalLevel(t *testing.T, data *configpb.LevelTableData) []byte {
	t.Helper()
	raw, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true}.Marshal(data)
	if err != nil {
		t.Fatalf("marshal level: %v", err)
	}
	return raw
}

func sampleLevelData() *configpb.LevelTableData {
	return &configpb.LevelTableData{Rows: []*configpb.LevelRow{
		{Id: 1, Name: "登录", AssetPath: "/Game/Level/Login/Lvl_Login.Lvl_Login",
			Category: configpb.LevelCategory_LEVEL_CATEGORY_LOGIN, DisableUiShortcut: true},
		{Id: 6, Name: "MOBA战斗", AssetPath: "/Game/Test/Level/MobaLevel.MobaLevel",
			GameModeClass: "/Script/Pandora.PandoraBattleGameMode",
			Category:      configpb.LevelCategory_LEVEL_CATEGORY_BATTLE, ShowInMatchList: true},
		{Id: 7, Name: "松林镇副本", AssetPath: "/Game/Test/Level/SonglinTown.SonglinTown",
			GameModeClass: "/Script/Pandora.PandoraPveGameMode",
			Category:      configpb.LevelCategory_LEVEL_CATEGORY_BATTLE, ShowInMatchList: true},
	}}
}

func checksumOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writeBatch 在 dir 写出一批产物(level.json + manifest.json),mutate 可在写盘前篡改清单。
func writeBatch(t *testing.T, dir string, version uint64, levelRaw []byte, rows uint32, mutate func(*Manifest)) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "level.json"), levelRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{
		Version:   version,
		Generator: "configtable-gen@test",
		SourceRev: "test",
		Tables: []ManifestTable{{
			Name: "level", File: "level.json",
			Proto: "pandora.config.v1.LevelTableData", Checksum: checksumOf(levelRaw), Rows: rows,
		}},
	}
	if mutate != nil {
		mutate(m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeGoodBatch(t *testing.T, dir string, version uint64) {
	t.Helper()
	writeBatch(t, dir, version, marshalLevel(t, sampleLevelData()), 3, nil)
}

func TestLoadHappyPath(t *testing.T) {
	dir := t.TempDir()
	writeGoodBatch(t, dir, 100)
	s := NewStore()
	res, err := s.Load(dir, 0)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !res.Reloaded || res.Version != 100 {
		t.Fatalf("res=%+v", res)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("意外告警: %v", res.Warnings)
	}
	tb := s.Tables()
	if tb == nil || tb.Version != 100 || tb.Level.Count() != 3 {
		t.Fatalf("Tables=%+v", tb)
	}
	if row, ok := tb.Level.ByID(6); !ok || row.GetName() != "MOBA战斗" {
		t.Fatalf("ByID(6)=%v %v", row, ok)
	}
	if !tb.Level.IsBattleLevel(6) || !tb.Level.IsBattleLevel(7) {
		t.Fatal("6/7 应为战斗关卡")
	}
	if tb.Level.IsBattleLevel(1) {
		t.Fatal("1(登录)不应为战斗关卡")
	}
	if tb.Level.IsBattleLevel(999) {
		t.Fatal("不存在的 map_id 不应通过")
	}
}

func TestLoadExpectVersion(t *testing.T) {
	dir := t.TempDir()
	writeGoodBatch(t, dir, 100)
	s := NewStore()
	if _, err := s.Load(dir, 99); err == nil {
		t.Fatal("expectVersion 不符应拒绝")
	}
	if s.Tables() != nil {
		t.Fatal("失败后不应有生效批次")
	}
	if _, err := s.Load(dir, 100); err != nil {
		t.Fatalf("expectVersion 相符应成功: %v", err)
	}
}

func TestLoadIdempotentSameVersion(t *testing.T) {
	dir := t.TempDir()
	writeGoodBatch(t, dir, 100)
	s := NewStore()
	if _, err := s.Load(dir, 0); err != nil {
		t.Fatal(err)
	}
	first := s.Tables()
	res, err := s.Load(dir, 0)
	if err != nil || res.Reloaded {
		t.Fatalf("同版本应幂等 no-op: res=%+v err=%v", res, err)
	}
	if s.Tables() != first {
		t.Fatal("no-op 不应更换快照指针")
	}
}

func TestLoadRejectVersionRegress(t *testing.T) {
	newDir, oldDir := t.TempDir(), t.TempDir()
	writeGoodBatch(t, newDir, 200)
	writeGoodBatch(t, oldDir, 100)
	s := NewStore()
	if _, err := s.Load(newDir, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(oldDir, 0); err == nil || !strings.Contains(err.Error(), "回退") {
		t.Fatalf("低版本应被拒绝: %v", err)
	}
	if s.Tables().Version != 200 {
		t.Fatal("拒绝回退后应保留 200")
	}
}

// TestLoadFailKeepsOld 标准流水线核心不变量:新批次任一步失败,旧批次原样生效。
func TestLoadFailKeepsOld(t *testing.T) {
	good := t.TempDir()
	writeGoodBatch(t, good, 100)
	s := NewStore()
	if _, err := s.Load(good, 0); err != nil {
		t.Fatal(err)
	}

	cases := map[string]func(dir string){
		"checksum 不匹配": func(dir string) {
			writeBatch(t, dir, 200, marshalLevel(t, sampleLevelData()), 3, func(m *Manifest) {
				m.Tables[0].Checksum = "sha256:" + strings.Repeat("0", 64)
			})
		},
		"行数与清单不符": func(dir string) {
			writeBatch(t, dir, 200, marshalLevel(t, sampleLevelData()), 99, nil)
		},
		"JSON 损坏": func(dir string) {
			raw := []byte(`{"rows": [{`)
			writeBatch(t, dir, 200, raw, 3, nil)
		},
		"主键重复": func(dir string) {
			d := sampleLevelData()
			d.Rows[1].Id = 1
			writeBatch(t, dir, 200, marshalLevel(t, d), 3, nil)
		},
		"asset_path 为空": func(dir string) {
			d := sampleLevelData()
			d.Rows[0].AssetPath = ""
			writeBatch(t, dir, 200, marshalLevel(t, d), 3, nil)
		},
		"category 未填": func(dir string) {
			d := sampleLevelData()
			d.Rows[0].Category = configpb.LevelCategory_LEVEL_CATEGORY_UNSPECIFIED
			writeBatch(t, dir, 200, marshalLevel(t, d), 3, nil)
		},
		"proto 全名不符": func(dir string) {
			writeBatch(t, dir, 200, marshalLevel(t, sampleLevelData()), 3, func(m *Manifest) {
				m.Tables[0].Proto = "pandora.config.v1.WrongTable"
			})
		},
		"file 路径逃逸": func(dir string) {
			writeBatch(t, dir, 200, marshalLevel(t, sampleLevelData()), 3, func(m *Manifest) {
				m.Tables[0].File = "../level.json"
			})
		},
		"缺必需表": func(dir string) {
			writeBatch(t, dir, 200, marshalLevel(t, sampleLevelData()), 3, func(m *Manifest) {
				m.Tables[0].Name = "renamed"
				m.Tables[0].File = "renamed.json"
			})
			// 同步改文件名,让失败原因落在「缺 level 表」而非文件缺失
			if err := os.Rename(filepath.Join(dir, "level.json"), filepath.Join(dir, "renamed.json")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			bad := t.TempDir()
			prepare(bad)
			if _, err := s.Load(bad, 0); err == nil {
				t.Fatal("应加载失败")
			}
			tb := s.Tables()
			if tb == nil || tb.Version != 100 || tb.Level.Count() != 3 {
				t.Fatalf("失败后旧批次应原样保留, got %+v", tb)
			}
		})
	}
}

// TestLoadUnknownTableSkipped 前向兼容:清单含未注册新表 → 跳过 + 告警,不失败。
func TestLoadUnknownTableSkipped(t *testing.T) {
	dir := t.TempDir()
	extra := []byte(`{"rows":[]}`)
	writeBatch(t, dir, 100, marshalLevel(t, sampleLevelData()), 3, func(m *Manifest) {
		m.Tables = append(m.Tables, ManifestTable{
			Name: "future_table", File: "future_table.json",
			Proto: "pandora.config.v1.FutureTableData", Checksum: checksumOf(extra), Rows: 0,
		})
	})
	if err := os.WriteFile(filepath.Join(dir, "future_table.json"), extra, 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStore()
	res, err := s.Load(dir, 0)
	if err != nil {
		t.Fatalf("未知表不应导致失败: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "future_table") {
		t.Fatalf("应有未知表告警: %v", res.Warnings)
	}
}

// TestLoadStrayFileWarned 目录里 manifest 未列出的 json = 脏数据告警,不拒载。
func TestLoadStrayFileWarned(t *testing.T) {
	dir := t.TempDir()
	writeGoodBatch(t, dir, 100)
	if err := os.WriteFile(filepath.Join(dir, "stray.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewStore()
	res, err := s.Load(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "stray.json") {
		t.Fatalf("应有脏文件告警: %v", res.Warnings)
	}
}

// TestLoadTolerateUnknownField 运行时 DiscardUnknown:新增字段的 JSON 旧进程可读(滚动窗口)。
func TestLoadTolerateUnknownField(t *testing.T) {
	dir := t.TempDir()
	raw := marshalLevel(t, sampleLevelData())
	patched := strings.Replace(string(raw), `"rows":[{`, `"rows":[{"future_field":123,`, 1)
	if patched == string(raw) {
		t.Fatal("补丁未生效")
	}
	writeBatch(t, dir, 100, []byte(patched), 3, nil)
	s := NewStore()
	if _, err := s.Load(dir, 0); err != nil {
		t.Fatalf("未知字段应被容忍: %v", err)
	}
}

// TestConcurrentReadDuringReload -race 下并发读 + 热切换:读方永远看到完整一致的批次。
func TestConcurrentReadDuringReload(t *testing.T) {
	v1, v2 := t.TempDir(), t.TempDir()
	writeGoodBatch(t, v1, 100)
	writeGoodBatch(t, v2, 200)
	s := NewStore()
	if _, err := s.Load(v1, 0); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				tb := s.Tables()
				if tb.Level.Count() != 3 {
					t.Error("读到不完整批次")
					return
				}
				if v := tb.Version; v != 100 && v != 200 {
					t.Errorf("非法版本 %d", v)
					return
				}
			}
		}()
	}
	if _, err := s.Load(v2, 0); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	if s.Tables().Version != 200 {
		t.Fatal("切换未生效")
	}
}
