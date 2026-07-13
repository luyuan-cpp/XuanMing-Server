package conf

import "testing"

func TestDefaults_BoundsWeakAuditQueue(t *testing.T) {
	var c Config
	c.Defaults()
	if c.Auction.AuditQueueCapacity != 1024 {
		t.Fatalf("audit queue capacity = %d, want 1024", c.Auction.AuditQueueCapacity)
	}
}
