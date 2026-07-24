// settlement.go 是 battle_result 的跨 region 结算回流落点 + 幂等键口径(纯函数 + nil-safe 接线)。
//
// 背景(scale-cellular-20m.md §4.4 / §5):放开跨 region 匹配后,overflow(跨 region)对局的
// 参战玩家可能来自不同 region;但结算仍各自回各玩家的 owner cell(不变量:同一 player_id 的
// owner 数据永远落同一 region+cell,见 pkg/cellroute)。battle_result 落库后发的 player.update
// 出箱事件,在多 region 部署下需回流到每名玩家 owner region 的 player 服务。
//
// 本文件只做两件服务内纯逻辑:
//   - 统一"结算回流幂等键口径"(SettlementKey),与 player 服务 mmr_history 唯一键
//     (player_id, match_id)同维度,避免跨 region 桥 at-least-once 重投时口径漂移。
//   - 用确定性 cellroute.Router 把一场对局每名玩家解析到 owner (region, cell),判定本局是否
//     需要跨 region 回流(DistinctSettlementRegions / CrossRegionSettlement),作为可观测信号。
//
// 边界(AGENTS.md §11.1):真正的跨 region player.update 桥、多 region topic 路由、回流去重表
// 属基础设施,由 Codex/人接;本轮只落纯口径 + 落点观测,router 为 nil(单 Cell)时行为不变。
package biz

import (
	"context"
	"fmt"
	"sort"

	plog "github.com/luyuancpp/pandora/pkg/log"
	battlev1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/battle/v1"
)

// SettlementKey 是一名玩家在一场对局的结算回流幂等键口径。
//
// 口径统一:canonical "match_id:player_id",与 player 服务 mmr_history 唯一键
// (player_id, match_id)同一维度。多 region 部署下,overflow 对局的 player.update 可能经
// 跨 region 桥 at-least-once 重投,所有路径一律用此键去重,杜绝因桥实现不同产生口径漂移。
// 纯函数,确定性,player_id 为 0 时返回的键含 0(调用方应先过滤无效 player_id)。
func SettlementKey(matchID, playerID uint64) string {
	return fmt.Sprintf("%d:%d", matchID, playerID)
}

// SettlementOwner 是一名玩家结算回流的落点 (region, cell)。
// 与 cellroute.Location 解耦(只取回流决策需要的两维 + player_id),让回流判定是纯函数、易测。
type SettlementOwner struct {
	PlayerID uint64
	RegionID uint32
	CellID   uint32
}

// DistinctSettlementRegions 返回一组结算落点里去重后的 region 列表(升序,确定性)。
// 空输入返回 nil。用于判定一场对局是否跨 region 结算(len>1 即 overflow 对局需多 region 回流)。
func DistinctSettlementRegions(owners []SettlementOwner) []uint32 {
	if len(owners) == 0 {
		return nil
	}
	seen := make(map[uint32]struct{}, len(owners))
	regions := make([]uint32, 0, len(owners))
	for _, o := range owners {
		if _, ok := seen[o.RegionID]; ok {
			continue
		}
		seen[o.RegionID] = struct{}{}
		regions = append(regions, o.RegionID)
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i] < regions[j] })
	return regions
}

// CrossRegionSettlement 判断一场对局的结算是否跨 region(回流到 >1 个 region)。
// 单 region(或空)→ false:同 region 内回流,无需跨 region 桥。
func CrossRegionSettlement(owners []SettlementOwner) bool {
	return len(DistinctSettlementRegions(owners)) > 1
}

// settlementOwners 解析一场对局每名玩家的结算回流落点 (region, cell)。
// router 为 nil(单 Cell / dev)或全部玩家路由失败 → 返回 (nil, false),调用方退化为不做观测。
// player_id 为 0 或单个玩家路由失败的跳过,不阻断(结算落库是权威路径,回流观测是弱信号)。
func (u *BattleResultUsecase) settlementOwners(result *battlev1.BattleResult) ([]SettlementOwner, bool) {
	if u.router == nil || result == nil {
		return nil, false
	}
	owners := make([]SettlementOwner, 0, len(result.GetStats()))
	for _, s := range result.GetStats() {
		pid := s.GetPlayerId()
		if pid == 0 {
			continue
		}
		loc, err := u.router.Route(pid)
		if err != nil {
			continue
		}
		owners = append(owners, SettlementOwner{PlayerID: pid, RegionID: loc.RegionID, CellID: loc.CellID})
	}
	if len(owners) == 0 {
		return nil, false
	}
	return owners, true
}

// logSettlementRouting 在 router 注入后,把一场对局结算的跨 region 回流落点打成观测日志。
//
// 仅可观测,不改回流路径:跨 region player.update 桥 / 多 region topic 路由 / 回流去重表属
// 基础设施(AGENTS.md §11.1,由 Codex/人接);本处只暴露"本局结算需回流几个 region、是否跨 region"
// 信号,供多 Region 上线前评估 overflow 占比。router 为 nil(单 Cell)时不调用此路径,行为不变。
// 跨 region 局额外带一个 sample_settle_key(SettlementKey 口径样例),作为回流去重键口径的排障锚点;
// 不逐玩家打键,避免高基数日志。
func (u *BattleResultUsecase) logSettlementRouting(ctx context.Context, result *battlev1.BattleResult) {
	owners, ok := u.settlementOwners(result)
	if !ok {
		return
	}
	regions := DistinctSettlementRegions(owners)
	crossRegion := len(regions) > 1
	if crossRegion {
		plog.With(ctx).Infow("msg", "battle_settlement_routing",
			"match_id", result.GetMatchId(),
			"region_count", len(regions),
			"cross_region", true,
			"sample_settle_key", SettlementKey(result.GetMatchId(), owners[0].PlayerID))
		return
	}
	plog.With(ctx).Debugw("msg", "battle_settlement_routing",
		"match_id", result.GetMatchId(),
		"region_count", len(regions),
		"cross_region", false)
}
