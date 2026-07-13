// 本文件实现 auction MySQL 分片拓扑的持久启动门禁。
// market_id/owner_id 都按 id%N 路由；N、逻辑下标或 DSN 顺序一旦漂移，历史数据就会被路由丢失。
package data

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	mysql "github.com/go-sql-driver/mysql"
)

const shardTopologySingletonID = 1

// ShardTopologyOptions 是启动时参与 exact-match 的拓扑声明。DSNs 必须与 DBRouter.All 顺序一致。
// AllowBootstrap 只允许“所有 shard 都还没有 marker”的首次双分片登记；已有 marker 不匹配时永不覆盖。
type ShardTopologyOptions struct {
	Generation     string
	DSNs           []string
	AllowBootstrap bool
}

type expectedShardTopology struct {
	generation   string
	topologyHash string
	count        int
	identities   []string
}

type storedShardTopology struct {
	generation   string
	topologyHash string
	count        int
	index        int
	identity     string
}

// ValidateShardTopology 校验并在允许时首次登记 auction 的有序物理分片拓扑。
// marker 表由版本化迁移创建；本函数不偷偷建表，也不允许用 bootstrap 覆盖已存在的不一致。
func ValidateShardTopology(ctx context.Context, router DBRouter, opts ShardTopologyOptions) error {
	expected, err := buildExpectedShardTopology(opts.Generation, opts.DSNs)
	if err != nil {
		return err
	}
	dbs := router.All()
	if len(dbs) != expected.count {
		return fmt.Errorf("auction shard topology router count=%d, dsn count=%d", len(dbs), expected.count)
	}

	present := make([]bool, expected.count)
	presentCount := 0
	for index, db := range dbs {
		stored, found, readErr := readStoredShardTopology(ctx, db)
		if readErr != nil {
			return fmt.Errorf("read auction shard topology index=%d: %w", index, readErr)
		}
		if !found {
			continue
		}
		if err := validateStoredShardTopology(stored, expected, index); err != nil {
			return err
		}
		present[index] = true
		presentCount++
	}
	if presentCount == expected.count {
		return nil
	}
	if presentCount == 0 && expected.count > 1 && !opts.AllowBootstrap {
		return fmt.Errorf("auction shard topology is uninitialized for %d shards; set allow_shard_topology_bootstrap=true for the reviewed first start only", expected.count)
	}

	// 单库首次升级可自动登记；双分片要求显式首次授权。若已有部分 matching marker，说明并发启动
	// 或上次启动在跨库登记中退出，可安全补齐缺片，因为已有 topology_hash 已锁定完整有序身份。
	for index, db := range dbs {
		if present[index] {
			continue
		}
		if err := insertStoredShardTopology(ctx, db, expected, index); err != nil {
			return fmt.Errorf("initialize auction shard topology index=%d: %w", index, err)
		}
		stored, found, err := readStoredShardTopology(ctx, db)
		if err != nil || !found {
			return fmt.Errorf("verify auction shard topology index=%d found=%v: %w", index, found, err)
		}
		if err := validateStoredShardTopology(stored, expected, index); err != nil {
			return err
		}
	}
	return nil
}

func buildExpectedShardTopology(generation string, dsns []string) (*expectedShardTopology, error) {
	generation = strings.TrimSpace(generation)
	if generation == "" || len(generation) > 64 {
		return nil, fmt.Errorf("auction shard topology generation must be 1..64 characters")
	}
	for _, c := range generation {
		if !isTopologyTokenChar(c) {
			return nil, fmt.Errorf("auction shard topology generation contains unsupported character %q", c)
		}
	}
	if len(dsns) == 0 {
		return nil, fmt.Errorf("auction shard topology requires at least one DSN")
	}
	identities := make([]string, 0, len(dsns))
	seen := make(map[string]int, len(dsns))
	for index, dsn := range dsns {
		cfg, err := mysql.ParseDSN(strings.TrimSpace(dsn))
		if err != nil {
			return nil, fmt.Errorf("parse auction shard DSN index=%d: %w", index, err)
		}
		if cfg.DBName == "" {
			return nil, fmt.Errorf("auction shard DSN index=%d must select a database", index)
		}
		network := cfg.Net
		if network == "" {
			network = "tcp"
		}
		identityMaterial := network + "\x00" + strings.ToLower(cfg.Addr) + "\x00" + cfg.DBName
		sum := sha256.Sum256([]byte(identityMaterial))
		identity := hex.EncodeToString(sum[:])
		if previous, ok := seen[identity]; ok {
			return nil, fmt.Errorf("auction shard DSNs %d and %d resolve to the same logical database identity", previous, index)
		}
		seen[identity] = index
		identities = append(identities, identity)
	}
	topologyMaterial := "auction-shard-topology-v1\x00" + generation + "\x00" + strings.Join(identities, "\x00")
	topologySum := sha256.Sum256([]byte(topologyMaterial))
	return &expectedShardTopology{
		generation:   generation,
		topologyHash: hex.EncodeToString(topologySum[:]),
		count:        len(identities),
		identities:   identities,
	}, nil
}

func isTopologyTokenChar(c rune) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
}

func readStoredShardTopology(ctx context.Context, db *sql.DB) (storedShardTopology, bool, error) {
	var stored storedShardTopology
	err := db.QueryRowContext(ctx, `SELECT topology_generation, topology_hash, shard_count, shard_index, shard_identity_hash
		FROM auction_shard_topology WHERE singleton_id = ? LIMIT 1`, shardTopologySingletonID).Scan(
		&stored.generation, &stored.topologyHash, &stored.count, &stored.index, &stored.identity)
	if errors.Is(err, sql.ErrNoRows) {
		return storedShardTopology{}, false, nil
	}
	if err != nil {
		return storedShardTopology{}, false, err
	}
	return stored, true, nil
}

func insertStoredShardTopology(ctx context.Context, db *sql.DB, expected *expectedShardTopology, index int) error {
	_, err := db.ExecContext(ctx, `INSERT IGNORE INTO auction_shard_topology
		(singleton_id, topology_generation, topology_hash, shard_count, shard_index, shard_identity_hash)
		VALUES (?, ?, ?, ?, ?, ?)`,
		shardTopologySingletonID, expected.generation, expected.topologyHash,
		expected.count, index, expected.identities[index])
	return err
}

func validateStoredShardTopology(stored storedShardTopology, expected *expectedShardTopology, index int) error {
	if stored.generation != expected.generation || stored.topologyHash != expected.topologyHash ||
		stored.count != expected.count || stored.index != index || stored.identity != expected.identities[index] {
		return fmt.Errorf("auction shard topology mismatch index=%d: stored generation=%q hash=%s count=%d index=%d identity=%s; expected generation=%q hash=%s count=%d index=%d identity=%s",
			index, stored.generation, stored.topologyHash, stored.count, stored.index, stored.identity,
			expected.generation, expected.topologyHash, expected.count, index, expected.identities[index])
	}
	return nil
}
