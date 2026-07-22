package configtable

import (
	"context"
	"testing"

	plog "github.com/luyuancpp/pandora/pkg/log"
	commonv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/common/v1"
	configv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
)

func TestAdminServiceReloadAndPlayerDeny(t *testing.T) {
	dir := t.TempDir()
	writeGoodBatch(t, dir, 100)
	store := NewStore()
	if _, err := store.Load(dir, 0); err != nil {
		t.Fatal(err)
	}
	admin := NewAdminService(store, dir)

	writeGoodBatch(t, dir, 200)
	resp, err := admin.ReloadConfigTable(context.Background(), &configv1.ReloadConfigTableRequest{ExpectVersion: 200})
	if err != nil || resp.GetCode() != commonv1.ErrCode_OK || !resp.GetReloaded() || resp.GetActiveVersion() != 200 {
		t.Fatalf("热更失败: resp=%+v err=%v", resp, err)
	}

	playerCtx := context.WithValue(context.Background(), plog.CtxKeyPlayerID, uint64(42))
	resp, err = admin.ReloadConfigTable(playerCtx, &configv1.ReloadConfigTableRequest{})
	if err != nil || resp.GetCode() != commonv1.ErrCode_ERR_PERMISSION_DENY || resp.GetActiveVersion() != 0 {
		t.Fatalf("玩家身份调用应拒绝: resp=%+v err=%v", resp, err)
	}
}
