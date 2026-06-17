// Package biz 是 player 服务的业务逻辑层(W4 ④,2026-06-06)。
//
// 职责(docs/design/go-services.md §2.2):
//   - 玩家档案(昵称 / 等级 / 段位 mmr / 战绩计数)读写
//   - 英雄解锁池
//   - MMR 读写:写由 battle_result 经 pandora.player.update 驱动,必须幂等
//     (idempotency_key=match_id,不变量 §2);GetMMR 供 battle_result 当 MMRReader
//
// 关键不变量:
//   - UpdateMMR 幂等(同一 idempotency_key 只算一次,mmr_history uk 兜底)
//   - 档案懒创建:GetProfile / 写操作前 EnsureProfile,保证后续行存在
package biz

import (
	"context"
	"strconv"
	"strings"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"

	"github.com/luyuancpp/pandora/services/account/player/internal/conf"
	"github.com/luyuancpp/pandora/services/account/player/internal/data"
)

// PlayerUsecase 是 player 服务业务逻辑核心。
type PlayerUsecase struct {
	repo data.PlayerRepo
	cfg  conf.PlayerConf
}

// NewPlayerUsecase 构造。
func NewPlayerUsecase(repo data.PlayerRepo, cfg conf.PlayerConf) *PlayerUsecase {
	if cfg.BaseMMR <= 0 {
		cfg.BaseMMR = 1500
	}
	if cfg.DefaultNicknamePrefix == "" {
		cfg.DefaultNicknamePrefix = "Player_"
	}
	if cfg.MaxNicknameLen <= 0 {
		cfg.MaxNicknameLen = 32
	}
	return &PlayerUsecase{repo: repo, cfg: cfg}
}

// defaultNickname 给新玩家生成唯一默认昵称(prefix + player_id,保证 uk_nickname 不冲突)。
func (u *PlayerUsecase) defaultNickname(playerID uint64) string {
	return u.cfg.DefaultNicknamePrefix + strconv.FormatUint(playerID, 10)
}

// ── 档案 ──────────────────────────────────────────────────────────────────────

// GetProfile 读玩家档案(懒创建:首次访问自动建默认档案)。
func (u *PlayerUsecase) GetProfile(ctx context.Context, playerID uint64) (*playerv1.PlayerProfile, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if err := u.repo.EnsureProfile(ctx, playerID, u.defaultNickname(playerID), u.cfg.BaseMMR); err != nil {
		return nil, err
	}
	p, found, err := u.repo.GetProfile(ctx, playerID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errcode.New(errcode.ErrPlayerNotFound, "player not found: %d", playerID)
	}
	return p, nil
}

// UpdateNickname 改昵称(懒创建档案后更新)。
func (u *PlayerUsecase) UpdateNickname(ctx context.Context, playerID uint64, nickname string) error {
	if playerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		return errcode.New(errcode.ErrInvalidArg, "nickname must not be empty")
	}
	if len([]rune(nickname)) > u.cfg.MaxNicknameLen {
		return errcode.New(errcode.ErrInvalidArg, "nickname too long (max %d)", u.cfg.MaxNicknameLen)
	}
	if err := u.repo.EnsureProfile(ctx, playerID, u.defaultNickname(playerID), u.cfg.BaseMMR); err != nil {
		return err
	}
	return u.repo.UpdateNickname(ctx, playerID, nickname)
}

// ── 英雄 ──────────────────────────────────────────────────────────────────────

// ListHeroes 列出玩家已解锁英雄。
func (u *PlayerUsecase) ListHeroes(ctx context.Context, playerID uint64) ([]uint32, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	return u.repo.ListHeroes(ctx, playerID)
}

// UnlockHero 解锁英雄(幂等:已拥有返回 ErrPlayerHeroAlreadyOwn)。
func (u *PlayerUsecase) UnlockHero(ctx context.Context, playerID uint64, heroID uint32, source string) error {
	if playerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if heroID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "hero_id required")
	}
	if err := u.repo.EnsureProfile(ctx, playerID, u.defaultNickname(playerID), u.cfg.BaseMMR); err != nil {
		return err
	}
	already, err := u.repo.UnlockHero(ctx, playerID, heroID, source)
	if err != nil {
		return err
	}
	if already {
		return errcode.New(errcode.ErrPlayerHeroAlreadyOwn, "hero already owned: player=%d hero=%d", playerID, heroID)
	}
	return nil
}

// ── MMR ──────────────────────────────────────────────────────────────────────

// GetMMR 读玩家当前 MMR(未建档 → 返回 BaseMMR,不创建行;供 battle_result 当 reader)。
func (u *PlayerUsecase) GetMMR(ctx context.Context, playerID uint64) (int, error) {
	if playerID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	mmr, found, err := u.repo.GetMMR(ctx, playerID)
	if err != nil {
		return 0, err
	}
	if !found {
		return u.cfg.BaseMMR, nil
	}
	return mmr, nil
}

// UpdateMMR 幂等改 MMR + 战绩计数(idempotency_key 一般是 match_id,不变量 §2)。
// 返回 (新 MMR, 是否幂等命中, error)。
func (u *PlayerUsecase) UpdateMMR(ctx context.Context, playerID uint64, delta int32, reason, idempotencyKey string) (int, bool, error) {
	if playerID == 0 {
		return 0, false, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if idempotencyKey == "" {
		return 0, false, errcode.New(errcode.ErrInvalidArg, "idempotency_key required")
	}

	if err := u.repo.EnsureProfile(ctx, playerID, u.defaultNickname(playerID), u.cfg.BaseMMR); err != nil {
		return 0, false, err
	}

	incBattle, incWin := battleFlags(reason)
	newMMR, already, err := u.repo.ApplyMMRChange(ctx, data.MMRChange{
		PlayerID:       playerID,
		IdempotencyKey: idempotencyKey,
		Delta:          delta,
		Reason:         reason,
		Floor:          u.cfg.MMRFloor,
		IncBattle:      incBattle,
		IncWin:         incWin,
	})
	if err != nil {
		return 0, false, err
	}
	if already {
		plog.With(ctx).Infow("msg", "update_mmr_idempotent_hit",
			"player_id", playerID, "idempotency_key", idempotencyKey, "new_mmr", newMMR)
		return newMMR, true, nil
	}
	plog.With(ctx).Infow("msg", "update_mmr_applied",
		"player_id", playerID, "delta", delta, "reason", reason, "new_mmr", newMMR)
	return newMMR, false, nil
}

// ── 出战养成(选英雄 / 加点 / 出战快照)──────────────────────────────────────────
//
// 边界(docs/design/ds-arch.md §0):这里只管大厅态持久化与配置,纯战斗内逻辑(技能/出装/
// 道具即时使用)走 UE GAS,不经 gRPC。GetLoadout 提供"开战前快照",供匹配/进战时下发。

// SelectHero 设定出战英雄。
//   - 功能开关 HeroSelectionEnabled=false → ErrPlayerFeatureDisabled(demo 阶段可跳过)
//   - 英雄未解锁 → ErrPlayerHeroLocked(只能选已拥有英雄)
func (u *PlayerUsecase) SelectHero(ctx context.Context, playerID uint64, heroID uint32) error {
	if playerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if heroID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "hero_id required")
	}
	if !u.cfg.HeroSelectionEnabled {
		return errcode.New(errcode.ErrPlayerFeatureDisabled, "hero selection disabled")
	}
	if err := u.repo.EnsureProfile(ctx, playerID, u.defaultNickname(playerID), u.cfg.BaseMMR); err != nil {
		return err
	}
	owned, err := u.repo.IsHeroOwned(ctx, playerID, heroID)
	if err != nil {
		return err
	}
	if !owned {
		return errcode.New(errcode.ErrPlayerHeroLocked, "hero not owned: player=%d hero=%d", playerID, heroID)
	}
	if err := u.repo.SetActiveHero(ctx, playerID, heroID); err != nil {
		return err
	}
	plog.With(ctx).Infow("msg", "select_hero", "player_id", playerID, "hero_id", heroID)
	return nil
}

// GetActiveHero 读出战英雄(未选定 → 返回 0)。
func (u *PlayerUsecase) GetActiveHero(ctx context.Context, playerID uint64) (uint32, error) {
	if playerID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	return u.repo.GetActiveHero(ctx, playerID)
}

// GrantAttributePoints 幂等授予可分配点(来源:升级 / 活动,idempotency_key 防重复授予)。
func (u *PlayerUsecase) GrantAttributePoints(ctx context.Context, playerID uint64, points int32, idempotencyKey string) (int, error) {
	if playerID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if points <= 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "points must be positive")
	}
	if idempotencyKey == "" {
		return 0, errcode.New(errcode.ErrInvalidArg, "idempotency_key required")
	}
	if err := u.repo.EnsureProfile(ctx, playerID, u.defaultNickname(playerID), u.cfg.BaseMMR); err != nil {
		return 0, err
	}
	unspent, already, err := u.repo.GrantAttributePoints(ctx, playerID, points, idempotencyKey)
	if err != nil {
		return 0, err
	}
	if already {
		plog.With(ctx).Infow("msg", "grant_attr_idempotent_hit",
			"player_id", playerID, "idempotency_key", idempotencyKey, "unspent", unspent)
	}
	return unspent, nil
}

// AllocateAttributePoints 分配属性点(点数不足 → ErrPlayerInsufficientPoints)。
func (u *PlayerUsecase) AllocateAttributePoints(ctx context.Context, playerID uint64, allocs []data.AttrAllocation) (int, error) {
	if playerID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if len(allocs) == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "allocations required")
	}
	for _, a := range allocs {
		if a.Key == "" {
			return 0, errcode.New(errcode.ErrInvalidArg, "attr_key must not be empty")
		}
		if a.Points <= 0 {
			return 0, errcode.New(errcode.ErrInvalidArg, "points must be positive: %s", a.Key)
		}
	}
	if err := u.repo.EnsureProfile(ctx, playerID, u.defaultNickname(playerID), u.cfg.BaseMMR); err != nil {
		return 0, err
	}
	return u.repo.AllocateAttributePoints(ctx, playerID, allocs)
}

// ResetAttributes 洗点(已分配点全退回可分配点)。
func (u *PlayerUsecase) ResetAttributes(ctx context.Context, playerID uint64) (int, error) {
	if playerID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if err := u.repo.EnsureProfile(ctx, playerID, u.defaultNickname(playerID), u.cfg.BaseMMR); err != nil {
		return 0, err
	}
	return u.repo.ResetAttributes(ctx, playerID)
}

// GetAttributes 读已分配属性点 + 未分配点。
func (u *PlayerUsecase) GetAttributes(ctx context.Context, playerID uint64) ([]data.AttrPoint, int, error) {
	if playerID == 0 {
		return nil, 0, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	return u.repo.GetAttributes(ctx, playerID)
}

// GetLoadout 组装开战前快照(出战英雄 + 属性点 + 未分配点),供匹配/进战下发。
func (u *PlayerUsecase) GetLoadout(ctx context.Context, playerID uint64) (*playerv1.PlayerLoadout, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	heroID, err := u.repo.GetActiveHero(ctx, playerID)
	if err != nil {
		return nil, err
	}
	attrs, unspent, err := u.repo.GetAttributes(ctx, playerID)
	if err != nil {
		return nil, err
	}
	pts := make([]*playerv1.AttributeAllocation, 0, len(attrs))
	for _, a := range attrs {
		pts = append(pts, &playerv1.AttributeAllocation{AttrKey: a.Key, Points: a.Points})
	}
	return &playerv1.PlayerLoadout{
		PlayerId:          playerID,
		ActiveHeroId:      heroID,
		Attributes:        pts,
		UnspentAttrPoints: int32(unspent),
	}, nil
}

// battleFlags 按 reason 决定是否计对局 / 计胜。
//
//   - win:计一场 + 计一胜
//   - lose / draw:计一场,不计胜
//   - abandon:对局作废,不计场不计胜(delta 应为 0)
//   - rollback / 其它:纯 MMR 修正,不计场不计胜
func battleFlags(reason string) (incBattle, incWin bool) {
	switch reason {
	case "win":
		return true, true
	case "lose", "draw":
		return true, false
	default:
		return false, false
	}
}
