// placement_preflight is a read-only release gate for the player-locator
// source-departure hard gate. It must run successfully before replacing the
// locator writer: legacy PENDING records without a complete immutable physical
// source cannot obtain an exact departure proof and would otherwise become
// permanently uncommittable after the upgrade. A canonical Begin-before-
// Confirm record is safe and must remain startable across a full Pod restart.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"
	"github.com/luyuancpp/pandora/pkg/redisx"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/conf"
	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/placementpreflight"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("placement_preflight", flag.ContinueOnError)
	flags.SetOutput(stderr)
	confPath := flags.String("conf", "etc/locator-dev.yaml", "player-locator config file")
	timeout := flags.Duration("timeout", 5*time.Minute, "hard deadline for the read-only audit")
	scanCount := flags.Int64("scan-count", 1000, "Redis SCAN count hint per master")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *timeout <= 0 || *scanCount <= 0 {
		fmt.Fprintln(stderr, "placement preflight: timeout and scan-count must be positive")
		return 2
	}

	cfg, err := loadLocatorConfig(*confPath)
	if err != nil {
		fmt.Fprintf(stderr, "placement preflight: load config: %v\n", err)
		return 1
	}
	rc := cfg.Node.RedisClient
	if rc.Host == "" && len(rc.Addrs) == 0 {
		fmt.Fprintln(stderr, "placement preflight: locator Redis endpoint is not configured")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	rdb := redisx.NewUniversalClient(rc)
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(ctx).Err(); err != nil {
		fmt.Fprintf(stderr, "placement preflight: Redis ping failed: %v\n", err)
		return 1
	}

	summary := new(placementpreflight.AuditSummary)
	if err := placementpreflight.AuditRedis(ctx, rdb, *scanCount, summary); err != nil {
		fmt.Fprintf(stderr, "placement preflight: Redis audit failed closed: %v\n", err)
		return 1
	}
	if summary.Nodes == 0 {
		fmt.Fprintln(stderr, "placement preflight: Redis audit visited zero nodes")
		return 1
	}

	sort.Slice(summary.Findings, func(i, j int) bool {
		a, b := summary.Findings[i], summary.Findings[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		if a.Reason != b.Reason {
			return a.Reason < b.Reason
		}
		return a.Source < b.Source
	})
	for _, f := range summary.Findings {
		fmt.Fprintf(stderr, "UNSAFE source=%q key=%q player_id=%d reason=%q\n",
			f.Source, f.Key, f.PlayerID, f.Reason)
	}
	if len(summary.Findings) != 0 {
		fmt.Fprintf(stderr,
			"placement preflight FAILED: nodes=%d records=%d skipped_non_record_keys=%d findings=%d; no data was modified\n",
			summary.Nodes, summary.Records, summary.Skipped, len(summary.Findings))
		return 1
	}
	fmt.Fprintf(stdout,
		"placement preflight PASSED: nodes=%d records=%d skipped_non_record_keys=%d; no data was modified\n",
		summary.Nodes, summary.Records, summary.Skipped)
	return 0
}

func loadLocatorConfig(path string) (conf.Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return conf.Config{}, err
	}
	c := config.New(config.WithSource(file.NewSource(abs)))
	defer func() { _ = c.Close() }()
	if err := c.Load(); err != nil {
		return conf.Config{}, err
	}
	var cfg conf.Config
	if err := c.Scan(&cfg); err != nil {
		return conf.Config{}, err
	}
	cfg.Defaults()
	return cfg, nil
}
