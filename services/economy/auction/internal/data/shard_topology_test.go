package data

import (
	"strings"
	"testing"
)

func TestBuildExpectedShardTopologyRejectsDuplicateLogicalDatabase(t *testing.T) {
	_, err := buildExpectedShardTopology("auction-v1", []string{
		"user-a:secret-a@tcp(127.0.0.1:3306)/pandora_auction?parseTime=true",
		"user-b:secret-b@tcp(127.0.0.1:3306)/pandora_auction?loc=UTC",
	})
	if err == nil || !strings.Contains(err.Error(), "same logical database identity") {
		t.Fatalf("duplicate logical shard error=%v", err)
	}
}

func TestBuildExpectedShardTopologyDoesNotHashCredentials(t *testing.T) {
	a, err := buildExpectedShardTopology("auction-v1", []string{
		"user-a:secret-a@tcp(127.0.0.1:3306)/pandora_auction?parseTime=true",
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := buildExpectedShardTopology("auction-v1", []string{
		"user-b:secret-b@tcp(127.0.0.1:3306)/pandora_auction?loc=UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.topologyHash != b.topologyHash || a.identities[0] != b.identities[0] {
		t.Fatal("credential rotation must not look like logical shard remapping")
	}
}
