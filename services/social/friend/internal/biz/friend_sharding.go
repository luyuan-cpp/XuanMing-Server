// friend_sharding.go 是好友图分片落地的服务内纯逻辑(分片落点 + 幂等键口径 + nil-safe 接线)。
//
// 背景(docs/design/friend-distributed-scaling.md §5/§6):全区全服千万级好友、边十亿级时,
// 好友图必须按 owner(player_id)分库分表;一旦分片,跨人强一致事务(当前 AcceptRequest 的
// 单事务双向建边)不成立,要拆成「request 单点 CAS + Kafka 异步幂等建边 + 软上限」(§5.1):
//
//	步骤 1:request 按 request_id 分片单行 CAS(pending→accepted,影响行数=1 判赢,强一致)
//	步骤 2:CAS 成功 emit FriendshipEstablished 事件(同分片 outbox,幂等键=request_id)
//	步骤 3:两条边各自 owner 分片幂等写(requester 分片写 requester→target,target 分片写反向)
//
// 本文件只落服务内纯逻辑,不改现状单 MySQL 事务实现(§1 现状正确够用):
//   - 统一好友图幂等键口径(AcceptIdempotencyKey / EdgeBuildKey),与 §5.1/§5.3 的
//     「幂等键=request_id」对齐,避免分片落地时各消费者口径漂移。
//   - 用确定性 cellroute.Router 把一条好友请求的两名玩家解析到各自 owner (region, cell),
//     判定本条好友边是否跨分片 / 跨 region(CrossShardFriendship / CrossRegionFriendship),
//     作为可观测信号(分片上线前评估跨 region 好友占比)。
//
// 边界(AGENTS.md §11.1):真正的分片 MySQL / Kafka 双向建边消费者 / 软上限对账属基础设施,
// 由 Codex/人接;本轮 router 为 nil(单 Cell)时行为不变,只在注入后打观测日志。
package biz

import (
	"context"
	"fmt"
	"sort"

	plog "github.com/luyuancpp/pandora/pkg/log"
)

// AcceptIdempotencyKey 是一条好友请求 accept 流程的幂等键口径(saga key)。
//
// 口径统一:canonical "friend_accept:request_id"。friend-distributed-scaling.md §5.1/§5.3 定
// 「幂等键=request_id」——request 单点 CAS、outbox 事件、两条边幂等写全锚定同一 request_id。
// 纯函数,确定性;request_id 为 0 时返回的键含 0(调用方应先校验 request_id 非 0)。
func AcceptIdempotencyKey(requestID uint64) string {
	return fmt.Sprintf("friend_accept:%d", requestID)
}

// EdgeBuildKey 是「某 owner 分片建某方向好友边」的幂等键口径。
//
// 步骤 3 双向建边拆两条 Kafka 消费:requester 分片写 requester→target、target 分片写反向。
// 两条消费各自 owner 分片幂等写(INSERT IGNORE / SETNX),需各带不同幂等键避免互相覆盖,
// 故在 saga key 上再缀 owner_id:canonical "friend_accept:request_id:owner_id"。确定性纯函数。
func EdgeBuildKey(requestID, ownerID uint64) string {
	return fmt.Sprintf("friend_accept:%d:%d", requestID, ownerID)
}

// EdgeOwner 是一条好友边 owner 的分片落点 (region, cell)。
// 与 cellroute.Location 解耦(只取分片决策需要的两维 + player_id),让分片判定是纯函数、易测。
type EdgeOwner struct {
	PlayerID uint64
	RegionID uint32
	CellID   uint32
}

// DistinctEdgeRegions 返回一组边 owner 落点里去重后的 region 列表(升序,确定性)。空输入返回 nil。
func DistinctEdgeRegions(owners []EdgeOwner) []uint32 {
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

// DistinctEdgeCells 返回一组边 owner 落点里去重后的 (region, cell) 数(确定性)。
// 用于判定本条好友边是否跨分片:两名玩家落不同 Cell → 双向建边落两个分片(§5.1 步骤 3)。
func DistinctEdgeCells(owners []EdgeOwner) int {
	if len(owners) == 0 {
		return 0
	}
	type rc struct{ r, c uint32 }
	seen := make(map[rc]struct{}, len(owners))
	for _, o := range owners {
		seen[rc{o.RegionID, o.CellID}] = struct{}{}
	}
	return len(seen)
}

// CrossShardFriendship 判断一条好友边的两名玩家是否落不同 Cell(双向建边跨分片)。
// 单 Cell(或空)→ false:同分片内本地建边,当前单事务实现即可。
func CrossShardFriendship(owners []EdgeOwner) bool {
	return DistinctEdgeCells(owners) > 1
}

// CrossRegionFriendship 判断一条好友边是否跨 region(两名玩家 owner region 不同)。
// 跨 region 好友按 §4.4「最小跨 region 通道」处理,占比应极低;单 region(或空)→ false。
func CrossRegionFriendship(owners []EdgeOwner) bool {
	return len(DistinctEdgeRegions(owners)) > 1
}

// edgeOwners 解析一条好友请求两名玩家的 owner 分片落点 (region, cell)。
// router 为 nil(单 Cell / dev)或任一玩家路由失败 → 返回 (nil, false),调用方退化为不做观测。
// player_id 为 0 跳过(调用方应已校验);两名玩家都解析成功才返回有效落点。
func (u *FriendUsecase) edgeOwners(requesterID, targetID uint64) ([]EdgeOwner, bool) {
	if u.router == nil || requesterID == 0 || targetID == 0 {
		return nil, false
	}
	owners := make([]EdgeOwner, 0, 2)
	for _, pid := range []uint64{requesterID, targetID} {
		loc, err := u.router.Route(pid)
		if err != nil {
			return nil, false
		}
		owners = append(owners, EdgeOwner{PlayerID: pid, RegionID: loc.RegionID, CellID: loc.CellID})
	}
	return owners, true
}

// logFriendshipSharding 在 router 注入后,把一条 accept 成功的好友边分片落点打成观测日志。
//
// 仅可观测,不改建边路径:分片 MySQL / Kafka 双向建边消费者 / 软上限对账属基础设施
// (AGENTS.md §11.1,由 Codex/人接);本处只暴露「本条好友边跨几个分片 / 是否跨 region」信号,
// 供分片上线前评估跨 region 好友占比。router 为 nil(单 Cell)时不调用此路径,行为不变。
// 跨 region 边额外带一个 sample_edge_key(EdgeBuildKey 口径样例),作为分片建边幂等键的排障锚点。
func (u *FriendUsecase) logFriendshipSharding(ctx context.Context, requestID, requesterID, targetID uint64) {
	owners, ok := u.edgeOwners(requesterID, targetID)
	if !ok {
		return
	}
	regions := DistinctEdgeRegions(owners)
	crossRegion := len(regions) > 1
	if crossRegion {
		plog.With(ctx).Debugw("msg", "friend_edge_sharding",
			"request_id", requestID,
			"region_count", len(regions),
			"cross_shard", true,
			"cross_region", true,
			"sample_edge_key", EdgeBuildKey(requestID, owners[0].PlayerID))
		return
	}
	plog.With(ctx).Debugw("msg", "friend_edge_sharding",
		"request_id", requestID,
		"region_count", len(regions),
		"cross_shard", CrossShardFriendship(owners),
		"cross_region", false)
}
