package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/dsauthfence/redisaudit"
)

func main() {
	addrsRaw := flag.String("addrs", "", "Redis address(es), comma separated")
	passwordEnv := flag.String("password-env", "PANDORA_REDIS_PASSWORD", "environment variable containing Redis password")
	db := flag.Int("db", 0, "Redis database (cluster must use 0)")
	cycles := flag.Int("cycles", 3, "consecutive stable heartbeat samples")
	interval := flag.Duration("interval", 6*time.Second, "sample interval")
	maxAge := flag.Duration("max-heartbeat-age", 30*time.Second, "maximum server heartbeat age")
	minHubs := flag.Int("min-hubs", 1, "minimum active Hub records")
	minBattles := flag.Int("min-battles", 1, "minimum active Battle records")
	timeout := flag.Duration("timeout", 10*time.Second, "timeout per Redis sample")
	flag.Parse()
	addrs := split(*addrsRaw)
	if len(addrs) == 0 || *cycles < 3 || *interval <= 0 {
		fatal(fmt.Errorf("invalid addrs/cycles/interval"))
	}
	client := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: addrs, Password: os.Getenv(*passwordEnv), DB: *db})
	defer func() { _ = client.Close() }()
	policy := redisaudit.Policy{TargetWriterEpoch: 2, MaxHeartbeatAge: *maxAge, MinHubs: *minHubs, MinBattles: *minBattles}
	var previous redisaudit.Snapshot
	for i := 0; i < *cycles; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		snapshot, err := redisaudit.ReadSnapshot(ctx, client)
		cancel()
		if err != nil {
			fatal(err)
		}
		findings := redisaudit.ValidateSnapshot(snapshot, time.Now(), policy)
		if i > 0 {
			findings = append(findings, redisaudit.CompareProgress(previous, snapshot)...)
		}
		if len(findings) > 0 {
			for _, finding := range findings {
				fmt.Fprintln(os.Stderr, "FAIL:", finding)
			}
			os.Exit(2)
		}
		fmt.Printf("Redis auth sample %d/%d 通过: hubs=%d battles=%d\n", i+1, *cycles, len(snapshot.Hubs), len(snapshot.Battles))
		previous = snapshot
		if i+1 < *cycles {
			time.Sleep(*interval)
		}
	}
}

func split(raw string) []string {
	var out []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
func fatal(err error) { fmt.Fprintln(os.Stderr, "ERROR:", err); os.Exit(1) }
