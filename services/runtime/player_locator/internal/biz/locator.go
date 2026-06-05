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
//	W4+ 接 DS 注册表后,加 Conflict 检测(同 player 被多个 DS 上报 → ErrLocatorConflict)。
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
	if err := u.repo.Set(ctx, in.PlayerID, rec, u.ttl); err != nil {
		return err
	}
	plog.With(ctx).Infow("msg", "location_set",
		"player_id", in.PlayerID, "state", in.State,
		"hub_pod", in.HubPod, "match_id", in.MatchID, "battle_pod", in.BattlePod,
		"ttl_ms", u.ttl.Milliseconds())
	return nil
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
