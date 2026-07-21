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
	"math"
	"strconv"
	"strings"

	"github.com/luyuancpp/pandora/pkg/cellroute"
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

	// router 是确定性 region/cell 路由器(scale-cellular-20m.md §4.2)。
	// 可为 nil:单 Cell / dev / 阶段 1~2 不分片,档案 owner 落点观测退化为不打日志(行为不变)。
	// 分片部署时由 main 经 SetCellRouter 注入,核心写(UpdateMMR)后额外打一条档案 owner 落点
	// 观测(供分片上线核对档案落点 == 玩家 owner cell,§4.2 line 142)。nil-safe。
	router *cellroute.Router

	// expPusher 把经验推送出箱行投 kafka(实时成长;nil-safe:未注入时出箱积压不丢)。
	// 由 main 经 SetExperiencePusher 注入(与 SetCellRouter 同风格)。
	expPusher ExperiencePusher
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

// SetCellRouter 注入确定性 region/cell 路由器(scale-cellular-20m.md §4.2 两级架构)。
//
// nil-safe:不调用 / 传 nil 时(单 Cell / dev / 阶段 1~2),不做档案 owner 落点观测,行为与历史
// 一致。用 setter 而非构造参数,避免单 Cell 阶段调用点被迫改签名(与 matchmaker / auction /
// battle_result / friend / chat / trade / dialogue / inventory / locator / push / team 一致)。
// Router 内部读路径无锁,并发安全。
func (u *PlayerUsecase) SetCellRouter(r *cellroute.Router) {
	u.router = r
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
	// 经验派生字段装饰(实时成长):满级 → is_max_level + 级内经验按 0 展示;
	// 曲线未配置(功能关闭)→ 不标满级,行为与历史一致。
	p.ExpInLevel, p.IsMaxLevel = u.DecorateExperience(p.GetLevel(), p.GetExpInLevel())
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
	// 分片:档案(含段位 mmr)是 owner 数据,锁定玩家 owner cell(ProfileShardKey=player_id,
	// §4.2 line 142)。router 为 nil(单 Cell)→ 不打,行为与历史一致。
	u.logProfilePlacement(ctx, playerID, "update_mmr")
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
	// 逐项校验并做溢出安全的累计:
	//   - Points 必须为正(只增点);
	//   - 同一 attr_key 的累计增量、以及所有增量之和都不得超过数据库有符号 INT 列上界
	//     (player_attributes.points / unspent_attr_points 均为 INT,上界 MaxInt32)。
	// 用 int64 累加并在每步与上界比较,单值 <= MaxInt32,累加前必然 < 2*MaxInt32,不会 int64 溢出。
	// 这里只堵「请求级」越界(总和 / 单键增量),列「当前值 + 增量」越界由 repo 在事务内权威兜底。
	perKey := make(map[string]int64, len(allocs))
	var sum int64
	for _, a := range allocs {
		if a.Key == "" {
			return 0, errcode.New(errcode.ErrInvalidArg, "attr_key must not be empty")
		}
		if a.Points <= 0 {
			return 0, errcode.New(errcode.ErrInvalidArg, "points must be positive: %s", a.Key)
		}
		perKey[a.Key] += int64(a.Points)
		if perKey[a.Key] > math.MaxInt32 {
			return 0, errcode.New(errcode.ErrInvalidArg, "attr %s allocation out of range", a.Key)
		}
		sum += int64(a.Points)
		if sum > math.MaxInt32 {
			// 总和超过 INT 列上界(必然 >= 任何可能的 unspent)→ 点数不足,零写入。
			return 0, errcode.New(errcode.ErrPlayerInsufficientPoints, "total allocation out of range")
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

// ── 出战装备预设 / 天赋树 ──────────────────────────────────────────────────────
//
// 边界(ds-arch.md §0.5):装备预设 / 天赋是大厅态持久化,开战前转成初始 GameplayEffect;
// 战斗内买装 / 换装 / 用道具走 UE GAS,不经 gRPC。

// SetEquipment 全量替换出战装备预设(功能开关关闭 → ErrPlayerFeatureDisabled;槽位重复 / 配置非法 → ErrInvalidArg)。
//
// ⚠️ 安全限制(2026-06-17 审查):此处**只校验槽位不重复 + item_config_id 非 0**,
// 尚未校验玩家是否拥有该装备 / item 是否为装备类型 / 槽位是否匹配。GetLoadout 会把装备转成
// Battle DS 初始效果,故在接 inventory/配置表做拥有权+类型+槽位校验前,**不可对客户端开放**
// (靠 LoadoutCustomizeEnabled 默认 false + player.v1 不在 Envoy 暴露双重关闭,见 conf.go 说明)。
// TODO(配置表就绪后):校验 ownEquipment(playerID,item) + isEquip(item) + slotMatch(item,slot)。
func (u *PlayerUsecase) SetEquipment(ctx context.Context, playerID uint64, slots []data.EquipmentSlot) error {
	if playerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if !u.cfg.LoadoutCustomizeEnabled {
		return errcode.New(errcode.ErrPlayerFeatureDisabled, "loadout customize disabled")
	}
	seen := make(map[uint32]struct{}, len(slots))
	for _, s := range slots {
		if s.ItemConfigID == 0 {
			return errcode.New(errcode.ErrInvalidArg, "item_config_id required for slot %d", s.Slot)
		}
		if _, dup := seen[s.Slot]; dup {
			return errcode.New(errcode.ErrInvalidArg, "duplicate slot %d", s.Slot)
		}
		seen[s.Slot] = struct{}{}
	}
	if err := u.repo.EnsureProfile(ctx, playerID, u.defaultNickname(playerID), u.cfg.BaseMMR); err != nil {
		return err
	}
	if err := u.repo.SetEquipment(ctx, playerID, slots); err != nil {
		return err
	}
	plog.With(ctx).Infow("msg", "set_equipment", "player_id", playerID, "slots", len(slots))
	return nil
}

// GetEquipment 读出战装备预设。
func (u *PlayerUsecase) GetEquipment(ctx context.Context, playerID uint64) ([]data.EquipmentSlot, error) {
	if playerID == 0 {
		return nil, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	return u.repo.GetEquipment(ctx, playerID)
}

// GrantTalentPoints 幂等授予天赋点(来源:升级 / 活动,系统驱动不受 LoadoutCustomizeEnabled 影响)。
func (u *PlayerUsecase) GrantTalentPoints(ctx context.Context, playerID uint64, points int32, idempotencyKey string) (int, error) {
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
	unspent, already, err := u.repo.GrantTalentPoints(ctx, playerID, points, idempotencyKey)
	if err != nil {
		return 0, err
	}
	if already {
		plog.With(ctx).Infow("msg", "grant_talent_idempotent_hit",
			"player_id", playerID, "idempotency_key", idempotencyKey, "unspent", unspent)
	}
	return unspent, nil
}

// SetTalents 全量重置天赋分配(功能开关关闭 → ErrPlayerFeatureDisabled;
// talent_id 重复 / level<=0 → ErrInvalidArg;sum(level) 超额 → ErrPlayerInsufficientPoints)。
func (u *PlayerUsecase) SetTalents(ctx context.Context, playerID uint64, talents []data.TalentLevel) (int, error) {
	if playerID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if !u.cfg.LoadoutCustomizeEnabled {
		return 0, errcode.New(errcode.ErrPlayerFeatureDisabled, "loadout customize disabled")
	}
	seen := make(map[uint32]struct{}, len(talents))
	for _, t := range talents {
		if t.TalentID == 0 {
			return 0, errcode.New(errcode.ErrInvalidArg, "talent_id required")
		}
		if t.Level <= 0 {
			return 0, errcode.New(errcode.ErrInvalidArg, "level must be positive: talent=%d", t.TalentID)
		}
		if _, dup := seen[t.TalentID]; dup {
			return 0, errcode.New(errcode.ErrInvalidArg, "duplicate talent_id %d", t.TalentID)
		}
		seen[t.TalentID] = struct{}{}
	}
	if err := u.repo.EnsureProfile(ctx, playerID, u.defaultNickname(playerID), u.cfg.BaseMMR); err != nil {
		return 0, err
	}
	return u.repo.SetTalents(ctx, playerID, talents)
}

// ResetTalents 清空天赋分配(功能开关关闭 → ErrPlayerFeatureDisabled)。
func (u *PlayerUsecase) ResetTalents(ctx context.Context, playerID uint64) (int, error) {
	if playerID == 0 {
		return 0, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	if !u.cfg.LoadoutCustomizeEnabled {
		return 0, errcode.New(errcode.ErrPlayerFeatureDisabled, "loadout customize disabled")
	}
	if err := u.repo.EnsureProfile(ctx, playerID, u.defaultNickname(playerID), u.cfg.BaseMMR); err != nil {
		return 0, err
	}
	return u.repo.ResetTalents(ctx, playerID)
}

// GetTalents 读已点天赋 + 可点天赋点。
func (u *PlayerUsecase) GetTalents(ctx context.Context, playerID uint64) ([]data.TalentLevel, int, error) {
	if playerID == 0 {
		return nil, 0, errcode.New(errcode.ErrInvalidArg, "player_id required")
	}
	return u.repo.GetTalents(ctx, playerID)
}

// GetLoadout 组装开战前快照(出战英雄 + 属性点 + 装备预设 + 天赋),供匹配/进战下发。
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
	equip, err := u.repo.GetEquipment(ctx, playerID)
	if err != nil {
		return nil, err
	}
	eq := make([]*playerv1.LoadoutEquipment, 0, len(equip))
	for _, s := range equip {
		eq = append(eq, &playerv1.LoadoutEquipment{Slot: s.Slot, ItemConfigId: s.ItemConfigID})
	}
	talents, talentUnspent, err := u.repo.GetTalents(ctx, playerID)
	if err != nil {
		return nil, err
	}
	tl := make([]*playerv1.TalentNode, 0, len(talents))
	for _, t := range talents {
		tl = append(tl, &playerv1.TalentNode{TalentId: t.TalentID, Level: t.Level})
	}
	return &playerv1.PlayerLoadout{
		PlayerId:            playerID,
		ActiveHeroId:        heroID,
		Attributes:          pts,
		UnspentAttrPoints:   int32(unspent),
		Equipment:           eq,
		Talents:             tl,
		UnspentTalentPoints: int32(talentUnspent),
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
