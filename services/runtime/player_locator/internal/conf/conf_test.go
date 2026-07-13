package conf

import "testing"

func TestDefaultsDSAuthorityMode(t *testing.T) {
	var cfg Config
	cfg.Defaults()
	if cfg.DSAuth.AuthorityMode != "legacy" {
		t.Fatalf("authority_mode default=%q want legacy", cfg.DSAuth.AuthorityMode)
	}

	cfg.DSAuth.AuthorityMode = "redis"
	cfg.Defaults()
	if cfg.DSAuth.AuthorityMode != "redis" {
		t.Fatalf("explicit authority_mode must be preserved, got %q", cfg.DSAuth.AuthorityMode)
	}
	if err := cfg.ValidateDSAuthAuthorityMode(); err != nil {
		t.Fatalf("redis must validate: %v", err)
	}

	cfg.DSAuth.AuthorityMode = "redsi"
	if err := cfg.ValidateDSAuthAuthorityMode(); err == nil {
		t.Fatal("unknown authority_mode must fail instead of silently disabling the checker")
	}
}
