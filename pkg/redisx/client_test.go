package redisx

import (
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"

	"github.com/luyuancpp/pandora/pkg/config"
)

func TestResolveMaintMode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want maintnotifications.Mode
	}{
		{"empty falls back to disabled", "", maintnotifications.ModeDisabled},
		{"explicit disabled", "disabled", maintnotifications.ModeDisabled},
		{"explicit auto", "auto", maintnotifications.ModeAuto},
		{"explicit enabled", "enabled", maintnotifications.ModeEnabled},
		{"invalid falls back to disabled", "garbage", maintnotifications.ModeDisabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveMaintMode(tc.in); got != tc.want {
				t.Fatalf("resolveMaintMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDefaultMaintModeIsDisabled(t *testing.T) {
	if DefaultMaintNotificationsMode != maintnotifications.ModeDisabled {
		t.Fatalf("default maint mode = %q, want disabled (自建 Redis 默认应关闭探测)", DefaultMaintNotificationsMode)
	}
}

func TestDeadlineUniversalClientEnablesContextTimeoutOnlyWhenRequested(t *testing.T) {
	cfg := config.RedisConf{Host: "127.0.0.1:6379"}

	legacy := NewUniversalClient(cfg)
	t.Cleanup(func() { _ = legacy.Close() })
	legacyClient, ok := legacy.(*redis.Client)
	if !ok {
		t.Fatalf("single endpoint client type = %T, want *redis.Client", legacy)
	}
	if legacyClient.Options().ContextTimeoutEnabled {
		t.Fatal("既有构造不应被本次修复全局改成 context timeout")
	}

	deadlineAware := NewDeadlineUniversalClient(cfg)
	t.Cleanup(func() { _ = deadlineAware.Close() })
	deadlineClient, ok := deadlineAware.(*redis.Client)
	if !ok {
		t.Fatalf("deadline client type = %T, want *redis.Client", deadlineAware)
	}
	if !deadlineClient.Options().ContextTimeoutEnabled {
		t.Fatal("严格截止构造必须启用 ContextTimeoutEnabled")
	}
}

func TestUniversalClientWithCredentialsReplacesWriterCredential(t *testing.T) {
	cfg := config.RedisConf{Host: "127.0.0.1:6379", Password: "writer-password-must-not-be-used"}
	client := NewUniversalClientWithCredentials(cfg, "dedicated-read-only", "read-only-password")
	t.Cleanup(func() { _ = client.Close() })
	single, ok := client.(*redis.Client)
	if !ok {
		t.Fatalf("client type = %T, want *redis.Client", client)
	}
	if single.Options().Username != "dedicated-read-only" ||
		single.Options().Password != "read-only-password" ||
		single.Options().Password == cfg.Password {
		t.Fatalf("dedicated credential was not an unconditional replacement")
	}
}
