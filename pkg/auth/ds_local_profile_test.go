package auth

import (
	"testing"
	"time"
)

func TestValidateDSLocalProfileOffV1(t *testing.T) {
	if err := ValidateDSLocalProfileOffV1("off", "legacy", true); err != nil {
		t.Fatalf("valid profile: %v", err)
	}
	for _, tc := range []struct {
		name, guard, authority string
		signer                 bool
	}{
		{name: "permissive", guard: "permissive", authority: "legacy", signer: true},
		{name: "enforce", guard: "enforce", authority: "legacy", signer: true},
		{name: "redis", guard: "off", authority: "redis", signer: true},
		{name: "missing signer", guard: "off", authority: "legacy", signer: false},
		{name: "future authority", guard: "off", authority: "future", signer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateDSLocalProfileOffV1(tc.guard, tc.authority, tc.signer); err == nil {
				t.Fatal("unsafe local profile should be rejected")
			}
		})
	}
}

func TestValidateDSLocalHubProfileOffV1RequiresSessionTTL(t *testing.T) {
	if err := ValidateDSLocalHubProfileOffV1("off", "legacy", true, 12*time.Hour); err != nil {
		t.Fatalf("12h local hub profile: %v", err)
	}
	if err := ValidateDSLocalHubProfileOffV1("off", "legacy", true, time.Hour); err == nil {
		t.Fatal("one-shot local Hub credential shorter than 12h must fail startup")
	}
	if err := ValidateDSLocalHubProfileOffV1("enforce", "legacy", true, 24*time.Hour); err == nil {
		t.Fatal("Hub TTL must not bypass the base profile gate")
	}
}
