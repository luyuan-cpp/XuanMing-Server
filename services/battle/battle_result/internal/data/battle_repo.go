// Package data 是 battle_result 服务的数据层(MySQL 战斗结算落库)。
//
// 库表(deploy/mysql-init/03-battle-tables.sql):
//
//	pandora_battle.battles              对局结算头(PK match_id,幂等键,不变量 §2)
//	pandora_battle.battle_player_stats  玩家战绩 + MMR 变化(uk match_id+player_id)
//
// 幂等:SaveResult 在一个事务里 INSERT battles;命中 1062 唯一键冲突 → 视为已落库
// (alreadyRecorded=true),回滚不重复写 stats。MMR 在 biz 层算好后由本层落库。
package data

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/placement"
	battlev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/battle/v1"
	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
)

// OutboxRecord 是一条待发布的 player.update 事务出箱记录(W4 ⑨,不变量 §4)。
//
// 落 battles + battle_player_stats 的同一事务里写入,二者原子提交;后台发布器轮询
// 出箱表逐条投递 Kafka,投递成功才删行。ID 仅 FetchOutbox 返回时填充。
type OutboxRecord struct {
	ID       int64  // 出箱行主键(SaveResult 入参时忽略,FetchOutbox 返回时填充)
	PlayerID uint64 // kafka key(不变量 §9 同玩家事件保序)
	Payload  []byte // player.v1.PlayerUpdateEvent proto bytes
}

// DropOutboxRecord 是一条待发放的战斗装备掉落事务出箱记录(W5 ④)。
//
// 与 battles + battle_player_stats 同一事务写入(原子提交);后台 RunDropPublisher
// 轮询本表,逐行调 inventory.GrantInstances(幂等键 battle_drop:{match_id}:{player_id}),
// 成功才删行。ItemConfigIDs 已由 biz 层按 drop 白名单过滤(DS 不可信)。
// ID 仅 FetchDropOutbox 返回时填充。
type DropOutboxRecord struct {
	ID            int64    // 出箱行主键(SaveResult 入参时忽略,FetchDropOutbox 返回时填充)
	MatchID       uint64   // 对局 ID(SaveResult 入参时忽略,取 result.MatchId;FetchDropOutbox 返回时填充,组幂等键用)
	PlayerID      uint64   // 受益玩家
	ItemConfigIDs []uint32 // 本局该玩家所获白名单内装备 config id(每个发一件实例)
}

// TerminalReleaseRecord 是正常结算的持久终态回收证明。
//
// 本记录只能由 ReportResult 完成 callback Guard + Redis active 校验后构造，并与
// battles / battle_player_stats 同一 MySQL 事务提交。Auth* 是“服务端校验时刻”的
// 完整凭据证明；relay 时允许该 token 已过期或 gen/jti 已轮换，但 stable identity
// (match/allocation/pod/UID/epoch/writer fence)必须仍精确一致。
type TerminalReleaseRecord struct {
	ID              uint64
	MatchID         uint64
	AllocationID    string
	DSPodName       string
	GameserverUID   string
	InstanceEpoch   uint32
	AuthGen         uint64
	AuthJTI         string
	AuthExpMs       int64
	AuthKid         string
	AuthTokenSHA256 string
	AuthWriterEpoch uint32
	AuthorizedAtMs  int64
	ReleaseAfterMs  int64
	// PlayerIDs is the immutable authoritative roster copied from the canonical
	// BattleStorageRecord at authorization time. It is deliberately not a DS
	// payload field and is not persisted in terminal_release_outbox; SaveResult
	// uses it in the same transaction to build match-release and exit-proof
	// outboxes. This prevents a compromised DS from omitting a player or adding
	// an outsider to recovery side effects.
	PlayerIDs []uint64
	// ReleasedAtMs>0 是阶段1“永久 Redis terminal + UID delete 已明确成功”的
	// MySQL durable ACK。只有该状态才允许阶段2给墓碑设 TTL并删除本行。
	ReleasedAtMs int64
	CreatedAtMs  int64
}

// MatchReleaseRecord 是 battle_result→matchmaker 的持久事务 outbox 行。
// payload 是 MatchReleaseStorageRecord；调用明确成功才 ACK 删除，未知/失败只延期。
type MatchReleaseRecord struct {
	ID              uint64
	OperationID     string
	MatchID         uint64
	PlayerIDs       []uint64
	AttemptCount    uint32
	NextAttemptAtMs int64
	CreatedAtMs     int64
}

// BattleExitProofRecord is a per-player durable terminal proof relay job. The
// initial SaveResult transaction stores the immutable result identity; a worker
// later reads authoritative placement and CAS-persistently fills Proof once.
type BattleExitProofRecord struct {
	ID              uint64
	MatchID         uint64
	PlayerID        uint64
	Proof           placement.BattleExitProof
	Prepared        bool
	AttemptCount    uint32
	NextAttemptAtMs int64
	CreatedAtMs     int64
}

// battleExitProofStorageRecord is intentionally private to battle_result. It
// is an internal MySQL outbox payload, not part of the player/client protocol.
type battleExitProofStorageRecord struct {
	MatchID         uint64 `json:"match_id"`
	PlayerID        uint64 `json:"player_id"`
	ExpectedVersion uint64 `json:"expected_version,omitempty"`
	OperationID     string `json:"operation_id,omitempty"`
	ProofType       int32  `json:"proof_type"`
	ProofID         string `json:"proof_id"`
	Signature       string `json:"signature,omitempty"`
	CreatedAtMs     int64  `json:"created_at_ms"`
}

// BattleRepo 是 battle_result 数据层抽象。biz 层只依赖此接口,不依赖 *sql.DB。
type BattleRepo interface {
	// SaveResult 事务写 battles + battle_player_stats + player_update_outbox +
	// battle_drop_outbox + 可选 terminal_release_outbox。
	// 五者原子提交(不变量 §4:落库、业务出箱、终态资源回收不会半成功)。
	// 幂等:match_id 已存在 → 返回 (true, nil),不重复写(两路出箱也不写)。
	// dropOutbox 可为空(无掉落 / ABANDONED)。
	// terminalRelease 仅正常的、已完成 Model-B 鉴权的同步 ReportResult 非空；ABANDONED
	// 已由 ds_allocator 先回收，不得写 completed 终态行。
	SaveResult(ctx context.Context, result *battlev1.BattleResult, outbox []OutboxRecord, dropOutbox []DropOutboxRecord, terminalRelease *TerminalReleaseRecord) (alreadyRecorded bool, err error)
	// GetResult 读一场对局结算(含全部玩家战绩)。not found → (nil, false, nil)。
	GetResult(ctx context.Context, matchID uint64) (*battlev1.BattleResult, bool, error)
	// ListPlayerHistory 倒序列出玩家参与的对局(ended_at_ms < beforeMs,最多 limit 条)。
	// beforeMs<=0 表示从最新开始。
	ListPlayerHistory(ctx context.Context, playerID uint64, limit int, beforeMs int64) ([]*battlev1.BattleResult, error)
	// FetchOutbox 按 id 升序取最多 limit 条待发布出箱记录(FIFO 保序)。
	FetchOutbox(ctx context.Context, limit int) ([]OutboxRecord, error)
	// DeleteOutbox 删除已成功投递的出箱行。
	DeleteOutbox(ctx context.Context, id int64) error
	// FetchDropOutbox 按 id 升序取最多 limit 条待发放掉落出箱记录(W5 ④)。
	FetchDropOutbox(ctx context.Context, limit int) ([]DropOutboxRecord, error)
	// DeleteDropOutbox 删除已成功发放的掉落出箱行。
	DeleteDropOutbox(ctx context.Context, id int64) error
	// FetchTerminalReleaseOutbox 只取已经到达客户端通知宽限窗的终态行。
	FetchTerminalReleaseOutbox(ctx context.Context, limit int, nowMs int64) ([]TerminalReleaseRecord, error)
	// MarkTerminalReleaseReleased 是 UID 条件回收明确成功后的 durable phase-1 ACK。
	// CAS 只允许 0→releasedAtMs；false 表示已由并发 worker 推进或行已不存在。
	MarkTerminalReleaseReleased(ctx context.Context, id uint64, releasedAtMs int64) (bool, error)
	// DeleteTerminalReleaseOutbox 只在 released 行的 finalize-only RPC 明确成功后 ACK。
	DeleteTerminalReleaseOutbox(ctx context.Context, id uint64) error
	FetchMatchReleaseOutbox(ctx context.Context, limit int, nowMs int64) ([]MatchReleaseRecord, error)
	DeferMatchReleaseOutbox(ctx context.Context, id uint64, nextAttemptAtMs int64) error
	DeleteMatchReleaseOutbox(ctx context.Context, id uint64) error
	FetchBattleExitProofOutbox(ctx context.Context, limit int, nowMs int64) ([]BattleExitProofRecord, error)
	PrepareBattleExitProofOutbox(ctx context.Context, rec BattleExitProofRecord, proof placement.BattleExitProof) (bool, error)
	DeferBattleExitProofOutbox(ctx context.Context, id uint64, nextAttemptAtMs int64) error
	MarkBattleExitProofSuperseded(ctx context.Context, id uint64, supersededAtMs int64) error
	DeleteBattleExitProofOutbox(ctx context.Context, id uint64) error
}

// MySQLBattleRepo 是基于 database/sql 的 BattleRepo 实现。
type MySQLBattleRepo struct {
	db *sql.DB
}

// NewMySQLBattleRepo 构造。db 由 pkg/mysqlx.MustNewClient 提供(连 pandora_battle 库)。
func NewMySQLBattleRepo(db *sql.DB) *MySQLBattleRepo {
	return &MySQLBattleRepo{db: db}
}

// ValidateRecoveryOutboxSchema fails at startup instead of waiting for the
// first settlement to discover a missing additive migration.
func (r *MySQLBattleRepo) ValidateRecoveryOutboxSchema(ctx context.Context) error {
	checks := []string{
		`SELECT id, match_id, payload, next_attempt_at_ms, attempt_count, created_at_ms FROM match_release_outbox LIMIT 0`,
		`SELECT id, match_id, player_id, payload, prepared, next_attempt_at_ms, attempt_count, superseded_at_ms, created_at_ms FROM battle_exit_proof_outbox LIMIT 0`,
	}
	for _, query := range checks {
		rows, err := r.db.QueryContext(ctx, query)
		if err != nil {
			return errcode.New(errcode.ErrInternal, "battle recovery outbox schema invalid: %v", err)
		}
		if err := rows.Close(); err != nil {
			return errcode.New(errcode.ErrInternal, "close battle recovery schema probe: %v", err)
		}
	}
	return nil
}

func (r *MySQLBattleRepo) SaveResult(ctx context.Context, result *battlev1.BattleResult, outbox []OutboxRecord, dropOutbox []DropOutboxRecord, terminalRelease *TerminalReleaseRecord) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, errcode.New(errcode.ErrBattleResultDBWrite, "begin tx: %v", err)
	}
	// 任何提前 return 前回滚;Commit 成功后 Rollback 返回 ErrTxDone 可忽略。
	defer func() { _ = tx.Rollback() }()

	const insBattle = `INSERT INTO battles
(match_id, started_at_ms, ended_at_ms, winner_team, outcome, ds_pod_name, game_mode, map_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	_, err = tx.ExecContext(ctx, insBattle,
		result.GetMatchId(),
		result.GetStartedAtMs(),
		result.GetEndedAtMs(),
		result.GetWinnerTeam(),
		int32(result.GetOutcome()),
		result.GetDsPodName(),
		result.GetGameMode(),
		result.GetMapId(),
	)
	if err != nil {
		if isDupErr(err) {
			// 幂等重放仍须恢复可能缺失的 recovery outbox。Redis-authority 路径只信
			// checker 从 canonical BattleStorageRecord 带回的 roster；legacy/ABANDONED
			// 才回读首笔已落库 stats，绝不信本次重复 payload。
			playerIDs := authoritativeRecoveryPlayerIDs(terminalRelease)
			if len(playerIDs) == 0 {
				var qerr error
				playerIDs, qerr = loadMatchPlayerIDsTx(ctx, tx, result.GetMatchId())
				if qerr != nil {
					return false, errcode.New(errcode.ErrBattleResultDBWrite,
						"load idempotent match release players match=%d: %v", result.GetMatchId(), qerr)
				}
			}
			if ierr := insertMatchReleaseOutboxTx(ctx, tx, result.GetMatchId(), playerIDs, time.Now().UnixMilli()); ierr != nil {
				return false, errcode.New(errcode.ErrBattleResultDBWrite,
					"restore match release outbox match=%d: %v", result.GetMatchId(), ierr)
			}
			if ierr := insertBattleExitProofOutboxTx(ctx, tx, result.GetMatchId(), playerIDs, time.Now().UnixMilli()); ierr != nil {
				return false, errcode.New(errcode.ErrBattleResultDBWrite,
					"restore battle exit proof outbox match=%d: %v", result.GetMatchId(), ierr)
			}
			if cerr := tx.Commit(); cerr != nil {
				return false, errcode.New(errcode.ErrBattleResultDBWrite,
					"commit idempotent match release match=%d: %v", result.GetMatchId(), cerr)
			}
			return true, nil
		}
		return false, errcode.New(errcode.ErrBattleResultDBWrite, "insert battles match=%d: %v", result.GetMatchId(), err)
	}

	const insStat = `INSERT INTO battle_player_stats
(match_id, player_id, hero_id, team, kills, deaths, assists, damage_dealt, damage_taken, healing, gold, mmr_delta)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	for _, s := range result.GetStats() {
		if _, serr := tx.ExecContext(ctx, insStat,
			result.GetMatchId(),
			s.GetPlayerId(),
			s.GetHeroId(),
			s.GetTeam(),
			s.GetKills(),
			s.GetDeaths(),
			s.GetAssists(),
			s.GetDamageDealt(),
			s.GetDamageTaken(),
			s.GetHealing(),
			s.GetGold(),
			s.GetMmrDelta(),
		); serr != nil {
			return false, errcode.New(errcode.ErrBattleResultDBWrite, "insert stats match=%d player=%d: %v",
				result.GetMatchId(), s.GetPlayerId(), serr)
		}
	}

	// 同事务写 player.update 出箱(不变量 §4:落库与待发布段位事件原子提交)。
	const insOutbox = `INSERT INTO player_update_outbox
(match_id, player_id, payload, created_at_ms)
VALUES (?, ?, ?, ?)`
	nowMs := time.Now().UnixMilli()
	for _, o := range outbox {
		if _, oerr := tx.ExecContext(ctx, insOutbox,
			result.GetMatchId(), o.PlayerID, o.Payload, nowMs,
		); oerr != nil {
			return false, errcode.New(errcode.ErrBattleResultDBWrite, "insert outbox match=%d player=%d: %v",
				result.GetMatchId(), o.PlayerID, oerr)
		}
	}

	// 同事务写战斗装备掉落出箱(W5 ④):落库与待发放装备掉落原子提交(不变量 §4)。
	const insDropOutbox = `INSERT INTO battle_drop_outbox
(match_id, player_id, item_config_ids, created_at_ms)
VALUES (?, ?, ?, ?)`
	for _, d := range dropOutbox {
		if len(d.ItemConfigIDs) == 0 {
			continue
		}
		if _, derr := tx.ExecContext(ctx, insDropOutbox,
			result.GetMatchId(), d.PlayerID, encodeConfigIDs(d.ItemConfigIDs), nowMs,
		); derr != nil {
			return false, errcode.New(errcode.ErrBattleResultDBWrite, "insert drop outbox match=%d player=%d: %v",
				result.GetMatchId(), d.PlayerID, derr)
		}
	}

	// 正常结算的终态回收证明必须与战绩原子提交。它不是 DS 可填写的业务字段，
	// 调用方已从 Guard + Redis active 的同一服务端快照构造并验证完整性。
	if terminalRelease != nil {
		const insTerminalRelease = `INSERT INTO terminal_release_outbox
(match_id, allocation_id, ds_pod_name, gameserver_uid, instance_epoch,
 auth_gen, auth_jti, auth_exp_ms, auth_kid, auth_token_sha256, auth_writer_epoch,
 authorized_at_ms, release_after_ms, created_at_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
		if _, terr := tx.ExecContext(ctx, insTerminalRelease,
			result.GetMatchId(), terminalRelease.AllocationID, terminalRelease.DSPodName,
			terminalRelease.GameserverUID, terminalRelease.InstanceEpoch,
			terminalRelease.AuthGen, terminalRelease.AuthJTI, terminalRelease.AuthExpMs,
			terminalRelease.AuthKid, terminalRelease.AuthTokenSHA256, terminalRelease.AuthWriterEpoch,
			terminalRelease.AuthorizedAtMs, terminalRelease.ReleaseAfterMs, nowMs,
		); terr != nil {
			return false, errcode.New(errcode.ErrBattleResultDBWrite,
				"insert terminal release outbox match=%d allocation=%s: %v",
				result.GetMatchId(), terminalRelease.AllocationID, terr)
		}
	}

	playerIDs := authoritativeRecoveryPlayerIDs(terminalRelease)
	if len(playerIDs) == 0 {
		playerIDs = make([]uint64, 0, len(result.GetStats()))
		for _, stat := range result.GetStats() {
			if stat.GetPlayerId() != 0 {
				playerIDs = append(playerIDs, stat.GetPlayerId())
			}
		}
	}
	if err := insertMatchReleaseOutboxTx(ctx, tx, result.GetMatchId(), playerIDs, nowMs); err != nil {
		return false, errcode.New(errcode.ErrBattleResultDBWrite,
			"insert match release outbox match=%d: %v", result.GetMatchId(), err)
	}
	if err := insertBattleExitProofOutboxTx(ctx, tx, result.GetMatchId(), playerIDs, nowMs); err != nil {
		return false, errcode.New(errcode.ErrBattleResultDBWrite,
			"insert battle exit proof outbox match=%d: %v", result.GetMatchId(), err)
	}

	if cerr := tx.Commit(); cerr != nil {
		return false, errcode.New(errcode.ErrBattleResultDBWrite, "commit match=%d: %v", result.GetMatchId(), cerr)
	}
	return false, nil
}

func authoritativeRecoveryPlayerIDs(rec *TerminalReleaseRecord) []uint64 {
	if rec == nil || len(rec.PlayerIDs) == 0 {
		return nil
	}
	return append([]uint64(nil), rec.PlayerIDs...)
}

func loadMatchPlayerIDsTx(ctx context.Context, tx *sql.Tx, matchID uint64) ([]uint64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT player_id FROM battle_player_stats WHERE match_id = ? ORDER BY player_id ASC`, matchID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var playerIDs []uint64
	for rows.Next() {
		var playerID uint64
		if err := rows.Scan(&playerID); err != nil {
			return nil, err
		}
		playerIDs = append(playerIDs, playerID)
	}
	return playerIDs, rows.Err()
}

func insertMatchReleaseOutboxTx(ctx context.Context, tx *sql.Tx, matchID uint64, playerIDs []uint64, nowMs int64) error {
	record := &matchv1.MatchReleaseStorageRecord{
		OperationId: uuid.NewString(),
		MatchId:     matchID,
		PlayerIds:   playerIDs,
		CreatedAtMs: nowMs,
	}
	payload, err := proto.Marshal(record)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO match_release_outbox
(match_id, payload, next_attempt_at_ms, attempt_count, created_at_ms)
VALUES (?, ?, 0, 0, ?)
ON DUPLICATE KEY UPDATE match_id = VALUES(match_id)`, matchID, payload, nowMs)
	return err
}

func insertBattleExitProofOutboxTx(ctx context.Context, tx *sql.Tx, matchID uint64, playerIDs []uint64, nowMs int64) error {
	proofID := "result:" + strconv.FormatUint(matchID, 10) + ":match:" + strconv.FormatUint(matchID, 10)
	for _, playerID := range playerIDs {
		if playerID == 0 {
			continue
		}
		storage := &battleExitProofStorageRecord{
			MatchID: matchID, PlayerID: playerID, ProofType: placement.ProofMatchTerminal,
			ProofID: proofID, CreatedAtMs: nowMs,
		}
		payload, err := json.Marshal(storage)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO battle_exit_proof_outbox
(match_id, player_id, payload, prepared, next_attempt_at_ms, attempt_count, superseded_at_ms, created_at_ms)
VALUES (?, ?, ?, 0, 0, 0, 0, ?)
ON DUPLICATE KEY UPDATE match_id = VALUES(match_id)`, matchID, playerID, payload, nowMs); err != nil {
			return err
		}
	}
	return nil
}

// FetchOutbox 按 id 升序取最多 limit 条待发布出箱记录(FIFO 保序)。
func (r *MySQLBattleRepo) FetchOutbox(ctx context.Context, limit int) ([]OutboxRecord, error) {
	if limit <= 0 {
		limit = 128
	}
	const q = `SELECT id, player_id, payload FROM player_update_outbox ORDER BY id ASC LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]OutboxRecord, 0, limit)
	for rows.Next() {
		var rec OutboxRecord
		if serr := rows.Scan(&rec.ID, &rec.PlayerID, &rec.Payload); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan outbox: %v", serr)
		}
		out = append(out, rec)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate outbox: %v", rerr)
	}
	return out, nil
}

// DeleteOutbox 删除已成功投递的出箱行。
func (r *MySQLBattleRepo) DeleteOutbox(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM player_update_outbox WHERE id = ?`, id); err != nil {
		return errcode.New(errcode.ErrInternal, "delete outbox id=%d: %v", id, err)
	}
	return nil
}

// FetchDropOutbox 按 id 升序取最多 limit 条待发放装备掉落出箱记录(W5 ④)。
func (r *MySQLBattleRepo) FetchDropOutbox(ctx context.Context, limit int) ([]DropOutboxRecord, error) {
	if limit <= 0 {
		limit = 128
	}
	const q = `SELECT id, match_id, player_id, item_config_ids FROM battle_drop_outbox ORDER BY id ASC LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query drop outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]DropOutboxRecord, 0, limit)
	for rows.Next() {
		var (
			rec DropOutboxRecord
			csv string
		)
		if serr := rows.Scan(&rec.ID, &rec.MatchID, &rec.PlayerID, &csv); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan drop outbox: %v", serr)
		}
		rec.ItemConfigIDs = decodeConfigIDs(csv)
		out = append(out, rec)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate drop outbox: %v", rerr)
	}
	return out, nil
}

// DeleteDropOutbox 删除已成功发放的掉落出箱行。
func (r *MySQLBattleRepo) DeleteDropOutbox(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM battle_drop_outbox WHERE id = ?`, id); err != nil {
		return errcode.New(errcode.ErrInternal, "delete drop outbox id=%d: %v", id, err)
	}
	return nil
}

// FetchTerminalReleaseOutbox 按到期时间/id 取一批待终态回收行。
func (r *MySQLBattleRepo) FetchTerminalReleaseOutbox(ctx context.Context, limit int, nowMs int64) ([]TerminalReleaseRecord, error) {
	if limit <= 0 {
		limit = 128
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	const q = `SELECT id, match_id, allocation_id, ds_pod_name, gameserver_uid, instance_epoch,
auth_gen, auth_jti, auth_exp_ms, auth_kid, auth_token_sha256, auth_writer_epoch,
authorized_at_ms, release_after_ms, released_at_ms, created_at_ms
FROM terminal_release_outbox
WHERE release_after_ms <= ?
ORDER BY release_after_ms ASC, id ASC
LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, nowMs, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query terminal release outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]TerminalReleaseRecord, 0, limit)
	for rows.Next() {
		var rec TerminalReleaseRecord
		if err := rows.Scan(
			&rec.ID, &rec.MatchID, &rec.AllocationID, &rec.DSPodName, &rec.GameserverUID,
			&rec.InstanceEpoch, &rec.AuthGen, &rec.AuthJTI, &rec.AuthExpMs, &rec.AuthKid,
			&rec.AuthTokenSHA256, &rec.AuthWriterEpoch, &rec.AuthorizedAtMs,
			&rec.ReleaseAfterMs, &rec.ReleasedAtMs, &rec.CreatedAtMs,
		); err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan terminal release outbox: %v", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate terminal release outbox: %v", err)
	}
	return out, nil
}

// MarkTerminalReleaseReleased 持久化阶段1 ACK。它必须发生在 Redis 永久 terminal
// 与 UID-precondition delete 明确成功之后、任何 Redis TTL 恢复之前。
func (r *MySQLBattleRepo) MarkTerminalReleaseReleased(ctx context.Context, id uint64, releasedAtMs int64) (bool, error) {
	if id == 0 || releasedAtMs <= 0 {
		return false, errcode.New(errcode.ErrInvalidArg, "terminal release mark requires id/time")
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE terminal_release_outbox SET released_at_ms=? WHERE id=? AND released_at_ms=0`,
		releasedAtMs, id)
	if err != nil {
		return false, fmt.Errorf("mark terminal release released: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark terminal release rows affected: %w", err)
	}
	return affected == 1, nil
}

// DeleteTerminalReleaseOutbox 是阶段2 finalize 的 ACK。外部回收或 finalize 结果未知时绝不能调用。
func (r *MySQLBattleRepo) DeleteTerminalReleaseOutbox(ctx context.Context, id uint64) error {
	if id == 0 {
		return errcode.New(errcode.ErrInvalidArg, "terminal release outbox id required")
	}
	res, err := r.db.ExecContext(ctx, deleteTerminalReleaseOutboxSQL, id)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "delete terminal release outbox id=%d: %v", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return errcode.New(errcode.ErrInternal, "delete terminal release rows affected id=%d: %v", id, err)
	}
	// 0 = 已被并发 worker 删除，或仍是 pending（WHERE 前置条件保护它不被误删）；
	// 两者都按幂等 no-op。PK 保证 >1 是结构/驱动异常，必须 fail-closed。
	if affected > 1 {
		return errcode.New(errcode.ErrInternal, "delete terminal release id=%d affected=%d", id, affected)
	}
	return nil
}

func (r *MySQLBattleRepo) FetchMatchReleaseOutbox(ctx context.Context, limit int, nowMs int64) ([]MatchReleaseRecord, error) {
	if limit <= 0 {
		limit = 128
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, payload, attempt_count, next_attempt_at_ms, created_at_ms
FROM match_release_outbox
WHERE next_attempt_at_ms <= ?
ORDER BY next_attempt_at_ms ASC, id ASC
LIMIT ?`, nowMs, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query match release outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]MatchReleaseRecord, 0, limit)
	for rows.Next() {
		var (
			rec     MatchReleaseRecord
			payload []byte
		)
		if err := rows.Scan(&rec.ID, &payload, &rec.AttemptCount, &rec.NextAttemptAtMs, &rec.CreatedAtMs); err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan match release outbox: %v", err)
		}
		storage := &matchv1.MatchReleaseStorageRecord{}
		if err := proto.Unmarshal(payload, storage); err != nil {
			return nil, errcode.New(errcode.ErrInternal, "decode match release outbox id=%d: %v", rec.ID, err)
		}
		if storage.GetMatchId() == 0 || storage.GetOperationId() == "" {
			return nil, errcode.New(errcode.ErrInternal, "invalid match release outbox id=%d", rec.ID)
		}
		rec.OperationID = storage.GetOperationId()
		rec.MatchID = storage.GetMatchId()
		rec.PlayerIDs = append([]uint64(nil), storage.GetPlayerIds()...)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate match release outbox: %v", err)
	}
	return out, nil
}

func (r *MySQLBattleRepo) DeferMatchReleaseOutbox(ctx context.Context, id uint64, nextAttemptAtMs int64) error {
	if id == 0 || nextAttemptAtMs <= 0 {
		return errcode.New(errcode.ErrInvalidArg, "match release defer requires id/time")
	}
	res, err := r.db.ExecContext(ctx, `UPDATE match_release_outbox
SET attempt_count = attempt_count + 1, next_attempt_at_ms = ?
WHERE id = ?`, nextAttemptAtMs, id)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "defer match release outbox id=%d: %v", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return errcode.New(errcode.ErrInternal, "defer match release rows affected id=%d: %v", id, err)
	}
	if affected > 1 {
		return errcode.New(errcode.ErrInternal, "defer match release id=%d affected=%d", id, affected)
	}
	return nil
}

func (r *MySQLBattleRepo) DeleteMatchReleaseOutbox(ctx context.Context, id uint64) error {
	if id == 0 {
		return errcode.New(errcode.ErrInvalidArg, "match release outbox id required")
	}
	res, err := r.db.ExecContext(ctx, `DELETE FROM match_release_outbox WHERE id = ?`, id)
	if err != nil {
		return errcode.New(errcode.ErrInternal, "delete match release outbox id=%d: %v", id, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return errcode.New(errcode.ErrInternal, "delete match release rows affected id=%d: %v", id, err)
	}
	if affected > 1 {
		return errcode.New(errcode.ErrInternal, "delete match release id=%d affected=%d", id, affected)
	}
	return nil
}

func (r *MySQLBattleRepo) FetchBattleExitProofOutbox(ctx context.Context, limit int, nowMs int64) ([]BattleExitProofRecord, error) {
	if limit <= 0 {
		limit = 128
	}
	if nowMs <= 0 {
		nowMs = time.Now().UnixMilli()
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, payload, prepared, attempt_count, next_attempt_at_ms, created_at_ms
FROM battle_exit_proof_outbox
WHERE superseded_at_ms = 0 AND next_attempt_at_ms <= ?
ORDER BY next_attempt_at_ms ASC, id ASC
LIMIT ?`, nowMs, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query battle exit proof outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]BattleExitProofRecord, 0, limit)
	for rows.Next() {
		var rec BattleExitProofRecord
		var payload []byte
		if err := rows.Scan(&rec.ID, &payload, &rec.Prepared, &rec.AttemptCount, &rec.NextAttemptAtMs, &rec.CreatedAtMs); err != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan battle exit proof outbox: %v", err)
		}
		storage := &battleExitProofStorageRecord{}
		if err := json.Unmarshal(payload, storage); err != nil {
			return nil, errcode.New(errcode.ErrInternal, "decode battle exit proof outbox id=%d: %v", rec.ID, err)
		}
		rec.MatchID, rec.PlayerID = storage.MatchID, storage.PlayerID
		rec.Proof = placement.BattleExitProof{ExpectedVersion: storage.ExpectedVersion,
			OperationID: storage.OperationID, ProofType: storage.ProofType,
			ProofID: storage.ProofID, Signature: storage.Signature}
		if rec.MatchID == 0 || rec.PlayerID == 0 || rec.Proof.ProofType != placement.ProofMatchTerminal || rec.Proof.ProofID == "" ||
			(rec.Prepared && (rec.Proof.ExpectedVersion == 0 || !placement.ValidOperationID(rec.Proof.OperationID) || rec.Proof.Signature == "")) ||
			(!rec.Prepared && (rec.Proof.ExpectedVersion != 0 || rec.Proof.OperationID != "" || rec.Proof.Signature != "")) {
			return nil, errcode.New(errcode.ErrInternal, "invalid battle exit proof outbox id=%d", rec.ID)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate battle exit proof outbox: %v", err)
	}
	return out, nil
}

func (r *MySQLBattleRepo) PrepareBattleExitProofOutbox(ctx context.Context, rec BattleExitProofRecord, proof placement.BattleExitProof) (bool, error) {
	if rec.ID == 0 || rec.MatchID == 0 || rec.PlayerID == 0 || proof.ExpectedVersion == 0 ||
		!placement.ValidOperationID(proof.OperationID) || proof.ProofType != placement.ProofMatchTerminal ||
		proof.ProofID != rec.Proof.ProofID || proof.Signature == "" {
		return false, errcode.New(errcode.ErrInvalidArg, "complete immutable battle exit proof required")
	}
	storage := &battleExitProofStorageRecord{MatchID: rec.MatchID, PlayerID: rec.PlayerID,
		ExpectedVersion: proof.ExpectedVersion, OperationID: proof.OperationID, ProofType: proof.ProofType,
		ProofID: proof.ProofID, Signature: proof.Signature, CreatedAtMs: rec.CreatedAtMs}
	payload, err := json.Marshal(storage)
	if err != nil {
		return false, err
	}
	res, err := r.db.ExecContext(ctx, `UPDATE battle_exit_proof_outbox
SET payload = ?, prepared = 1
WHERE id = ? AND prepared = 0 AND superseded_at_ms = 0`, payload, rec.ID)
	if err != nil {
		return false, errcode.New(errcode.ErrInternal, "prepare battle exit proof outbox id=%d: %v", rec.ID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected > 1 {
		return false, errcode.New(errcode.ErrInternal, "prepare battle exit proof id=%d affected=%d", rec.ID, affected)
	}
	return affected == 1, nil
}

func (r *MySQLBattleRepo) DeferBattleExitProofOutbox(ctx context.Context, id uint64, nextAttemptAtMs int64) error {
	if id == 0 || nextAttemptAtMs <= 0 {
		return errcode.New(errcode.ErrInvalidArg, "battle exit proof defer requires id/time")
	}
	_, err := r.db.ExecContext(ctx, `UPDATE battle_exit_proof_outbox
SET attempt_count = attempt_count + 1, next_attempt_at_ms = ?
WHERE id = ? AND superseded_at_ms = 0`, nextAttemptAtMs, id)
	return err
}

func (r *MySQLBattleRepo) MarkBattleExitProofSuperseded(ctx context.Context, id uint64, supersededAtMs int64) error {
	if id == 0 || supersededAtMs <= 0 {
		return errcode.New(errcode.ErrInvalidArg, "battle exit proof supersede requires id/time")
	}
	_, err := r.db.ExecContext(ctx, `UPDATE battle_exit_proof_outbox
SET superseded_at_ms = ? WHERE id = ? AND superseded_at_ms = 0`, supersededAtMs, id)
	return err
}

func (r *MySQLBattleRepo) DeleteBattleExitProofOutbox(ctx context.Context, id uint64) error {
	if id == 0 {
		return errcode.New(errcode.ErrInvalidArg, "battle exit proof outbox id required")
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM battle_exit_proof_outbox WHERE id = ? AND prepared = 1`, id)
	return err
}

const deleteTerminalReleaseOutboxSQL = `DELETE FROM terminal_release_outbox WHERE id = ? AND released_at_ms > 0`

// encodeConfigIDs 把 item_config_id 列表编码成 CSV(如 "5001,5002"),存 drop 出箱。
func encodeConfigIDs(ids []uint32) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatUint(uint64(id), 10))
	}
	return strings.Join(parts, ",")
}

// decodeConfigIDs 解析 CSV item_config_id(非法/空段跳过,防御性)。
func decodeConfigIDs(csv string) []uint32 {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	ids := make([]uint32, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if v, err := strconv.ParseUint(p, 10, 32); err == nil && v != 0 {
			ids = append(ids, uint32(v))
		}
	}
	return ids
}

func (r *MySQLBattleRepo) GetResult(ctx context.Context, matchID uint64) (*battlev1.BattleResult, bool, error) {
	const q = `SELECT started_at_ms, ended_at_ms, winner_team, outcome, ds_pod_name, game_mode, map_id
FROM battles WHERE match_id = ? LIMIT 1`
	var (
		startedAt, endedAt  int64
		winnerTeam, outcome int32
		dsPod, gameMode     string
		mapID               uint32
	)
	err := r.db.QueryRowContext(ctx, q, matchID).Scan(
		&startedAt, &endedAt, &winnerTeam, &outcome, &dsPod, &gameMode, &mapID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errcode.New(errcode.ErrInternal, "query battle match=%d: %v", matchID, err)
	}

	stats, err := r.loadStats(ctx, matchID)
	if err != nil {
		return nil, false, err
	}
	return &battlev1.BattleResult{
		MatchId:     matchID,
		StartedAtMs: startedAt,
		EndedAtMs:   endedAt,
		WinnerTeam:  winnerTeam,
		Outcome:     battlev1.BattleOutcome(outcome),
		DsPodName:   dsPod,
		GameMode:    gameMode,
		MapId:       mapID,
		Stats:       stats,
	}, true, nil
}

func (r *MySQLBattleRepo) ListPlayerHistory(ctx context.Context, playerID uint64, limit int, beforeMs int64) ([]*battlev1.BattleResult, error) {
	if limit <= 0 {
		limit = 20
	}
	// 先取玩家参与的 match_id(按结束时间倒序,游标分页),再逐场 load 完整结算。
	const q = `SELECT b.match_id FROM battle_player_stats s
JOIN battles b ON b.match_id = s.match_id
WHERE s.player_id = ? AND (? <= 0 OR b.ended_at_ms < ?)
ORDER BY b.ended_at_ms DESC
LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, playerID, beforeMs, beforeMs, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query history player=%d: %v", playerID, err)
	}
	defer func() { _ = rows.Close() }()

	matchIDs := make([]uint64, 0, limit)
	for rows.Next() {
		var mid uint64
		if serr := rows.Scan(&mid); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan history player=%d: %v", playerID, serr)
		}
		matchIDs = append(matchIDs, mid)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate history player=%d: %v", playerID, rerr)
	}

	out := make([]*battlev1.BattleResult, 0, len(matchIDs))
	for _, mid := range matchIDs {
		res, found, gerr := r.GetResult(ctx, mid)
		if gerr != nil {
			return nil, gerr
		}
		if found {
			out = append(out, res)
		}
	}
	return out, nil
}

// loadStats 读一场对局的全部玩家战绩。
func (r *MySQLBattleRepo) loadStats(ctx context.Context, matchID uint64) ([]*battlev1.PlayerStats, error) {
	const q = `SELECT player_id, hero_id, team, kills, deaths, assists,
damage_dealt, damage_taken, healing, gold, mmr_delta
FROM battle_player_stats WHERE match_id = ? ORDER BY team, player_id`
	rows, err := r.db.QueryContext(ctx, q, matchID)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query stats match=%d: %v", matchID, err)
	}
	defer func() { _ = rows.Close() }()

	var stats []*battlev1.PlayerStats
	for rows.Next() {
		s := &battlev1.PlayerStats{}
		if serr := rows.Scan(
			&s.PlayerId, &s.HeroId, &s.Team, &s.Kills, &s.Deaths, &s.Assists,
			&s.DamageDealt, &s.DamageTaken, &s.Healing, &s.Gold, &s.MmrDelta,
		); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan stats match=%d: %v", matchID, serr)
		}
		stats = append(stats, s)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate stats match=%d: %v", matchID, rerr)
	}
	return stats, nil
}

// isDupErr 判断是否 MySQL 唯一键冲突(1062 ER_DUP_ENTRY)。
// 字符串匹配避免强依赖 driver 错误类型(对齐 login data/account.go)。
func isDupErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "1062") || strings.Contains(msg, "Duplicate entry")
}
