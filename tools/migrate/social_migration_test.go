package main

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

const (
	socialBaselinePath = "migrations/pandora_social/000001_baseline.up.sql"
	socialV2UpPath     = "migrations/pandora_social/000002_guild_counter_tables.up.sql"
	socialV2DownPath   = "migrations/pandora_social/000002_guild_counter_tables.down.sql"
	socialV3UpPath     = "migrations/pandora_social/000003_mail_growth_bounded.up.sql"
	socialV3DownPath   = "migrations/pandora_social/000003_mail_growth_bounded.down.sql"
)

func TestPandoraSocialV2MigrationContract(t *testing.T) {
	version, err := latestMigrationVersion("pandora_social")
	if err != nil {
		t.Fatalf("latestMigrationVersion: %v", err)
	}
	if version != 3 {
		t.Fatalf("pandora_social latest version=%d, 期望=3", version)
	}

	baseline := readEmbeddedMigration(t, socialBaselinePath)
	if strings.Contains(baseline, "pending_request_count") || strings.Contains(baseline, "player_group_counts") {
		t.Fatal("000001 baseline 必须保持 immutable；新计数结构只能存在于 000002")
	}
	if strings.Contains(baseline, "player_mail_archive") || strings.Contains(baseline, "idx_expire") {
		t.Fatal("000001 baseline 必须保持 immutable；邮件清理结构只能存在于 000003")
	}

	up := readEmbeddedMigration(t, socialV2UpPath)
	requiredFragments := []string{
		"ADD COLUMN `pending_request_count`",
		"CREATE TABLE IF NOT EXISTS `player_group_counts`",
		"FROM `guild_join_requests`",
		"WHERE `status` = 1",
		"SET g.`pending_request_count` = COALESCE(p.`pending_count`, 0)",
		"FROM `chat_group_members`",
		"ON DUPLICATE KEY UPDATE `group_count` = VALUES(`group_count`)",
	}
	for _, fragment := range requiredFragments {
		if !strings.Contains(up, fragment) {
			t.Fatalf("000002 up 缺少契约片段 %q", fragment)
		}
	}

	assertAdditiveOnlyDown(t, "000002", readEmbeddedMigration(t, socialV2DownPath))

	upV3 := readEmbeddedMigration(t, socialV3UpPath)
	requiredV3Fragments := []string{
		"ADD INDEX `idx_expire` (`expire_ms`)",
		"ADD INDEX `idx_end` (`end_ms`)",
		"ADD INDEX `idx_mail` (`mail_id`)",
		"CREATE TABLE IF NOT EXISTS `player_mail_archive`",
		"information_schema.statistics",
	}
	for _, fragment := range requiredV3Fragments {
		if !strings.Contains(upV3, fragment) {
			t.Fatalf("000003 up 缺少契约片段 %q", fragment)
		}
	}
	assertAdditiveOnlyDown(t, "000003", readEmbeddedMigration(t, socialV3DownPath))
}

// assertAdditiveOnlyDown 校验 down 迁移为 additive-only no-op(不在线删除兼容结构)。
func assertAdditiveOnlyDown(t *testing.T, version, down string) {
	t.Helper()
	var executableDownLines []string
	for _, line := range strings.Split(down, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			executableDownLines = append(executableDownLines, trimmed)
		}
	}
	executableDown := strings.ToUpper(strings.Join(executableDownLines, "\n"))
	if strings.Contains(executableDown, "DROP TABLE") || strings.Contains(executableDown, "DROP COLUMN") || strings.Contains(executableDown, "DROP INDEX") {
		t.Fatalf("%s down 必须是 additive-only no-op，不能在线删除兼容结构", version)
	}
}

func TestPandoraSocialV2MigratesLegacyRowsAcrossBackends(t *testing.T) {
	backends := []struct {
		name string
		env  string
		tidb bool
	}{
		{name: "mysql", env: "PANDORA_TEST_MYSQL_DSN"},
		{name: "tidb", env: "PANDORA_TEST_TIDB_DSN", tidb: true},
	}
	for _, backend := range backends {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			dsn := os.Getenv(backend.env)
			if dsn == "" {
				t.Skipf("跳过真实 %s 迁移测试:未设 %s", backend.name, backend.env)
			}
			for _, precreateFinalSchema := range []bool{false, true} {
				name := "legacy_missing_counters"
				if precreateFinalSchema {
					name = "fresh_schema_idempotent"
				}
				t.Run(name, func(t *testing.T) {
					target, db := setupSocialMigrationDB(t, dsn, precreateFinalSchema, backend.tidb)

					if err := migrateTarget(target, false); err != nil {
						t.Fatalf("migrateTarget first run: %v", err)
					}
					assertSocialCounterBackfill(t, db)

					// 已完成目标重跑必须保持 clean version=2，不能重复破坏 backfill。
					if err := migrateTarget(target, false); err != nil {
						t.Fatalf("migrateTarget second run: %v", err)
					}
					assertSocialCounterBackfill(t, db)
				})
			}
		})
	}
}

func readEmbeddedMigration(t *testing.T, path string) string {
	t.Helper()
	b, err := fs.ReadFile(migrationsFS, path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return string(b)
}

func setupSocialMigrationDB(t *testing.T, dsn string, precreateFinalSchema, isTiDB bool) (migrationTarget, *sql.DB) {
	t.Helper()
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("解析 PANDORA_TEST_MYSQL_DSN: %v", err)
	}
	adminCfg := *cfg
	adminCfg.DBName = ""
	admin, err := sql.Open("mysql", adminCfg.FormatDSN())
	if err != nil {
		t.Fatalf("打开 MySQL 管理连接: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Fatalf("已提供 PANDORA_TEST_MYSQL_DSN 但无法连接: %v", err)
	}

	dbName := fmt.Sprintf("pandora_social_mig_it_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+dbName+"` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		_ = admin.Close()
		t.Fatalf("创建迁移测试库: %v", err)
	}
	var db *sql.DB
	t.Cleanup(func() {
		if db != nil {
			_ = db.Close()
		}
		_, _ = admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
		_ = admin.Close()
	})

	testCfg := *cfg
	testCfg.DBName = dbName
	testCfg.MultiStatements = true
	if isTiDB {
		if testCfg.Params == nil {
			testCfg.Params = make(map[string]string)
		}
		// golang-migrate/mysql 以 SERIALIZABLE 开事务；TiDB 需显式接受并降级为其
		// 支持的悲观隔离语义，生产 TiDB migration DSN 也必须带同一 session 参数。
		testCfg.Params["tidb_skip_isolation_level_check"] = "1"
	}
	db, err = sql.Open("mysql", testCfg.FormatDSN())
	if err != nil {
		t.Fatalf("打开迁移测试库: %v", err)
	}
	if _, err := db.ExecContext(ctx, readEmbeddedMigration(t, socialBaselinePath)); err != nil {
		t.Fatalf("执行 legacy baseline: %v", err)
	}
	if precreateFinalSchema {
		precreateSocialCounterSchema(t, ctx, db)
	}
	seedLegacySocialRows(t, ctx, db, precreateFinalSchema)

	dsnPath := filepath.Join(t.TempDir(), "social.dsn")
	if err := os.WriteFile(dsnPath, []byte(testCfg.FormatDSN()), 0o600); err != nil {
		t.Fatalf("写测试 DSN: %v", err)
	}
	version, err := latestMigrationVersion("pandora_social")
	if err != nil {
		t.Fatalf("读取 pandora_social latest version: %v", err)
	}
	target := migrationTarget{
		Name:                     "social-it",
		MigrationSet:             "pandora_social",
		Database:                 dbName,
		DSNFile:                  dsnPath,
		TimeoutSeconds:           120,
		LockWaitTimeoutSeconds:   10,
		expectedMigrationVersion: version,
	}
	return target, db
}

func precreateSocialCounterSchema(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	statements := []string{
		`ALTER TABLE guilds ADD COLUMN pending_request_count INT NOT NULL DEFAULT 0 AFTER member_count`,
		`CREATE TABLE player_group_counts (
			player_id BIGINT UNSIGNED NOT NULL,
			group_count INT NOT NULL DEFAULT 0,
			PRIMARY KEY (player_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,
		// v3 邮件清理结构预建(docker-init fresh schema 形态,验证 000003 幂等)
		`ALTER TABLE player_mail ADD INDEX idx_expire (expire_ms)`,
		`ALTER TABLE sys_mail ADD INDEX idx_end (end_ms)`,
		`ALTER TABLE guild_mail ADD INDEX idx_end (end_ms)`,
		`ALTER TABLE player_mail_claim ADD INDEX idx_mail (mail_id)`,
		`CREATE TABLE player_mail_archive (
			mail_id BIGINT UNSIGNED NOT NULL,
			player_id BIGINT UNSIGNED NOT NULL,
			status TINYINT NOT NULL,
			expire_ms BIGINT NOT NULL,
			created_ms BIGINT NOT NULL,
			payload BLOB NOT NULL,
			archived_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (mail_id),
			KEY idx_player (player_id),
			KEY idx_archived (archived_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci`,
	}
	for _, stmt := range statements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("预建 final schema: %v", err)
		}
	}
}

func seedLegacySocialRows(t *testing.T, ctx context.Context, db *sql.DB, precreated bool) {
	t.Helper()
	seed := `
INSERT INTO guilds (guild_id, name, leader_id, member_count, max_members) VALUES
  (1001, 'guild-1001', 11, 1, 100),
  (1002, 'guild-1002', 12, 1, 100);
INSERT INTO guild_join_requests (request_id, guild_id, player_id, status) VALUES
  (2001, 1001, 21, 1),
  (2002, 1001, 22, 1),
  (2003, 1001, 23, 2),
  (2004, 1002, 24, 3);
INSERT INTO chat_groups (group_id, name, owner_id, member_count, max_members) VALUES
  (3001, 'group-3001', 31, 2, 50),
  (3002, 'group-3002', 32, 2, 50);
INSERT INTO chat_group_members (group_id, player_id, role) VALUES
  (3001, 41, 1),
  (3001, 42, 2),
  (3002, 41, 2),
  (3002, 43, 1);`
	if _, err := db.ExecContext(ctx, seed); err != nil {
		t.Fatalf("写入 legacy 数据: %v", err)
	}
	if precreated {
		if _, err := db.ExecContext(ctx, `
UPDATE guilds SET pending_request_count = 99;
INSERT INTO player_group_counts (player_id, group_count) VALUES
  (41, 99),
  (999, 7);`); err != nil {
			t.Fatalf("写入错误预建计数: %v", err)
		}
	}
}

func assertSocialCounterBackfill(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wantGuild := map[uint64]int{1001: 2, 1002: 0}
	rows, err := db.QueryContext(ctx, `SELECT guild_id, pending_request_count FROM guilds ORDER BY guild_id`)
	if err != nil {
		t.Fatalf("读取 guild backfill: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var guildID uint64
		var count int
		if err := rows.Scan(&guildID, &count); err != nil {
			t.Fatalf("扫描 guild backfill: %v", err)
		}
		if want, ok := wantGuild[guildID]; !ok || count != want {
			t.Fatalf("guild %d pending_request_count=%d, 期望=%d", guildID, count, want)
		}
		delete(wantGuild, guildID)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 guild backfill: %v", err)
	}
	if len(wantGuild) != 0 {
		t.Fatalf("缺少 guild backfill: %v", wantGuild)
	}

	wantPlayers := map[uint64]int{41: 2, 42: 1, 43: 1}
	groupRows, err := db.QueryContext(ctx, `SELECT player_id, group_count FROM player_group_counts ORDER BY player_id`)
	if err != nil {
		t.Fatalf("读取 group backfill: %v", err)
	}
	defer func() { _ = groupRows.Close() }()
	for groupRows.Next() {
		var playerID uint64
		var count int
		if err := groupRows.Scan(&playerID, &count); err != nil {
			t.Fatalf("扫描 group backfill: %v", err)
		}
		if want, ok := wantPlayers[playerID]; !ok || count != want {
			t.Fatalf("player %d group_count=%d, 期望=%d", playerID, count, want)
		}
		delete(wantPlayers, playerID)
	}
	if err := groupRows.Err(); err != nil {
		t.Fatalf("遍历 group backfill: %v", err)
	}
	if len(wantPlayers) != 0 {
		t.Fatalf("缺少 player group backfill: %v", wantPlayers)
	}

	var version uint
	var dirty bool
	if err := db.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty); err != nil {
		t.Fatalf("读取 schema_migrations: %v", err)
	}
	wantVersion, err := latestMigrationVersion("pandora_social")
	if err != nil {
		t.Fatalf("latestMigrationVersion: %v", err)
	}
	if version != wantVersion || dirty {
		t.Fatalf("schema_migrations version=%d dirty=%v, 期望 version=%d dirty=false", version, dirty, wantVersion)
	}

	// v3 邮件清理结构:归档表 + 各清理索引必须就位(legacy 与 fresh 两形态跑完等价)
	var archiveTables int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'player_mail_archive'`).Scan(&archiveTables); err != nil {
		t.Fatalf("查询 player_mail_archive: %v", err)
	}
	if archiveTables != 1 {
		t.Fatal("v3 迁移后缺少 player_mail_archive 表")
	}
	wantIndexes := [][2]string{
		{"player_mail", "idx_expire"},
		{"sys_mail", "idx_end"},
		{"guild_mail", "idx_end"},
		{"player_mail_claim", "idx_mail"},
	}
	for _, ti := range wantIndexes {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT index_name) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`,
			ti[0], ti[1]).Scan(&n); err != nil {
			t.Fatalf("查询索引 %s.%s: %v", ti[0], ti[1], err)
		}
		if n != 1 {
			t.Fatalf("v3 迁移后缺少索引 %s.%s", ti[0], ti[1])
		}
	}
}
