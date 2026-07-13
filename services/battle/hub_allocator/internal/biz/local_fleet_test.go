package biz

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/battle/hub_allocator/internal/conf"
)

// newLocalFleetForTest 造一个可构造的 LocalHubFleetProvider:executable_path 指向临时文件
// (满足 os.Stat 存在校验),token 签发器故意返回错误以复现 enforce 下的 fail-closed 分支。
// buildEnv 在拿 token 前失败 → start() 在 exec 前返回错误 → 不会真正拉起任何进程。
func newLocalFleetForTest(t *testing.T, required bool) *LocalHubFleetProvider {
	t.Helper()
	dir := t.TempDir()
	exe := filepath.Join(dir, "PandoraServer.exe")
	if err := os.WriteFile(exe, []byte("stub"), 0o644); err != nil {
		t.Fatalf("write stub exe: %v", err)
	}
	p, err := NewLocalHubFleetProvider(conf.LocalHubConf{
		ExecutablePath: exe,
		AdvertiseHost:  "127.0.0.1",
		Port:           7777,
		Region:         "cn-1",
		Capacity:       500,
	})
	if err != nil {
		t.Fatalf("NewLocalHubFleetProvider: %v", err)
	}
	// 令牌签发器恒失败:enforce 下应导致 start 失败并 fail-closed。
	p.SetDSTokenIssuer(func(string, string, uint32) (string, int64, uint64, error) {
		return "", 0, 0, errors.New("boom: ds token sign failed")
	}, required)
	return p
}

// TestLocalFleet_EnforceFailClosed:enforce(required=true)下令牌签发失败 → ListShards
// 不返回候选(返回 ErrHubNoAvailable),避免把客户端路由到一个回调必被守卫全拒的 Hub。
func TestLocalFleet_EnforceFailClosed(t *testing.T) {
	p := newLocalFleetForTest(t, true)
	cands, err := p.ListShards(context.Background(), "cn-1")
	if err == nil {
		t.Fatal("enforce 下启动失败应返回错误,却成功了")
	}
	if errcode.As(err) != errcode.ErrHubNoAvailable {
		t.Fatalf("errcode: got %d want ErrHubNoAvailable(%d)", errcode.As(err), errcode.ErrHubNoAvailable)
	}
	if len(cands) != 0 {
		t.Fatalf("enforce fail-closed 不应返回候选,却返回了 %d 个", len(cands))
	}
}

// TestLocalFleet_PermissiveStillReturnsCandidate:off/permissive(required=false)下
// 即使 token 签发失败,也照旧返回候选(便于排查 DS 启动问题,保持原语义不变)。
func TestLocalFleet_PermissiveStillReturnsCandidate(t *testing.T) {
	p := newLocalFleetForTest(t, false)
	cands, err := p.ListShards(context.Background(), "cn-1")
	if err != nil {
		t.Fatalf("permissive 下不应因启动失败而返回错误: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("permissive 应返回 1 个候选,却返回了 %d 个", len(cands))
	}
}

func TestLocalFleetBuildEnvCarriesModelBIdentityAndLocalProfile(t *testing.T) {
	p := newLocalFleetForTest(t, false)
	var gotPod, gotUID string
	var gotEpoch uint32
	p.SetDSTokenIssuer(func(pod, uid string, epoch uint32) (string, int64, uint64, error) {
		gotPod, gotUID, gotEpoch = pod, uid, epoch
		return "model-b-token", 123, 7, nil
	}, true)

	env, err := p.buildEnv()
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	if gotPod != p.podName || gotUID != p.instanceUID || gotEpoch != 1 {
		t.Fatalf("issuer identity=(%q,%q,%d), want (%q,%q,1)", gotPod, gotUID, gotEpoch, p.podName, p.instanceUID)
	}
	if _, err := uuid.Parse(gotUID); err != nil {
		t.Fatalf("instance uid 不是 UUID: %q: %v", gotUID, err)
	}
	if value := lastEnvValue(env, "PANDORA_DS_TOKEN"); value != "model-b-token" {
		t.Fatalf("PANDORA_DS_TOKEN=%q", value)
	}
	if value := lastEnvValue(env, "PANDORA_DS_LOCAL_PROFILE"); value != "local-off-v1" {
		t.Fatalf("PANDORA_DS_LOCAL_PROFILE=%q", value)
	}
	if p.tokenGen != 7 {
		t.Fatalf("tokenGen=%d, want 7", p.tokenGen)
	}
}

func TestLocalFleetExtraEnvCannotOverrideLocalProfile(t *testing.T) {
	p := newLocalFleetForTest(t, false)
	p.dsTokenIssuer = nil
	p.cfg.ExtraEnv = map[string]string{"pandora_ds_local_profile": "evil"}
	env, err := p.buildEnv()
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	if value := lastEnvValue(env, "PANDORA_DS_LOCAL_PROFILE"); value != "local-off-v1" {
		t.Fatalf("profile 被 extra_env 覆盖: %q", value)
	}
}

func lastEnvValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		parts := strings.SplitN(env[i], "=", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], key) {
			return parts[1]
		}
	}
	return ""
}
