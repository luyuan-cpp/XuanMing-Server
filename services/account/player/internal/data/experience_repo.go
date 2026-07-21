// experience_repo.go — 玩家等级经验幂等入账 + 经验推送事务出箱(实时成长,2026-07-20)。
//
// 库表(deploy/mysql-init/04-player-tables.sql / tools/migrate pandora_player 000002):
//
//	pandora_player.players.exp        级内经验列(满级恒 0)
//	pandora_player.exp_history        经验入账历史 + 幂等键(uk player_id+idempotency_key,不变量 §2)
//	pandora_player.player_push_outbox 经验推送事务出箱(与入账同事务原子提交,不变量 §4)
//
// 幂等:ApplyExperience 在一个事务里 INSERT exp_history;命中 1062 唯一键冲突 → 视为已入账
// (already=true),回读当前权威快照返回,不重复加经验、不重复出箱。
// 等级结算(连升多级 / 封顶 / 满级不累加)是纯函数 AdvanceExperience,单测覆盖。
package data

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"
)

// ExpApply 是一次经验入账请求(biz 校验合法性后传入)。
// Curve 是等级经验曲线:第 i 项(0 基)= 从 Lv(i+1) 升到 Lv(i+2) 所需级内经验(>0);
// 最高等级 = len(Curve)+1(与 conf.ExpCurve / 客户端 CfgPlayerLevelExp 同源)。
type ExpApply struct {
	PlayerID       uint64
	Delta          uint64
	Reason         string
	IdempotencyKey string
	Curve          []uint64
}

// ExpState 是入账后(或幂等命中时当前)的权威经验快照。
type ExpState struct {
	Level        int32
	ExpInLevel   uint64
	IsMaxLevel   bool
	LevelsGained uint32
}

// AdvanceExperience 纯函数:级内经验加 delta 后按曲线循环进位(天然支持一次连升多级)。
//
//	返回 (新等级, 新级内经验, 升级数)。
//	- 满级(level >= len(curve)+1)→ 原样返回且级内经验恒 0(不再累加,需求「满级 MAX」);
//	- 升到满级的瞬间级内经验清 0;
//	- 曲线项为 0(非法配置,防御)→ 视为不可升级,停止进位;
//	- 加法回绕(理论上 delta 有上限不会发生,防御)→ 按「足够升满」处理。
func AdvanceExperience(level int32, expInLevel uint64, delta uint64, curve []uint64) (int32, uint64, uint32) {
	maxLevel := int32(len(curve)) + 1
	if level < 1 {
		level = 1
	}
	if level >= maxLevel {
		return maxLevel, 0, 0
	}
	exp := expInLevel + delta
	if exp < expInLevel {
		// uint64 回绕(防御):按可升满处理。
		exp = ^uint64(0)
	}
	var gained uint32
	for level < maxLevel {
		need := curve[level-1]
		if need == 0 || exp < need {
			break
		}
		exp -= need
		level++
		gained++
		if level >= maxLevel {
			exp = 0 // 满级不累加(需求:满级后显示 MAX,不再增加经验)
			break
		}
	}
	return level, exp, gained
}

// ApplyExperience 幂等入账经验并结算等级,与经验推送出箱同一事务原子提交。
//
// 事务顺序(锁序固定,防死锁):先锁 players 行 → 满级判定 → INSERT exp_history(幂等)
// → UPDATE players → INSERT player_push_outbox。
// 满级 no-op:不消费幂等键、不出箱、无任何写(重复调用恒无副作用)。
func (r *MySQLPlayerRepo) ApplyExperience(ctx context.Context, apply ExpApply) (ExpState, bool, error) {
	maxLevel := int32(len(apply.Curve)) + 1

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ExpState{}, false, errcode.New(errcode.ErrInternal, "begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		level int32
		exp   uint64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT level, exp FROM players WHERE player_id = ? FOR UPDATE`, apply.PlayerID,
	).Scan(&level, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return ExpState{}, false, errcode.New(errcode.ErrPlayerNotFound, "player not found: %d", apply.PlayerID)
	}
	if err != nil {
		return ExpState{}, false, errcode.New(errcode.ErrInternal, "lock player=%d: %v", apply.PlayerID, err)
	}

	// 满级 no-op:零写返回(不消费幂等键;Commit 只为干净释放行锁)。
	if level >= maxLevel {
		if cerr := tx.Commit(); cerr != nil {
			return ExpState{}, false, errcode.New(errcode.ErrInternal, "commit max-level noop player=%d: %v", apply.PlayerID, cerr)
		}
		return ExpState{Level: maxLevel, ExpInLevel: 0, IsMaxLevel: true}, false, nil
	}

	newLevel, newExp, gained := AdvanceExperience(level, exp, apply.Delta, apply.Curve)

	const insHistory = `INSERT INTO exp_history
(player_id, idempotency_key, exp_delta, reason, old_level, old_exp, new_level, new_exp)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, herr := tx.ExecContext(ctx, insHistory,
		apply.PlayerID, apply.IdempotencyKey, apply.Delta, apply.Reason,
		level, exp, newLevel, newExp,
	); herr != nil {
		if isDupErr(herr) {
			// 幂等命中:回读锁内当前权威快照(原次入账已生效),不重复加、不重复出箱。
			if cerr := tx.Commit(); cerr != nil {
				return ExpState{}, false, errcode.New(errcode.ErrInternal, "commit idempotent hit player=%d: %v", apply.PlayerID, cerr)
			}
			return ExpState{
				Level:      level,
				ExpInLevel: exp,
				IsMaxLevel: level >= maxLevel,
			}, true, nil
		}
		return ExpState{}, false, errcode.New(errcode.ErrInternal, "insert exp history player=%d: %v", apply.PlayerID, herr)
	}

	if _, uerr := tx.ExecContext(ctx,
		`UPDATE players SET level = ?, exp = ? WHERE player_id = ?`,
		newLevel, newExp, apply.PlayerID,
	); uerr != nil {
		return ExpState{}, false, errcode.New(errcode.ErrInternal, "update exp player=%d: %v", apply.PlayerID, uerr)
	}

	// 经验推送出箱:与入账同事务(不变量 §4)。payload 是入账后的权威快照,
	// push 经 pandora.player.update(event_type=EXPERIENCE)透传给客户端。
	evt := &playerv1.PlayerExperienceEvent{
		PlayerId:     apply.PlayerID,
		Level:        newLevel,
		ExpInLevel:   newExp,
		IsMaxLevel:   newLevel >= maxLevel,
		LevelsGained: gained,
		TsMs:         time.Now().UnixMilli(),
	}
	payload, merr := proto.Marshal(evt)
	if merr != nil {
		return ExpState{}, false, errcode.New(errcode.ErrInternal, "marshal experience event player=%d: %v", apply.PlayerID, merr)
	}
	const insOutbox = `INSERT INTO player_push_outbox (player_id, event_type, payload, created_at_ms)
VALUES (?, ?, ?, ?)`
	if _, oerr := tx.ExecContext(ctx, insOutbox,
		apply.PlayerID, uint32(playerv1.PlayerPushEventType_PLAYER_PUSH_EVENT_TYPE_EXPERIENCE),
		payload, time.Now().UnixMilli(),
	); oerr != nil {
		return ExpState{}, false, errcode.New(errcode.ErrInternal, "insert push outbox player=%d: %v", apply.PlayerID, oerr)
	}

	if cerr := tx.Commit(); cerr != nil {
		return ExpState{}, false, errcode.New(errcode.ErrInternal, "commit exp player=%d: %v", apply.PlayerID, cerr)
	}
	return ExpState{
		Level:        newLevel,
		ExpInLevel:   newExp,
		IsMaxLevel:   newLevel >= maxLevel,
		LevelsGained: gained,
	}, false, nil
}

// PushOutboxRecord 是一条待发布的玩家推送事务出箱记录(FIFO 按 id)。
type PushOutboxRecord struct {
	ID        int64
	PlayerID  uint64
	EventType uint32
	Payload   []byte
}

// FetchPushOutbox 按 id 升序取最多 limit 条待发布推送出箱记录(FIFO 保序,同玩家事件有序)。
func (r *MySQLPlayerRepo) FetchPushOutbox(ctx context.Context, limit int) ([]PushOutboxRecord, error) {
	if limit <= 0 {
		limit = 128
	}
	const q = `SELECT id, player_id, event_type, payload FROM player_push_outbox ORDER BY id ASC LIMIT ?`
	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, errcode.New(errcode.ErrInternal, "query push outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]PushOutboxRecord, 0, limit)
	for rows.Next() {
		var rec PushOutboxRecord
		if serr := rows.Scan(&rec.ID, &rec.PlayerID, &rec.EventType, &rec.Payload); serr != nil {
			return nil, errcode.New(errcode.ErrInternal, "scan push outbox: %v", serr)
		}
		out = append(out, rec)
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, errcode.New(errcode.ErrInternal, "iterate push outbox: %v", rerr)
	}
	return out, nil
}

// DeletePushOutbox 删除已成功投递的推送出箱行。
func (r *MySQLPlayerRepo) DeletePushOutbox(ctx context.Context, id int64) error {
	if _, err := r.db.ExecContext(ctx, `DELETE FROM player_push_outbox WHERE id = ?`, id); err != nil {
		return errcode.New(errcode.ErrInternal, "delete push outbox id=%d: %v", id, err)
	}
	return nil
}
