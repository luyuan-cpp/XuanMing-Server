// consumer_sharding.go 是 push 消费者按 owner cell 归属定向路由的服务内纯逻辑(nil-safe 接线)。
//
// 背景(scale-cellular-20m.md §3.2/§4.2 + 不变量 §1):多 Cell 下每个玩家的 push 连接(订阅 stream)
// 必在其 owner cell 的 push 实例上(同一 player_id 所有 owner 数据 + 在线连接同 cell);跨 region/cell
// 弱实时事件经全局桥重投时 key=接收方 player_id(§4.4)。某个 cell 的 push 消费者只应交付它所拥有
// (owner cell == 本实例 cell)的玩家消息;非本 cell 玩家的消息说明发生了路由抖动 / topic 分区漂移 /
// rebalance,应由边缘网关 / 服务发现 / 跨 cell 桥(基础设施)转投到对的 cell。
//
// 2026-07-22 审计修订:非 owner cell 的玩家消息从「告警 + 照常交付」改为**毒丸投 DLQ**
// (handle 内判定)——本 cell 的 Redis 投递缓冲对连接所在 cell 不可见,"照常交付"实为
// 写错缓存 + ACK = 静默丢;DLQ 留证可由基础设施 / 人工重投到 owner cell。router 为 nil
// (单 Cell)时本实例拥有全部玩家,行为与历史一致。
package biz

import (
	"github.com/luyuancpp/pandora/pkg/cellroute"
)

// PlayerOwner 是一名玩家 push 连接锚定的 owner 落点(只取归属判定需要的维度)。
type PlayerOwner struct {
	RegionID uint32
	CellID   uint32
}

// SetCellOwnership 注入确定性 region/cell 路由器 + 本 push 实例所在 cell 身份
// (scale-cellular-20m.md §4.2 两级架构,main.go 分片部署时调用)。
//
// nil-safe:不调用 / router 传 nil 时(单 Cell / dev / 阶段 1~2),消费者拥有全部玩家,
// handle 不做归属告警,行为与历史一致。用 setter 而非构造参数,避免单 Cell 阶段
// NewKafkaConsumer 调用点被迫改签名(与 matchmaker / auction / locator 等一致)。
// Router 内部读路径无锁,并发安全。
func (k *KafkaConsumer) SetCellOwnership(router *cellroute.Router, selfRegion, selfCell uint32) {
	k.router = router
	k.selfRegion = selfRegion
	k.selfCell = selfCell
}

// ownsPlayer 判断一名玩家是否归本 push 实例所在 cell 所有。
//
// 返回 (owner, owned, known):
//   - router 为 nil(单 Cell / dev)或 player_id 为 0 或路由失败 → (PlayerOwner{}, true, false):
//     视为本实例拥有(不阻断交付),known=false 表示归属未知 / 不适用,调用方不打漂移告警。
//   - 否则 known=true,owned = (玩家 owner region/cell == 本实例 region/cell)。
func (k *KafkaConsumer) ownsPlayer(playerID uint64) (owner PlayerOwner, owned bool, known bool) {
	if k.router == nil || playerID == 0 {
		return PlayerOwner{}, true, false
	}
	loc, err := k.router.Route(playerID)
	if err != nil {
		return PlayerOwner{}, true, false
	}
	o := PlayerOwner{RegionID: loc.RegionID, CellID: loc.CellID}
	return o, o.RegionID == k.selfRegion && o.CellID == k.selfCell, true
}
