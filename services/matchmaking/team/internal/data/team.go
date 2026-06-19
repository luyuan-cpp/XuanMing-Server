// Package data 是 team 服务的数据层。
//
// Redis key 模板(所有业务 ID 用 uint64,%d 格式化):
//
//	pandora:team:{%d}        → protobuf bytes(TeamStorageRecord)
//	                           hashtag {} 确保同 team 的所有 key 落同一 redis cluster slot(兜底)
//	pandora:team:player:%d   → string(team_id,uint64),TTL 跟随队伍生命周期
//	pandora:team:invite:%d   → hash(team_id/target_player_id),TTL=InviteTTL(60s)
//
// 状态机写用 WATCH/MULTI/EXEC 乐观锁:
//
//	GET(proto bytes) → fn(modify) → MULTI/SET/EXEC
//	EXEC 返回 nil(key 被并发修改) → 重试至 maxRetry 次 → 返 ErrTeamConcurrent(3007)
//
// 队伍主体序列化为 protobuf bytes 存入 Redis value。
package data

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	teamv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/team/v1"
)

// ── 常量 ─────────────────────────────────────────────────────────────────────

const (
	// fieldTeamID / fieldTargetPlayerID — invite hash 字段
	fieldTeamID         = "team_id"
	fieldTargetPlayerID = "target_player_id"
)

// teamKey returns "pandora:team:{teamID}" — hashtag 括住 teamID 保 cluster slot 一致性。
func teamKey(teamID uint64) string {
	return fmt.Sprintf("pandora:team:{%d}", teamID)
}

// playerKey returns "pandora:team:player:playerID".
func playerKey(playerID uint64) string {
	return fmt.Sprintf("pandora:team:player:%d", playerID)
}

// inviteKey returns "pandora:team:invite:inviteID".
func inviteKey(inviteID uint64) string {
	return fmt.Sprintf("pandora:team:invite:%d", inviteID)
}

// ── 数据模型 ──────────────────────────────────────────────────────────────────
//
// 队伍主体直接使用 proto 存储类型 teamv1.TeamStorageRecord /
// teamv1.TeamMemberStorageRecord，不再起本地别名，保证存储结构全局只有一个权威
// 命名（CLAUDE.md §5.10：存储字段命名以 <Domain>StorageRecord 为准）。

// InviteRecord 是邀请令牌的内存表示，对应 Redis hash pandora:team:invite:{inviteID}。
// 邀请是 2 字段短 TTL 小令牌，按 CLAUDE.md §5.9 保留 hash 不升级为 proto bytes，
// 因此用本地 struct（它不是 proto 存储记录，不叫 StorageRecord）。
type InviteRecord struct {
	TeamID         uint64
	TargetPlayerID uint64
}

// ── 接口 ──────────────────────────────────────────────────────────────────────

// TeamRepo 是 team 数据层抽象。biz 层只依赖此接口,不依赖 redis。
type TeamRepo interface {
	// Get 读取队伍。not found 时返回 false(不报错)。
	Get(ctx context.Context, teamID uint64) (*teamv1.TeamStorageRecord, bool, error)

	// Create 创建队伍：仅写 team protobuf value + TTL=teamTTL。
	// player 归属由上层 ClaimPlayer(SETNX) 独立保证（不变量 §1），不在此处写 player index。
	Create(ctx context.Context, team *teamv1.TeamStorageRecord, teamTTL time.Duration) error

	// UpdateWithLock 使用 WATCH/MULTI/EXEC 读-改-写 team protobuf value。
	//   1. WATCH team key
	//   2. GET → proto 反序列化
	//   3. 调 fn(team) — fn 可返错误，返错则 UNWATCH 并透传
	//   4. MULTI → SET(value+TTL) → EXEC
	//   5. EXEC=nil（CAS 失败）→ 重试，耗尽返 ErrTeamConcurrent(3007)
	UpdateWithLock(ctx context.Context, teamID uint64, maxRetry int, fn func(*teamv1.TeamStorageRecord) error, teamTTL time.Duration) error

	// GetPlayerTeamID 查玩家当前所在队伍 ID。not found 返 (0, false, nil)。
	GetPlayerTeamID(ctx context.Context, playerID uint64) (uint64, bool, error)

	// ClaimPlayer 原子声明 player→teamID 归属(SETNX),保证不变量 §1(一人只能在一个队)。
	// 声明成功返回 (teamID, true, nil);玩家已属其他队伍返回 (existingTeamID, false, nil)。
	ClaimPlayer(ctx context.Context, playerID, teamID uint64, ttl time.Duration) (uint64, bool, error)

	// SetPlayerIndex 设置或覆盖 player→teamID 映射。
	SetPlayerIndex(ctx context.Context, playerID, teamID uint64, ttl time.Duration) error

	// DeletePlayerIndex 删除 player→teamID 映射。
	DeletePlayerIndex(ctx context.Context, playerID uint64) error

	// ExpireTeam 单独刷新 team key 的 TTL(不读改写 value),供解散后改短 TTL 用。
	ExpireTeam(ctx context.Context, teamID uint64, ttl time.Duration) error

	// SetInvite 存储邀请令牌,TTL=inviteTTL。
	SetInvite(ctx context.Context, inviteID, teamID, targetPlayerID uint64, ttl time.Duration) error

	// GetInvite 读取邀请令牌。已过期或不存在时返回 (nil, false, nil)。
	GetInvite(ctx context.Context, inviteID uint64) (*InviteRecord, bool, error)

	// DeleteInvite 删除邀请令牌(AcceptInvite 或取消时调用)。
	DeleteInvite(ctx context.Context, inviteID uint64) error
}

// ── Redis 实现 ────────────────────────────────────────────────────────────────

// RedisTeamRepo 是基于 go-redis/v9 的 TeamRepo 实现。
type RedisTeamRepo struct {
	rdb redis.UniversalClient
}

// NewRedisTeamRepo 构造 RedisTeamRepo。
func NewRedisTeamRepo(rdb redis.UniversalClient) *RedisTeamRepo {
	return &RedisTeamRepo{rdb: rdb}
}

// --- Get ---

func (r *RedisTeamRepo) Get(ctx context.Context, teamID uint64) (*teamv1.TeamStorageRecord, bool, error) {
	b, err := r.rdb.Get(ctx, teamKey(teamID)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rec, err := unmarshalTeam(teamID, b)
	if err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// --- Create ---

func (r *RedisTeamRepo) Create(ctx context.Context, team *teamv1.TeamStorageRecord, teamTTL time.Duration) error {
	payload, err := marshalTeam(team)
	if err != nil {
		return err
	}
	key := teamKey(team.TeamId)

	// 仅写 team protobuf value + TTL。player 归属由上层 ClaimPlayer(SETNX) 独立保证(不变量 §1),
	// 不在此处写 player index,避免覆盖已声明的 claim。
	return r.rdb.Set(ctx, key, payload, teamTTL).Err()
}

// --- UpdateWithLock ---

func (r *RedisTeamRepo) UpdateWithLock(
	ctx context.Context,
	teamID uint64,
	maxRetry int,
	fn func(*teamv1.TeamStorageRecord) error,
	teamTTL time.Duration,
) error {
	key := teamKey(teamID)

	for attempt := 0; attempt <= maxRetry; attempt++ {
		var team *teamv1.TeamStorageRecord
		var fnErr error

		// TxPipelined with WATCH
		txErr := r.rdb.Watch(ctx, func(tx *redis.Tx) error {
			// 1. 读取当前 team
			b, err := tx.Get(ctx, key).Bytes()
			if err == redis.Nil {
				return errcode.New(errcode.ErrTeamNotFound, "team %d not found", teamID)
			}
			if err != nil {
				return err
			}
			team, err = unmarshalTeam(teamID, b)
			if err != nil {
				return err
			}

			// 2. 调用 fn 修改 team
			if fnErr = fn(team); fnErr != nil {
				return fnErr
			}

			// 3. MULTI → 写回 → EXEC
			payload, err := marshalTeam(team)
			if err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, key, payload, teamTTL)
				return nil
			})
			return err
		}, key)

		if txErr == nil {
			return nil
		}
		// fn 自身返回的业务错误 — 不重试,直接透传
		if txErr == fnErr && fnErr != nil {
			return fnErr
		}
		// WATCH 冲突(redis.TxFailedErr) — 重试
		if txErr == redis.TxFailedErr {
			continue
		}
		// 其他 redis 错误 — 不重试
		return txErr
	}
	return errcode.New(errcode.ErrTeamConcurrent, "team %d update concurrent retry exhausted", teamID)
}

// --- Player index ---

func (r *RedisTeamRepo) GetPlayerTeamID(ctx context.Context, playerID uint64) (uint64, bool, error) {
	val, err := r.rdb.Get(ctx, playerKey(playerID)).Result()
	if err == redis.Nil {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	teamID, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return 0, false, err
	}
	return teamID, true, nil
}

func (r *RedisTeamRepo) SetPlayerIndex(ctx context.Context, playerID, teamID uint64, ttl time.Duration) error {
	return r.rdb.Set(ctx, playerKey(playerID), strconv.FormatUint(teamID, 10), ttl).Err()
}

// ClaimPlayer 用 SETNX 原子声明 player→teamID 归属(不变量 §1)。
func (r *RedisTeamRepo) ClaimPlayer(ctx context.Context, playerID, teamID uint64, ttl time.Duration) (uint64, bool, error) {
	key := playerKey(playerID)
	val := strconv.FormatUint(teamID, 10)
	// 最多两次:首次 SETNX 失败后若发现刚好过期(redis.Nil)再抢一次。
	for attempt := 0; attempt < 2; attempt++ {
		ok, err := r.rdb.SetNX(ctx, key, val, ttl).Result()
		if err != nil {
			return 0, false, err
		}
		if ok {
			return teamID, true, nil
		}
		cur, err := r.rdb.Get(ctx, key).Result()
		if err == redis.Nil {
			// 占用者刚好过期,重试一次 SETNX
			continue
		}
		if err != nil {
			return 0, false, err
		}
		existing, err := strconv.ParseUint(cur, 10, 64)
		if err != nil {
			return 0, false, err
		}
		return existing, false, nil
	}
	return 0, false, errcode.New(errcode.ErrTeamConcurrent, "claim player %d concurrent", playerID)
}

func (r *RedisTeamRepo) DeletePlayerIndex(ctx context.Context, playerID uint64) error {
	return r.rdb.Del(ctx, playerKey(playerID)).Err()
}

// ExpireTeam 单独刷新 team key 的 TTL(单条 EXPIRE,不读改写 value)。
func (r *RedisTeamRepo) ExpireTeam(ctx context.Context, teamID uint64, ttl time.Duration) error {
	return r.rdb.Expire(ctx, teamKey(teamID), ttl).Err()
}

// --- Invite ---

func (r *RedisTeamRepo) SetInvite(ctx context.Context, inviteID, teamID, targetPlayerID uint64, ttl time.Duration) error {
	key := inviteKey(inviteID)
	_, err := r.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, key,
			fieldTeamID, strconv.FormatUint(teamID, 10),
			fieldTargetPlayerID, strconv.FormatUint(targetPlayerID, 10),
		)
		pipe.Expire(ctx, key, ttl)
		return nil
	})
	return err
}

func (r *RedisTeamRepo) GetInvite(ctx context.Context, inviteID uint64) (*InviteRecord, bool, error) {
	fields, err := r.rdb.HGetAll(ctx, inviteKey(inviteID)).Result()
	if err != nil {
		return nil, false, err
	}
	if len(fields) == 0 {
		return nil, false, nil
	}
	teamID, err := strconv.ParseUint(fields[fieldTeamID], 10, 64)
	if err != nil {
		return nil, false, fmt.Errorf("invite %d bad team_id: %w", inviteID, err)
	}
	targetPlayerID, err := strconv.ParseUint(fields[fieldTargetPlayerID], 10, 64)
	if err != nil {
		return nil, false, fmt.Errorf("invite %d bad target_player_id: %w", inviteID, err)
	}
	return &InviteRecord{TeamID: teamID, TargetPlayerID: targetPlayerID}, true, nil
}

func (r *RedisTeamRepo) DeleteInvite(ctx context.Context, inviteID uint64) error {
	return r.rdb.Del(ctx, inviteKey(inviteID)).Err()
}

// ── 序列化辅助 ────────────────────────────────────────────────────────────────

func marshalTeam(team *teamv1.TeamStorageRecord) ([]byte, error) {
	if team == nil {
		return nil, fmt.Errorf("nil team")
	}
	return proto.Marshal(team)
}

// unmarshalTeam 从 Redis value 反序列化成 teamv1.TeamStorageRecord。
func unmarshalTeam(teamID uint64, payload []byte) (*teamv1.TeamStorageRecord, error) {
	rec := &teamv1.TeamStorageRecord{}
	if err := proto.Unmarshal(payload, rec); err != nil {
		return nil, fmt.Errorf("team %d bad proto: %w", teamID, err)
	}
	if rec.TeamId == 0 {
		rec.TeamId = teamID
	}
	if rec.TeamId != teamID {
		return nil, fmt.Errorf("team %d id mismatch: %d", teamID, rec.TeamId)
	}
	return rec, nil
}
