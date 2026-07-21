package tablegen

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	configpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
)

func testTables(t *testing.T) []Generated {
	t.Helper()
	def := levelDef(t)
	data, rows, err := def.Build(sampleGrid())
	if err != nil {
		t.Fatal(err)
	}
	return []Generated{{
		Name: def.Name, ProtoName: def.ProtoName,
		Data: data, RowCount: rows,
	}}
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestWriteBatchAndVersioning(t *testing.T) {
	dir := t.TempDir()
	// 首批:日期基准版本
	res, err := WriteBatch(testTables(t), Options{OutDir: dir, SourceRev: "svn-r1", Now: at(t, "2026-07-21")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 20260721001 || res.Unchanged {
		t.Fatalf("res=%+v", res)
	}

	// 同内容重跑:幂等跳过,不升版本
	res2, err := WriteBatch(testTables(t), Options{OutDir: dir, SourceRev: "svn-r1", Now: at(t, "2026-07-21")})
	if err != nil {
		t.Fatal(err)
	}
	if !res2.Unchanged || res2.Version != 20260721001 {
		t.Fatalf("同内容应幂等: %+v", res2)
	}

	// 内容变化:同日 +1
	changed := testTables(t)
	changed[0].Data.(*configpb.LevelTableData).Rows[0].Name = "登录2"
	res3, err := WriteBatch(changed, Options{OutDir: dir, Now: at(t, "2026-07-21")})
	if err != nil {
		t.Fatal(err)
	}
	if res3.Version != 20260721002 {
		t.Fatalf("同日第二批应 +1: %+v", res3)
	}

	// 次日:回到日期基准
	changed[0].Data.(*configpb.LevelTableData).Rows[0].Name = "登录3"
	res4, err := WriteBatch(changed, Options{OutDir: dir, Now: at(t, "2026-07-22")})
	if err != nil {
		t.Fatal(err)
	}
	if res4.Version != 20260722001 {
		t.Fatalf("次日应回日期基准: %+v", res4)
	}

	// 强制版本:不得低于上一批
	if _, err := WriteBatch(testTables(t), Options{OutDir: dir, ForceVersion: 5, Now: at(t, "2026-07-22")}); err == nil {
		t.Fatal("低版本 ForceVersion 应被拒绝")
	}

	// manifest 结构可被 json 严格读回
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifestFile
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != 20260722001 || len(m.Tables) != 1 || m.Tables[0].Rows != 4 {
		t.Fatalf("manifest=%+v", m)
	}
}

// TestMarshalDeterministic 同内容多次序列化字节级一致(checksum 稳定 / git diff 干净)。
func TestMarshalDeterministic(t *testing.T) {
	data, _, err := levelDef(t).Build(sampleGrid())
	if err != nil {
		t.Fatal(err)
	}
	a, err := marshalDeterministic(data)
	if err != nil {
		t.Fatal(err)
	}
	for range 16 {
		b, err := marshalDeterministic(data)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatal("序列化结果不确定")
		}
	}
	if !bytes.Contains(a, []byte(`"asset_path"`)) {
		t.Fatalf("应使用 proto 原名 snake_case: %s", a[:200])
	}
	if bytes.Contains(a, []byte("LEVEL_CATEGORY")) {
		t.Fatal("枚举应输出数字而非名字")
	}
}
