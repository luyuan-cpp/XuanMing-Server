// login_test.go — LoginUsecase.resolveHub 行为单测(W4 ⑥,2026-06-06)。
//
// 覆盖 hub_allocator 弱依赖三态:
//   - hubAssigner 非 nil 且 AssignHub 成功 → 用 allocator 返回的 hub_ds_addr + hub_ticket
//   - hubAssigner 为 nil → 回退自签 hub 票据 + 静态 hubDSAddr
//   - hubAssigner 返回错误 → 回退自签(不阻断登录)
package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luyuancpp/pandora/pkg/auth"
	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/passwd"
	"github.com/luyuancpp/pandora/pkg/snowflake"
	"github.com/luyuancpp/pandora/services/account/login/internal/data"
)

const testSecret = "pandora-dev-jwt-secret-change-me-32!" // 36 字节,满足 HS256 ≥32

// mustBcrypt 用 DevCost 哈希明文密码,失败 fatal。
func mustBcrypt(t *testing.T, plain string) string {
	t.Helper()
	h, err := passwd.Hash(plain, passwd.DevCost)
	if err != nil {
		t.Fatalf("passwd.Hash: %v", err)
	}
	return h
}

// ---- fakes ----

type fakeAccountRepo struct {
	playerID     uint64
	passwordHash string
	banned       bool
}

func (f *fakeAccountRepo) FindByAccount(_ context.Context, _ string) (uint64, string, error) {
	return f.playerID, f.passwordHash, nil
}
func (f *fakeAccountRepo) CreateAccount(_ context.Context, _ uint64, _, _ string) error { return nil }
func (f *fakeAccountRepo) CheckBanned(_ context.Context, _ uint64, _ string) (bool, error) {
	return f.banned, nil
}
func (f *fakeAccountRepo) TouchDevice(_ context.Context, _ uint64, _ string) error { return nil }

type fakeSessionRepo struct{}

func (fakeSessionRepo) Set(_ context.Context, _ uint64, _, _, _ string, _ time.Duration) error {
	return nil
}
func (fakeSessionRepo) Delete(_ context.Context, _ uint64) error { return nil }

type fakeHubAssigner struct {
	res *data.HubAssignment
	err error

	gotPlayerID uint64
	gotRegion   string
	gotTeamID   uint64
}

func (f *fakeHubAssigner) AssignHub(_ context.Context, playerID uint64, region string, teamID uint64) (*data.HubAssignment, error) {
	f.gotPlayerID = playerID
	f.gotRegion = region
	f.gotTeamID = teamID
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

// newTestUsecase 构造一个登录用例(密码 bcrypt 校验在 biz 之外,这里直接给明文等值匹配)。
func newTestUsecase(t *testing.T, hub data.HubAssigner) *LoginUsecase {
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
	// bcrypt 哈希一个固定密码 "pw",让 passwd.Verify 通过。
	hash := mustBcrypt(t, "pw")
	repo := &fakeAccountRepo{playerID: 42, passwordHash: hash}
	sf := snowflake.NewNode(1)
	return NewLoginUsecase(repo, fakeSessionRepo{}, nil, hub, sf, "127.0.0.1:7777", "cn", signer, verifier, false, false)
}

func TestLogin_HubAssignerSuccess(t *testing.T) {
	hub := &fakeHubAssigner{res: &data.HubAssignment{
		HubDSAddr:  "10.0.0.9:7777",
		HubTicket:  "", // 见下:用真实签名替换以便 verifier 能解析 exp
		HubPodName: "pandora-hub-cn-2",
		ShardID:    2,
	}}
	uc := newTestUsecase(t, hub)

	// 用 uc.signer 真实签一张 hub 票据塞进 allocator 返回,模拟 hub_allocator 用共享 secret 签的票。
	tk, _, err := uc.signer.SignDSTicket(42, auth.DSTypeHub, 0, "jti-hub")
	if err != nil {
		t.Fatalf("sign hub ticket: %v", err)
	}
	hub.res.HubTicket = tk

	res, err := uc.Login(context.Background(), "acc", "pw", "dev-1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res.HubDSAddr != "10.0.0.9:7777" {
		t.Errorf("HubDSAddr = %q, want allocator addr", res.HubDSAddr)
	}
	if res.HubTicket != tk {
		t.Errorf("HubTicket not the allocator-signed ticket")
	}
	if res.HubTicketExpMs <= 0 {
		t.Errorf("HubTicketExpMs = %d, want >0 (parsed from ticket)", res.HubTicketExpMs)
	}
	if hub.gotPlayerID != 42 || hub.gotRegion != "cn" || hub.gotTeamID != 0 {
		t.Errorf("AssignHub args = (%d,%q,%d), want (42,\"cn\",0)", hub.gotPlayerID, hub.gotRegion, hub.gotTeamID)
	}
}

func TestLogin_HubAssignerNil_FallbackSelfSign(t *testing.T) {
	uc := newTestUsecase(t, nil)

	res, err := uc.Login(context.Background(), "acc", "pw", "dev-1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if res.HubDSAddr != "127.0.0.1:7777" {
		t.Errorf("HubDSAddr = %q, want static fallback addr", res.HubDSAddr)
	}
	// 自签票据应能被 verifier 验通过且是 hub 类型。
	claims, verr := uc.verifier.VerifyDSTicket(res.HubTicket)
	if verr != nil {
		t.Fatalf("self-signed hub ticket not verifiable: %v", verr)
	}
	if claims.DSType != string(auth.DSTypeHub) || claims.PlayerID() != 42 {
		t.Errorf("self-signed ticket claims = (%s, pid=%d), want (hub, 42)", claims.DSType, claims.PlayerID())
	}
}

func TestLogin_HubAssignerError_FallbackSelfSign(t *testing.T) {
	hub := &fakeHubAssigner{err: errors.New("hub_allocator down")}
	uc := newTestUsecase(t, hub)

	res, err := uc.Login(context.Background(), "acc", "pw", "dev-1")
	if err != nil {
		t.Fatalf("Login should fall back, got err: %v", err)
	}
	if res.HubDSAddr != "127.0.0.1:7777" {
		t.Errorf("HubDSAddr = %q, want static fallback addr on AssignHub error", res.HubDSAddr)
	}
	if _, verr := uc.verifier.VerifyDSTicket(res.HubTicket); verr != nil {
		t.Fatalf("fallback hub ticket not verifiable: %v", verr)
	}
}

// ---- dev_skip_password fakes / tests ----

// devFakeRepo 模拟 MySQL 行为:按 account 名查/建,验证免密模式下的懒注册稳定性。
type devFakeRepo struct {
	accounts map[string]uint64 // account -> player_id
	hashes   map[string]string // account -> bcrypt password_hash
	created  []string          // 记录被 CreateAccount 的账号(断言"只建一次")
}

func newDevFakeRepo() *devFakeRepo {
	return &devFakeRepo{accounts: map[string]uint64{}, hashes: map[string]string{}}
}

func (r *devFakeRepo) FindByAccount(_ context.Context, account string) (uint64, string, error) {
	if id, ok := r.accounts[account]; ok {
		return id, r.hashes[account], nil
	}
	return 0, "", errcode.New(errcode.ErrLoginAccountNotFound, "account=%s not found", account)
}
func (r *devFakeRepo) CreateAccount(_ context.Context, playerID uint64, account, passwordHash string) error {
	if _, ok := r.accounts[account]; ok {
		return errcode.New(errcode.ErrAlreadyExists, "account=%s exists", account)
	}
	r.accounts[account] = playerID
	r.hashes[account] = passwordHash
	r.created = append(r.created, account)
	return nil
}
func (r *devFakeRepo) CheckBanned(_ context.Context, _ uint64, _ string) (bool, error) {
	return false, nil
}
func (r *devFakeRepo) TouchDevice(_ context.Context, _ uint64, _ string) error { return nil }

func newDevSkipUsecase(t *testing.T, repo data.AccountRepo) *LoginUsecase {
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
	sf := snowflake.NewNode(1)
	// hubAssigner=nil 走自签回退;devSkipPassword=true。
	return NewLoginUsecase(repo, fakeSessionRepo{}, nil, nil, sf, "127.0.0.1:7777", "cn", signer, verifier, true, false)
}

// newDevAutoRegUsecase 构造 devAutoRegister=true 、 devSkipPassword=false 的用例。
func newDevAutoRegUsecase(t *testing.T, repo data.AccountRepo) *LoginUsecase {
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
	sf := snowflake.NewNode(1)
	return NewLoginUsecase(repo, fakeSessionRepo{}, nil, nil, sf, "127.0.0.1:7777", "cn", signer, verifier, false, true)
}

// TestLogin_DevSkipPassword_AutoProvision 验证:免密模式下任意新账号自动建号,
// 同一账号名两次登录拿到同一个稳定 player_id,且账号只被创建一次。
func TestLogin_DevSkipPassword_AutoProvision(t *testing.T) {
	repo := newDevFakeRepo()
	uc := newDevSkipUsecase(t, repo)

	res1, err := uc.Login(context.Background(), "anybody", "whatever", "dev-1")
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	if res1.PlayerID == 0 {
		t.Fatalf("PlayerID = 0, want auto-provisioned id")
	}

	res2, err := uc.Login(context.Background(), "anybody", "another-pw", "dev-2")
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	if res2.PlayerID != res1.PlayerID {
		t.Errorf("PlayerID not stable: first=%d second=%d", res1.PlayerID, res2.PlayerID)
	}
	if len(repo.created) != 1 {
		t.Errorf("account created %d times, want exactly 1", len(repo.created))
	}
}

// TestLogin_DevSkipPassword_ExistingAccountWrongPassword 验证:已存在账号在免密模式下
// 任意密码都放行(不做 bcrypt 校验)。
func TestLogin_DevSkipPassword_ExistingAccountWrongPassword(t *testing.T) {
	repo := newDevFakeRepo()
	repo.accounts["known"] = 777
	uc := newDevSkipUsecase(t, repo)

	res, err := uc.Login(context.Background(), "known", "definitely-wrong", "dev-1")
	if err != nil {
		t.Fatalf("login with wrong password should pass in skip mode: %v", err)
	}
	if res.PlayerID != 777 {
		t.Errorf("PlayerID = %d, want existing 777", res.PlayerID)
	}
	if len(repo.created) != 0 {
		t.Errorf("existing account should not be re-created, got %d creates", len(repo.created))
	}
}

// TestLogin_DevAutoRegister_FirstLoginRegisters 验证假注册(不免密)语义:
//   - 首登未知账号 → 自动注册,存本次密码;
//   - 同账号同密码再登 → 走正常 bcrypt 校验通过,player_id 稳定;
//   - 同账号错密码 → ErrLoginPasswordMismatch(密码仍生效)。
func TestLogin_DevAutoRegister_FirstLoginRegisters(t *testing.T) {
	repo := newDevFakeRepo()
	uc := newDevAutoRegUsecase(t, repo)

	res1, err := uc.Login(context.Background(), "newbie", "pw1", "dev-1")
	if err != nil {
		t.Fatalf("first login (register): %v", err)
	}
	if res1.PlayerID == 0 {
		t.Fatalf("PlayerID = 0, want auto-registered id")
	}
	if len(repo.created) != 1 {
		t.Fatalf("account created %d times, want exactly 1", len(repo.created))
	}

	// 同密码复登 → bcrypt 校验通过,同一 player_id。
	res2, err := uc.Login(context.Background(), "newbie", "pw1", "dev-2")
	if err != nil {
		t.Fatalf("second login (verify): %v", err)
	}
	if res2.PlayerID != res1.PlayerID {
		t.Errorf("PlayerID not stable: first=%d second=%d", res1.PlayerID, res2.PlayerID)
	}

	// 错密码 → 仍拦(假注册不等于免密)。
	if _, err := uc.Login(context.Background(), "newbie", "wrong-pw", "dev-3"); err == nil {
		t.Errorf("wrong password should be rejected when only auto_register is on")
	} else if errcode.As(err) != errcode.ErrLoginPasswordMismatch {
		t.Errorf("err code = %d, want ErrLoginPasswordMismatch(%d)", errcode.As(err), errcode.ErrLoginPasswordMismatch)
	}
}
