// team_sharding.go 是队伍 owner cell 锚定 + 跨 region 组队判定的服务内纯逻辑(nil-safe 接线)。
//
// 背景(scale-cellular-20m.md §4.2 owner 不变量 + §4.4 两级撮合):
//   - 队伍是「队长拥有」的多人聚合,其单写者存储(redis,UpdateWithLock)必须锚定**队长 owner cell**
//     (队长是队伍 owner;TeamShardKey=captain_id),保证同一队伍的所有写落同一 cell,
//     不破坏「单写者覆盖」前提(不变量 §1 一人一队由 ClaimPlayer 保证,队伍状态机由单 cell 串行)。
//   - 跨 region 组队(成员落不同 owner region)是合法的(§4.4 已放开社交/匹配跨 region):
//     这类队伍进撮合后,battle DS 在「参战玩家多数所在 region 的 Cell」拉起(MajorityRegion,
//     见 matchmaker region_affinity.go),少数派成员承担稍高 RTT,结算仍各自回 owner cell。
//
// 本文件只落服务内纯逻辑,不改现状队伍存储(redis by team_id + ClaimPlayer 索引)实现:
//   - 统一队伍存储分片键口径(TeamShardKey = captain_id),为分片落地把队伍锚定到队长 owner cell
//     提供单一口径,避免误用 team_id(snowflake,与落点无关)分片导致队伍与队长 owner 数据跨 cell。
//   - 用确定性 cellroute.Router 解析成员 owner region,判定本队是否跨 region(CrossRegionTeam),
//     作为可观测信号,供撮合 / battle 放置评估跨 region 组队占比。
//
// 边界(AGENTS.md §11.1):真正的队伍 redis 按 owner cell 分片 / battle DS 跨 region 放置属基础设施,
// 由 Codex/人接;本轮 router 为 nil(单 Cell)时行为不变,只在注入后打观测日志。
package biz

import (
	"context"
	"sort"
	"strconv"

	plog "github.com/luyuancpp/pandora/pkg/log"

	teamv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/team/v1"
)

// TeamShardKey 是队伍存储的分片键口径(canonical)。
//
// 口径统一:= captain_id 十进制串(队长是队伍 owner,owner cell 决定者,§4.2)。队伍状态机须由
// 单 cell 串行写;**不取 team_id**(snowflake 与落点无关,误用会让队伍与队长 owner 数据落不同 cell)。
// 纯函数,确定性;captain_id 为 0 时返回 "0"(调用方应先校验非 0)。
func TeamShardKey(captainID uint64) string {
	return strconv.FormatUint(captainID, 10)
}

// DistinctTeamRegions 返回一组成员 owner region 去重 + 升序后的列表(空 → nil)。
// 用于判定队伍跨 region 分布;确定性(升序)便于日志比对。
func DistinctTeamRegions(regions []uint32) []uint32 {
	if len(regions) == 0 {
		return nil
	}
	seen := make(map[uint32]struct{}, len(regions))
	out := make([]uint32, 0, len(regions))
	for _, r := range regions {
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// CrossRegionTeam 判断一组成员 owner region 是否跨 region(落 ≥2 个不同 region)。
// 空 / 单 region → false(本地组队)。
func CrossRegionTeam(regions []uint32) bool {
	return len(DistinctTeamRegions(regions)) > 1
}

// teamMemberRegions 解析队伍全体成员的 owner region 列表。
// router 为 nil(单 Cell / dev)→ 返回 (nil, false),调用方退化为不做观测(单 Cell 本地组队语义不变)。
// 单个成员 player_id=0 或路由失败时跳过该成员(尽力解析,不阻断)。
func (u *TeamUsecase) teamMemberRegions(team *teamv1.TeamStorageRecord) ([]uint32, bool) {
	if u.router == nil || team == nil {
		return nil, false
	}
	out := make([]uint32, 0, len(team.Members))
	for _, m := range team.Members {
		if m.GetPlayerId() == 0 {
			continue
		}
		loc, err := u.router.Route(m.GetPlayerId())
		if err != nil {
			continue
		}
		out = append(out, loc.RegionID)
	}
	return out, true
}

// logTeamComposition 在 router 注入后,把一次队伍成员变更后的 region 分布打成观测日志。
//
// 仅可观测,不改队伍路径:队伍 redis 按 owner cell 分片 / battle DS 跨 region 放置属基础设施
// (AGENTS.md §11.1,由 Codex/人接);本处只暴露「本队是否跨 region 及涉及哪些 region」信号,
// 供撮合 / battle 放置评估跨 region 组队占比。router 为 nil(单 Cell)时不调用此路径,行为不变。
func (u *TeamUsecase) logTeamComposition(ctx context.Context, team *teamv1.TeamStorageRecord) {
	regions, ok := u.teamMemberRegions(team)
	if !ok {
		return
	}
	distinct := DistinctTeamRegions(regions)
	plog.With(ctx).Debugw("msg", "team_composition_routing",
		"team_id", team.GetTeamId(),
		"captain_id", team.GetCaptainId(),
		"member_count", len(team.Members),
		"region_count", len(distinct),
		"cross_region", len(distinct) > 1,
		"shard_key", TeamShardKey(team.GetCaptainId()))
}
