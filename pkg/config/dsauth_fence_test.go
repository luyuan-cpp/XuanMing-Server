package config

import "testing"

func TestDSAuthRedisFenceValidation(t *testing.T) {
	var cfg DSAuthConf
	cfg.Defaults()
	if cfg.AuthorityMode != "legacy" {
		t.Fatalf("default authority=%q", cfg.AuthorityMode)
	}
	if err := cfg.ValidateRedisFence(); err != nil {
		t.Fatalf("legacy must not require fence: %v", err)
	}
	cfg.AuthorityMode = "redis"
	if err := cfg.ValidateRedisFence(); err == nil {
		t.Fatal("redis without enforce/fence must fail closed")
	}
	cfg.Mode = "enforce"
	cfg.Fence.EtcdEndpoints = []string{"etcd:2379"}
	cfg.Fence.KeysetRevision = "pandora-ds-auth-v2-r1"
	if err := cfg.ValidateRedisFence(); err != nil {
		t.Fatalf("complete redis fence rejected: %v", err)
	}
}
