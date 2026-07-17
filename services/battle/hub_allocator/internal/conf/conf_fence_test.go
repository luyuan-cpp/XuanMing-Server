package conf

import (
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/placement"
)

// TestHeartbeatTimeoutFenceBarrierFloor 锁住脑裂再入屏障机械下限(2026-07-16,
// pkg/placement 契约):AssignHub 只有在分片心跳超过 HeartbeatTimeout 后才会把
// 玩家改派到新分片,该窗口低于「DS 授权租约上限 + 偏差余量」时,分区的旧 Hub
// 还没对存量玩家自我 fencing 就会产生第二个 Hub 归属(一人两 DS)。
func TestHeartbeatTimeoutFenceBarrierFloor(t *testing.T) {
	t.Run("低于屏障的配置被机械抬回", func(t *testing.T) {
		c := &Config{}
		c.Hub.HeartbeatTimeout = config.Duration(10 * time.Second)
		c.Defaults()
		if c.Hub.HeartbeatTimeout.Std() != placement.DSFenceReentryBarrier {
			t.Fatalf("heartbeat_timeout below fence barrier must be raised to %v, got %v",
				placement.DSFenceReentryBarrier, c.Hub.HeartbeatTimeout.Std())
		}
	})

	t.Run("默认值 30s 不低于屏障且保持不变", func(t *testing.T) {
		c := &Config{}
		c.Defaults()
		if got := c.Hub.HeartbeatTimeout.Std(); got != 30*time.Second {
			t.Fatalf("default heartbeat_timeout should stay 30s, got %v", got)
		}
		if c.Hub.HeartbeatTimeout.Std() < placement.DSFenceReentryBarrier {
			t.Fatalf("default heartbeat_timeout %v violates fence barrier %v",
				c.Hub.HeartbeatTimeout.Std(), placement.DSFenceReentryBarrier)
		}
	})

	t.Run("高于屏障的显式配置保持原值", func(t *testing.T) {
		c := &Config{}
		c.Hub.HeartbeatTimeout = config.Duration(45 * time.Second)
		c.Defaults()
		if got := c.Hub.HeartbeatTimeout.Std(); got != 45*time.Second {
			t.Fatalf("explicit heartbeat_timeout=45s expected, got %v", got)
		}
	})
}
