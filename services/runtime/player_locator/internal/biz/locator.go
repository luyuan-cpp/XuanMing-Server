// Package biz 是 player_locator 服务的业务用例层。
//
// W3 ⑤(2026-06-05):
//   - SetLocation 输入校验 + 调 redis 覆盖式写
//   - GetLocation 返回 OFFLINE 状态当 key miss(state=LOCATION_STATE_OFFLINE=1)
//   - ClearLocation 直接 Delete
//
// 不变量 §1(CLAUDE.md §9.1)"玩家只能在一个 Location":
//
//	redis hash 是单写者(SetLocation),覆盖语义 = 自动顶号;
//	W4 ⑩(2026-06-06):接 hub DS 上报后,加状态机守卫(guardTransition):
//	只有 HUB 上报来自数据面(hub DS),可能 stale;LOGIN_PENDING / MATCHING / BATTLE
//	来自可信控制面(login / matchmaker),直接顶号。HUB 上报覆盖控制面 MATCHING 时
//	返回 ErrLocatorConflict(玩家在确认期仍连 hub DS,hub DS 会持续上报 HUB,
//	必须挡住以免顶掉 matchmaker 刚写的 MATCHING)。
package biz

import (
	"context"
	"time"

	"github.com/luyuancpp/pandora/pkg/errcode"
	plog "github.com/luyuancpp/pandora/pkg/log"

	"github.com/luyuancpp/pandora/services/runtime/player_locator/internal/data"
)

// LocationState 是 biz 层的 location state(跟 proto enum 数值 1:1)。
const (
	LocationStateUnspecified  int32 = 0
	LocationStateOffline      int32 = 1
	LocationStateLoginPending int32 = 2
	LocationStateHub          int32 = 3
	LocationStateMatching     int32 = 4
	LocationStateBattle       int32 = 5
)

// optimisticRetry 是 SetGuarded WATCH/MULTI/EXEC 的 CAS 冲突重试次数。
const optimisticRetry = 3

// LocationInput 是 SetLocation 的入参(从 service 层 proto 翻译)。
type LocationInput struct {
	PlayerID  uint64
	State     int32
	HubPod    string
	ShardID   uint32
	MatchID   uint64
	BattlePod string
}

// LocationOutput 是 GetLocation 的出参。
type LocationOutput struct {
	State       int32
	HubPod      string
	ShardID     uint32
	MatchID     uint64
	BattlePod   string
	UpdatedAtMs int64
}

// LocatorUsecase 实现 SetLocation / GetLocation / ClearLocation。
type LocatorUsecase struct {
	repo data.LocationRepo
	ttl  time.Duration
}

// NewLocatorUsecase 构造用例。
func NewLocatorUsecase(repo data.LocationRepo, ttl time.Duration) *LocatorUsecase {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &LocatorUsecase{repo: repo, ttl: ttl}
}

// SetLocation 写入 redis hash。
//
// 校验:
//   - playerID > 0
//   - state 在合法枚举范围(0-5)
//   - state=HUB → hub_pod 非空
//   - state=MATCHING / BATTLE → match_id 非空
//   - state=BATTLE → battle_pod 非空
func (u *LocatorUsecase) SetLocation(ctx context.Context, in LocationInput) error {
	if in.PlayerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "player_id must > 0")
	}
	if in.State < LocationStateUnspecified || in.State > LocationStateBattle {
		return errcode.New(errcode.ErrInvalidArg, "invalid state=%d", in.State)
	}
	switch in.State {
	case LocationStateHub:
		if in.HubPod == "" {
			return errcode.New(errcode.ErrInvalidArg, "HUB state requires hub_pod")
		}
	case LocationStateMatching:
		if in.MatchID == 0 {
			return errcode.New(errcode.ErrInvalidArg, "MATCHING state requires match_id")
		}
	case LocationStateBattle:
		if in.MatchID == 0 || in.BattlePod == "" {
			return errcode.New(errcode.ErrInvalidArg, "BATTLE state requires match_id + battle_pod")
		}
	}

	rec := data.LocationRecord{
		State:       in.State,
		HubPod:      in.HubPod,
		ShardID:     in.ShardID,
		MatchID:     in.MatchID,
		BattlePod:   in.BattlePod,
		UpdatedAtMs: time.Now().UnixMilli(),
	}
	if err := u.repo.SetGuarded(ctx, in.PlayerID, rec, u.ttl, optimisticRetry, guardTransition(in)); err != nil {
		return err
	}
	plog.With(ctx).Infow("msg", "location_set",
		"player_id", in.PlayerID, "state", in.State,
		"hub_pod", in.HubPod, "match_id", in.MatchID, "battle_pod", in.BattlePod,
		"ttl_ms", u.ttl.Milliseconds())
	return nil
}

// guardTransition 返回 SetGuarded 的状态机守卫闭包,实现不变量 §1。
//
// 守卫只针对 HUB 上报(唯一来自数据面 hub DS、可能 stale 的写):
//   - 当前是 MATCHING 时拒绝 HUB 上报 → ErrLocatorConflict。
//     玩家在撮合确认期物理上仍连着 hub DS,hub DS 会持续上报 HUB,
//     若放行会把 matchmaker 刚写的 MATCHING 顶掉,使其他服务误判玩家仍在大厅闲逛。
//
// 控制面写(LOGIN_PENDING / MATCHING / BATTLE 来自 login / matchmaker)一律放行(顶号语义)。
// BATTLE→HUB(战斗结束返回大厅)是合法回流,放行;stale hub DS 顶掉 BATTLE 的极端场景
// 需要 fence/match_id 令牌区分,留待 hub DS(UE)落地后做。
func guardTransition(in LocationInput) func(cur data.LocationRecord, found bool) error {
	return func(cur data.LocationRecord, found bool) error {
		if !found {
			return nil
		}
		if in.State == LocationStateHub && cur.State == LocationStateMatching {
			return errcode.New(errcode.ErrLocatorConflict,
				"player %d in MATCHING(match_id=%d), reject stale HUB report pod=%s",
				in.PlayerID, cur.MatchID, in.HubPod)
		}
		return nil
	}
}

// GetLocation 读 redis hash;key 不存在返回 OFFLINE 占位记录(不报错)。
func (u *LocatorUsecase) GetLocation(ctx context.Context, playerID uint64) (LocationOutput, error) {
	if playerID == 0 {
		return LocationOutput{}, errcode.New(errcode.ErrInvalidArg, "player_id must > 0")
	}
	rec, found, err := u.repo.Get(ctx, playerID)
	if err != nil {
		return LocationOutput{}, err
	}
	if !found {
		// 不变量 §1:不存在等价 OFFLINE,客户端 / DS 看到这个就知道"玩家不在线"
		return LocationOutput{State: LocationStateOffline}, nil
	}
	return LocationOutput{
		State:       rec.State,
		HubPod:      rec.HubPod,
		ShardID:     rec.ShardID,
		MatchID:     rec.MatchID,
		BattlePod:   rec.BattlePod,
		UpdatedAtMs: rec.UpdatedAtMs,
	}, nil
}

// ClearLocation Unlink redis hash。
func (u *LocatorUsecase) ClearLocation(ctx context.Context, playerID uint64) error {
	if playerID == 0 {
		return errcode.New(errcode.ErrInvalidArg, "player_id must > 0")
	}
	if err := u.repo.Delete(ctx, playerID); err != nil {
		return err
	}
	plog.With(ctx).Infow("msg", "location_cleared", "player_id", playerID)
	return nil
}
