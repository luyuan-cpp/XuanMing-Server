package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/config"
	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
	"github.com/luyuancpp/pandora/services/battle/ds_allocator/internal/poduidpreflight"
)

var testPreflightCredentials = podUIDPreflightRedisCredentials{
	Username: poduidpreflight.CanonicalReadOnlyUsername,
	Password: "test-only-read-password-32-bytes-minimum",
}

const testPreflightRedisRunID = "0123456789abcdef0123456789abcdef01234567"

func TestRunPodUIDReleasePreflightPassAndFail(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := config.RedisConf{Host: mr.Addr(), MaintNotifications: "disabled"}
	evidence := podUIDPreflightEvidence{
		RunID: "release-run-01", Phase: "prepare",
		ImageDigest: "sha256:" + strings.Repeat("a", 64),
	}
	rec := &dsv1.BattleStorageRecord{
		MatchId: 101, State: "running", AllocationId: "10110110-1101-4101-8101-101101101101",
		DsPodName: "battle-101", GameserverUid: "gs-uid-101",
		PodUid: "pod-uid-101", ReleaseTrack: "stable",
	}
	writeBattleRecord(t, mr, "pandora:ds:battle:{101}", rec)
	masterSetDigest := testAuditMasterSetDigest(t, mr.Addr())
	identity := poduidpreflight.RedisTargetIdentity{
		Digest: "sha256:" + strings.Repeat("b", 64), Topology: "standalone", Nodes: 1,
		MasterSetDigest: masterSetDigest,
	}
	dependencies := podUIDPreflightDependencies{
		newClient: func(config.RedisConf, string, string) redis.UniversalClient {
			return &preflightTestRedisClient{Client: redis.NewClient(&redis.Options{Addr: mr.Addr()})}
		},
		proveTarget: func(context.Context, redis.UniversalClient, config.RedisConf, string) (
			poduidpreflight.RedisTargetIdentity, error) {
			return identity, nil
		},
	}

	var stdout, stderr bytes.Buffer
	if err := runPodUIDReleasePreflightWithDependencies(context.Background(), rc, 10, evidence,
		testPreflightCredentials, &stdout, &stderr, dependencies); err != nil {
		t.Fatalf("pass: %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "pod_uid release preflight PASSED:") ||
		!strings.Contains(stdout.String(), "run_id=release-run-01 phase=prepare") ||
		!strings.Contains(stdout.String(), "visited_masters=1") ||
		!strings.Contains(stdout.String(), "findings=0") || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	rec.PodUid = ""
	writeBattleRecord(t, mr, "pandora:ds:battle:{101}", rec)
	stdout.Reset()
	stderr.Reset()
	err := runPodUIDReleasePreflightWithDependencies(context.Background(), rc, 10, evidence,
		testPreflightCredentials, &stdout, &stderr, dependencies)
	if !errorsIsUnsafePodUID(err) || !strings.Contains(stderr.String(), "missing pod_uid") ||
		!strings.Contains(stderr.String(), "findings=1") {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

func TestPodUIDReleasePreflightRejectsUnboundEvidence(t *testing.T) {
	mr := miniredis.RunT(t)
	var stdout, stderr bytes.Buffer
	for _, evidence := range []podUIDPreflightEvidence{
		{},
		{RunID: "release-run-01", Phase: "future", ImageDigest: "sha256:" + strings.Repeat("a", 64)},
		{RunID: "release-run-01", Phase: "drained", ImageDigest: "latest"},
		{RunID: "release-run-01", Phase: "drained", ImageDigest: "sha256:" + strings.Repeat("a", 64)},
		{RunID: "release-run-01", Phase: "final", ImageDigest: "sha256:" + strings.Repeat("a", 64),
			ExpectedTargetIdentity: "not-a-digest"},
		{RunID: "release-run-01", Phase: "prepare", ImageDigest: "sha256:" + strings.Repeat("a", 64),
			ExpectedTargetIdentity: "sha256:" + strings.Repeat("b", 64)},
	} {
		if err := runPodUIDReleasePreflight(context.Background(),
			config.RedisConf{Host: mr.Addr(), MaintNotifications: "disabled"},
			10, evidence, testPreflightCredentials, &stdout, &stderr); err == nil {
			t.Fatalf("evidence %+v unexpectedly accepted", evidence)
		}
	}
}

func TestPodUIDReleasePreflightPhaseTargetBinding(t *testing.T) {
	image := "sha256:" + strings.Repeat("a", 64)
	target := "sha256:" + strings.Repeat("b", 64)
	for _, evidence := range []podUIDPreflightEvidence{
		{RunID: "release-run-03", Phase: "prepare", ImageDigest: image},
		{RunID: "release-run-03", Phase: "drained", ImageDigest: image,
			ExpectedTargetIdentity: target},
		{RunID: "release-run-03", Phase: "final", ImageDigest: image,
			ExpectedTargetIdentity: target},
	} {
		if !evidence.valid() {
			t.Fatalf("canonical evidence unexpectedly rejected: %+v", evidence)
		}
	}
}

func TestLoadPodUIDPreflightRedisCredentialsRequiresDedicatedIdentity(t *testing.T) {
	t.Setenv(podUIDPreflightRedisUsernameEnv, poduidpreflight.CanonicalReadOnlyUsername)
	t.Setenv(podUIDPreflightRedisPasswordEnv, "secret-from-dedicated-k8s-secret-32-bytes")
	got, err := loadPodUIDPreflightRedisCredentials()
	if err != nil || got.Username != poduidpreflight.CanonicalReadOnlyUsername || got.Password == "" {
		t.Fatalf("credentials=%+v err=%v", got, err)
	}
	for _, username := range []string{"", "default", "ds-allocator-writer"} {
		t.Run("reject_"+username, func(t *testing.T) {
			t.Setenv(podUIDPreflightRedisUsernameEnv, username)
			t.Setenv(podUIDPreflightRedisPasswordEnv, "secret-from-dedicated-k8s-secret-32-bytes")
			if _, err := loadPodUIDPreflightRedisCredentials(); err == nil {
				t.Fatal("unexpectedly accepted non-canonical username")
			}
		})
	}
	for _, password := range []string{"", "short", strings.Repeat("a", 31)} {
		t.Run("reject_short_password", func(t *testing.T) {
			t.Setenv(podUIDPreflightRedisUsernameEnv, poduidpreflight.CanonicalReadOnlyUsername)
			t.Setenv(podUIDPreflightRedisPasswordEnv, password)
			if _, err := loadPodUIDPreflightRedisCredentials(); err == nil {
				t.Fatal("unexpectedly accepted low-length preflight credential")
			}
		})
	}
}

func TestPodUIDReleasePreflightRejectsWriterPasswordAndTargetChange(t *testing.T) {
	mr := miniredis.RunT(t)
	client := &preflightTestRedisClient{Client: redis.NewClient(&redis.Options{Addr: mr.Addr()})}
	defer func() { _ = client.Close() }()
	masterSetDigest := testAuditMasterSetDigest(t, mr.Addr())
	evidence := podUIDPreflightEvidence{
		RunID: "release-run-02", Phase: "prepare",
		ImageDigest: "sha256:" + strings.Repeat("a", 64),
	}
	var stdout, stderr bytes.Buffer
	called := 0
	dependencies := podUIDPreflightDependencies{
		newClient: func(config.RedisConf, string, string) redis.UniversalClient { return client },
		proveTarget: func(context.Context, redis.UniversalClient, config.RedisConf, string) (
			poduidpreflight.RedisTargetIdentity, error) {
			called++
			letter := "b"
			if called > 1 {
				letter = "c"
			}
			return poduidpreflight.RedisTargetIdentity{
				Digest: "sha256:" + strings.Repeat(letter, 64), Topology: "standalone", Nodes: 1,
				MasterSetDigest: masterSetDigest,
			}, nil
		},
	}
	rc := config.RedisConf{Host: mr.Addr(), MaintNotifications: "disabled", Password: "writer-secret"}
	if err := runPodUIDReleasePreflightWithDependencies(context.Background(), rc, 10, evidence,
		testPreflightCredentials, &stdout, &stderr, dependencies); err == nil || called != 0 {
		t.Fatalf("writer password err=%v prove calls=%d", err, called)
	}
	rc.Password = ""
	if err := runPodUIDReleasePreflightWithDependencies(context.Background(), rc, 10, evidence,
		testPreflightCredentials, &stdout, &stderr, dependencies); err == nil ||
		!strings.Contains(err.Error(), "changed during") {
		t.Fatalf("target change err=%v", err)
	}
}

func TestPodUIDReleasePreflightRejectsScanMasterSetMismatch(t *testing.T) {
	mr := miniredis.RunT(t)
	evidence := podUIDPreflightEvidence{
		RunID: "release-run-master-set", Phase: "prepare",
		ImageDigest: "sha256:" + strings.Repeat("a", 64),
	}
	identity := poduidpreflight.RedisTargetIdentity{
		Digest: "sha256:" + strings.Repeat("b", 64), Topology: "standalone", Nodes: 1,
		MasterSetDigest: "sha256:" + strings.Repeat("c", 64),
	}
	dependencies := podUIDPreflightDependencies{
		newClient: func(config.RedisConf, string, string) redis.UniversalClient {
			return &preflightTestRedisClient{Client: redis.NewClient(&redis.Options{Addr: mr.Addr()})}
		},
		proveTarget: func(context.Context, redis.UniversalClient, config.RedisConf, string) (
			poduidpreflight.RedisTargetIdentity, error) {
			return identity, nil
		},
	}
	var stdout, stderr bytes.Buffer
	err := runPodUIDReleasePreflightWithDependencies(context.Background(),
		config.RedisConf{Host: mr.Addr(), MaintNotifications: "disabled"}, 10, evidence,
		testPreflightCredentials, &stdout, &stderr, dependencies)
	if err == nil || !strings.Contains(err.Error(), "exact preflight master identity set") ||
		stdout.Len() != 0 {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

type preflightTestRedisClient struct {
	*redis.Client
}

func (c *preflightTestRedisClient) Do(ctx context.Context, args ...interface{}) *redis.Cmd {
	if len(args) == 2 && fmt.Sprint(args[0]) == "INFO" && fmt.Sprint(args[1]) == "server" {
		cmd := redis.NewCmd(ctx, args...)
		cmd.SetVal("# Server\r\nrun_id:" + testPreflightRedisRunID + "\r\n")
		return cmd
	}
	return c.Client.Do(ctx, args...)
}

func testAuditMasterSetDigest(t *testing.T, addr string) string {
	t.Helper()
	client := &preflightTestRedisClient{Client: redis.NewClient(&redis.Options{Addr: addr})}
	defer func() { _ = client.Close() }()
	summary := new(poduidpreflight.AuditSummary)
	if err := poduidpreflight.AuditRedis(context.Background(), client, 10, summary); err != nil {
		t.Fatalf("derive test master set: %v", err)
	}
	digest, err := summary.RuntimeMasterSetDigest()
	if err != nil {
		t.Fatalf("derive test master set digest: %v", err)
	}
	return digest
}

func writeBattleRecord(t *testing.T, mr *miniredis.Miniredis, key string, rec *dsv1.BattleStorageRecord) {
	t.Helper()
	body, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	mr.Set(key, string(body))
}

func errorsIsUnsafePodUID(err error) bool {
	return err == errUnsafeLegacyPodUID
}
