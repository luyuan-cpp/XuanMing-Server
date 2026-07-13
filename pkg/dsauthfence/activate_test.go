package dsauthfence

import (
	"testing"

	etcdserverpb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

func TestBuildAdvanceComparesBindsRequiredModRevision(t *testing.T) {
	key := requiredKey(DefaultPrefix)
	compares, err := buildAdvanceCompares(
		DefaultPrefix, "lock-token", key, DefaultPrefix+"activations/2", 1, 41,
		[]LiveCapability{{Key: DefaultPrefix + "capabilities/login/uid-a", ModRevision: 73}},
	)
	if err != nil {
		t.Fatal(err)
	}
	foundRequiredMod := false
	foundCapabilityMod := false
	for i := range compares {
		cmp := &compares[i]
		wire := (*etcdserverpb.Compare)(cmp)
		switch string(cmp.KeyBytes()) {
		case key:
			if wire.GetModRevision() == 41 {
				foundRequiredMod = true
			}
		case DefaultPrefix + "capabilities/login/uid-a":
			if wire.GetModRevision() == 73 {
				foundCapabilityMod = true
			}
		}
	}
	if !foundRequiredMod || !foundCapabilityMod {
		t.Fatalf("required/capability mod revision compare missing: %+v", compares)
	}
	if _, err := buildAdvanceCompares(DefaultPrefix, "lock-token", key,
		DefaultPrefix+"activations/2", 1, 0, nil); err == nil {
		t.Fatal("zero required mod revision accepted")
	}
}
