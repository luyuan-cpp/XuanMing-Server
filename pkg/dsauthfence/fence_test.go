package dsauthfence

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

const testDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type fakeLease struct {
	lost chan struct{}
	once sync.Once
}

func newFakeLease() *fakeLease             { return &fakeLease{lost: make(chan struct{})} }
func (l *fakeLease) Lost() <-chan struct{} { return l.lost }
func (l *fakeLease) Close() error          { l.once.Do(func() { close(l.lost) }); return nil }

type fakeBackend struct {
	epoch               uint32
	found               bool
	err                 error
	watch               chan RequiredEvent
	lease               *fakeLease
	key                 string
	value               []byte
	requiredEpoch       uint32
	requiredModRevision int64
}

func (f *fakeBackend) GetRequired(context.Context, string) (uint32, int64, int64, bool, error) {
	return f.epoch, 7, 5, f.found, f.err
}
func (f *fakeBackend) AcquireCapability(_ context.Context, key, _, _ string, required uint32, modRevision int64, value []byte, _ int64) (Lease, error) {
	f.key, f.value = key, value
	f.requiredEpoch, f.requiredModRevision = required, modRevision
	if f.lease == nil {
		f.lease = newFakeLease()
	}
	return f.lease, nil
}
func (f *fakeBackend) WatchRequired(context.Context, string, int64) <-chan RequiredEvent {
	return f.watch
}
func (f *fakeBackend) Close() error { return nil }

func validConfig() Config {
	return Config{Endpoints: []string{"etcd:2379"}, Service: "hub_allocator", InstanceUID: "pod-uid", ImageDigest: testDigest, KeysetRevision: "r1", WriterEpoch: 2}
}

func TestStartRequiresLinearRequiredKey(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend *fakeBackend
	}{
		{name: "missing", backend: &fakeBackend{watch: make(chan RequiredEvent)}},
		{name: "read_failure", backend: &fakeBackend{err: errors.New("down"), watch: make(chan RequiredEvent)}},
		{name: "future", backend: &fakeBackend{epoch: 3, found: true, watch: make(chan RequiredEvent)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Start(context.Background(), tc.backend, validConfig()); err == nil {
				t.Fatal("expected fail-closed error")
			}
		})
	}
}

func TestStartFencesCapabilityWithExactRequiredRevision(t *testing.T) {
	backend := &fakeBackend{epoch: 1, found: true, watch: make(chan RequiredEvent)}
	holder, err := Start(context.Background(), backend, validConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	if backend.requiredEpoch != 1 || backend.requiredModRevision != 5 {
		t.Fatalf("capability compare epoch=%d mod_revision=%d", backend.requiredEpoch, backend.requiredModRevision)
	}
}

func TestRuntimeIdentityHasNoFallback(t *testing.T) {
	t.Setenv(EnvPodUID, "")
	t.Setenv(EnvImageDigest, "")
	_, err := AcquireRuntime(context.Background(), RuntimeConfig{
		Endpoints: []string{"etcd:2379"}, Service: "hub_allocator", KeysetRevision: "r1",
	})
	if err == nil || !strings.Contains(err.Error(), "instance uid") {
		t.Fatalf("expected missing downward-api identity error, got %v", err)
	}
}

func TestHolderMonotonicAndFailClosed(t *testing.T) {
	watch := make(chan RequiredEvent, 3)
	backend := &fakeBackend{epoch: 1, found: true, watch: watch}
	holder, err := Start(context.Background(), backend, validConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if holder.RequiredEpoch() != 1 {
		t.Fatalf("required=%d", holder.RequiredEpoch())
	}
	watch <- RequiredEvent{Epoch: 2, Revision: 8}
	deadline := time.Now().Add(time.Second)
	for holder.RequiredEpoch() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if holder.RequiredEpoch() != 2 {
		t.Fatal("epoch did not advance")
	}
	watch <- RequiredEvent{Epoch: 1, Revision: 9}
	select {
	case <-holder.Lost():
	case <-time.After(time.Second):
		t.Fatal("rollback did not fence")
	}
}

func TestLeaseLossAndWatchCloseFence(t *testing.T) {
	for _, leaseLoss := range []bool{true, false} {
		watch := make(chan RequiredEvent)
		lease := newFakeLease()
		backend := &fakeBackend{epoch: 1, found: true, watch: watch, lease: lease}
		holder, err := Start(context.Background(), backend, validConfig())
		if err != nil {
			t.Fatal(err)
		}
		if leaseLoss {
			lease.Close()
		} else {
			close(watch)
		}
		select {
		case <-holder.Lost():
		case <-time.After(time.Second):
			t.Fatal("loss did not fence")
		}
		_ = holder.Close()
	}
}

func TestCapabilityAuditExactSet(t *testing.T) {
	cap := Capability{Service: "hub_allocator", InstanceUID: "uid", WriterEpoch: 2, ImageDigest: testDigest, KeysetRevision: "r1", EtcdIdentityRevision: "r7", Features: []string{"hub-reservation-ledger-v1", "hub-heartbeat-capacity-v1"}}
	policy := AuditPolicy{RequiredServices: map[string]int{"hub_allocator": 1}, TargetEpoch: 2, KeysetRevision: "r1", AllowedDigests: map[string]struct{}{testDigest: {}}, ExpectedDigests: map[string]string{"hub_allocator": testDigest}}
	policy.RequiredInstances = map[string]map[string]struct{}{"hub_allocator": {"uid": {}}}
	policy.RequiredFeatures = map[string]map[string]struct{}{"hub_allocator": {"hub-reservation-ledger-v1": {}, "hub-heartbeat-capacity-v1": {}}}
	policy.EtcdIdentityRevision = "r7"
	if got := AuditCapabilities([]LiveCapability{{Capability: cap, LeaseID: 1, Key: "/pandora/ds-auth/capabilities/hub_allocator/uid"}}, policy); len(got) != 0 {
		t.Fatalf("unexpected findings: %v", got)
	}
	cap.WriterEpoch = 1
	policy.RequiredServices["hub_allocator"] = 2
	got := AuditCapabilities([]LiveCapability{{Capability: cap, LeaseID: 1, Key: "/pandora/ds-auth/capabilities/hub_allocator/uid"}}, policy)
	if strings.Join(got, " ") == "" {
		t.Fatal("expected findings")
	}
	cap.WriterEpoch = 2
	cap.Features = append(cap.Features, "not canonical")
	policy.RequiredServices["hub_allocator"] = 1
	got = AuditCapabilities([]LiveCapability{{Capability: cap, LeaseID: 1, Key: "/pandora/ds-auth/capabilities/hub_allocator/uid"}}, policy)
	if !strings.Contains(strings.Join(got, " "), "非 canonical") {
		t.Fatalf("malformed live capability feature accepted: %v", got)
	}
	cap.Features = []string{"hub-reservation-ledger-v1", "hub-heartbeat-capacity-v1", "unexpected-feature-v1"}
	got = AuditCapabilities([]LiveCapability{{Capability: cap, LeaseID: 1, Key: "/pandora/ds-auth/capabilities/hub_allocator/uid"}}, policy)
	if !strings.Contains(strings.Join(got, " "), "未批准") {
		t.Fatalf("extra live capability feature accepted: %v", got)
	}
	cap.Features = []string{"hub-reservation-ledger-v1", "hub-heartbeat-capacity-v1"}
	got = AuditCapabilities([]LiveCapability{{Capability: cap, LeaseID: 1, Key: "/wrong/capabilities/hub_allocator/uid"}}, policy)
	if !strings.Contains(strings.Join(got, " "), "key 与 payload") {
		t.Fatalf("capability outside the configured prefix accepted: %v", got)
	}
	cap.ImageDigest = "sha256:" + strings.Repeat("b", 64)
	policy.AllowedDigests[cap.ImageDigest] = struct{}{}
	got = AuditCapabilities([]LiveCapability{{Capability: cap, LeaseID: 1, Key: "/pandora/ds-auth/capabilities/hub_allocator/uid"}}, policy)
	if !strings.Contains(strings.Join(got, " "), "service expected") {
		t.Fatalf("digest from another allowed service accepted: %v", got)
	}
}

func TestCapabilityFeaturesAreValidatedWithoutMutatingCaller(t *testing.T) {
	features := []string{"z-feature-v1", "a-feature-v1"}
	cfg := validConfig()
	cfg.Features = features
	backend := &fakeBackend{epoch: 1, found: true, watch: make(chan RequiredEvent)}
	holder, err := Start(context.Background(), backend, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if got := strings.Join(features, ","); got != "z-feature-v1,a-feature-v1" {
		t.Fatalf("caller feature slice mutated: %s", got)
	}
	var capability Capability
	if err := json.Unmarshal(backend.value, &capability); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(capability.Features, ","); got != "z-feature-v1,a-feature-v1" {
		t.Fatalf("capability features=%s", got)
	}

	for _, invalid := range [][]string{{"bad feature"}, {"ok-feature-v1", "ok-feature-v1"}, {"UPPER-v1"}} {
		cfg := validConfig()
		cfg.Features = invalid
		if _, err := Start(context.Background(), &fakeBackend{epoch: 1, found: true, watch: make(chan RequiredEvent)}, cfg); err == nil {
			t.Fatalf("accepted invalid features %v", invalid)
		}
	}
}

func TestParseEpochAndExpectedServices(t *testing.T) {
	for _, raw := range []string{"", "0", "02", " 2", "2 ", "-1"} {
		if _, err := ParseEpoch([]byte(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if epoch, err := ParseEpoch([]byte("2")); err != nil || epoch != 2 {
		t.Fatalf("epoch=%d err=%v", epoch, err)
	}
	services, err := ParseExpectedServices("hub_allocator=2,player_locator=3")
	if err != nil || services["player_locator"] != 3 {
		t.Fatalf("services=%v err=%v", services, err)
	}
	instances, err := ParseExpectedInstances("hub_allocator=uid-a|uid-b,player_locator=uid-c")
	if err != nil || len(instances["hub_allocator"]) != 2 {
		t.Fatalf("instances=%v err=%v", instances, err)
	}
	features, err := ParseRequiredFeatures("hub_allocator=hub-reservation-ledger-v1|hub-heartbeat-capacity-v1,battle_result=battle-terminal-outbox-v1")
	if err != nil || len(features["hub_allocator"]) != 2 {
		t.Fatalf("features=%v err=%v", features, err)
	}
	for _, raw := range []string{
		"hub_allocator=bad feature",
		"hub_allocator=hub-reservation-ledger-v1|hub-reservation-ledger-v1",
		"hub_allocator=x,hub_allocator=y",
	} {
		if _, err := ParseRequiredFeatures(raw); err == nil {
			t.Fatalf("accepted invalid required features %q", raw)
		}
	}
	digests, err := ParseExpectedDigests("hub_allocator=" + testDigest + ",player_locator=" + testDigest)
	if err != nil || digests["hub_allocator"] != testDigest {
		t.Fatalf("digests=%v err=%v", digests, err)
	}
	for _, raw := range []string{"", "hub_allocator=latest", "hub_allocator=" + testDigest + ",hub_allocator=" + testDigest, "bad/name=" + testDigest} {
		if _, err := ParseExpectedDigests(raw); err == nil {
			t.Fatalf("accepted invalid expected digest %q", raw)
		}
	}
}
