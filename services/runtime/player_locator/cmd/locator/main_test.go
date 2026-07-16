package main

import (
	"strings"
	"testing"
)

func validPlacementAuthoritySecrets() map[string]string {
	return map[string]string{
		"account-bootstrap": strings.Repeat("a", 32),
		"match-start":       strings.Repeat("b", 32),
		"battle-exit":       strings.Repeat("c", 32),
		"hub-transfer":      strings.Repeat("d", 32),
		"battle-departure":  strings.Repeat("e", 32),
	}
}

func TestValidatePlacementAuthoritySecretsAcceptsIndependentKeys(t *testing.T) {
	if err := validatePlacementAuthoritySecrets(validPlacementAuthoritySecrets(), strings.Repeat("f", 32)); err != nil {
		t.Fatalf("validatePlacementAuthoritySecrets() error = %v", err)
	}
}

func TestValidatePlacementAuthoritySecretsRejectsMissingOrShortKey(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string){
		"missing": func(keys map[string]string) { delete(keys, "match-start") },
		"blank":   func(keys map[string]string) { keys["match-start"] = "  " },
		"short":   func(keys map[string]string) { keys["match-start"] = strings.Repeat("x", 31) },
	} {
		t.Run(name, func(t *testing.T) {
			keys := validPlacementAuthoritySecrets()
			mutate(keys)
			if err := validatePlacementAuthoritySecrets(keys, ""); err == nil {
				t.Fatal("validatePlacementAuthoritySecrets() error = nil, want rejection")
			}
		})
	}
}

func TestValidatePlacementAuthoritySecretsRejectsSharedAuthorityKey(t *testing.T) {
	keys := validPlacementAuthoritySecrets()
	keys["battle-departure"] = keys["hub-transfer"]
	if err := validatePlacementAuthoritySecrets(keys, ""); err == nil || !strings.Contains(err.Error(), "reuse one key") {
		t.Fatalf("validatePlacementAuthoritySecrets() error = %v, want shared-key rejection", err)
	}
}

func TestValidatePlacementAuthoritySecretsRejectsDSCallbackKeyReuse(t *testing.T) {
	keys := validPlacementAuthoritySecrets()
	if err := validatePlacementAuthoritySecrets(keys, keys["battle-exit"]); err == nil || !strings.Contains(err.Error(), "DS callback key") {
		t.Fatalf("validatePlacementAuthoritySecrets() error = %v, want DS callback reuse rejection", err)
	}
}
