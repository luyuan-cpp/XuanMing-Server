package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/config"
	"github.com/luyuancpp/pandora/pkg/redisx"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/poduidpreflight"
)

var (
	flagPodUIDReleasePreflight                       bool
	flagPodUIDReleasePreflightTimeout                time.Duration
	flagPodUIDReleasePreflightScanCount              int64
	flagPodUIDReleasePreflightRunID                  string
	flagPodUIDReleasePreflightPhase                  string
	flagPodUIDReleasePreflightImageDigest            string
	flagPodUIDReleasePreflightExpectedTargetIdentity string
)

func init() {
	flag.BoolVar(&flagPodUIDReleasePreflight, "pod-uid-release-preflight", false,
		"run the read-only Model-B legacy pod_uid release audit and exit")
	flag.DurationVar(&flagPodUIDReleasePreflightTimeout, "pod-uid-release-preflight-timeout", 10*time.Minute,
		"hard deadline for the read-only Model-B pod_uid audit")
	flag.Int64Var(&flagPodUIDReleasePreflightScanCount, "pod-uid-release-preflight-scan-count", 1000,
		"Redis SCAN count hint per master for the Model-B pod_uid audit")
	flag.StringVar(&flagPodUIDReleasePreflightRunID, "pod-uid-release-preflight-run-id", "",
		"immutable strict activation run identity")
	flag.StringVar(&flagPodUIDReleasePreflightPhase, "pod-uid-release-preflight-phase", "",
		"strict activation phase: prepare, drained or final")
	flag.StringVar(&flagPodUIDReleasePreflightImageDigest, "pod-uid-release-preflight-image-digest", "",
		"immutable image digest bound by the activation Job contract")
	flag.StringVar(&flagPodUIDReleasePreflightExpectedTargetIdentity,
		"pod-uid-release-preflight-expected-target-identity", "",
		"prepare leaves empty; drained/final must bind the prepare Redis target identity")
}

var errUnsafeLegacyPodUID = errors.New("unsafe legacy Model-B pod_uid records found")

var (
	preflightRunIDPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{7,23}$`)
	preflightDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type podUIDPreflightEvidence struct {
	RunID                  string
	Phase                  string
	ImageDigest            string
	ExpectedTargetIdentity string
}

func (e podUIDPreflightEvidence) valid() bool {
	if !preflightRunIDPattern.MatchString(e.RunID) ||
		!preflightDigestPattern.MatchString(e.ImageDigest) {
		return false
	}
	switch e.Phase {
	case "prepare":
		return e.ExpectedTargetIdentity == ""
	case "drained", "final":
		return poduidpreflight.ValidTargetIdentity(e.ExpectedTargetIdentity)
	default:
		return false
	}
}

const (
	podUIDPreflightRedisUsernameEnv          = "PANDORA_POD_UID_PREFLIGHT_REDIS_USERNAME"
	podUIDPreflightRedisPasswordEnv          = "PANDORA_POD_UID_PREFLIGHT_REDIS_PASSWORD"
	minimumPodUIDPreflightRedisPasswordBytes = 32
)

type podUIDPreflightRedisCredentials struct {
	Username string
	Password string
}

func loadPodUIDPreflightRedisCredentials() (podUIDPreflightRedisCredentials, error) {
	username, usernameSet := os.LookupEnv(podUIDPreflightRedisUsernameEnv)
	password, passwordSet := os.LookupEnv(podUIDPreflightRedisPasswordEnv)
	if !usernameSet || username != poduidpreflight.CanonicalReadOnlyUsername {
		return podUIDPreflightRedisCredentials{}, fmt.Errorf(
			"%s must be the canonical dedicated read-only identity", podUIDPreflightRedisUsernameEnv)
	}
	if !passwordSet || len(password) < minimumPodUIDPreflightRedisPasswordBytes ||
		strings.ContainsRune(password, '\x00') {
		return podUIDPreflightRedisCredentials{}, fmt.Errorf(
			"%s must provide a dedicated high-entropy Secret credential of at least %d bytes",
			podUIDPreflightRedisPasswordEnv, minimumPodUIDPreflightRedisPasswordBytes)
	}
	return podUIDPreflightRedisCredentials{Username: username, Password: password}, nil
}

type podUIDPreflightDependencies struct {
	newClient   func(config.RedisConf, string, string) redis.UniversalClient
	proveTarget func(context.Context, redis.UniversalClient, config.RedisConf, string) (
		poduidpreflight.RedisTargetIdentity, error)
}

var productionPodUIDPreflightDependencies = podUIDPreflightDependencies{
	newClient:   redisx.NewUniversalClientWithCredentials,
	proveTarget: poduidpreflight.ProveReadOnlyAndIdentify,
}

func runPodUIDReleasePreflight(
	ctx context.Context,
	rc config.RedisConf,
	scanCount int64,
	evidence podUIDPreflightEvidence,
	credentials podUIDPreflightRedisCredentials,
	stdout, stderr io.Writer,
) error {
	return runPodUIDReleasePreflightWithDependencies(ctx, rc, scanCount, evidence,
		credentials, stdout, stderr, productionPodUIDPreflightDependencies)
}

func runPodUIDReleasePreflightWithDependencies(
	ctx context.Context,
	rc config.RedisConf,
	scanCount int64,
	evidence podUIDPreflightEvidence,
	credentials podUIDPreflightRedisCredentials,
	stdout, stderr io.Writer,
	dependencies podUIDPreflightDependencies,
) error {
	if ctx == nil || scanCount <= 0 || stdout == nil || stderr == nil {
		return fmt.Errorf("pod_uid release preflight requires context, positive scan count and output writers")
	}
	if !evidence.valid() {
		return fmt.Errorf("pod_uid release preflight requires canonical run_id, prepare/drained/final phase, image digest and phase target binding")
	}
	if credentials.Username != poduidpreflight.CanonicalReadOnlyUsername ||
		len(credentials.Password) < minimumPodUIDPreflightRedisPasswordBytes {
		return fmt.Errorf("pod_uid release preflight requires dedicated read-only Redis credentials")
	}
	if rc.Host == "" && len(rc.Addrs) == 0 {
		return fmt.Errorf("ds_allocator Redis endpoint is not configured")
	}
	if rc.Password != "" {
		return fmt.Errorf("pod_uid release preflight config must not contain the writer Redis password")
	}
	if rc.MaintNotifications != "disabled" {
		return fmt.Errorf("pod_uid release preflight config must set Redis maint_notifications=disabled")
	}
	if dependencies.newClient == nil || dependencies.proveTarget == nil {
		return fmt.Errorf("pod_uid release preflight dependencies are incomplete")
	}
	configIdentity, err := poduidpreflight.IdentifyRedisConfig(rc)
	if err != nil {
		return fmt.Errorf("Redis config identity failed: %w", err)
	}
	rdb := dependencies.newClient(rc, credentials.Username, credentials.Password)
	if rdb == nil {
		return fmt.Errorf("Redis client construction failed")
	}
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("Redis ping failed: %w", err)
	}

	before, err := dependencies.proveTarget(ctx, rdb, rc, credentials.Username)
	if err != nil {
		return fmt.Errorf("Redis read-only ACL/target proof failed: %w", err)
	}
	if evidence.ExpectedTargetIdentity != "" && before.Digest != evidence.ExpectedTargetIdentity {
		return fmt.Errorf("Redis target identity differs from the prepare evidence")
	}
	summary := new(poduidpreflight.AuditSummary)
	if err := poduidpreflight.AuditRedis(ctx, rdb, scanCount, summary); err != nil {
		return fmt.Errorf("Redis audit failed closed: %w", err)
	}
	if summary.MastersVisited == 0 {
		return fmt.Errorf("Redis audit visited zero masters")
	}
	scanMasterSetDigest, err := summary.RuntimeMasterSetDigest()
	if err != nil {
		return fmt.Errorf("Redis audit master coverage failed closed: %w", err)
	}
	if summary.MastersVisited != before.Nodes || scanMasterSetDigest != before.MasterSetDigest {
		return fmt.Errorf("Redis audit did not scan the exact preflight master identity set")
	}
	after, err := dependencies.proveTarget(ctx, rdb, rc, credentials.Username)
	if err != nil {
		return fmt.Errorf("post-audit Redis read-only ACL/target proof failed: %w", err)
	}
	if after != before {
		return fmt.Errorf("Redis target identity changed during the audit; start a new activation evidence chain")
	}
	if scanMasterSetDigest != after.MasterSetDigest {
		return fmt.Errorf("Redis audit master identity set differs from post-audit topology")
	}
	summary.SortFindings()
	for _, finding := range summary.Findings {
		fmt.Fprintf(stderr, "UNSAFE source=%q key=%q match_id=%d reason=%q\n",
			finding.Source, finding.Key, finding.MatchID, finding.Reason)
	}
	if len(summary.Findings) != 0 {
		fmt.Fprintf(stderr,
			"pod_uid release preflight FAILED: run_id=%s phase=%s image_digest=%s redis_config_identity=%s redis_target_identity=%s redis_topology=%s redis_acl_user=%s visited_masters=%d visited_keys=%d decoded_records=%d allocation_uncertain=%d findings=%d; no data was modified\n",
			evidence.RunID, evidence.Phase, evidence.ImageDigest,
			configIdentity.Digest, before.Digest, before.Topology, credentials.Username,
			summary.MastersVisited, summary.KeysVisited, summary.RecordsDecoded,
			summary.AllocationUncertain, len(summary.Findings))
		return errUnsafeLegacyPodUID
	}
	fmt.Fprintf(stdout,
		"pod_uid release preflight PASSED: run_id=%s phase=%s image_digest=%s redis_config_identity=%s redis_target_identity=%s redis_topology=%s redis_acl_user=%s visited_masters=%d visited_keys=%d decoded_records=%d allocation_uncertain=%d findings=0; no data was modified\n",
		evidence.RunID, evidence.Phase, evidence.ImageDigest,
		configIdentity.Digest, before.Digest, before.Topology, credentials.Username,
		summary.MastersVisited, summary.KeysVisited, summary.RecordsDecoded,
		summary.AllocationUncertain)
	return nil
}
