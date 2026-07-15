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
	if version != 4 {
		t.Fatalf("pandora_battle latest version=%d, want 4", version)
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
}
