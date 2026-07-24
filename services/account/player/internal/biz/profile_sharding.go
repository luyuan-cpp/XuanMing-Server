// profile_sharding.go 是玩家档案 owner cell 锚定的服务内纯逻辑(nil-safe 接线)。
//
// 背景(scale-cellular-20m.md §4.2 owner 不变量,line 142):
//
//	「同一 player_id 的所有 owner 数据(档案 / 背包 / 段位 / 好友)必落同一 region_id 同一 cell_id」。
//
// 玩家档案(昵称 / 等级 / 段位 mmr / 战绩 / 英雄 / 加点 / 天赋)是最核心的 owner 数据,其 MySQL 行 +
// 幂等记录必须锚定玩家 owner cell(ProfileShardKey = player_id),保证档案与背包 / 好友等同源 owner
// 数据同 cell,避免跨 cell 读写放大与 mmr 幂等键(idempotency_key=match_id)跨 cell 漂移。
//
// 本文件只落服务内纯逻辑,不改现状档案存储(MySQL by player_id + EnsureProfile 懒创建)实现:
//   - 统一档案存储分片键口径(ProfileShardKey = player_id),为分片落地把档案锚定到玩家 owner cell
//     提供单一口径,**不取 nickname / hero_id / 任何配置 ID**(与落点无关)。
//   - 用确定性 cellroute.Router 解析玩家 owner (region, cell),在核心写(UpdateMMR)成功后接观测,
//     供分片上线核对「档案落点 == 玩家 owner cell」。
//
// 边界(AGENTS.md §11.1):真正的档案 MySQL 按 owner cell 分库 / 跨 cell 一致性属基础设施,
// 由 Codex/人接;本轮 router 为 nil(单 Cell)时行为不变,只在注入后打观测日志。
package biz

import (
	"context"
	"strconv"

	plog "github.com/luyuancpp/pandora/pkg/log"
)

// ProfileShardKey 是玩家档案存储的分片键口径(canonical)。
//
// 口径统一:= player_id 十进制串(玩家 owner cell 决定者,§4.2 line 142)。同一玩家的档案 / 背包 /
// 段位 / 好友必落同一 owner cell;**不取 nickname / hero_id / 任何配置 ID**(与落点无关)。
// 纯函数,确定性;player_id 为 0 时返回 "0"(调用方应先校验非 0)。
func ProfileShardKey(playerID uint64) string {
	return strconv.FormatUint(playerID, 10)
}

// ProfileOwner 是玩家档案 owner 落点(region + cell)。
type ProfileOwner struct {
	RegionID uint32
	CellID   uint32
}

// profileOwner 经确定性路由解析玩家 owner 落点。
// router 为 nil(单 Cell / dev)或 player_id=0 或路由失败 → 返回 (ProfileOwner{}, false),调用方退化
// 为不做观测(单 Cell 档案落点语义不变)。
func (u *PlayerUsecase) profileOwner(playerID uint64) (ProfileOwner, bool) {
	if u.router == nil || playerID == 0 {
		return ProfileOwner{}, false
	}
	loc, err := u.router.Route(playerID)
	if err != nil {
		return ProfileOwner{}, false
	}
	return ProfileOwner{RegionID: loc.RegionID, CellID: loc.CellID}, true
}

// logProfilePlacement 在 router 注入后,把一次档案写后的 owner 落点打成观测日志。
//
// 仅可观测,不改档案路径:档案 MySQL 按 owner cell 分库属基础设施(AGENTS.md §11.1,由 Codex/人接);
// 本处只暴露「本次写所属玩家的 owner (region, cell) 与分片键」,供分片上线核对档案落点 == 玩家 owner
// cell(§4.2 line 142)。router 为 nil(单 Cell)时不调用此路径,行为不变。
func (u *PlayerUsecase) logProfilePlacement(ctx context.Context, playerID uint64, op string) {
	owner, ok := u.profileOwner(playerID)
	if !ok {
		return
	}
	plog.With(ctx).Debugw("msg", "profile_placement",
		"player_id", playerID,
		"op", op,
		"region", owner.RegionID,
		"cell", owner.CellID,
		"shard_key", ProfileShardKey(playerID))
}
