package conf

import (
	"testing"

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
	if err := cfg.ValidateRedisAuthorityIngress(); err != nil {
		t.Fatalf("lifecycle-only config: %v", err)
	}
}

func TestValidateRedisAuthorityIngressKeepsLegacyProfile(t *testing.T) {
	cfg := Config{}
	cfg.Defaults()
	if err := cfg.ValidateRedisAuthorityIngress(); err != nil {
		t.Fatalf("legacy/off profile remains compatible: %v", err)
	}
}
