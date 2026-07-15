package conf

import (
	"os"
	"path/filepath"
	"testing"

	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"

	"github.com/luyuancpp/pandora/pkg/kafkax"
)

func TestValidateRedisAuthorityIngressRejectsLegacyBattleResultTopic(t *testing.T) {
	cfg := Config{}
	cfg.Defaults()
	cfg.DSAuth.AuthorityMode = "redis"
	if err := cfg.ValidateRedisAuthorityIngress(); err == nil {
		t.Fatal("Redis authority must reject unauthenticated pandora.battle.result consumer")
	}

	cfg.Battle.ConsumeTopics = []string{kafkax.TopicDSLifecycle}
	cfg.Battle.DSAllocatorAddr = "ds-allocator:50020"
	cfg.Battle.LocatorAddr = "player-locator:50006"
	if err := cfg.ValidateRedisAuthorityIngress(); err != nil {
		t.Fatalf("lifecycle-only config: %v", err)
	}

	cfg.Battle.DSAllocatorAddr = ""
	if err := cfg.ValidateRedisAuthorityIngress(); err == nil {
		t.Fatal("Redis authority accepted missing terminal release relay")
	}
}

func TestValidateRedisAuthorityIngressKeepsLegacyProfile(t *testing.T) {
	cfg := Config{}
	cfg.Defaults()
	if err := cfg.ValidateRedisAuthorityIngress(); err != nil {
		t.Fatalf("legacy/off profile remains compatible: %v", err)
	}
}

func TestProductionExampleIsModelBOnly(t *testing.T) {
	examplePath := filepath.Join("..", "..", "etc", "battle_result-prod.yaml.example")
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "battle_result-prod.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c := kconfig.New(kconfig.WithSource(file.NewSource(path)))
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Load(); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := c.Scan(&cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Defaults()
	if cfg.DSAuth.Mode != "enforce" || !cfg.DSAuth.AuthorityModeRedis() {
		t.Fatalf("prod DS authority mode=%q authority=%q", cfg.DSAuth.Mode, cfg.DSAuth.AuthorityMode)
	}
	if len(cfg.Battle.ConsumeTopics) != 1 || cfg.Battle.ConsumeTopics[0] != kafkax.TopicDSLifecycle {
		t.Fatalf("prod consume topics=%v", cfg.Battle.ConsumeTopics)
	}
	if cfg.Node.RedisClient.Host == "" || cfg.Battle.DSAllocatorAddr == "" || cfg.Battle.LocatorAddr == "" {
		t.Fatal("prod Model-B Redis/terminal relay dependency missing")
	}
	if err := cfg.DSAuth.ValidateRedisFence(); err != nil {
		t.Fatalf("prod fence: %v", err)
	}
	if err := cfg.ValidateRedisAuthorityIngress(); err != nil {
		t.Fatalf("prod ingress: %v", err)
	}
}
