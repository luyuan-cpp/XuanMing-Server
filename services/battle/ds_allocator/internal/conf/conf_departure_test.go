package conf

import "testing"

func TestValidateBattleDepartureConfig(t *testing.T) {
	t.Run("production requires locator", func(t *testing.T) {
		t.Setenv("PANDORA_PLACEMENT_BATTLE_DEPARTURE_SECRET", "")
		cfg := Config{Mode: ModeAgones}
		cfg.DSAuth.AuthorityMode = "redis"
		if err := cfg.ValidateBattleDepartureConfig(); err == nil {
			t.Fatal("production without locator must fail closed")
		}
	})

	t.Run("locator requires independent strong key", func(t *testing.T) {
		t.Setenv("PANDORA_PLACEMENT_BATTLE_DEPARTURE_SECRET", "")
		cfg := Config{LocatorAddr: "player-locator:50006"}
		cfg.Allocator.PlacementBattleDepartureProofSecret = "short"
		if err := cfg.ValidateBattleDepartureConfig(); err == nil {
			t.Fatal("short departure key must be rejected")
		}
	})

	t.Run("environment key overrides yaml", func(t *testing.T) {
		t.Setenv("PANDORA_PLACEMENT_BATTLE_DEPARTURE_SECRET",
			"battle-departure-env-key-is-at-least-32-bytes")
		cfg := Config{LocatorAddr: "player-locator:50006"}
		cfg.Allocator.PlacementBattleDepartureProofSecret = "short"
		if err := cfg.ValidateBattleDepartureConfig(); err != nil {
			t.Fatalf("valid env key rejected: %v", err)
		}
	})
}

func TestValidateAllocationAbortAuthConfig(t *testing.T) {
	t.Setenv("PANDORA_PLACEMENT_BATTLE_DEPARTURE_SECRET", "")
	base := Config{}
	base.DSAuth.AuthorityMode = "redis"
	base.DSAuth.Secret = "ds-callback-key-is-at-least-32-bytes-long"
	base.Allocator.PlacementBattleDepartureProofSecret = "battle-departure-key-is-at-least-32-bytes"
	base.Allocator.AllocationAbortAuthSecret = "04b47e36-a832-4529-87b0-e0407b6225ef"
	base.Allocator.AllocationAbortAuthAudience = "ds-allocator:battle-allocation-abort"
	if err := base.ValidateAllocationAbortAuthConfig(); err != nil {
		t.Fatalf("valid dedicated auth rejected: %v", err)
	}
	for name, reused := range map[string]string{
		"ds-callback":      base.DSAuth.Secret,
		"battle-departure": base.Allocator.PlacementBattleDepartureProofSecret,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			cfg.Allocator.AllocationAbortAuthSecret = reused
			if err := cfg.ValidateAllocationAbortAuthConfig(); err == nil {
				t.Fatalf("allocation abort key reused %s trust domain", name)
			}
		})
	}
	cfg := base
	cfg.Allocator.AllocationAbortAuthAudience = ""
	if err := cfg.ValidateAllocationAbortAuthConfig(); err == nil {
		t.Fatal("empty allocation abort audience accepted")
	}
}
