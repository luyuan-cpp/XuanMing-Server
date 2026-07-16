package poduidpreflight

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

func TestClassifyBattleConservativePodUIDGate(t *testing.T) {
	const allocationID = "71717171-7171-4171-8171-717171717171"
	exact := func(state string) *dsv1.BattleStorageRecord {
		return &dsv1.BattleStorageRecord{
			MatchId: 71, State: state, AllocationId: allocationID,
			DsPodName: "battle-71", DsAddr: "10.0.0.71:7777",
			GameserverUid: "gs-uid-71", PodUid: "pod-uid-71", ReleaseTrack: "stable",
		}
	}
	for _, state := range []string{
		"warming", "ready", "running", "ended", "abandoned",
		"allocation_reconcile_release_pending", "preactive_release_pending",
		"allocation_abort_pending",
	} {
		t.Run("exact_"+state, func(t *testing.T) {
			got := ClassifyBattle(71, exact(state))
			if got.Category != CategoryExactIdentity || len(got.Reasons) != 0 {
				t.Fatalf("classification=%+v", got)
			}
		})
	}

	t.Run("legacy exact identity without pod UID is unsafe", func(t *testing.T) {
		rec := exact("running")
		rec.PodUid = ""
		got := ClassifyBattle(71, rec)
		if got.Category != CategoryUnsafe || !hasReason(got.Reasons, "missing pod_uid") {
			t.Fatalf("classification=%+v", got)
		}
	})

	t.Run("canonical allocation uncertain is distinct and safe", func(t *testing.T) {
		rec := &dsv1.BattleStorageRecord{
			MatchId: 71, State: "allocation_uncertain", AllocationId: allocationID,
		}
		got := ClassifyBattle(71, rec)
		if got.Category != CategoryAllocationUncertain || len(got.Reasons) != 0 {
			t.Fatalf("classification=%+v", got)
		}
	})

	t.Run("allocation uncertain partial identity is not mislabeled exact", func(t *testing.T) {
		rec := &dsv1.BattleStorageRecord{
			MatchId: 71, State: "allocation_uncertain", AllocationId: allocationID,
			DsPodName: "battle-71",
		}
		got := ClassifyBattle(71, rec)
		if got.Category != CategoryUnsafe ||
			!hasReason(got.Reasons, "partial or unexpected") ||
			hasReason(got.Reasons, "missing pod_uid") {
			t.Fatalf("classification=%+v", got)
		}
	})

	for _, state := range []string{"allocating", "allocation_reconcile_empty_tombstone", "abandoned"} {
		t.Run("empty_"+state, func(t *testing.T) {
			rec := &dsv1.BattleStorageRecord{MatchId: 71, State: state, AllocationId: allocationID}
			got := ClassifyBattle(71, rec)
			if got.Category != CategoryNoPhysicalIdentity || len(got.Reasons) != 0 {
				t.Fatalf("classification=%+v", got)
			}
		})
	}

	for name, mutate := range map[string]func(*dsv1.BattleStorageRecord){
		"key mismatch":     func(rec *dsv1.BattleStorageRecord) { rec.MatchId = 72 },
		"empty allocation": func(rec *dsv1.BattleStorageRecord) { rec.AllocationId = "" },
		"non UUID allocation": func(rec *dsv1.BattleStorageRecord) {
			rec.AllocationId = "alloc-71"
		},
		"non v4 allocation": func(rec *dsv1.BattleStorageRecord) {
			rec.AllocationId = "71717171-7171-5171-8171-717171717171"
		},
		"non RFC4122 variant allocation": func(rec *dsv1.BattleStorageRecord) {
			rec.AllocationId = "71717171-7171-4171-0171-717171717171"
		},
		"non canonical allocation": func(rec *dsv1.BattleStorageRecord) {
			rec.AllocationId = "71717171-7171-4171-8171-71717171717A"
		},
		"empty pod name": func(rec *dsv1.BattleStorageRecord) { rec.DsPodName = "" },
		"empty GS UID":   func(rec *dsv1.BattleStorageRecord) { rec.GameserverUid = "" },
		"bad track":      func(rec *dsv1.BattleStorageRecord) { rec.ReleaseTrack = "beta" },
		"spaced track":   func(rec *dsv1.BattleStorageRecord) { rec.ReleaseTrack = " stable " },
		"spaced pod UID": func(rec *dsv1.BattleStorageRecord) { rec.PodUid = " pod-uid-71" },
		"unknown state":  func(rec *dsv1.BattleStorageRecord) { rec.State = "future_state" },
	} {
		t.Run(name, func(t *testing.T) {
			rec := exact("running")
			mutate(rec)
			if got := ClassifyBattle(71, rec); got.Category != CategoryUnsafe || len(got.Reasons) == 0 {
				t.Fatalf("classification=%+v", got)
			}
		})
	}
}

func TestAuditRedisReadsOnlyAndReportsAllNamespaceFindings(t *testing.T) {
	mr := miniredis.RunT(t)
	baseClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	client := &fixedRuntimeIDClient{Client: baseClient, runIDs: []string{testRedisRunID}}
	defer func() { _ = baseClient.Close() }()

	canonical := &dsv1.BattleStorageRecord{
		MatchId: 81, State: "ready", AllocationId: "81818181-8181-4181-8181-818181818181",
		DsPodName: "battle-81", GameserverUid: "gs-uid-81",
		PodUid: "pod-uid-81", ReleaseTrack: "canary",
	}
	legacy := proto.Clone(canonical).(*dsv1.BattleStorageRecord)
	legacy.MatchId = 82
	legacy.AllocationId = "82828282-8282-4282-8282-828282828282"
	legacy.PodUid = ""
	uncertain := &dsv1.BattleStorageRecord{
		MatchId: 83, State: "allocation_uncertain", AllocationId: "83838383-8383-4383-8383-838383838383",
	}
	setProto(t, mr, "pandora:ds:battle:{81}", canonical)
	setProto(t, mr, "pandora:ds:battle:{82}", legacy)
	setProto(t, mr, "pandora:ds:battle:{83}", uncertain)
	mr.Set("pandora:ds:battle:legacy-84", "bad-key-shape")
	mr.Set("pandora:ds:battle:{85}", string([]byte{0xff, 0xff}))
	unknownBody, err := proto.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	unknownBody = protowire.AppendTag(unknownBody, 19000, protowire.VarintType)
	unknownBody = protowire.AppendVarint(unknownBody, 1)
	mr.Set("pandora:ds:battle:{86}", string(unknownBody))

	before := mr.Dump()
	summary := new(AuditSummary)
	if err := AuditRedis(context.Background(), client, 2, summary); err != nil {
		t.Fatalf("audit: %v", err)
	}
	if after := mr.Dump(); after != before {
		t.Fatalf("read-only audit mutated Redis\nbefore=%s\nafter=%s", before, after)
	}
	if summary.MastersVisited != 1 || summary.KeysVisited != 6 ||
		summary.RecordsDecoded != 4 || summary.AllocationUncertain != 1 {
		t.Fatalf("summary=%+v", summary)
	}
	if !findingContains(summary.Findings, "{82}", "missing pod_uid") ||
		!findingContains(summary.Findings, "legacy-84", "unexpected key shape") ||
		!findingContains(summary.Findings, "{85}", "protobuf decode failed") ||
		!findingContains(summary.Findings, "{86}", "unknown protobuf fields") {
		t.Fatalf("findings=%+v", summary.Findings)
	}
}

func TestAuditRedisFailsClosedWhenPrimaryIdentityChangesDuringScan(t *testing.T) {
	mr := miniredis.RunT(t)
	baseClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = baseClient.Close() }()
	client := &fixedRuntimeIDClient{Client: baseClient, runIDs: []string{
		strings.Repeat("a", 40), strings.Repeat("b", 40),
	}}
	err := AuditRedis(context.Background(), client, 10, new(AuditSummary))
	if err == nil || !strings.Contains(err.Error(), "changed during scan") {
		t.Fatalf("err=%v", err)
	}
}

func TestAuditSummaryBindsExactRuntimeMasterSet(t *testing.T) {
	summary := new(AuditSummary)
	summary.masterStarted()
	if err := summary.registerRuntimeMaster(strings.Repeat("a", 40)); err != nil {
		t.Fatal(err)
	}
	first, err := summary.RuntimeMasterSetDigest()
	if err != nil || !ValidTargetIdentity(first) {
		t.Fatalf("digest=%q err=%v", first, err)
	}
	if err := summary.registerRuntimeMaster(strings.Repeat("a", 40)); err == nil {
		t.Fatal("duplicate runtime master unexpectedly accepted")
	}
	summary.masterStarted()
	if _, err := summary.RuntimeMasterSetDigest(); err == nil {
		t.Fatal("visited/master identity count mismatch unexpectedly accepted")
	}
}

func TestClassifyBattleRejectsUnknownProtobufFields(t *testing.T) {
	rec := &dsv1.BattleStorageRecord{
		MatchId: 87, State: "allocation_uncertain",
		AllocationId: "87878787-8787-4787-8787-878787878787",
	}
	rec.ProtoReflect().SetUnknown(protowire.AppendVarint(
		protowire.AppendTag(nil, 19000, protowire.VarintType), 1))
	got := ClassifyBattle(87, rec)
	if got.Category != CategoryUnsafe || !hasReason(got.Reasons, "unknown protobuf fields") {
		t.Fatalf("classification=%+v", got)
	}
}

func TestScanFailsClosedWhenCanonicalKeyDisappears(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()
	rec := &dsv1.BattleStorageRecord{
		MatchId: 91, State: "allocation_uncertain", AllocationId: "91919191-9191-4191-8191-919191919191",
	}
	setProto(t, mr, "pandora:ds:battle:{91}", rec)
	scanner := &deleteAfterScan{Client: client, key: "pandora:ds:battle:{91}"}
	err := scanRedisNode(context.Background(), scanner, testRedisRunID,
		"test-master", 100, new(AuditSummary))
	if err == nil || !strings.Contains(err.Error(), "disappeared during audit") {
		t.Fatalf("err=%v", err)
	}
}

func TestScanAndGetErrorsFailClosed(t *testing.T) {
	for name, scanner := range map[string]*errorScanner{
		"scan": {scanErr: errors.New("scan unavailable")},
		"get": {
			keys:   []string{"pandora:ds:battle:{92}"},
			getErr: errors.New("get unavailable"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := scanRedisNode(context.Background(), scanner, testRedisRunID,
				"test-master", 100, new(AuditSummary))
			if err == nil || !strings.Contains(err.Error(), "unavailable") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestScanDuplicateKeysAreDeduplicatedAndMustKeepSameBody(t *testing.T) {
	record := &dsv1.BattleStorageRecord{
		MatchId: 93, State: "allocation_uncertain",
		AllocationId: "93939393-9393-4393-8393-939393939393",
	}
	body, err := proto.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	stable := &duplicateRecordScanner{
		key: "pandora:ds:battle:{93}", bodies: [][]byte{body, append([]byte(nil), body...)},
	}
	summary := new(AuditSummary)
	if err := scanRedisNode(context.Background(), stable, testRedisRunID,
		"test-master", 1, summary); err != nil {
		t.Fatalf("stable duplicate: %v", err)
	}
	if summary.KeysVisited != 1 || summary.RecordsDecoded != 1 || summary.AllocationUncertain != 1 {
		t.Fatalf("stable duplicate summary=%+v", summary)
	}

	changedRecord := proto.Clone(record).(*dsv1.BattleStorageRecord)
	changedRecord.State = "allocating"
	changedBody, err := proto.Marshal(changedRecord)
	if err != nil {
		t.Fatal(err)
	}
	changed := &duplicateRecordScanner{
		key: "pandora:ds:battle:{93}", bodies: [][]byte{body, changedBody},
	}
	err = scanRedisNode(context.Background(), changed, testRedisRunID,
		"test-master", 1, new(AuditSummary))
	if err == nil || !strings.Contains(err.Error(), "changed during audit") {
		t.Fatalf("changed duplicate err=%v", err)
	}
}

func TestScanRejectsSameKeyObservedOnDifferentMasters(t *testing.T) {
	record := &dsv1.BattleStorageRecord{
		MatchId: 94, State: "allocation_uncertain",
		AllocationId: "94000000-0000-4000-8000-000000000001",
	}
	body, err := proto.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	summary := new(AuditSummary)
	masterA := strings.Repeat("a", 40)
	masterB := strings.Repeat("b", 40)
	first := &duplicateRecordScanner{
		key: "pandora:ds:battle:{94}", bodies: [][]byte{body, body},
	}
	if err := scanRedisNode(context.Background(), first, masterA, "master-a", 1, summary); err != nil {
		t.Fatalf("first master: %v", err)
	}
	second := &duplicateRecordScanner{
		key: "pandora:ds:battle:{94}", bodies: [][]byte{body, body},
	}
	err = scanRedisNode(context.Background(), second, masterB, "master-b", 1, summary)
	if err == nil || !strings.Contains(err.Error(), "multiple Redis masters") {
		t.Fatalf("cross-master duplicate accepted: %v", err)
	}
}

func TestParseBattleRecordKey(t *testing.T) {
	for key, want := range map[string]uint64{
		"pandora:ds:battle:{1}": 1, "pandora:ds:battle:{42}": 42,
	} {
		if id, err := ParseBattleRecordKey(key); err != nil || id != want {
			t.Fatalf("key=%q id=%d err=%v", key, id, err)
		}
	}
	for _, key := range []string{
		"pandora:ds:battle:{0}", "pandora:ds:battle:{00}", "pandora:ds:battle:{01}",
		"pandora:ds:battle:42",
		"pandora:ds:battle:{-1}", "pandora:ds:battle:{18446744073709551616}",
	} {
		if _, err := ParseBattleRecordKey(key); err == nil {
			t.Fatalf("key %q unexpectedly accepted", key)
		}
	}
}

type deleteAfterScan struct {
	*redis.Client
	key     string
	deleted bool
}

type errorScanner struct {
	keys    []string
	scanErr error
	getErr  error
}

type fixedRuntimeIDClient struct {
	*redis.Client
	runIDs []string
	reads  int
}

type duplicateRecordScanner struct {
	key       string
	bodies    [][]byte
	scanCalls int
	getCalls  int
}

func (s *duplicateRecordScanner) Scan(
	ctx context.Context,
	_ uint64,
	_ string,
	_ int64,
) *redis.ScanCmd {
	cmd := redis.NewScanCmd(ctx, nil, "scan")
	next := uint64(0)
	if s.scanCalls == 0 {
		next = 1
	}
	s.scanCalls++
	cmd.SetVal([]string{s.key}, next)
	return cmd
}

func (s *duplicateRecordScanner) Get(ctx context.Context, _ string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx, "get")
	index := s.getCalls
	if index >= len(s.bodies) {
		index = len(s.bodies) - 1
	}
	s.getCalls++
	cmd.SetVal(string(s.bodies[index]))
	return cmd
}

func (c *fixedRuntimeIDClient) Do(ctx context.Context, args ...interface{}) *redis.Cmd {
	if len(args) == 2 && fmt.Sprint(args[0]) == "INFO" && fmt.Sprint(args[1]) == "server" {
		cmd := redis.NewCmd(ctx, args...)
		index := c.reads
		if index >= len(c.runIDs) {
			index = len(c.runIDs) - 1
		}
		c.reads++
		cmd.SetVal("# Server\r\nrun_id:" + c.runIDs[index] + "\r\n")
		return cmd
	}
	return c.Client.Do(ctx, args...)
}

func (s *errorScanner) Scan(ctx context.Context, _ uint64, _ string, _ int64) *redis.ScanCmd {
	cmd := redis.NewScanCmd(ctx, nil, "scan")
	if s.scanErr != nil {
		cmd.SetErr(s.scanErr)
	} else {
		cmd.SetVal(s.keys, 0)
	}
	return cmd
}

func (s *errorScanner) Get(ctx context.Context, _ string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx, "get")
	cmd.SetErr(s.getErr)
	return cmd
}

func (s *deleteAfterScan) Scan(ctx context.Context, cursor uint64, match string, count int64) *redis.ScanCmd {
	cmd := s.Client.Scan(ctx, cursor, match, count)
	keys, _, err := cmd.Result()
	if err == nil && !s.deleted {
		for _, key := range keys {
			if key == s.key {
				_ = s.Client.Del(ctx, s.key).Err()
				s.deleted = true
				break
			}
		}
	}
	return cmd
}

func setProto(t *testing.T, mr *miniredis.Miniredis, key string, rec *dsv1.BattleStorageRecord) {
	t.Helper()
	body, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	mr.Set(key, string(body))
}

func hasReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, want) {
			return true
		}
	}
	return false
}

func findingContains(findings []Finding, key, reason string) bool {
	for _, finding := range findings {
		if strings.Contains(finding.Key, key) && strings.Contains(finding.Reason, reason) {
			return true
		}
	}
	return false
}
