// r6_ticket_session_binding_test.go — R6 复审 P0-3 回归(INC-20260722-004 §4.1.4)。
//
// 覆盖:
//   - SelectRole 角色落库 precommit fencing:预检通过后会话被轮换,角色写必须不落地
//     (ROLLBACK),错误为顶号专属码;
//   - RequireTicketSessionCurrent 兑换点复核:旧 sjti 拒(Superseded)、会话消失拒、
//     现行放行、空 sjti 硬拒(R7 收口)、权威不可达 fail-closed;
//   - SetRole 同事务域持久化代际 fencing(R7 P0-4):Redis 投影落后也确定性拒提交。
package biz

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/snowflake"
)

// fencedRoleRepo 按真实 MySQLPlayerRoleRepo 的契约模拟:代际复核/precommit 失败 = 写不落地。
// persistedJTI 非空时模拟 player_session_generations 行(R7 P0-4 同事务域 fencing);
// 空 = 代际行缺失兼容窗,退化为仅 precommit。
type fencedRoleRepo struct {
	committedRole uint32
	setCalls      int
	persistedJTI  string
}

func (f *fencedRoleRepo) GetRole(_ context.Context, _ uint64) (uint32, error) {
	return f.committedRole, nil
}

func (f *fencedRoleRepo) SetRole(ctx context.Context, _ uint64, roleID uint32, expectedSessJTI string, precommit func(context.Context) error) error {
	f.setCalls++
	if expectedSessJTI != "" && f.persistedJTI != "" && f.persistedJTI != expectedSessJTI {
		return errcode.New(errcode.ErrSessionSuperseded, "session superseded; role write rolled back")
	}
	if precommit != nil {
		if err := precommit(ctx); err != nil {
			return err // ROLLBACK:不改 committedRole
		}
	}
	f.committedRole = roleID
	return nil
}

func newRoleFenceUsecase(t *testing.T, sessions *jtiSessionRepo, roles *fencedRoleRepo) *LoginUsecase {
	t.Helper()
	cfg := auth.Config{Secret: []byte(testSecret)}
	signer, err := auth.NewSigner(cfg)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	verifier, err := auth.NewVerifier(cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	repo := &fakeAccountRepo{playerID: 42, passwordHash: mustBcrypt(t, "pw")}
	sf := snowflake.NewNode(1)
	// hubAssigner nil → resolveHub 走自签回退;devAllowAnyRole=true 免白名单。
	return NewLoginUsecase(repo, sessions, nil, nil, roles, sf, "127.0.0.1:7777", "cn",
		signer, verifier, nil, false, false, nil, true)
}

// 预检(service 层)通过后、SetRole 事务提交前会话被新登录轮换:角色写必须 ROLLBACK
// 不落地(R6:不再"落库后才终检"),错误可判别为顶号。
func TestSelectRole_RolePersistFencedOnMidFlightRotation(t *testing.T) {
	sessions := &jtiSessionRepo{cur: "jti-B", found: true} // 权威已轮换到 B
	roles := &fencedRoleRepo{committedRole: 1}
	uc := newRoleFenceUsecase(t, sessions, roles)

	// 旧会话 A(预检时刻仍现行的快照 jti-A)提交 SelectRole。
	_, _, _, err := uc.SelectRole(context.Background(), 42, 7, "jti-A")
	if errcode.As(err) != errcode.ErrSessionSuperseded {
		t.Fatalf("P0-3: stale-session role write must fail as superseded, got: %v", err)
	}
	if roles.setCalls != 1 || roles.committedRole != 1 {
		t.Fatalf("P0-3: role write must not be committed (calls=%d committed=%d)",
			roles.setCalls, roles.committedRole)
	}
}

// 会话仍现行:角色写照常提交(fencing 不误杀)。
func TestSelectRole_RolePersistPassesForCurrentSession(t *testing.T) {
	sessions := &jtiSessionRepo{cur: "jti-A", found: true}
	roles := &fencedRoleRepo{persistedJTI: "jti-A"}
	uc := newRoleFenceUsecase(t, sessions, roles)

	if _, _, _, err := uc.SelectRole(context.Background(), 42, 7, "jti-A"); err != nil {
		t.Fatalf("current session role write must pass: %v", err)
	}
	if roles.committedRole != 7 {
		t.Fatalf("role must be committed for current session, got %d", roles.committedRole)
	}
}

// R7 复审 P0-4 确定性交错:Redis precommit 时刻仍是旧代(未轮换),但 MySQL 持久化
// 代际已被新登录写入——同事务域 FOR UPDATE 复核必须拒提交,不依赖 Redis 窗口时序。
func TestSelectRole_MySQLGenerationFencesEvenWhenRedisStale(t *testing.T) {
	// Redis 投影落后(仍报 jti-A 现行),但登录 fail-closed 落库的代际已是 jti-B。
	sessions := &jtiSessionRepo{cur: "jti-A", found: true}
	roles := &fencedRoleRepo{committedRole: 1, persistedJTI: "jti-B"}
	uc := newRoleFenceUsecase(t, sessions, roles)
	// MySQL 代际强制门是分阶段激活开关(默认 emit-only,滚动兼容);本测试断言的
	// 正是强制档的确定性 fencing,显式开启。
	uc.SetSessionGenerationEnforce(true)

	_, _, _, err := uc.SelectRole(context.Background(), 42, 7, "jti-A")
	if errcode.As(err) != errcode.ErrSessionSuperseded {
		t.Fatalf("P0-4: persisted-generation mismatch must fence role write, got: %v", err)
	}
	if roles.committedRole != 1 {
		t.Fatalf("P0-4: role write must not be committed, got %d", roles.committedRole)
	}
}

// 兑换点复核语义(VerifyDSTicket 调用面)。
func TestRequireTicketSessionCurrent(t *testing.T) {
	sessions := &jtiSessionRepo{cur: "jti-B", found: true}
	uc := newRoleFenceUsecase(t, sessions, &fencedRoleRepo{})
	ctx := context.Background()

	// 旧 sjti(签发后被轮换):Superseded——已交付旧票在兑换点作废。
	if err := uc.RequireTicketSessionCurrent(ctx, 42, "jti-A"); errcode.As(err) != errcode.ErrSessionSuperseded {
		t.Fatalf("stale sjti must be superseded, got: %v", err)
	}
	// 现行 sjti:放行。
	if err := uc.RequireTicketSessionCurrent(ctx, 42, "jti-B"); err != nil {
		t.Fatalf("current sjti must pass: %v", err)
	}
	// 空 sjti(R8 收口,P0-5 滚动兼容):默认兼容档告警放行——混版窗口内旧签发面仍
	// 持续签空 sjti 票,硬拒会令战斗准入整体不可用;非空 sjti 现行性复核不受门影响。
	if err := uc.RequireTicketSessionCurrent(ctx, 42, ""); err != nil {
		t.Fatalf("empty sjti must be compat-allowed by default (mixed-version window), got: %v", err)
	}
	// 收口档(require_ticket_sjti=true,签发面排空 + 等满票据最大 TTL 后激活):硬拒。
	uc.SetRequireTicketSJTI(true)
	if err := uc.RequireTicketSessionCurrent(ctx, 42, ""); errcode.As(err) != errcode.ErrUnauthorized {
		t.Fatalf("empty sjti must be rejected when require_ticket_sjti on, got: %v", err)
	}
	// 收口档下非空现行 sjti 仍放行(门只作用于空值)。
	if err := uc.RequireTicketSessionCurrent(ctx, 42, "jti-B"); err != nil {
		t.Fatalf("current sjti must pass under require gate: %v", err)
	}
	// 会话消失(登出/过期):拒。
	sessions.found = false
	sessions.cur = ""
	if err := uc.RequireTicketSessionCurrent(ctx, 42, "jti-A"); err == nil {
		t.Fatal("vanished session must reject ticket redemption")
	}
	// 权威不可达:fail-closed。
	sessions.err = errcode.New(errcode.ErrUnavailable, "authority down")
	if err := uc.RequireTicketSessionCurrent(ctx, 42, "jti-A"); errcode.As(err) != errcode.ErrUnavailable {
		t.Fatalf("authority down must fail-closed, got: %v", err)
	}
}
