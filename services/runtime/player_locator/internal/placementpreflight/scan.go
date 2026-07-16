package placementpreflight

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

const placementScanPattern = "pandora:placement:*"

var placementRecordKey = regexp.MustCompile("^pandora:placement:\\{([0-9]+)\\}$")

type Finding struct {
	Source   string
	Key      string
	PlayerID uint64
	Reason   string
}

type AuditSummary struct {
	mu       sync.Mutex
	Nodes    int
	Records  int
	Skipped  int
	Findings []Finding
}

func (s *AuditSummary) nodeStarted() {
	s.mu.Lock()
	s.Nodes++
	s.mu.Unlock()
}

func (s *AuditSummary) recordScanned() {
	s.mu.Lock()
	s.Records++
	s.mu.Unlock()
}

func (s *AuditSummary) keySkipped() {
	s.mu.Lock()
	s.Skipped++
	s.mu.Unlock()
}

func (s *AuditSummary) addFinding(f Finding) {
	s.mu.Lock()
	s.Findings = append(s.Findings, f)
	s.mu.Unlock()
}

func AuditRedis(
	ctx context.Context,
	rdb redis.UniversalClient,
	scanCount int64,
	summary *AuditSummary,
) error {
	if rdb == nil || scanCount <= 0 || summary == nil {
		return fmt.Errorf("placement preflight requires Redis, positive scan count and summary")
	}
	if cluster, ok := rdb.(*redis.ClusterClient); ok {
		// ClusterClient.Scan only visits one shard. ForEachMaster is mandatory:
		// placement keys are player-hash-tagged and distributed across masters.
		return cluster.ForEachMaster(ctx, func(nodeCtx context.Context, node *redis.Client) error {
			source := node.Options().Addr
			if source == "" {
				source = "redis-cluster-master"
			}
			if err := scanRedisNode(nodeCtx, node, source, scanCount, summary); err != nil {
				return fmt.Errorf("master %s: %w", source, err)
			}
			return nil
		})
	}
	return scanRedisNode(ctx, rdb, "redis-primary", scanCount, summary)
}

type redisScanner interface {
	Scan(context.Context, uint64, string, int64) *redis.ScanCmd
	Get(context.Context, string) *redis.StringCmd
}

func scanRedisNode(
	ctx context.Context,
	node redisScanner,
	source string,
	scanCount int64,
	summary *AuditSummary,
) error {
	summary.nodeStarted()
	var cursor uint64
	for {
		keys, next, err := node.Scan(ctx, cursor, placementScanPattern, scanCount).Result()
		if err != nil {
			return fmt.Errorf("SCAN %q cursor=%d: %w", placementScanPattern, cursor, err)
		}
		for _, key := range keys {
			playerID, recordKey, keyErr := ParsePlacementRecordKey(key)
			if keyErr != nil {
				summary.addFinding(Finding{Source: source, Key: key, Reason: keyErr.Error()})
				continue
			}
			if !recordKey {
				summary.keySkipped()
				continue
			}
			body, err := node.Get(ctx, key).Bytes()
			if errors.Is(err, redis.Nil) {
				return fmt.Errorf("durable placement key %q disappeared during audit", key)
			}
			if err != nil {
				return fmt.Errorf("GET %q: %w", key, err)
			}
			rec := new(locatorv1.PlayerPlacementStorageRecord)
			if err := proto.Unmarshal(body, rec); err != nil {
				summary.addFinding(Finding{Source: source, Key: key, PlayerID: playerID,
					Reason: "protobuf decode failed: " + err.Error()})
				continue
			}
			summary.recordScanned()
			for _, reason := range ClassifyPlacement(playerID, rec) {
				summary.addFinding(Finding{Source: source, Key: key, PlayerID: playerID, Reason: reason})
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

func ParsePlacementRecordKey(key string) (uint64, bool, error) {
	match := placementRecordKey.FindStringSubmatch(key)
	if len(match) == 2 {
		playerID, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil {
			return 0, true, fmt.Errorf("invalid placement key player_id: %w", err)
		}
		return playerID, true, nil
	}
	// These are separate, non-record keyspaces that intentionally share the
	// broad placement prefix. They are scanned but never decoded as records.
	if strings.HasPrefix(key, "pandora:placement:proof:") ||
		strings.HasPrefix(key, "pandora:placement:fence:") {
		return 0, false, nil
	}
	return 0, false, fmt.Errorf("unexpected key shape under placement namespace")
}

func parsePlacementRecordKey(key string) (uint64, bool, error) {
	return ParsePlacementRecordKey(key)
}
