// mail_test.go — biz 层单测:装备实例型附件领取(as_instance → GrantInstances)+
// 溢出邮件源键透传(instance_grant_key 与直发链共享 → 至多一次)。
package biz

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	mailv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/mail/v1"

	"github.com/luyuancpp/pandora/services/social/mail/internal/conf"
	"github.com/luyuancpp/pandora/services/social/mail/internal/data"
)

// ── 测试替身 ──────────────────────────────────────────────────────────────────

type storedMail struct {
	playerID uint64
	payload  []byte
}

// fakeMailRepo 是内存版 data.MailRepo,只落地个人邮件 + 领取幂等(其余方法返回零值)。
type fakeMailRepo struct {
	personal map[uint64]storedMail // mailID → 邮件
	claimed  map[string]bool       // "player:mail" → 已领
	status   map[string]int32
}

func newFakeMailRepo() *fakeMailRepo {
	return &fakeMailRepo{
		personal: map[uint64]storedMail{},
		claimed:  map[string]bool{},
		status:   map[string]int32{},
	}
}

func ck(playerID, mailID uint64) string {
	return itoa(playerID) + ":" + itoa(mailID)
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func (r *fakeMailRepo) GetCursor(context.Context, uint64) (uint64, uint64, error) { return 0, 0, nil }
func (r *fakeMailRepo) GetPlayerGuild(context.Context, uint64) (uint64, bool, error) {
	return 0, false, nil
}
func (r *fakeMailRepo) ListPersonal(context.Context, uint64, int64, uint64, int) ([]data.MailRow, error) {
	return nil, nil
}
func (r *fakeMailRepo) ListSysSince(context.Context, uint64, int64) ([]data.MailRow, error) {
	return nil, nil
}
func (r *fakeMailRepo) ListGuildSince(context.Context, uint64, uint64, int64) ([]data.MailRow, error) {
	return nil, nil
}
func (r *fakeMailRepo) AdvanceCursor(context.Context, uint64, uint64, uint64) error { return nil }
func (r *fakeMailRepo) SetPersonalStatus(_ context.Context, playerID, mailID uint64, status int32) error {
	r.status[ck(playerID, mailID)] = status
	return nil
}
func (r *fakeMailRepo) DeletePersonal(context.Context, uint64, uint64) error { return nil }
func (r *fakeMailRepo) GetClaimablePayload(_ context.Context, playerID, mailID uint64, _ int64) ([]byte, bool, error) {
	m, ok := r.personal[mailID]
	if !ok || m.playerID != playerID {
		return nil, false, nil
	}
	return m.payload, true, nil
}
func (r *fakeMailRepo) HasClaimed(_ context.Context, playerID, mailID uint64) (bool, error) {
	return r.claimed[ck(playerID, mailID)], nil
}
func (r *fakeMailRepo) RecordClaim(_ context.Context, playerID, mailID uint64) (bool, error) {
	key := ck(playerID, mailID)
	if r.claimed[key] {
		return false, nil
	}
	r.claimed[key] = true
	return true, nil
}
func (r *fakeMailRepo) InsertSysMail(context.Context, uint64, int64, int64, []byte) error { return nil }
func (r *fakeMailRepo) InsertGuildMail(context.Context, uint64, uint64, int64, int64, []byte) error {
	return nil
}
func (r *fakeMailRepo) InsertPersonalMail(_ context.Context, mailID, playerID uint64, _ int64, payload []byte) error {
	r.personal[mailID] = storedMail{playerID: playerID, payload: payload}
	return nil
}

// fakeItemGranter 捕获可堆叠附件发放。
type fakeItemGranter struct {
	calls []itemCall
}
type itemCall struct {
	playerID uint64
	atts     []*mailv1.MailAttachment
	key      string
}

func (g *fakeItemGranter) Grant(_ context.Context, playerID uint64, atts []*mailv1.MailAttachment, key string) error {
	g.calls = append(g.calls, itemCall{playerID: playerID, atts: atts, key: key})
	return nil
}

// fakeInstGranter 捕获装备实例发放。
type fakeInstGranter struct {
	calls []instCall
}
type instCall struct {
	playerID uint64
	ids      []uint32
	key      string
}

func (g *fakeInstGranter) GrantInstances(_ context.Context, playerID uint64, ids []uint32, key string) error {
	g.calls = append(g.calls, instCall{playerID: playerID, ids: append([]uint32(nil), ids...), key: key})
	return nil
}

func testCfg() conf.MailConf {
	return conf.MailConf{DefaultSysTtlDays: 7, MaxTitleLen: 64, MaxBodyLen: 2048, MaxAttachments: 16}
}

// ── 测试 ──────────────────────────────────────────────────────────────────────

// TestSendPersonalMailStoresGrantKey 发信时 instance_grant_key 落入 payload 存储记录。
func TestSendPersonalMailStoresGrantKey(t *testing.T) {
	repo := newFakeMailRepo()
	uc := NewMailUsecase(repo, testCfg(), &fakeItemGranter{})
	atts := []*mailv1.MailAttachment{{ItemConfigId: 5001, Count: 2, AsInstance: true}}
	id, err := uc.SendPersonalMail(context.Background(), 100, 1, "掉落", "背包已满", atts, 0, "battle_drop:9:1")
	if err != nil {
		t.Fatalf("SendPersonalMail err: %v", err)
	}
	rec := &mailv1.MailContentStorageRecord{}
	if err := proto.Unmarshal(repo.personal[id].payload, rec); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if rec.GetInstanceGrantKey() != "battle_drop:9:1" {
		t.Fatalf("grant key not stored, got %q", rec.GetInstanceGrantKey())
	}
}

// TestClaimInstanceUsesGrantKey 领取装备型附件走 GrantInstances,用 payload 里的源键去重(逐件展开)。
func TestClaimInstanceUsesGrantKey(t *testing.T) {
	repo := newFakeMailRepo()
	item := &fakeItemGranter{}
	inst := &fakeInstGranter{}
	uc := NewMailUsecase(repo, testCfg(), item)
	uc.SetInstanceGranter(inst)

	atts := []*mailv1.MailAttachment{{ItemConfigId: 5001, Count: 2, AsInstance: true}}
	id, err := uc.SendPersonalMail(context.Background(), 100, 1, "掉落", "背包已满", atts, 0, "battle_drop:9:1")
	if err != nil {
		t.Fatalf("SendPersonalMail err: %v", err)
	}
	if _, err := uc.ClaimMail(context.Background(), 1, id, 1000); err != nil {
		t.Fatalf("ClaimMail err: %v", err)
	}
	if len(inst.calls) != 1 {
		t.Fatalf("expected 1 GrantInstances call, got %d", len(inst.calls))
	}
	if inst.calls[0].key != "battle_drop:9:1" {
		t.Fatalf("instance grant key wrong: %s", inst.calls[0].key)
	}
	// count=2 → 逐件展开 [5001,5001]
	if len(inst.calls[0].ids) != 2 || inst.calls[0].ids[0] != 5001 || inst.calls[0].ids[1] != 5001 {
		t.Fatalf("instance ids expand wrong: %v", inst.calls[0].ids)
	}
	// 装备型附件不应走可堆叠 GrantItems。
	if len(item.calls) != 0 {
		t.Fatalf("instance attachment must not call item granter, got %d", len(item.calls))
	}
}

// TestClaimInstanceDefaultKey 无源键时装备型附件用默认键 mail_inst:{mail}:{player}。
func TestClaimInstanceDefaultKey(t *testing.T) {
	repo := newFakeMailRepo()
	inst := &fakeInstGranter{}
	uc := NewMailUsecase(repo, testCfg(), &fakeItemGranter{})
	uc.SetInstanceGranter(inst)

	atts := []*mailv1.MailAttachment{{ItemConfigId: 5001, Count: 1, AsInstance: true}}
	id, err := uc.SendPersonalMail(context.Background(), 200, 7, "装备", "运营发放", atts, 0, "")
	if err != nil {
		t.Fatalf("SendPersonalMail err: %v", err)
	}
	if _, err := uc.ClaimMail(context.Background(), 7, id, 1000); err != nil {
		t.Fatalf("ClaimMail err: %v", err)
	}
	if len(inst.calls) != 1 || inst.calls[0].key != "mail_inst:200:7" {
		t.Fatalf("default instance grant key wrong: %+v", inst.calls)
	}
}

// TestClaimStackableUsesItemGranter 可堆叠附件走 GrantItems,不碰 GrantInstances。
func TestClaimStackableUsesItemGranter(t *testing.T) {
	repo := newFakeMailRepo()
	item := &fakeItemGranter{}
	inst := &fakeInstGranter{}
	uc := NewMailUsecase(repo, testCfg(), item)
	uc.SetInstanceGranter(inst)

	atts := []*mailv1.MailAttachment{{ItemConfigId: 3001, Count: 10}} // as_instance=false
	id, err := uc.SendPersonalMail(context.Background(), 300, 5, "金币", "领奖", atts, 0, "")
	if err != nil {
		t.Fatalf("SendPersonalMail err: %v", err)
	}
	if _, err := uc.ClaimMail(context.Background(), 5, id, 1000); err != nil {
		t.Fatalf("ClaimMail err: %v", err)
	}
	if len(item.calls) != 1 || item.calls[0].key != "mail:300:5" {
		t.Fatalf("stackable must call item granter with mail key, got %+v", item.calls)
	}
	if len(inst.calls) != 0 {
		t.Fatalf("stackable must not call instance granter, got %d", len(inst.calls))
	}
}

// TestClaimMixedBothGranters 混合附件:可堆叠 + 装备实例分别走各自发放器。
func TestClaimMixedBothGranters(t *testing.T) {
	repo := newFakeMailRepo()
	item := &fakeItemGranter{}
	inst := &fakeInstGranter{}
	uc := NewMailUsecase(repo, testCfg(), item)
	uc.SetInstanceGranter(inst)

	atts := []*mailv1.MailAttachment{
		{ItemConfigId: 3001, Count: 5},                   // 可堆叠
		{ItemConfigId: 5001, Count: 1, AsInstance: true}, // 装备实例
	}
	id, err := uc.SendPersonalMail(context.Background(), 400, 8, "混合", "领奖", atts, 0, "battle_drop:1:8")
	if err != nil {
		t.Fatalf("SendPersonalMail err: %v", err)
	}
	if _, err := uc.ClaimMail(context.Background(), 8, id, 1000); err != nil {
		t.Fatalf("ClaimMail err: %v", err)
	}
	if len(item.calls) != 1 || len(item.calls[0].atts) != 1 || item.calls[0].atts[0].GetItemConfigId() != 3001 {
		t.Fatalf("mixed: stackable routing wrong: %+v", item.calls)
	}
	if len(inst.calls) != 1 || len(inst.calls[0].ids) != 1 || inst.calls[0].ids[0] != 5001 {
		t.Fatalf("mixed: instance routing wrong: %+v", inst.calls)
	}
}
