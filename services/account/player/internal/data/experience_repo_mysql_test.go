// experience_repo_mysql_test.go — 满级经验 no-op 收据的真实 MySQL 幂等回归。
package data

import (
	"context"
	"database/sql"
	"testing"
)

// prepareExperienceMySQLSchema 在现有严格随机 attribute fixture 内补齐本测试所需的
// 生产列/表；约束与 deploy/mysql-init/04-player-tables.sql 保持一致。
func prepareExperienceMySQLSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), attributeSetupTimeout)
	defer cancel()
	statements := []string{
		`ALTER TABLE players
			ADD COLUMN level INT NOT NULL DEFAULT 1,
			ADD COLUMN exp BIGINT UNSIGNED NOT NULL DEFAULT 0`,
		`CREATE TABLE exp_history (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
			player_id BIGINT UNSIGNED NOT NULL,
			idempotency_key VARCHAR(64) NOT NULL,
			exp_delta BIGINT UNSIGNED NOT NULL,
			reason VARCHAR(32) NOT NULL DEFAULT '',
			old_level INT NOT NULL,
			old_exp BIGINT UNSIGNED NOT NULL,
			new_level INT NOT NULL,
			new_exp BIGINT UNSIGNED NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (id),
			UNIQUE KEY uk_player_idem (player_id, idempotency_key),
			KEY idx_player_created (player_id, created_at),
			KEY idx_created (created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE player_push_outbox (
			id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
			player_id BIGINT UNSIGNED NOT NULL,
			event_type INT UNSIGNED NOT NULL,
			payload VARBINARY(512) NOT NULL,
			created_at_ms BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("初始化经验测试 schema: %v\nSQL: %s", err, statement)
		}
	}
}

type experienceReceipt struct {
	ID       uint64
	Delta    uint64
	Reason   string
	OldLevel int32
	OldExp   uint64
	NewLevel int32
	NewExp   uint64
}

// TestApplyExperienceMaxLevelNoopReceipt_MySQL 验证满级事件首次消费时虽不修改经验、
// 不产生推送，仍原子落一条稳定幂等收据；相同 key 的普通重试及未来曲线扩容后的
// 迟到重试都只能命中该收据，不能把旧事件重新入账。
func TestApplyExperienceMaxLevelNoopReceipt_MySQL(t *testing.T) {
	f := openAttributeTestDB(t)
	prepareExperienceMySQLSchema(t, f.db)
	repo := NewMySQLPlayerRepo(f.db)
	ctx := context.Background()
	const (
		playerID = uint64(73001)
		idemKey  = "progress:61001:9:73001:exp"
	)
	if _, err := f.db.ExecContext(ctx,
		`INSERT INTO players (player_id, nickname, unspent_attr_points, level, exp)
		 VALUES (?, ?, 0, 3, 0)`, playerID, "exp_max_73001"); err != nil {
		t.Fatalf("创建满级玩家: %v", err)
	}
	apply := ExpApply{
		PlayerID: playerID, Delta: 50, Reason: "monster_kill",
		IdempotencyKey: idemKey, Curve: []uint64{100, 200}, // max level = 3
	}

	first, already, err := repo.ApplyExperience(ctx, apply)
	if err != nil || already {
		t.Fatalf("首次满级 no-op: state=%+v already=%v err=%v", first, already, err)
	}
	if first.Level != 3 || first.ExpInLevel != 0 || !first.IsMaxLevel || first.LevelsGained != 0 {
		t.Fatalf("首次满级 no-op 返回错误状态: %+v", first)
	}
	receipt := readExperienceReceipt(t, f.db, playerID, idemKey)
	if receipt.Delta != apply.Delta || receipt.Reason != apply.Reason ||
		receipt.OldLevel != 3 || receipt.OldExp != 0 || receipt.NewLevel != 3 || receipt.NewExp != 0 {
		t.Fatalf("满级 no-op 收据内容错误: %+v", receipt)
	}
	assertExperienceNoopState(t, f.db, playerID, 3, 0, 1, 0)

	second, already, err := repo.ApplyExperience(ctx, apply)
	if err != nil || !already {
		t.Fatalf("相同 key 重试: state=%+v already=%v err=%v", second, already, err)
	}
	if second.Level != 3 || second.ExpInLevel != 0 || !second.IsMaxLevel || second.LevelsGained != 0 {
		t.Fatalf("相同曲线重试返回错误状态: %+v", second)
	}
	if got := readExperienceReceipt(t, f.db, playerID, idemKey); got != receipt {
		t.Fatalf("相同 key 重试改写收据: before=%+v after=%+v", receipt, got)
	}
	assertExperienceNoopState(t, f.db, playerID, 3, 0, 1, 0)

	// 配置扩容后 Lv3 不再是满级。旧事件仍必须被原 no-op 收据消费，不能补记 50 经验。
	expanded := apply
	expanded.Curve = []uint64{100, 200, 300} // max level = 4
	third, already, err := repo.ApplyExperience(ctx, expanded)
	if err != nil || !already {
		t.Fatalf("曲线扩容后的旧 key 重试: state=%+v already=%v err=%v", third, already, err)
	}
	if third.Level != 3 || third.ExpInLevel != 0 || third.IsMaxLevel || third.LevelsGained != 0 {
		t.Fatalf("曲线扩容重试应返回当前未满级快照且不入账: %+v", third)
	}
	if got := readExperienceReceipt(t, f.db, playerID, idemKey); got != receipt {
		t.Fatalf("曲线扩容重试改写收据: before=%+v after=%+v", receipt, got)
	}
	assertExperienceNoopState(t, f.db, playerID, 3, 0, 1, 0)
}

func readExperienceReceipt(t *testing.T, db *sql.DB, playerID uint64, key string) experienceReceipt {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), attributeSetupTimeout)
	defer cancel()
	var got experienceReceipt
	if err := db.QueryRowContext(ctx, `SELECT id, exp_delta, reason, old_level, old_exp, new_level, new_exp
		FROM exp_history WHERE player_id = ? AND idempotency_key = ?`, playerID, key).Scan(
		&got.ID, &got.Delta, &got.Reason, &got.OldLevel, &got.OldExp, &got.NewLevel, &got.NewExp,
	); err != nil {
		t.Fatalf("读取经验幂等收据: %v", err)
	}
	return got
}

func assertExperienceNoopState(t *testing.T, db *sql.DB, playerID uint64,
	wantLevel int32, wantExp uint64, wantHistory, wantOutbox int,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), attributeSetupTimeout)
	defer cancel()
	var level int32
	var exp uint64
	if err := db.QueryRowContext(ctx,
		`SELECT level, exp FROM players WHERE player_id = ?`, playerID).Scan(&level, &exp); err != nil {
		t.Fatalf("读取玩家经验状态: %v", err)
	}
	if level != wantLevel || exp != wantExp {
		t.Fatalf("玩家经验状态=(level=%d exp=%d), want=(%d,%d)", level, exp, wantLevel, wantExp)
	}
	var history, outbox int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM exp_history WHERE player_id = ?`, playerID).Scan(&history); err != nil {
		t.Fatalf("统计经验收据: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM player_push_outbox WHERE player_id = ?`, playerID).Scan(&outbox); err != nil {
		t.Fatalf("统计经验推送 outbox: %v", err)
	}
	if history != wantHistory || outbox != wantOutbox {
		t.Fatalf("副作用计数 history=%d outbox=%d, want=%d/%d", history, outbox, wantHistory, wantOutbox)
	}
}
