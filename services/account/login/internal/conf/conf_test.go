package conf

import (
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
)

func TestRequireHubAssignmentBindingValidation(t *testing.T) {
	t.Run("default-compatible", func(t *testing.T) {
		var cfg Config
		cfg.Defaults()
		if cfg.Login.RequireHubAssignmentBinding {
			t.Fatal("default must remain false for rolling compatibility")
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate default: %v", err)
		}
	})

	t.Run("requires-redis", func(t *testing.T) {
		var cfg Config
		cfg.Login.RequireHubAssignmentBinding = true
		cfg.Login.Hub.Addr = "hub-allocator:50021"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected missing Redis validation error")
		}
	})

	t.Run("requires-hub-allocator", func(t *testing.T) {
		var cfg Config
		cfg.Login.RequireHubAssignmentBinding = true
		cfg.Node.RedisClient.Host = "redis:6379"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected missing hub allocator validation error")
		}
	})

	t.Run("valid", func(t *testing.T) {
		var cfg Config
		cfg.Login.RequireHubAssignmentBinding = true
		cfg.Node.RedisClient.Addrs = []string{"redis-0:6379", "redis-1:6379"}
		cfg.Login.Hub.Addr = "hub-allocator:50021"
		cfg.Login.Locator.Addr = "player-locator:50006"
		cfg.Login.HubAssignmentFence.EtcdEndpoints = []string{"etcd:2379"}
		cfg.Login.HubAssignmentFence.KeysetRevision = "pandora-auth-r1"
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("requires-player-locator", func(t *testing.T) {
		var cfg Config
		cfg.Login.RequireHubAssignmentBinding = true
		cfg.Node.RedisClient.Host = "redis:6379"
		cfg.Login.Hub.Addr = "hub-allocator:50021"
		cfg.Login.HubAssignmentFence.EtcdEndpoints = []string{"etcd:2379"}
		cfg.Login.HubAssignmentFence.KeysetRevision = "pandora-auth-r1"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected missing player locator validation error")
		}
	})
}

func TestRedisDSAdmissionRequiresSingleConsistentFence(t *testing.T) {
	valid := func() Config {
		var cfg Config
		cfg.Defaults()
		cfg.Node.RedisClient.Host = "redis:6379"
		cfg.Login.Hub.Addr = "hub-allocator:50021"
		cfg.Login.Locator.Addr = "player-locator:50006"
		cfg.Login.RequireHubAssignmentBinding = true
		fence := config.DSAuthFenceConf{
			EtcdEndpoints: []string{"etcd:2379"}, EtcdPrefix: "/pandora/ds-auth/",
			EtcdLeaseTTLSec: 15, EtcdDialTimeout: config.Duration(5 * time.Second),
			KeysetRevision: "pandora-auth-r1",
		}
		cfg.Login.HubAssignmentFence = fence
		cfg.DSAuth.Mode = "enforce"
		cfg.DSAuth.AuthorityMode = "redis"
		cfg.DSAuth.Fence = fence
		return cfg
	}

	t.Run("valid-and-one-capability", func(t *testing.T) {
		cfg := valid()
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		fence, enabled := cfg.CapabilityFence()
		if !enabled || fence.KeysetRevision != "pandora-auth-r1" {
			t.Fatalf("fence=%+v enabled=%v", fence, enabled)
		}
	})

	t.Run("fence-mismatch", func(t *testing.T) {
		cfg := valid()
		cfg.Login.HubAssignmentFence.KeysetRevision = "other"
		if err := cfg.Validate(); err == nil {
			t.Fatal("expected inconsistent fence rejection")
		}
	})

	t.Run("binding-missing", func(t *testing.T) {
		cfg := valid()
		cfg.Login.RequireHubAssignmentBinding = false
		if err := cfg.Validate(); err == nil {
			t.Fatal("redis admission must require hub assignment binding")
		}
	})

	t.Run("redis-permissive", func(t *testing.T) {
		cfg := valid()
		cfg.DSAuth.Mode = "permissive"
		if err := cfg.Validate(); err == nil {
			t.Fatal("redis admission must require enforce")
		}
	})
}
