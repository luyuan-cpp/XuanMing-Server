// hub_route_gate_test.go — Hub 签票入口 active-BATTLE 显式三态权威门单测(P0 止血 2026-07-14,
// Codex 复审修复 2026-07-15:三态 ACTIVE/TERMINAL/UNKNOWN,通用 ErrPermissionDeny 不再充当终态证明)。
//
// 封 battle-reconnect.md §7.3 A / decision-revisit-ds-callback-auth.md §7.16.3 的旁路:
// IssueDSTicket(hub)(→ ResolveHubEndpoint)与 SelectRole 在玩家仍属 live 对局时必须
// 零副作用拒绝(不 AssignHub、不写 role、不签票)；只有权威记录显式终态才放行；
// 其余一切(roster 漂移/记录缺失/错误)UNKNOWN fail-closed。
package biz

import (
	"context"
	"errors"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/account/login/internal/data"
)

// newHubGateUsecase 构造带可控 locator + 可控 roster 权威的用例。
// authz 为 nil 时把 battleTicketIssuer 置空(模拟未接 roster 权威)。
func newHubGateUsecase(t *testing.T, hub data.HubAssigner, notifier data.LocationNotifier, authz *loginBattleAuthorizerFake, require bool) *LoginUsecase {
	t.Helper()
	uc := newTestUsecaseWithNotifier(t, hub, notifier)
	uc.SetRequireHubAssignmentBinding(require)
	if authz == nil {
		uc.battleTicketIssuer = nil
		return uc
	}
	ticketUC := NewTicketUsecase(uc.signer, uc.verifier, nil)
	ticketUC.SetBattleTicketAuthorizer(authz)
	uc.SetBattleTicketIssuer(ticketUC)
	return uc
}

// wantCode 断言 err 携带指定 errcode。
func wantCode(t *testing.T, err error, code errcode.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("want errcode %d, got nil", code)
	}
	if got := errcode.As(err); got != code {
		t.Fatalf("errcode = %d, want %d (err=%v)", got, code, err)
	}
}

func TestHubRouteGate_ActiveBattle_Rejected(t *testing.T) {
	// locator BATTLE + roster 权威判 live → BATTLE_ACTIVE,ErrInvalidState 拒绝。
	notifier := &fakeNotifier{bl: data.BattleLocation{InBattle: true, MatchID: 9001}}
	uc := newHubGateUsecase(t, nil, notifier, &loginBattleAuthorizerFake{}, true)

	err := uc.guardHubRouteAgainstActiveBattle(context.Background(), 42)
	wantCode(t, err, errcode.ErrInvalidState)
}

func TestHubRouteGate_RosterDriftPermissionDeny_Rejected(t *testing.T) {
	// Codex 复审 P0(2026-07-15):locator BATTLE + roster 漂移/非成员 ErrPermissionDeny
	// 只证明“不该给 Battle 票”,不证明“对局已终局”→ UNKNOWN fail-closed,必须拒绝 Hub。
	notifier := &fakeNotifier{bl: data.BattleLocation{InBattle: true, MatchID: 9001}}
	authz := &loginBattleAuthorizerFake{err: errcode.New(errcode.ErrPermissionDeny, "player not in roster (drifted)")}
	uc := newHubGateUsecase(t, nil, notifier, authz, true)

	err := uc.guardHubRouteAgainstActiveBattle(context.Background(), 42)
	wantCode(t, err, errcode.ErrUnavailable)
}

func TestHubRouteGate_ExplicitTerminal_Allowed(t *testing.T) {
	// 唯一放行路径:权威记录显式终态(ended/abandoned)→ locator BATTLE 仅为 TTL 残留。
	notifier := &fakeNotifier{bl: data.BattleLocation{InBattle: true, MatchID: 9001}}
	authz := &loginBattleAuthorizerFake{routeState: data.BattleRouteTerminal}
	uc := newHubGateUsecase(t, nil, notifier, authz, true)

	if err := uc.guardHubRouteAgainstActiveBattle(context.Background(), 42); err != nil {
		t.Fatalf("explicit terminal record should allow hub, got %v", err)
	}
}

func TestHubRouteGate_BattleUnknown_FailClosed_BothProfiles(t *testing.T) {
	// locator 明确 InBattle 但 roster 权威不可读 → 阳性信号下不分 profile 一律 fail-closed。
	for _, require := range []bool{true, false} {
		notifier := &fakeNotifier{bl: data.BattleLocation{InBattle: true, MatchID: 9001}}
		authz := &loginBattleAuthorizerFake{err: errcode.New(errcode.ErrUnavailable, "redis down")}
		uc := newHubGateUsecase(t, nil, notifier, authz, require)

		err := uc.guardHubRouteAgainstActiveBattle(context.Background(), 42)
		wantCode(t, err, errcode.ErrUnavailable)
	}
}

func TestHubRouteGate_BattleButNoAuthority_FailClosed(t *testing.T) {
	// locator InBattle 且未接 roster 权威 → 不可判定,fail-closed。
	notifier := &fakeNotifier{bl: data.BattleLocation{InBattle: true, MatchID: 9001}}
	uc := newHubGateUsecase(t, nil, notifier, nil, false)

	err := uc.guardHubRouteAgainstActiveBattle(context.Background(), 42)
	wantCode(t, err, errcode.ErrUnavailable)
}

func TestHubRouteGate_NotInBattle_Allowed(t *testing.T) {
	notifier := &fakeNotifier{bl: data.BattleLocation{InBattle: false}}
	uc := newHubGateUsecase(t, nil, notifier, &loginBattleAuthorizerFake{}, true)

	if err := uc.guardHubRouteAgainstActiveBattle(context.Background(), 42); err != nil {
		t.Fatalf("not-in-battle should allow hub, got %v", err)
	}
}

func TestHubRouteGate_LocatorError_RequireBinding_FailClosed(t *testing.T) {
	// UNKNOWN(locator 查询失败)+ B1 → ErrUnavailable。
	notifier := &fakeNotifier{blErr: errors.New("locator dial timeout")}
	uc := newHubGateUsecase(t, nil, notifier, &loginBattleAuthorizerFake{}, true)

	err := uc.guardHubRouteAgainstActiveBattle(context.Background(), 42)
	wantCode(t, err, errcode.ErrUnavailable)
}

func TestHubRouteGate_LocatorError_WeakProfile_Allowed(t *testing.T) {
	// local/off 仅对“locator 查询失败”保留历史弱降级(不阻断 dev)。
	notifier := &fakeNotifier{blErr: errors.New("locator dial timeout")}
	uc := newHubGateUsecase(t, nil, notifier, &loginBattleAuthorizerFake{}, false)

	if err := uc.guardHubRouteAgainstActiveBattle(context.Background(), 42); err != nil {
		t.Fatalf("weak profile locator error should allow, got %v", err)
	}
}

func TestHubRouteGate_NoNotifier_RequireBinding_FailClosed(t *testing.T) {
	uc := newHubGateUsecase(t, nil, nil, &loginBattleAuthorizerFake{}, true)

	err := uc.guardHubRouteAgainstActiveBattle(context.Background(), 42)
	wantCode(t, err, errcode.ErrUnavailable)
}

func TestHubRouteGate_NoNotifier_WeakProfile_Allowed(t *testing.T) {
	uc := newHubGateUsecase(t, nil, nil, &loginBattleAuthorizerFake{}, false)

	if err := uc.guardHubRouteAgainstActiveBattle(context.Background(), 42); err != nil {
		t.Fatalf("weak profile without notifier should allow, got %v", err)
	}
}

func TestResolveHubEndpoint_ActiveBattle_ZeroSideEffects(t *testing.T) {
	// §7.16.3 验收:active placement 下 Hub assigner、票据全零。
	hub := &fakeHubAssigner{res: &data.HubAssignment{HubDSAddr: "10.0.0.9:7777"}}
	notifier := &fakeNotifier{bl: data.BattleLocation{InBattle: true, MatchID: 9001}}
	uc := newHubGateUsecase(t, hub, notifier, &loginBattleAuthorizerFake{}, true)

	addr, ticket, _, err := uc.ResolveHubEndpoint(context.Background(), 42)
	wantCode(t, err, errcode.ErrInvalidState)
	if addr != "" || ticket != "" {
		t.Errorf("rejected hub route must not leak endpoint/ticket, got addr=%q ticket=%q", addr, ticket)
	}
	if hub.gotPlayerID != 0 {
		t.Errorf("AssignHub must not be called on rejection (gotPlayerID=%d)", hub.gotPlayerID)
	}
}

func TestResolveHubEndpoint_UnknownPlacement_ZeroSideEffects(t *testing.T) {
	// §7.16.3 验收:unknown placement(B1)下同样零副作用。
	hub := &fakeHubAssigner{res: &data.HubAssignment{HubDSAddr: "10.0.0.9:7777"}}
	notifier := &fakeNotifier{blErr: errors.New("locator down")}
	uc := newHubGateUsecase(t, hub, notifier, &loginBattleAuthorizerFake{}, true)

	addr, ticket, _, err := uc.ResolveHubEndpoint(context.Background(), 42)
	wantCode(t, err, errcode.ErrUnavailable)
	if addr != "" || ticket != "" {
		t.Errorf("rejected hub route must not leak endpoint/ticket, got addr=%q ticket=%q", addr, ticket)
	}
	if hub.gotPlayerID != 0 {
		t.Errorf("AssignHub must not be called on rejection (gotPlayerID=%d)", hub.gotPlayerID)
	}
}

// fakeRoleRepo 是 data.PlayerRoleRepo 写入间谍:断言被拒路径零 role 落库。
type fakeRoleRepo struct {
	roleID   uint32
	setCalls int
}

func (f *fakeRoleRepo) GetRole(context.Context, uint64) (uint32, error) { return f.roleID, nil }
func (f *fakeRoleRepo) SetRole(_ context.Context, _ uint64, roleID uint32) error {
	f.setCalls++
	f.roleID = roleID
	return nil
}

func TestResolveHubEndpoint_RosterDrift_ZeroSideEffects(t *testing.T) {
	// locator BATTLE + roster 漂移 PermissionDeny → UNKNOWN 拒绝,零 AssignHub、零票。
	hub := &fakeHubAssigner{res: &data.HubAssignment{HubDSAddr: "10.0.0.9:7777"}}
	notifier := &fakeNotifier{bl: data.BattleLocation{InBattle: true, MatchID: 9001}}
	authz := &loginBattleAuthorizerFake{err: errcode.New(errcode.ErrPermissionDeny, "player not in roster (drifted)")}
	uc := newHubGateUsecase(t, hub, notifier, authz, true)

	addr, ticket, _, err := uc.ResolveHubEndpoint(context.Background(), 42)
	wantCode(t, err, errcode.ErrUnavailable)
	if addr != "" || ticket != "" {
		t.Errorf("rejected hub route must not leak endpoint/ticket, got addr=%q ticket=%q", addr, ticket)
	}
	if hub.gotPlayerID != 0 {
		t.Errorf("AssignHub must not be called on rejection (gotPlayerID=%d)", hub.gotPlayerID)
	}
}

func TestResolveHubEndpoint_ExplicitTerminal_Allowed(t *testing.T) {
	// 显式终态 → 放行,AssignHub 恰好一次,交付 allocator 签的票(B1 需 v2 票+完整归属绑定)。
	keys := newHubV2TestKeys(t)
	tk, _ := signHubV2ForResolve(t, keys, "", nil)
	hub := &fakeHubAssigner{res: &data.HubAssignment{
		HubDSAddr: "10.0.0.9:7777", HubTicket: tk, HubPodName: "hub-stable-1", ShardID: 7,
	}}
	notifier := &fakeNotifier{bl: data.BattleLocation{InBattle: true, MatchID: 9001}}
	authz := &loginBattleAuthorizerFake{routeState: data.BattleRouteTerminal}
	uc := newHubGateUsecase(t, hub, notifier, authz, true)
	uc.v2Verifier = keys.verifier

	addr, ticket, _, err := uc.ResolveHubEndpoint(context.Background(), 42)
	if err != nil {
		t.Fatalf("explicit terminal should allow hub, got %v", err)
	}
	if addr != "10.0.0.9:7777" || ticket != tk {
		t.Errorf("addr=%q ticket ok=%v, want allocator endpoint+ticket", addr, ticket == tk)
	}
	if hub.gotPlayerID != 42 {
		t.Errorf("AssignHub playerID=%d, want 42 (exactly once)", hub.gotPlayerID)
	}
}

func TestResolveHubEndpoint_TOCTOU_ConcurrentTerminalFlip(t *testing.T) {
	// 并发终局切换(TOCTOU):第一次询问时权威仍判 ACTIVE → 拒绝且零副作用;
	// 对局随即终局,第二次重试权威判 TERMINAL → 放行且 AssignHub 恰好一次。
	// 门必须每次实时问权威,不缓存、不猜测。
	keys := newHubV2TestKeys(t)
	tk, _ := signHubV2ForResolve(t, keys, "", nil)
	hub := &fakeHubAssigner{res: &data.HubAssignment{
		HubDSAddr: "10.0.0.9:7777", HubTicket: tk, HubPodName: "hub-stable-1", ShardID: 7,
	}}
	notifier := &fakeNotifier{bl: data.BattleLocation{InBattle: true, MatchID: 9001}}
	authz := &loginBattleAuthorizerFake{routeStates: []data.BattleRouteState{
		data.BattleRouteActive, data.BattleRouteTerminal,
	}}
	uc := newHubGateUsecase(t, hub, notifier, authz, true)
	uc.v2Verifier = keys.verifier

	addr, ticket, _, err := uc.ResolveHubEndpoint(context.Background(), 42)
	wantCode(t, err, errcode.ErrInvalidState)
	if addr != "" || ticket != "" || hub.gotPlayerID != 0 {
		t.Fatalf("first (ACTIVE) attempt must be zero side effects: addr=%q ticket=%q assignPID=%d",
			addr, ticket, hub.gotPlayerID)
	}

	addr, ticket, _, err = uc.ResolveHubEndpoint(context.Background(), 42)
	if err != nil {
		t.Fatalf("second (TERMINAL) attempt should allow, got %v", err)
	}
	if addr != "10.0.0.9:7777" || ticket != tk || hub.gotPlayerID != 42 {
		t.Errorf("second attempt addr=%q ticketOK=%v assignPID=%d, want allocator result", addr, ticket == tk, hub.gotPlayerID)
	}
	if authz.routeCalls != 2 {
		t.Errorf("route authority consulted %d times, want 2 (per attempt, no caching)", authz.routeCalls)
	}
}

func TestSelectRole_ActiveBattle_Rejected_ZeroRoleWrite(t *testing.T) {
	// SelectRole 同为 Hub 签票入口:active battle 下拒绝且零 AssignHub、零 role 落库。
	hub := &fakeHubAssigner{res: &data.HubAssignment{HubDSAddr: "10.0.0.9:7777"}}
	notifier := &fakeNotifier{bl: data.BattleLocation{InBattle: true, MatchID: 9001}}
	uc := newHubGateUsecase(t, hub, notifier, &loginBattleAuthorizerFake{}, true)
	roles := &fakeRoleRepo{}
	uc.roleRepo = roles
	uc.devAllowAnyRole = true // 隔离白名单逻辑,确保拒绝只来自三态门

	addr, ticket, _, err := uc.SelectRole(context.Background(), 42, 1)
	wantCode(t, err, errcode.ErrInvalidState)
	if addr != "" || ticket != "" {
		t.Errorf("rejected SelectRole must not leak endpoint/ticket, got addr=%q ticket=%q", addr, ticket)
	}
	if hub.gotPlayerID != 0 {
		t.Errorf("AssignHub must not be called on rejection (gotPlayerID=%d)", hub.gotPlayerID)
	}
	if roles.setCalls != 0 {
		t.Errorf("SetRole called %d times on rejection, want 0", roles.setCalls)
	}
}

func TestSelectRole_RosterDrift_Rejected_ZeroRoleWrite(t *testing.T) {
	// locator BATTLE + roster 漂移 PermissionDeny → UNKNOWN 拒绝,零写 role、零签票。
	hub := &fakeHubAssigner{res: &data.HubAssignment{HubDSAddr: "10.0.0.9:7777"}}
	notifier := &fakeNotifier{bl: data.BattleLocation{InBattle: true, MatchID: 9001}}
	authz := &loginBattleAuthorizerFake{err: errcode.New(errcode.ErrPermissionDeny, "player not in roster (drifted)")}
	uc := newHubGateUsecase(t, hub, notifier, authz, true)
	roles := &fakeRoleRepo{}
	uc.roleRepo = roles
	uc.devAllowAnyRole = true

	addr, ticket, _, err := uc.SelectRole(context.Background(), 42, 1)
	wantCode(t, err, errcode.ErrUnavailable)
	if addr != "" || ticket != "" {
		t.Errorf("rejected SelectRole must not leak endpoint/ticket, got addr=%q ticket=%q", addr, ticket)
	}
	if hub.gotPlayerID != 0 {
		t.Errorf("AssignHub must not be called on rejection (gotPlayerID=%d)", hub.gotPlayerID)
	}
	if roles.setCalls != 0 {
		t.Errorf("SetRole called %d times on rejection, want 0", roles.setCalls)
	}
}

func TestSelectRole_ExplicitTerminal_Allowed(t *testing.T) {
	// 显式终态 → SelectRole 正常落库 + 签票(终局残留不阻断选角回大厅)。
	keys := newHubV2TestKeys(t)
	tk, _ := signHubV2ForResolve(t, keys, "", nil)
	hub := &fakeHubAssigner{res: &data.HubAssignment{
		HubDSAddr: "10.0.0.9:7777", HubTicket: tk, HubPodName: "hub-stable-1", ShardID: 7,
	}}
	notifier := &fakeNotifier{bl: data.BattleLocation{InBattle: true, MatchID: 9001}}
	authz := &loginBattleAuthorizerFake{routeState: data.BattleRouteTerminal}
	uc := newHubGateUsecase(t, hub, notifier, authz, true)
	uc.v2Verifier = keys.verifier
	roles := &fakeRoleRepo{}
	uc.roleRepo = roles
	uc.devAllowAnyRole = true

	addr, ticket, _, err := uc.SelectRole(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("explicit terminal SelectRole should succeed, got %v", err)
	}
	if addr != "10.0.0.9:7777" || ticket != tk {
		t.Errorf("addr=%q ticketOK=%v, want allocator result", addr, ticket == tk)
	}
	if roles.setCalls != 1 || roles.roleID != 7 {
		t.Errorf("SetRole calls=%d roleID=%d, want exactly one write of role 7", roles.setCalls, roles.roleID)
	}
	if hub.gotRoleID != 7 {
		t.Errorf("AssignHub roleID=%d, want 7", hub.gotRoleID)
	}
}
