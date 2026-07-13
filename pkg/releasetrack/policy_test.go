package releasetrack

import "testing"

func TestPolicyDeterministicAndBounded(t *testing.T) {
	if _, err := New(101, "seed"); err == nil {
		t.Fatal("percent >100 must fail")
	}
	if _, err := New(1, ""); err == nil {
		t.Fatal("enabled canary without seed must fail")
	}
	stable, err := New(0, "")
	if err != nil || stable.Select(42) != Stable {
		t.Fatalf("zero canary policy: track=%s err=%v", stable.Select(42), err)
	}
	canary, err := New(100, "release-2026-07")
	if err != nil || canary.Select(42) != Canary {
		t.Fatalf("full canary policy: track=%s err=%v", canary.Select(42), err)
	}
	p, err := New(37, "release-2026-07")
	if err != nil {
		t.Fatal(err)
	}
	for id := uint64(1); id <= 1000; id++ {
		first := p.Select(id)
		if !Valid(first) || p.Select(id) != first {
			t.Fatalf("non-deterministic/invalid track id=%d track=%q", id, first)
		}
	}
}
