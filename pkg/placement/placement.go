// Package placement contains the shared, transport-independent contract for
// versioned player placement leases. The durable record itself lives in
// player_locator; callers only pass an exact version/operation binding.
package placement

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Mode controls the zero-downtime rollout of placement enforcement.
// Mode 控制落位强制校验的不停服灰度上线开关。
type Mode uint8

const (
	ModeOff     Mode = iota // 关闭：不做落位校验
	ModeShadow              // 影子模式：只观察记录、不拦截，用于灰度验证
	ModeEnforce             // 强制模式：校验不过直接拒绝
)

// Wire enum values are kept here so proof producers do not need to import the
// locator transport package merely to build a signed canonical statement.
// 这里保留线上枚举值，让证明(proof)生产方无需 import 定位器传输包即可构造签名的规范化声明。
const (
	RouteUnknown          int32 = 0 // 路由未知
	RouteHub              int32 = 1 // 路由到 Hub DS
	RouteBattle           int32 = 2 // 路由到 Battle DS
	ProofAccountBootstrap int32 = 3 // 证明类型：账号首次登录建档
	ProofMatchTerminal    int32 = 1 // 证明类型：对局结束
	ProofPlayerLeave      int32 = 2 // 证明类型：玩家离开
	ProofMatchStart       int32 = 4 // 证明类型：对局开始
	ProofHubTransfer      int32 = 5 // 证明类型：Hub 间转移
)

func (m Mode) String() string {
	switch m {
	case ModeOff:
		return "off"
	case ModeShadow:
		return "shadow"
	case ModeEnforce:
		return "enforce"
	default:
		return "invalid"
	}
}

// ParseMode rejects misspellings instead of silently weakening enforcement.
func ParseMode(raw string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "off":
		return ModeOff, nil
	case "shadow":
		return ModeShadow, nil
	case "enforce":
		return ModeEnforce, nil
	default:
		return ModeOff, fmt.Errorf("placement mode %q invalid (want off|shadow|enforce)", raw)
	}
}

// Binding is copied into a Hub assignment and its signed DS ticket.
// Complete is deliberately all-or-nothing; partial bindings are never usable.
// Binding 会被拷贝进 Hub 分配结果及其签名的 DS 票据。
// 刻意设计成全有或全无(all-or-nothing)——半个绑定永远不可用。
type Binding struct {
	Version       uint64 // 落位记录版本号，单调递增
	OperationID   string // 操作 ID(规范小写 UUIDv4)，标识本次落位操作
	SourceMatchID uint64 // 来源对局 ID(从哪个 match 转移过来)
}

func (b Binding) Empty() bool {
	return b.Version == 0 && b.OperationID == "" && b.SourceMatchID == 0
}

func (b Binding) Complete() bool {
	return b.Version > 0 && ValidOperationID(b.OperationID)
}

func (b Binding) ValidateOptional() error {
	if b.Empty() || b.Complete() {
		return nil
	}
	return fmt.Errorf("placement binding must be all present or all absent")
}

func (b Binding) Equal(other Binding) bool {
	return b.Version == other.Version && b.OperationID == other.OperationID &&
		b.SourceMatchID == other.SourceMatchID
}

// ValidOperationID accepts only canonical lowercase RFC4122 UUIDv4 strings.
func ValidOperationID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.Version() == uuid.Version(4) &&
		id.Variant() == uuid.RFC4122 && id.String() == value
}

// Target is the exact Hub DS identity committed at final Admission.
// Target 是最终 Admission 时确定的 Hub DS 精确身份。
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
