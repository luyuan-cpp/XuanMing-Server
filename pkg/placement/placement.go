// Package placement now only carries the transport-independent DS instance
// identity primitives (Target, ValidOperationID) shared by the battle
// allocation saga and the battle abort contract, plus the cross-repo DS
// fence-lease protocol constants. The versioned placement routing/lease/proof
// system that used to live here was removed (2026-07): player routing is
// derived from player_locator TTL leases + match state.
package placement

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// ── DS 授权租约 fencing 协议常量(脑裂根治,2026-07-16)──────────────────────────
//
// 跨仓库契约(UE 侧对应 UPandoraDSBackendSubsystem 的授权租约 fencing;
// docs/design/battle-reconnect.md §8):
//
//  1. DS(Hub/Battle)以最近一次「绑定 active 凭据的权威心跳响应」为租约起点
//     (单调时钟)。连续 DSFenceLeaseMaxSeconds 未能续租 → DS 必须对**存量玩家**
//     自我 fencing:关闭输入、Kick 所有已准入连接、销毁 Pawn(不只是拒新玩家)。
//     UE 侧代码把租约硬钳制在 [5, DSFenceLeaseMaxSeconds],配置无法放大。
//  2. 服务端任何「把静默 DS 上的玩家交给新 DS」的再入门,必须等待该 DS 的
//     last_heartbeat_ms 至少经过 DSFenceReentryBarrier(= 租约上限 + 时钟/网络
//     偏差余量)。由此保证核心时序:旧 DS 最晚停止可玩时间 < 新 DS 最早开始可玩时间。
//  3. 相关窗口启动校验:player_locator TTL 与 hub_allocator heartbeat_timeout
//     都必须 ≥ DSFenceReentryBarrier(当前默认 30s ≥ 27s,启动时机械下限保护)。
//
// 这些是正确性常量而非调优参数:调大只增加故障恢复延迟,调小会重新打开
// 「一名玩家同时存在于两台 DS」的脑裂窗口(CLAUDE.md §9.1/§9.22)。
const (
	// DSFenceLeaseMaxSeconds 是 DS 侧授权租约的协议上限(秒)。
	DSFenceLeaseMaxSeconds = 20

	// DSFenceSkewMarginSeconds 是安全余量(秒),预算构成必须完整覆盖三项:
	// 心跳响应在途上限(UE HeartbeatRequestTimeoutSeconds=4s) + fencing 检测粒度
	// (1s ticker) + 服务间时钟漂移专属预留(≥2s;ds_allocator 写 last_heartbeat_ms
	// 与 login 读 now() 是两台机器的时钟)。2026-07-18 从 5 提到 7:原值被前两项
	// 恰好占满,时钟漂移零预留,UE Automation 断言等号成立即无余量。
	DSFenceSkewMarginSeconds = 7

	// DSFenceReentryBarrier 是服务端再入屏障:自 DS 最后一次心跳起,必须经过
	// 该时长才允许把这台 DS 上的玩家路由到任何新 DS。
	DSFenceReentryBarrier = (DSFenceLeaseMaxSeconds + DSFenceSkewMarginSeconds) * time.Second
)

// ValidOperationID accepts only canonical lowercase RFC4122 UUIDv4 strings.
func ValidOperationID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.Version() == uuid.Version(4) &&
		id.Variant() == uuid.RFC4122 && id.String() == value
}

// Target is an exact DS instance identity tuple (Hub assignment or Battle
// allocation).  Same-name pod replacements never compare Equal because the
// InstanceUID/InstanceEpoch differ.
type Target struct {
	PodName       string // DS Pod 名称
	InstanceUID   string // 实例 UID，区分同名 Pod 的不同实例
	InstanceEpoch uint32 // 实例纪元，随实例重启递增
	AssignmentID  string // Hub 分配 ID
	AllocationID  string // Battle 分配 ID
	ReleaseTrack  string // 发布轨道(灰度/正式)
}

func (t Target) CompleteHub() bool {
	return strings.TrimSpace(t.PodName) != "" && strings.TrimSpace(t.InstanceUID) != "" &&
		t.InstanceEpoch > 0 && strings.TrimSpace(t.AssignmentID) != "" &&
		strings.TrimSpace(t.ReleaseTrack) != ""
}

func (t Target) CompleteBattle() bool {
	return strings.TrimSpace(t.PodName) != "" && strings.TrimSpace(t.InstanceUID) != "" &&
		t.InstanceEpoch > 0 && strings.TrimSpace(t.AllocationID) != "" &&
		strings.TrimSpace(t.ReleaseTrack) != ""
}

func (t Target) Equal(other Target) bool {
	return t.PodName == other.PodName && t.InstanceUID == other.InstanceUID &&
		t.InstanceEpoch == other.InstanceEpoch && t.AssignmentID == other.AssignmentID &&
		t.AllocationID == other.AllocationID && t.ReleaseTrack == other.ReleaseTrack
}
