// locator_sharding.go 是玩家位置(location)owner cell 锚定 + 位置存储分片键口径的服务内纯逻辑
// (nil-safe 接线)。
//
// 背景(scale-cellular-20m.md §4.2 owner 不变量 + 不变量 §1「玩家在线只能在一个 Location」):
// 玩家位置状态是该玩家 owner 数据,同一 player_id 的 location 必落同一 owner cell。多 Cell 下若
// 位置读写分散到不同 cell,会破坏「单写者覆盖 = 自动顶号」前提(同一玩家两个 cell 各持一份 location,
// 顶号失效 → 玩家可能被判定同时在两处),直接撞 §1 不变量。因此位置存储分片键必须取 player_id
// (owner cell 决定者),保证 SetLocation / GetLocation / ClearLocation 三类操作落同一 cell。
//
// 本文件只落服务内纯逻辑,不改现状位置存储(redis hash by player_id)与状态机守卫实现:
//   - 统一位置存储分片键口径(LocationShardKey = player_id),为分片落地时把位置路由到玩家
//     owner cell 提供单一口径,避免误用其它维度(hub_pod / shard_id)分片导致位置与 owner 数据跨 cell。
//   - 用确定性 cellroute.Router 把玩家解析到 owner (region, cell),作为可观测信号,供分片上线
//     核对「位置落点 == 玩家 owner cell」。
//
// 边界(AGENTS.md §11.1):真正的位置 redis 按 owner cell 分片 / 跨 cell 顶号一致性属基础设施,
// 由 Codex/人接;本轮 router 为 nil(单 Cell)时行为不变,只在注入后打观测日志。
package biz

import (
	"context"
	"strconv"

	plog "github.com/luyuancpp/pandora/pkg/log"
)

// LocationShardKey 是玩家位置状态的存储分片键口径(canonical)。
//
// 口径统一:= player_id 十进制串(owner cell 决定者,scale-cellular-20m.md §4.2 owner 不变量)。
// 位置归属单个玩家,必须锚定该玩家 owner cell 以保「单写者覆盖 = 自动顶号」(不变量 §1);
// **不取 hub_pod / shard_id / battle_pod**(那些是运行时落点,与 owner 分片无关,误用会让同一
// 玩家位置随状态在不同 cell 漂移)。纯函数,确定性;player_id 为 0 时返回 "0"(调用方应先校验非 0)。
func LocationShardKey(playerID uint64) string {
	return strconv.FormatUint(playerID, 10)
}

// LocationOwner 是一名玩家位置锚定的 owner 落点(只取分片落点判定需要的维度)。
type LocationOwner struct {
	RegionID uint32
	CellID   uint32
}

// locationOwner 解析一名玩家位置锚定的 owner 落点 (region, cell)。
// router 为 nil(单 Cell / dev)或路由失败 / player_id 为 0 → 返回 (LocationOwner{}, false),
// 调用方退化为不做观测(单 Cell 本地位置语义不变)。
func (u *LocatorUsecase) locationOwner(playerID uint64) (LocationOwner, bool) {
	if u.router == nil || playerID == 0 {
		return LocationOwner{}, false
	}
	loc, err := u.router.Route(playerID)
	if err != nil {
		return LocationOwner{}, false
	}
	return LocationOwner{RegionID: loc.RegionID, CellID: loc.CellID}, true
}

// logLocationPlacement 在 router 注入后,把一次位置写入锚定的 owner 落点打成观测日志。
//
// 仅可观测,不改位置路径:位置 redis 按 owner cell 分片 / 跨 cell 顶号一致性属基础设施
// (AGENTS.md §11.1,由 Codex/人接);本处只暴露「本玩家位置锚定哪个 region/cell」信号,供分片
// 上线核对位置落点与玩家其余 owner 数据同 cell(防 §1 单写者覆盖前提被破坏)。router 为 nil
// (单 Cell)时不调用此路径,行为不变。
func (u *LocatorUsecase) logLocationPlacement(ctx context.Context, playerID uint64, state int32) {
	loc, ok := u.locationOwner(playerID)
	if !ok {
		return
	}
	plog.With(ctx).Debugw("msg", "location_placement",
		"player_id", playerID,
		"state", state,
		"region", loc.RegionID,
		"cell", loc.CellID,
		"shard_key", LocationShardKey(playerID))
}
