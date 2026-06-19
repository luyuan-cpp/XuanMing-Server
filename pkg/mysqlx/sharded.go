// 本文件实现 ShardSet:按 snowflake 业务 ID 水平分库的只读路由器。
//
// 背景(docs/design/scale-dau-2m.md):DAU 200万 / 千万注册量级下,单 MySQL 实例的写吞吐
// 和单表行数都会触顶。Pandora 的业务 ID 已统一 uint64 snowflake(CLAUDE.md §5.5),天然
// 自带均匀分布的分片键,按 player_id 水平分库分表即可。
//
// ShardSet 只做"按 ID 选库",不做跨库聚合 / 分布式事务 / 重均衡:
//   - 选库公式固定 shard = id % N,N 一旦定稿不可随意改(改 N 会让历史数据 rehash,代价极高);
//     扩容走"翻倍 + 双写迁移"或预分配逻辑分片,详见 scale-dau-2m.md §MySQL。
//   - 同一业务实体的相关行必须用同一分片键(player_id),避免单次业务跨库。
//   - 跨玩家的聚合查询(排行榜等)不要打散到分库扫描,走单独的离线/缓存路径。
package mysqlx

import (
	"database/sql"
	"fmt"

	"github.com/luyuancpp/pandora/pkg/config"
)

// ShardSet 持有 N 个分库连接,按业务 ID 路由。线程安全(*sql.DB 本身并发安全)。
type ShardSet struct {
	shards []*sql.DB
}

// NewShardSet 按 config.MySQLConf.Shards(DSN 列表)构造分库集合。
//
// 每个分片用本结构的同名池参数(MaxOpenConns 等)构造并 Ping 验证;任一分片建不起来则整体失败
// 并回滚已建连接,不返回半残的 ShardSet。Shards 为空时返回错误(单库请直接用 MustNewClient)。
func NewShardSet(c config.MySQLConf) (*ShardSet, error) {
	if len(c.Shards) == 0 {
		return nil, fmt.Errorf("mysqlx.NewShardSet: empty shards (use MustNewClient for single DB)")
	}

	shards := make([]*sql.DB, 0, len(c.Shards))
	for i, dsn := range c.Shards {
		shardConf := c // 复制池参数,仅替换 DSN
		shardConf.DSN = dsn
		shardConf.Shards = nil
		db, err := NewClient(shardConf)
		if err != nil {
			for _, opened := range shards {
				_ = opened.Close()
			}
			return nil, fmt.Errorf("mysqlx.NewShardSet: shard %d: %w", i, err)
		}
		shards = append(shards, db)
	}
	return &ShardSet{shards: shards}, nil
}

// Count 返回分片数 N。
func (s *ShardSet) Count() int { return len(s.shards) }

// For 按业务 ID 选库:shard = id % N。同一 ID 永远落同一分库。
//
// 调用方拿到 *sql.DB 后照常写 SQL;同一次业务操作涉及的所有行必须用同一个 id 选库,
// 不要在一次事务里跨 For 返回的不同 *sql.DB(那不是同一连接,无法单库事务)。
func (s *ShardSet) For(id uint64) *sql.DB {
	return s.shards[id%uint64(len(s.shards))]
}

// Shard 按分片下标直接取库,用于运维 / 迁移 / 全分片巡检(如建表、统计)。
func (s *ShardSet) Shard(i int) *sql.DB { return s.shards[i] }

// All 返回全部分库,用于需要广播执行的场景(DDL、全分片巡检)。
// ⚠️ 不要用 All 做业务读写聚合,那会把单次请求放大成 N 倍库压。
func (s *ShardSet) All() []*sql.DB { return s.shards }

// Close 关闭全部分库连接。
func (s *ShardSet) Close() error {
	var firstErr error
	for _, db := range s.shards {
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
