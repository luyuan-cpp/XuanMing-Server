package data

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
)

const battleKeyScanPattern = "pandora:ds:battle:{*}"

type battleRedisScanner interface {
	Scan(context.Context, uint64, string, int64) *redis.ScanCmd
	Get(context.Context, string) *redis.StringCmd
	PTTL(context.Context, string) *redis.DurationCmd
}

type activeBattleCandidate struct {
	matchID uint64
	score   int64
}

func activeIndexRequired(state string, persistent bool) (bool, error) {
	switch state {
	case "allocating", "warming", "ready", "running",
		BattleStateAllocationUncertain,
		BattleStateAllocationReconcileReleasePending,
		BattleStateAllocationReconcileEmptyTombstone,
		BattleStatePreactiveReleasePending,
		BattleStateAllocationAbortPending:
		return true, nil
	case "abandoned":
		// Model-B keeps a terminal record persistent until physical release and
		// lifecycle publication both ACK. Once ACKed, ExpireBattle gives it a TTL
		// and removes active; a reconciler must not resurrect that retained audit.
		return persistent, nil
	case "ended":
		return false, nil
	default:
		return false, fmt.Errorf("unknown canonical battle state %q", state)
	}
}

func parseBattleIDFromKey(key string) (uint64, error) {
	const prefix = "pandora:ds:battle:{"
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, "}") {
		return 0, fmt.Errorf("invalid canonical battle key %q", key)
	}
	id, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(key, prefix), "}"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid canonical battle id in key %q: %w", key, err)
	}
	if id == 0 {
		return 0, fmt.Errorf("invalid canonical battle id in key %q: zero is reserved", key)
	}
	return id, nil
}

func scanBattleCandidates(ctx context.Context, node battleRedisScanner, count int64) ([]activeBattleCandidate, error) {
	if count <= 0 {
		count = 128
	}
	var out []activeBattleCandidate
	for cursor := uint64(0); ; {
		keys, next, err := node.Scan(ctx, cursor, battleKeyScanPattern, count).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			matchID, err := parseBattleIDFromKey(key)
			if err != nil {
				return nil, err
			}
			payload, err := node.Get(ctx, key).Bytes()
			if err == redis.Nil {
				continue
			}
			if err != nil {
				return nil, err
			}
			record, err := unmarshalBattle(matchID, payload)
			if err != nil {
				return nil, err
			}
			pttl, err := node.PTTL(ctx, key).Result()
			if err != nil {
				return nil, err
			}
			if pttl == -2 {
				continue
			}
			required, err := activeIndexRequired(record.GetState(), pttl == -1)
			if err != nil {
				return nil, fmt.Errorf("battle %d: %w", matchID, err)
			}
			if required {
				out = append(out, activeBattleCandidate{matchID: matchID, score: record.GetLastHeartbeatMs()})
			}
		}
		cursor = next
		if cursor == 0 {
			return out, nil
		}
	}
}

// ReconcileBattleActiveIndex scans every Redis Cluster master. Calling SCAN on
// UniversalClient directly would inspect only one shard and permanently miss
// recovery records in other hash slots.
func (r *RedisBattleRepo) ReconcileBattleActiveIndex(ctx context.Context, count int64) error {
	if r == nil || r.rdb == nil {
		return fmt.Errorf("battle active reconciler redis unavailable")
	}
	var (
		mu         sync.Mutex
		candidates []activeBattleCandidate
	)
	scanNode := func(node battleRedisScanner) error {
		found, err := scanBattleCandidates(ctx, node, count)
		if err != nil {
			return err
		}
		mu.Lock()
		candidates = append(candidates, found...)
		mu.Unlock()
		return nil
	}
	if cluster, ok := r.rdb.(*redis.ClusterClient); ok {
		if err := cluster.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
			return scanNode(node)
		}); err != nil {
			return err
		}
	} else {
		if err := scanNode(r.rdb); err != nil {
			return err
		}
	}
	for _, candidate := range candidates {
		if err := r.rdb.ZAddArgs(ctx, activeKey, redis.ZAddArgs{
			NX: true,
			Members: []redis.Z{{
				Score: float64(candidate.score), Member: candidate.matchID,
			}},
		}).Err(); err != nil {
			return err
		}
	}
	return nil
}
