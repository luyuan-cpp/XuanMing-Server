package biz

import (
	"github.com/luyuancpp/pandora/pkg/placement"
	locatorv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/locator/v1"
)

// 编译期守卫:pkg/placement 手写的线上枚举值必须与 proto 生成值逐一相等。
//
// pkg/placement 刻意不 import locator proto(避免给证明生产方引入传输依赖),
// 所以这份等价关系无法在该包内表达。本包是唯一同时 import 两者的边界包,由它兜底:
// 任一常量漂移都会让本包(进而整个依赖它的 workspace)编译失败,而不是等到运行时
// HMAC canonical 串错位 / 密钥环 lookup miss,把落位迁移验签全部拒掉、玩家卡死在
// Hub/Battle 之外(击穿「玩家只在一个 DS」核心链路)。
//
// 断言手法:把常量差值当作 [1]struct{} 的下标。合法下标只有 0;差值非 0(无论正负)
// 都会触发 "index out of bounds" 编译错误,双向漂移都能抓。
const _ = "keep pkg/placement enum values in sync with proto/pandora/locator/v1"

// PlacementRoute(pkg/placement 的 Route*)
var _ = [1]struct{}{}[placement.RouteUnknown-int32(locatorv1.PlacementRoute_PLACEMENT_ROUTE_UNSPECIFIED)]
var _ = [1]struct{}{}[placement.RouteHub-int32(locatorv1.PlacementRoute_PLACEMENT_ROUTE_HUB)]
var _ = [1]struct{}{}[placement.RouteBattle-int32(locatorv1.PlacementRoute_PLACEMENT_ROUTE_BATTLE)]

// PlacementProofType(pkg/placement 的 Proof*)
var _ = [1]struct{}{}[placement.ProofMatchTerminal-int32(locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_TERMINAL)]
var _ = [1]struct{}{}[placement.ProofPlayerLeave-int32(locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_PLAYER_LEAVE)]
var _ = [1]struct{}{}[placement.ProofAccountBootstrap-int32(locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_ACCOUNT_BOOTSTRAP)]
var _ = [1]struct{}{}[placement.ProofMatchStart-int32(locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_MATCH_START)]
var _ = [1]struct{}{}[placement.ProofHubTransfer-int32(locatorv1.PlacementProofType_PLACEMENT_PROOF_TYPE_HUB_TRANSFER)]

// PlacementSourceDepartureProofType(pkg/placement 的 ProofHubDeparture / ProofBattleDeparture,见 proof.go)
var _ = [1]struct{}{}[placement.ProofHubDeparture-int32(locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_HUB_DEPARTURE)]
var _ = [1]struct{}{}[placement.ProofBattleDeparture-int32(locatorv1.PlacementSourceDepartureProofType_PLACEMENT_SOURCE_DEPARTURE_PROOF_TYPE_BATTLE_DEPARTURE)]
