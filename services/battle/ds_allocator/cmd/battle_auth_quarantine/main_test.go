package main

import (
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestQuarantineCommandAlwaysConstructsStrictModelBWriter(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	repo := newQuarantineRepo(client)
	if !repo.StrictModelBWritesEnabled() {
		t.Fatal("standalone quarantine writer was constructed without the irreversible Model-B storage gate")
	}
}
