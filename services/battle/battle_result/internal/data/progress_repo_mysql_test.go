// progress_repo_mysql_test.go — 实时进度累计上限与水位 CAS 的真实 MySQL 回归。
package data

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

// openBattleProgressPeerDB 复用 openBattleRetentionDB 创建的严格随机临时库，再建立
// 一个独立、单物理连接的连接池。连接 ID 必须不同，确保测试确实模拟两个服务副本，
// 而不是同一连接上的顺序调用。
func openBattleProgressPeerDB(t *testing.T, primary *sql.DB) *sql.DB {
	t.Helper()
	primary.SetMaxOpenConns(1)
	primary.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var dbName string
	if err := primary.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&dbName); err != nil {
		t.Fatalf("读取随机测试库名: %v", err)
	}
	if !battleRetentionTestDBPattern.MatchString(dbName) {
		t.Fatalf("主连接不在受保护的随机测试库: %q", dbName)
	}

	cfg, err := mysql.ParseDSN(os.Getenv("PANDORA_TEST_MYSQL_DSN"))
	if err != nil {
		t.Fatalf("解析第二连接 DSN: %v", err)
	}
	if cfg.DBName != "" {
		t.Fatalf("PANDORA_TEST_MYSQL_DSN 禁止带 database, got=%q", cfg.DBName)
	}
	cfg.DBName = dbName
	cfg.ParseTime = true
	cfg.Timeout = 5 * time.Second
	peer, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("打开第二连接: %v", err)
	}
	peer.SetMaxOpenConns(1)
	peer.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = peer.Close() })
	if err := peer.PingContext(ctx); err != nil {
		t.Fatalf("连接随机测试库的第二连接: %v", err)
	}

	var primaryID, peerID uint64
	if err := primary.QueryRowContext(ctx, `SELECT CONNECTION_ID()`).Scan(&primaryID); err != nil {
		t.Fatalf("读取主连接 ID: %v", err)
	}
	if err := peer.QueryRowContext(ctx, `SELECT CONNECTION_ID()`).Scan(&peerID); err != nil {
		t.Fatalf("读取第二连接 ID: %v", err)
	}
	if primaryID == peerID {
		t.Fatalf("测试未建立两个独立 MySQL 连接: connection_id=%d", primaryID)
	}
	return peer
}

// TestApplyProgressPlayerCapAndStaleCAS_MySQL 控制交错如下：连接 A 读取 seq=1 的
// 水位；连接 B 把同一玩家累计推进到上限并提交 seq=2；连接 A 再携带旧水位重放。
// 旧请求必须先在 CAS 处得到 UNAVAILABLE，不能把竞争误判为永久超限；真正基于最新
// 水位的超限批次则必须返回 INVALID_ARG，并整体回滚水位、玩家累计和 outbox。
func TestApplyProgressPlayerCapAndStaleCAS_MySQL(t *testing.T) {
	primary := openBattleRetentionDB(t)
	peer := openBattleProgressPeerDB(t, primary)
	repoA := NewMySQLBattleRepo(primary)
	repoB := NewMySQLBattleRepo(peer)
	ctx := context.Background()

	const (
		matchID  = uint64(88001)
		playerID = uint64(99001)
	)
	caps := ProgressCaps{
		MatchExp: 1000, MatchItems: 100,
		PlayerExp: 100, PlayerItems: 100, PlayerKills: 100,
	}
	row := func(seq, exp uint64) []ProgressOutboxRecord {
		return []ProgressOutboxRecord{{
			Seq: seq, PlayerID: playerID, Kind: ProgressGrantExp, ExpDelta: exp,
		}}
	}

	if err := repoA.ApplyProgress(ctx, matchID, 0, 1, 90, 0,
		[]ProgressPlayerDelta{{PlayerID: playerID, Exp: 90}}, row(1, 90), caps); err != nil {
		t.Fatalf("建立 seq=1 初始进度: %v", err)
	}
	stale, err := repoA.GetProgressWatermark(ctx, matchID)
	if err != nil || !stale.Existed || stale.LastAppliedSeq != 1 || stale.TotalExp != 90 {
		t.Fatalf("连接 A 读取旧水位: %+v err=%v", stale, err)
	}

	// 连接 B 在 A 保留旧快照期间提交，把单玩家累计精确推到上限。
	if err := repoB.ApplyProgress(ctx, matchID, stale.LastAppliedSeq, 2, 10, 0,
		[]ProgressPlayerDelta{{PlayerID: playerID, Exp: 10}}, row(2, 10), caps); err != nil {
		t.Fatalf("连接 B 推进 seq=2: %v", err)
	}

	// A 重放同一批；正确分类是水位竞争，而不是把已由 B 计入的 delta 再算一次后报超限。
	err = repoA.ApplyProgress(ctx, matchID, stale.LastAppliedSeq, 2, 10, 0,
		[]ProgressPlayerDelta{{PlayerID: playerID, Exp: 10}}, row(2, 10), caps)
	if errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("旧水位重放 code=%d err=%v, want ErrUnavailable", errcode.As(err), err)
	}
	assertProgressMySQLState(t, primary, repoA, matchID, playerID, 2, 100, 2)

	// 重新读取最新水位后提交真实超限的新批，必须永久拒绝且事务零副作用。
	fresh, err := repoA.GetProgressWatermark(ctx, matchID)
	if err != nil || fresh.LastAppliedSeq != 2 {
		t.Fatalf("读取最新水位: %+v err=%v", fresh, err)
	}
	err = repoA.ApplyProgress(ctx, matchID, fresh.LastAppliedSeq, 3, 1, 0,
		[]ProgressPlayerDelta{{PlayerID: playerID, Exp: 1}}, row(3, 1), caps)
	if errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("真实单玩家超限 code=%d err=%v, want ErrInvalidArg", errcode.As(err), err)
	}
	assertProgressMySQLState(t, primary, repoA, matchID, playerID, 2, 100, 2)
}

func assertProgressMySQLState(t *testing.T, db *sql.DB, repo *MySQLBattleRepo,
	matchID, playerID, wantSeq, wantPlayerExp uint64, wantOutbox int,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wm, err := repo.GetProgressWatermark(ctx, matchID)
	if err != nil || wm.LastAppliedSeq != wantSeq || wm.TotalExp != wantPlayerExp {
		t.Fatalf("水位状态 %+v err=%v, want seq=%d total_exp=%d", wm, err, wantSeq, wantPlayerExp)
	}
	var playerExp uint64
	if err := db.QueryRowContext(ctx,
		`SELECT total_exp FROM battle_progress_player WHERE match_id = ? AND player_id = ?`,
		matchID, playerID).Scan(&playerExp); err != nil {
		t.Fatalf("读取单玩家累计: %v", err)
	}
	if playerExp != wantPlayerExp {
		t.Fatalf("player total_exp=%d, want %d", playerExp, wantPlayerExp)
	}
	var outbox int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM battle_progress_outbox WHERE match_id = ?`, matchID).Scan(&outbox); err != nil {
		t.Fatalf("统计进度 outbox: %v", err)
	}
	if outbox != wantOutbox {
		t.Fatalf("progress outbox rows=%d, want %d", outbox, wantOutbox)
	}
}
