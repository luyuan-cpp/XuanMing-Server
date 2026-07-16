package poduidpreflight

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	dsv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/ds/v1"
)

// The broad namespace pattern is deliberate. It includes every canonical
// pandora:ds:battle:{*} key and also makes malformed legacy key shapes visible
// as findings instead of silently omitting them from a release proof.
const battleScanPattern = "pandora:ds:battle:*"

var battleRecordKey = regexp.MustCompile(`^pandora:ds:battle:\{([0-9]+)\}$`)

type Finding struct {
	Source  string
	Key     string
	MatchID uint64
	Reason  string
}

type AuditSummary struct {
	mu                  sync.Mutex
	MastersVisited      int
	KeysVisited         int
	RecordsDecoded      int
	AllocationUncertain int
	Findings            []Finding
	runtimeMasterIDs    map[string]struct{}
	seenKeys            map[string]string
	recordDigests       map[string][sha256.Size]byte
}

func (s *AuditSummary) registerRuntimeMaster(id string) error {
	if !redisRuntimeIDPattern.MatchString(id) {
		return fmt.Errorf("scan observed a non-canonical Redis master identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runtimeMasterIDs == nil {
		s.runtimeMasterIDs = make(map[string]struct{})
	}
	if _, duplicate := s.runtimeMasterIDs[id]; duplicate {
		return fmt.Errorf("scan observed a duplicate Redis master identity")
	}
	s.runtimeMasterIDs[id] = struct{}{}
	return nil
}

// RuntimeMasterSetDigest binds the exact Redis runtime identities that
// actually executed SCAN. It intentionally exposes only a safe digest.
func (s *AuditSummary) RuntimeMasterSetDigest() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runtimeMasterIDs) == 0 || len(s.runtimeMasterIDs) != s.MastersVisited {
		return "", fmt.Errorf("scan master identity coverage does not match visited master count")
	}
	ids := make([]string, 0, len(s.runtimeMasterIDs))
	for id := range s.runtimeMasterIDs {
		ids = append(ids, id)
	}
	return runtimeMasterSetDigest(ids)
}

func (s *AuditSummary) masterStarted() {
	s.mu.Lock()
	s.MastersVisited++
	s.mu.Unlock()
}

func (s *AuditSummary) registerScannedKey(key, ownerRuntimeID string) (bool, error) {
	if !redisRuntimeIDPattern.MatchString(ownerRuntimeID) {
		return false, fmt.Errorf("scan key owner has a non-canonical Redis runtime identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenKeys == nil {
		s.seenKeys = make(map[string]string)
	}
	if firstOwner, duplicate := s.seenKeys[key]; duplicate {
		if firstOwner != ownerRuntimeID {
			return false, fmt.Errorf(
				"durable battle key %q appeared on multiple Redis masters", key)
		}
		return false, nil
	}
	s.seenKeys[key] = ownerRuntimeID
	s.KeysVisited++
	return true, nil
}

// registerRecordBody makes SCAN's permitted duplicate-key behaviour safe.
// The first body is audited once; an identical duplicate is ignored, while a
// value change during the same traversal fails closed and requires a restart.
func (s *AuditSummary) registerRecordBody(key string, body []byte) (bool, error) {
	digest := sha256.Sum256(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recordDigests == nil {
		s.recordDigests = make(map[string][sha256.Size]byte)
	}
	previous, duplicate := s.recordDigests[key]
	if !duplicate {
		s.recordDigests[key] = digest
		return true, nil
	}
	if previous != digest {
		return false, fmt.Errorf("durable battle key %q changed during audit", key)
	}
	return false, nil
}

func (s *AuditSummary) recordDecoded(category string) {
	s.mu.Lock()
	s.RecordsDecoded++
	if category == CategoryAllocationUncertain {
		s.AllocationUncertain++
	}
	s.mu.Unlock()
}

func (s *AuditSummary) addFinding(f Finding) {
	s.mu.Lock()
	s.Findings = append(s.Findings, f)
	s.mu.Unlock()
}

func (s *AuditSummary) SortFindings() {
	s.mu.Lock()
	defer s.mu.Unlock()
	sort.Slice(s.Findings, func(i, j int) bool {
		a, b := s.Findings[i], s.Findings[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		if a.Reason != b.Reason {
			return a.Reason < b.Reason
		}
		return a.Source < b.Source
	})
}

// AuditRedis scans every Redis Cluster master. ClusterClient.Scan alone visits
// only one shard and therefore cannot support a production release proof.
// It performs only read-only runtime-identity commands plus SCAN and GET.
func AuditRedis(
	ctx context.Context,
	rdb redis.UniversalClient,
	scanCount int64,
	summary *AuditSummary,
) error {
	if rdb == nil || scanCount <= 0 || summary == nil {
		return fmt.Errorf("pod_uid preflight requires Redis, positive scan count and summary")
	}
	if cluster, ok := rdb.(*redis.ClusterClient); ok {
		return cluster.ForEachMaster(ctx, func(nodeCtx context.Context, node *redis.Client) error {
			source := safeRedisSource("redis-cluster-master", node.Options().Addr)
			beforeID, err := clusterMasterID(nodeCtx, node)
			if err != nil {
				return fmt.Errorf("%s identity before scan: %w", source, err)
			}
			if err := scanRedisNode(nodeCtx, node, beforeID, source, scanCount, summary); err != nil {
				return fmt.Errorf("%s: %w", source, err)
			}
			afterID, err := clusterMasterID(nodeCtx, node)
			if err != nil {
				return fmt.Errorf("%s identity after scan: %w", source, err)
			}
			if afterID != beforeID {
				return fmt.Errorf("%s runtime identity changed during scan", source)
			}
			if err := summary.registerRuntimeMaster(beforeID); err != nil {
				return err
			}
			return nil
		})
	}
	beforeID, err := standaloneRuntimeID(ctx, rdb)
	if err != nil {
		return fmt.Errorf("Redis primary identity before scan: %w", err)
	}
	if err := scanRedisNode(ctx, rdb, beforeID, "redis-primary", scanCount, summary); err != nil {
		return err
	}
	afterID, err := standaloneRuntimeID(ctx, rdb)
	if err != nil {
		return fmt.Errorf("Redis primary identity after scan: %w", err)
	}
	if afterID != beforeID {
		return fmt.Errorf("Redis primary runtime identity changed during scan")
	}
	return summary.registerRuntimeMaster(beforeID)
}

// safeRedisSource keeps findings useful for correlating failures from the
// same master without printing internal Redis endpoints into release logs.
func safeRedisSource(kind, endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return kind
	}
	sum := sha256.Sum256([]byte(strings.ToLower(endpoint)))
	return fmt.Sprintf("%s-%x", kind, sum[:6])
}

type redisScanner interface {
	Scan(context.Context, uint64, string, int64) *redis.ScanCmd
	Get(context.Context, string) *redis.StringCmd
}

func scanRedisNode(
	ctx context.Context,
	node redisScanner,
	ownerRuntimeID string,
	source string,
	scanCount int64,
	summary *AuditSummary,
) error {
	summary.masterStarted()
	var cursor uint64
	for {
		keys, next, err := node.Scan(ctx, cursor, battleScanPattern, scanCount).Result()
		if err != nil {
			return fmt.Errorf("SCAN %q cursor=%d: %w", battleScanPattern, cursor, err)
		}
		for _, key := range keys {
			firstKeyVisit, err := summary.registerScannedKey(key, ownerRuntimeID)
			if err != nil {
				return err
			}
			matchID, err := ParseBattleRecordKey(key)
			if err != nil {
				if firstKeyVisit {
					summary.addFinding(Finding{Source: source, Key: key, Reason: err.Error()})
				}
				continue
			}
			body, err := node.Get(ctx, key).Bytes()
			if errors.Is(err, redis.Nil) {
				return fmt.Errorf("durable battle key %q disappeared during audit", key)
			}
			if err != nil {
				return fmt.Errorf("GET %q: %w", key, err)
			}
			firstBody, err := summary.registerRecordBody(key, body)
			if err != nil {
				return err
			}
			if !firstBody {
				continue
			}
			rec := new(dsv1.BattleStorageRecord)
			if err := proto.Unmarshal(body, rec); err != nil {
				summary.addFinding(Finding{Source: source, Key: key, MatchID: matchID,
					Reason: "protobuf decode failed: " + err.Error()})
				continue
			}
			classification := ClassifyBattle(matchID, rec)
			summary.recordDecoded(classification.Category)
			for _, reason := range classification.Reasons {
				summary.addFinding(Finding{
					Source: source, Key: key, MatchID: matchID, Reason: reason,
				})
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

func ParseBattleRecordKey(key string) (uint64, error) {
	match := battleRecordKey.FindStringSubmatch(key)
	if len(match) != 2 {
		return 0, fmt.Errorf("unexpected key shape under battle namespace")
	}
	matchID, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid battle key match_id: %w", err)
	}
	if strconv.FormatUint(matchID, 10) != match[1] {
		return 0, fmt.Errorf("invalid battle key match_id: non-canonical decimal")
	}
	if matchID == 0 {
		return 0, fmt.Errorf("invalid battle key match_id: zero is reserved")
	}
	return matchID, nil
}
