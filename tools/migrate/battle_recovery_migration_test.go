package main

import (
	"strings"
	"testing"
)

func TestPandoraBattleRecoveryMigrationsStayAdditive(t *testing.T) {
	version, err := latestMigrationVersion("pandora_battle")
	if err != nil {
		t.Fatalf("latestMigrationVersion: %v", err)
	}
	if version != 5 {
		t.Fatalf("pandora_battle latest version=%d, want 5", version)
	}

	v3 := readEmbeddedMigration(t, "migrations/pandora_battle/000003_match_release_outbox.up.sql")
	if strings.Contains(v3, "battle_exit_proof_outbox") {
		t.Fatal("already-versioned 000003 must remain immutable; battle exit proof belongs to 000004")
	}
	v4 := readEmbeddedMigration(t, "migrations/pandora_battle/000004_battle_exit_proof_outbox.up.sql")
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS `battle_exit_proof_outbox`",
		"UNIQUE KEY `uk_battle_exit_match_player` (`match_id`, `player_id`)",
		"KEY `idx_battle_exit_due` (`superseded_at_ms`, `next_attempt_at_ms`, `id`)",
	} {
		if !strings.Contains(v4, fragment) {
			t.Fatalf("000004 up missing contract fragment %q", fragment)
		}
	}
	down := readEmbeddedMigration(t, "migrations/pandora_battle/000004_battle_exit_proof_outbox.down.sql")
	if !strings.Contains(down, "DROP TABLE IF EXISTS `battle_exit_proof_outbox`") ||
		strings.Contains(down, "match_release_outbox") {
		t.Fatal("000004 down must roll back only battle_exit_proof_outbox")
	}

	// 000005 实时进度通道:水位表须带单场累计上限列,出箱表须带失败退避列
	// (battle_result 启动 schema gate 逐列探测,契约漂移在此拦住)。
	v5 := readEmbeddedMigration(t, "migrations/pandora_battle/000005_battle_progress.up.sql")
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS `battle_progress_stream`",
		"`total_exp`",
		"`total_items`",
		"CREATE TABLE IF NOT EXISTS `battle_progress_outbox`",
		"`next_attempt_at_ms`",
		"`attempt_count`",
		"UNIQUE KEY `uk_match_seq_player_kind` (`match_id`, `seq`, `player_id`, `kind`)",
		"KEY `idx_progress_due` (`next_attempt_at_ms`, `id`)",
	} {
		if !strings.Contains(v5, fragment) {
			t.Fatalf("000005 up missing contract fragment %q", fragment)
		}
	}
}

// TestPandoraPlayerExperienceMigrationIsInitSafe 保证 pandora_player 000002 与
// deploy/mysql-init fresh-init 双路兼容:init 已建 exp 列时条件加列必须跳过而不是
// duplicate column 失败(审计 P1)。
func TestPandoraPlayerExperienceMigrationIsInitSafe(t *testing.T) {
	version, err := latestMigrationVersion("pandora_player")
	if err != nil {
		t.Fatalf("latestMigrationVersion: %v", err)
	}
	if version != 2 {
		t.Fatalf("pandora_player latest version=%d, want 2", version)
	}
	v2 := readEmbeddedMigration(t, "migrations/pandora_player/000002_experience.up.sql")
	for _, fragment := range []string{
		"information_schema.COLUMNS", // 条件加列:fresh-init 已建列时跳过
		"ALGORITHM=INSTANT",          // 在线 DDL 显式声明,不能静默退化成锁表拷贝
		"CREATE TABLE IF NOT EXISTS `exp_history`",
		"CREATE TABLE IF NOT EXISTS `player_push_outbox`",
	} {
		if !strings.Contains(v2, fragment) {
			t.Fatalf("000002 up missing contract fragment %q", fragment)
		}
	}
	if !strings.Contains(v2, "PREPARE") {
		t.Fatal("ALTER ADD COLUMN must go through conditional PREPARE/EXECUTE; bare ALTER breaks fresh-init databases")
	}
}
