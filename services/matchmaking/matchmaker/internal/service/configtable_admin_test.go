// configtable_admin_test.go — 配置表热更入口(ReloadConfigTable)语义测试:
// 幂等 no-op / 成功切换 / 失败保留旧表 / expect_version / 玩家身份拒绝。
package service

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
	plog "github.com/luyuancpp/pandora/pkg/log"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	configpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
	configv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
)

func writeLevelBatchDir(t *testing.T, dir string, version uint64) {
	t.Helper()
	raw, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true}.Marshal(
		&configpb.LevelTableData{Rows: []*configpb.LevelRow{{
			Id: 6, Name: "MOBA战斗", AssetPath: "/Game/L/MobaLevel.MobaLevel",
			Category: configpb.LevelCategory_LEVEL_CATEGORY_BATTLE,
		}}})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	mraw, err := json.Marshal(map[string]any{
		"version": version,
		"tables": []map[string]any{{
			"name": "level", "file": "level.json",
			"proto":    "pandora.config.v1.LevelTableData",
			"checksum": "sha256:" + hex.EncodeToString(sum[:]),
			"rows":     1,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "level.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, configtable.ManifestFileName), mraw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReloadConfigTable(t *testing.T) {
	dir := t.TempDir()
	writeLevelBatchDir(t, dir, 100)
	store := configtable.NewStore()
	if _, err := store.Load(dir, 0); err != nil {
		t.Fatal(err)
	}
	svc := NewConfigTableAdminService(store, dir)
	ctx := context.Background()

	// 同版本 → 幂等 no-op
	resp, err := svc.ReloadConfigTable(ctx, &configv1.ReloadConfigTableRequest{})
	if err != nil || resp.GetCode() != commonv1.ErrCode_OK || resp.GetReloaded() || resp.GetActiveVersion() != 100 {
		t.Fatalf("同版本应 no-op: %+v err=%v", resp, err)
	}

	// 新版本 → 切换
	writeLevelBatchDir(t, dir, 200)
	resp, err = svc.ReloadConfigTable(ctx, &configv1.ReloadConfigTableRequest{ExpectVersion: 200})
	if err != nil || resp.GetCode() != commonv1.ErrCode_OK || !resp.GetReloaded() || resp.GetActiveVersion() != 200 {
		t.Fatalf("新版本应切换: %+v err=%v", resp, err)
	}

	// expect_version 不符 → 拒绝且保留 200
	resp, err = svc.ReloadConfigTable(ctx, &configv1.ReloadConfigTableRequest{ExpectVersion: 999})
	if err != nil || resp.GetCode() != commonv1.ErrCode_ERR_INVALID_STATE || resp.GetActiveVersion() != 200 {
		t.Fatalf("expect 不符应拒绝并保留旧版本: %+v err=%v", resp, err)
	}

	// active 目录损坏 → 失败保留旧表(标准流水线核心不变量)
	if err := os.WriteFile(filepath.Join(dir, "level.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	mraw, _ := json.Marshal(map[string]any{"version": 300, "tables": []map[string]any{{
		"name": "level", "file": "level.json",
		"proto": "pandora.config.v1.LevelTableData", "checksum": "sha256:deadbeef", "rows": 1,
	}}})
	if err := os.WriteFile(filepath.Join(dir, configtable.ManifestFileName), mraw, 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err = svc.ReloadConfigTable(ctx, &configv1.ReloadConfigTableRequest{})
	if err != nil || resp.GetCode() != commonv1.ErrCode_ERR_INVALID_STATE || resp.GetActiveVersion() != 200 {
		t.Fatalf("损坏批次应失败并保留 200: %+v err=%v", resp, err)
	}
	if store.Tables().Version != 200 || store.Tables().Level.Count() != 1 {
		t.Fatalf("旧表应原样生效: %+v", store.Tables())
	}

	// 带玩家身份(经 Envoy 注入)调用 → 拒绝
	playerCtx := context.WithValue(ctx, plog.CtxKeyPlayerID, uint64(42))
	resp, err = svc.ReloadConfigTable(playerCtx, &configv1.ReloadConfigTableRequest{})
	if err != nil || resp.GetCode() != commonv1.ErrCode_ERR_PERMISSION_DENY {
		t.Fatalf("玩家身份应被拒: %+v err=%v", resp, err)
	}
}
