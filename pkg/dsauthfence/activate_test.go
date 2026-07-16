package dsauthfence

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	etcdserverpb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

func TestBuildAdvanceComparesBindsRequiredModRevision(t *testing.T) {
	key := requiredKey(DefaultPrefix)
	compares, err := buildAdvanceCompares(
		DefaultPrefix, "lock-token", key, DefaultPrefix+"activations/2", "1", 41,
		[]LiveCapability{{Key: DefaultPrefix + "capabilities/login/uid-a", ModRevision: 73}},
	)
	if err != nil {
		t.Fatal(err)
	}
	foundRequiredMod := false
	foundRequiredValue := false
	foundCapabilityMod := false
	for i := range compares {
		cmp := &compares[i]
		wire := (*etcdserverpb.Compare)(cmp)
		switch string(cmp.KeyBytes()) {
		case key:
			if wire.GetModRevision() == 41 {
				foundRequiredMod = true
			}
			if string(wire.GetValue()) == "1" {
				foundRequiredValue = true
			}
		case DefaultPrefix + "capabilities/login/uid-a":
			if wire.GetModRevision() == 73 {
				foundCapabilityMod = true
			}
		}
	}
	if !foundRequiredMod || !foundRequiredValue || !foundCapabilityMod {
		t.Fatalf("required/capability mod revision compare missing: %+v", compares)
	}
	if _, err := buildAdvanceCompares(DefaultPrefix, "lock-token", key,
		DefaultPrefix+"activations/2", "1", 0, nil); err == nil {
		t.Fatal("zero required mod revision accepted")
	}
}

func TestBuildZeroWriterPolicyAdvanceComparesBindsEmptyCapabilityRange(t *testing.T) {
	key := requiredKey(DefaultPrefix)
	recordKey := policyActivationRecordKey(DefaultPrefix, RequiredPolicyGenerationV3, RequiredPolicyV3)
	compares, err := buildZeroWriterPolicyAdvanceCompares(DefaultPrefix, "lock-token", key, recordKey, 41)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := capabilityPrefix(DefaultPrefix)
	wantEnd := clientPrefixEnd(wantPrefix)
	foundRange := false
	foundRaw := false
	foundRevision := false
	for i := range compares {
		cmp := &compares[i]
		wire := (*etcdserverpb.Compare)(cmp)
		if string(cmp.KeyBytes()) == wantPrefix && string(wire.GetRangeEnd()) == wantEnd &&
			wire.GetCreateRevision() == 0 {
			foundRange = true
		}
		if string(cmp.KeyBytes()) == key && string(wire.GetValue()) == "1" {
			foundRaw = true
		}
		if string(cmp.KeyBytes()) == key && wire.GetModRevision() == 41 {
			foundRevision = true
		}
	}
	if !foundRange || !foundRaw || !foundRevision {
		t.Fatalf("zero-writer CAS does not bind raw/revision/empty prefix: %+v", compares)
	}
	if _, err := buildZeroWriterPolicyAdvanceCompares(DefaultPrefix, "", key, recordKey, 41); err == nil {
		t.Fatal("zero-writer CAS accepted missing activation lock token")
	}
}

func TestBuildMissingV3GenesisComparesRequiredRecordLockAndEmptyCapabilities(t *testing.T) {
	key := requiredKey(DefaultPrefix)
	recordKey := policyActivationRecordKey(DefaultPrefix, RequiredPolicyGenerationV3, RequiredPolicyV3)
	compares, err := buildMissingZeroWriterPolicyBootstrapCompares(DefaultPrefix, "lock-token", key, recordKey)
	if err != nil {
		t.Fatal(err)
	}
	foundRequiredMissing, foundRecordMissing, foundLock, foundEmptyRange := false, false, false, false
	for i := range compares {
		cmp := &compares[i]
		wire := (*etcdserverpb.Compare)(cmp)
		switch string(cmp.KeyBytes()) {
		case key:
			foundRequiredMissing = wire.GetCreateRevision() == 0
		case recordKey:
			foundRecordMissing = wire.GetCreateRevision() == 0
		case activationLockKey(DefaultPrefix):
			foundLock = string(wire.GetValue()) == "lock-token"
		case capabilityPrefix(DefaultPrefix):
			foundEmptyRange = wire.GetCreateRevision() == 0 &&
				string(wire.GetRangeEnd()) == clientPrefixEnd(capabilityPrefix(DefaultPrefix))
		}
	}
	if !foundRequiredMissing || !foundRecordMissing || !foundLock || !foundEmptyRange {
		t.Fatalf("genesis CAS missing a create-only/lock/empty-range comparison: %+v", compares)
	}
}

func clientPrefixEnd(prefix string) string {
	// Mirrors clientv3.GetPrefixRangeEnd without hiding the expected bytes in
	// the same production helper under test.
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return "\x00"
}

func TestV3AuditedPolicyRequiresCompiledIdentityForEveryWriter(t *testing.T) {
	services := make(map[string]int, len(requiredPolicyV3Features))
	audited := make([]LiveCapability, 0, len(requiredPolicyV3Features))
	for service, features := range requiredPolicyV3Features {
		services[service] = 1
		uid := service + "-uid"
		audited = append(audited, LiveCapability{Capability: Capability{
			Service: service, InstanceUID: uid, WriterEpoch: ProtocolEpochV2,
			SupportedPolicyGeneration: RequiredPolicyGenerationV3, SupportedPolicyID: RequiredPolicyV3,
			AcquiredPolicyGeneration: RequiredPolicyGenerationV2, AcquiredPolicyID: RequiredPolicyV2,
			Features: append([]string(nil), features...),
		}, Key: capabilityKey(DefaultPrefix, service, uid), LeaseID: 1, ModRevision: 9})
	}
	if err := validateAuditedPolicyGeneration(RequiredPolicyGenerationV3, services, audited); err != nil {
		t.Fatalf("exact V3 writer set rejected: %v", err)
	}
	for i := range audited {
		if audited[i].Capability.Service == "login" {
			audited[i].Capability.SupportedPolicyGeneration = 0
			audited[i].Capability.SupportedPolicyID = ""
			break
		}
	}
	err := validateAuditedPolicyGeneration(RequiredPolicyGenerationV3, services, audited)
	if err == nil || !strings.Contains(err.Error(), "exact target policy") {
		t.Fatalf("old non-Hub writer passed V3 audit: %v", err)
	}
}

func TestCoreAdvanceCannotBypassMissingTopologyProvider(t *testing.T) {
	lock := &ActivationLock{}
	err := lock.AdvanceRequired(nil, 1, 2, 1, nil, nil,
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 1)
	if !errors.Is(err, ErrTopologyChangeLockProviderUnavailable) {
		t.Fatalf("AdvanceRequired() error=%v", err)
	}
}

func TestActivationEvidenceDigestAndRecordAreFailClosed(t *testing.T) {
	valid := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := validateActivationEvidenceSHA256(valid); err != nil {
		t.Fatalf("valid evidence rejected: %v", err)
	}
	for _, bad := range []string{"", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "sha256:AAAA"} {
		if err := validateActivationEvidenceSHA256(bad); err == nil {
			t.Fatalf("invalid evidence accepted: %q", bad)
		}
	}

	legacy := []byte(`{"from":1,"to":2,"from_mod_revision":41,"expected_services_hash":"x","activated_at_ms":42}`)
	var record ActivationRecord
	if err := json.Unmarshal(legacy, &record); err != nil {
		t.Fatal(err)
	}
	if err := validateActivationEvidenceSHA256(record.ActivationEvidenceSHA256); err == nil {
		t.Fatal("legacy epoch-2 activation record without evidence was accepted")
	}
	if record.ActivationEvidenceCompletedAtMS != 0 {
		t.Fatal("legacy activation record unexpectedly has an evidence completion time")
	}
	validRecord := []byte(`{"from":1,"to":2,"from_required_value":"1","to_required_value":"2@ds-auth-v2-pod-uid-write-invariant-v1","required_policy_id":"ds-auth-v2-pod-uid-write-invariant-v1","from_mod_revision":41,"expected_services_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","activation_evidence_sha256":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","activation_evidence_completed_at_ms":42,"activated_at_ms":43}`)
	if _, err := decodeActivationRecord(validRecord); err != nil {
		t.Fatalf("canonical activation record rejected: %v", err)
	}
	withUnknown := append(validRecord[:len(validRecord)-1], []byte(`,"smuggled":true}`)...)
	if _, err := decodeActivationRecord(withUnknown); err == nil {
		t.Fatal("activation record with unknown field accepted")
	}
	withTrailing := append(append([]byte{}, validRecord...), []byte(` {}`)...)
	if _, err := decodeActivationRecord(withTrailing); err == nil {
		t.Fatal("activation record with trailing JSON accepted")
	}
	for _, hash := range []string{"", "Aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "not-hexaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if isCanonicalLowerHexSHA256(hash) {
			t.Fatalf("invalid expected-services hash accepted: %q", hash)
		}
	}
}

func TestActivationEvidenceInputKeepsEpoch1ReadOnlyAndFencesEpoch2(t *testing.T) {
	valid := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := ValidateActivationEvidenceInput(1, 2, "", 0, false); err != nil {
		t.Fatalf("legacy epoch-1 read-only audit rejected: %v", err)
	}
	if err := ValidateActivationEvidenceInput(2, 2, "", 0, false); err == nil {
		t.Fatal("epoch-2 audit without evidence accepted")
	}
	if err := ValidateActivationEvidenceInput(1, 2, "", 0, true); err == nil {
		t.Fatal("epoch advance without evidence accepted")
	}
	if err := ValidateActivationEvidenceInput(2, 2, valid, 123, false); err != nil {
		t.Fatalf("complete epoch-2 evidence rejected: %v", err)
	}
}
