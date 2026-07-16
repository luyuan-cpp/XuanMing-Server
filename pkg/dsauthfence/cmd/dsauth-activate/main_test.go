package main

import (
	"errors"
	"testing"

	"github.com/luyuancpp/pandora/pkg/dsauthfence"
)

func TestDirectApplyIsBlockedWithoutTopologyProvider(t *testing.T) {
	if err := validateApplyMode(false, false); err != nil {
		t.Fatalf("read-only audit rejected: %v", err)
	}
	if err := validateApplyMode(true, true); err != nil {
		t.Fatalf("baseline bootstrap rejected: %v", err)
	}
	if err := validateApplyMode(false, true); !errors.Is(err, dsauthfence.ErrTopologyChangeLockProviderUnavailable) {
		t.Fatalf("direct target CAS error=%v", err)
	}
}
