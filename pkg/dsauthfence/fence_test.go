package dsauthfence

import (
	"context"
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
	cap := Capability{Service: "hub_allocator", InstanceUID: "uid", WriterEpoch: 2, ImageDigest: testDigest, KeysetRevision: "r1"}
	policy := AuditPolicy{RequiredServices: map[string]int{"hub_allocator": 1}, TargetEpoch: 2, KeysetRevision: "r1", AllowedDigests: map[string]struct{}{testDigest: {}}}
	policy.RequiredInstances = map[string]map[string]struct{}{"hub_allocator": {"uid": {}}}
	if got := AuditCapabilities([]LiveCapability{{Capability: cap, LeaseID: 1, Key: "/pandora/ds-auth/capabilities/hub_allocator/uid"}}, policy); len(got) != 0 {
		t.Fatalf("unexpected findings: %v", got)
	}
	cap.WriterEpoch = 1
	policy.RequiredServices["hub_allocator"] = 2
	got := AuditCapabilities([]LiveCapability{{Capability: cap, LeaseID: 1, Key: "/pandora/ds-auth/capabilities/hub_allocator/uid"}}, policy)
	if strings.Join(got, " ") == "" {
		t.Fatal("expected findings")
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
}
