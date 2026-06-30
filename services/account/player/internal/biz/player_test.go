// player_test.go — PlayerUsecase 业务逻辑单测(W4 ④,2026-06-06)。
//
// 用内存版 fakeRepo 复刻 MySQL 幂等 / clamp / 战绩计数语义,无需真 DB。
package biz

import (
	"context"
	"strconv"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"
	"github.com/luyuancpp/pandora/services/account/player/internal/conf"
	"github.com/luyuancpp/pandora/services/account/player/internal/data"
)

// fakeProfile 是内存玩家档案。
type fakeProfile struct {
	nickname     string
	mmr          int
	totalBattles int32
	totalWins    int32
}

// fakeRepo 是 data.PlayerRepo 的内存实现(复刻 MySQL 幂等语义)。
type fakeRepo struct {
	players      map[uint64]*fakeProfile
	heroes       map[uint64]map[uint32]bool
	idem         map[string]int // key=playerID|idempotencyKey → 已记录 new_mmr
	activeHero   map[uint64]uint32
	attrs        map[uint64]map[string]int32
	unspent      map[uint64]int
	grants       map[string]bool // key=playerID|idempotencyKey
	equipment    map[uint64][]data.EquipmentSlot
	talents      map[uint64]map[uint32]int32
	talentTotal  map[uint64]int
	talentGrants map[string]bool   // key=playerID|idempotencyKey
	rewardRec    map[uint64][]byte // 领奖记录序列化 bytes
	rewardVer    map[uint64]int32  // 领奖记录乐观锁版本
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		players:      map[uint64]*fakeProfile{},
		heroes:       map[uint64]map[uint32]bool{},
		idem:         map[string]int{},
		activeHero:   map[uint64]uint32{},
		attrs:        map[uint64]map[string]int32{},
		unspent:      map[uint64]int{},
		grants:       map[string]bool{},
		equipment:    map[uint64][]data.EquipmentSlot{},
		talents:      map[uint64]map[uint32]int32{},
		talentTotal:  map[uint64]int{},
		talentGrants: map[string]bool{},
		rewardRec:    map[uint64][]byte{},
		rewardVer:    map[uint64]int32{},
	}
}

func (f *fakeRepo) EnsureProfile(_ context.Context, playerID uint64, defaultNickname string, baseMMR int) error {
	if _, ok := f.players[playerID]; !ok {
		f.players[playerID] = &fakeProfile{nickname: defaultNickname, mmr: baseMMR}
	}
	return nil
}

func (f *fakeRepo) GetProfile(_ context.Context, playerID uint64) (*playerv1.PlayerProfile, bool, error) {
	p, ok := f.players[playerID]
	if !ok {
		return nil, false, nil
	}
	return &playerv1.PlayerProfile{
		PlayerId:     playerID,
		Nickname:     p.nickname,
		Mmr:          int32(p.mmr),
		TotalBattles: p.totalBattles,
		TotalWins:    p.totalWins,
	}, true, nil
}

func (f *fakeRepo) UpdateNickname(_ context.Context, playerID uint64, nickname string) error {
	for pid, p := range f.players {
		if pid != playerID && p.nickname == nickname {
			return errcode.New(errcode.ErrPlayerNicknameTaken, "taken")
		}
	}
	p, ok := f.players[playerID]
	if !ok {
		return errcode.New(errcode.ErrPlayerNotFound, "not found")
	}
	p.nickname = nickname
	return nil
}

func (f *fakeRepo) ListHeroes(_ context.Context, playerID uint64) ([]uint32, error) {
	var out []uint32
	for h := range f.heroes[playerID] {
		out = append(out, h)
	}
	return out, nil
}

func (f *fakeRepo) UnlockHero(_ context.Context, playerID uint64, heroID uint32, _ string) (bool, error) {
	if f.heroes[playerID] == nil {
		f.heroes[playerID] = map[uint32]bool{}
	}
	if f.heroes[playerID][heroID] {
		return true, nil
	}
	f.heroes[playerID][heroID] = true
	return false, nil
}

func (f *fakeRepo) GetMMR(_ context.Context, playerID uint64) (int, bool, error) {
	p, ok := f.players[playerID]
	if !ok {
		return 0, false, nil
	}
	return p.mmr, true, nil
}

func (f *fakeRepo) ApplyMMRChange(_ context.Context, c data.MMRChange) (int, bool, error) {
	p, ok := f.players[c.PlayerID]
	if !ok {
		return 0, false, errcode.New(errcode.ErrPlayerNotFound, "not found")
	}
	idemKey := keyOf(c.PlayerID, c.IdempotencyKey)
	if recorded, hit := f.idem[idemKey]; hit {
		return recorded, true, nil
	}
	newMMR := p.mmr + int(c.Delta)
	if newMMR < c.Floor {
		newMMR = c.Floor
	}
	p.mmr = newMMR
	if c.IncBattle {
		p.totalBattles++
	}
	if c.IncWin {
		p.totalWins++
	}
	f.idem[idemKey] = newMMR
	return newMMR, false, nil
}

func keyOf(pid uint64, k string) string {
	return strconv.FormatUint(pid, 10) + "|" + k
}

func (f *fakeRepo) IsHeroOwned(_ context.Context, playerID uint64, heroID uint32) (bool, error) {
	return f.heroes[playerID][heroID], nil
}

func (f *fakeRepo) SetActiveHero(_ context.Context, playerID uint64, heroID uint32) error {
	f.activeHero[playerID] = heroID
	return nil
}

func (f *fakeRepo) GetActiveHero(_ context.Context, playerID uint64) (uint32, error) {
	return f.activeHero[playerID], nil
}

func (f *fakeRepo) GrantAttributePoints(_ context.Context, playerID uint64, points int32, idempotencyKey string) (int, bool, error) {
	gk := keyOf(playerID, idempotencyKey)
	if f.grants[gk] {
		return f.unspent[playerID], true, nil
	}
	f.grants[gk] = true
	f.unspent[playerID] += int(points)
	return f.unspent[playerID], false, nil
}

func (f *fakeRepo) AllocateAttributePoints(_ context.Context, playerID uint64, allocs []data.AttrAllocation) (int, error) {
	var sum int32
	for _, a := range allocs {
		sum += a.Points
	}
	if int(sum) > f.unspent[playerID] {
		return 0, errcode.New(errcode.ErrPlayerInsufficientPoints, "insufficient")
	}
	if f.attrs[playerID] == nil {
		f.attrs[playerID] = map[string]int32{}
	}
	for _, a := range allocs {
		f.attrs[playerID][a.Key] += a.Points
	}
	f.unspent[playerID] -= int(sum)
	return f.unspent[playerID], nil
}

func (f *fakeRepo) ResetAttributes(_ context.Context, playerID uint64) (int, error) {
	var total int32
	for _, p := range f.attrs[playerID] {
		total += p
	}
	f.attrs[playerID] = map[string]int32{}
	f.unspent[playerID] += int(total)
	return f.unspent[playerID], nil
}

func (f *fakeRepo) GetAttributes(_ context.Context, playerID uint64) ([]data.AttrPoint, int, error) {
	var out []data.AttrPoint
	for k, p := range f.attrs[playerID] {
		out = append(out, data.AttrPoint{Key: k, Points: p})
	}
	return out, f.unspent[playerID], nil
}

func (f *fakeRepo) SetEquipment(_ context.Context, playerID uint64, slots []data.EquipmentSlot) error {
	cp := make([]data.EquipmentSlot, len(slots))
	copy(cp, slots)
	f.equipment[playerID] = cp
	return nil
}

func (f *fakeRepo) GetEquipment(_ context.Context, playerID uint64) ([]data.EquipmentSlot, error) {
	return f.equipment[playerID], nil
}

func (f *fakeRepo) talentUsed(playerID uint64) int {
	var used int
	for _, lv := range f.talents[playerID] {
		used += int(lv)
	}
	return used
}

func (f *fakeRepo) GrantTalentPoints(_ context.Context, playerID uint64, points int32, idempotencyKey string) (int, bool, error) {
	gk := keyOf(playerID, idempotencyKey)
	if f.talentGrants[gk] {
		return f.talentTotal[playerID] - f.talentUsed(playerID), true, nil
	}
	f.talentGrants[gk] = true
	f.talentTotal[playerID] += int(points)
	return f.talentTotal[playerID] - f.talentUsed(playerID), false, nil
}

func (f *fakeRepo) SetTalents(_ context.Context, playerID uint64, talents []data.TalentLevel) (int, error) {
	var sum int32
	for _, t := range talents {
		sum += t.Level
	}
	if int(sum) > f.talentTotal[playerID] {
		return 0, errcode.New(errcode.ErrPlayerInsufficientPoints, "insufficient")
	}
	m := map[uint32]int32{}
	for _, t := range talents {
		m[t.TalentID] = t.Level
	}
	f.talents[playerID] = m
	return f.talentTotal[playerID] - int(sum), nil
}

func (f *fakeRepo) ResetTalents(_ context.Context, playerID uint64) (int, error) {
	f.talents[playerID] = map[uint32]int32{}
	return f.talentTotal[playerID], nil
}

func (f *fakeRepo) GetTalents(_ context.Context, playerID uint64) ([]data.TalentLevel, int, error) {
	var out []data.TalentLevel
	for id, lv := range f.talents[playerID] {
		out = append(out, data.TalentLevel{TalentID: id, Level: lv})
	}
	return out, f.talentTotal[playerID] - f.talentUsed(playerID), nil
}

func (f *fakeRepo) LoadRewardClaims(_ context.Context, playerID uint64) ([]byte, int32, error) {
	return f.rewardRec[playerID], f.rewardVer[playerID], nil
}

func (f *fakeRepo) SaveRewardClaims(_ context.Context, playerID uint64, record []byte, expectVersion int32) error {
	if f.rewardVer[playerID] != expectVersion {
		return errcode.New(errcode.ErrPlayerVersionMismatch, "reward claims player=%d version mismatch", playerID)
	}
	f.rewardRec[playerID] = record
	f.rewardVer[playerID] = expectVersion + 1
	return nil
}

func newUC(repo data.PlayerRepo) *PlayerUsecase {
	return NewPlayerUsecase(repo, conf.PlayerConf{BaseMMR: 1500, MMRFloor: 0, DefaultNicknamePrefix: "Player_", MaxNicknameLen: 32})
}

func newUCHero(repo data.PlayerRepo) *PlayerUsecase {
	return NewPlayerUsecase(repo, conf.PlayerConf{BaseMMR: 1500, MMRFloor: 0, DefaultNicknamePrefix: "Player_", MaxNicknameLen: 32, HeroSelectionEnabled: true})
}

func newUCLoadout(repo data.PlayerRepo) *PlayerUsecase {
	return NewPlayerUsecase(repo, conf.PlayerConf{BaseMMR: 1500, MMRFloor: 0, DefaultNicknamePrefix: "Player_", MaxNicknameLen: 32, HeroSelectionEnabled: true, LoadoutCustomizeEnabled: true})
}

func TestUpdateMMR_AppliesDelta(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	newMMR, already, err := uc.UpdateMMR(context.Background(), 100, 16, "win", "m1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if already {
		t.Fatal("first call should not be idempotent hit")
	}
	if newMMR != 1516 {
		t.Fatalf("want 1516, got %d", newMMR)
	}
	if repo.players[100].totalBattles != 1 || repo.players[100].totalWins != 1 {
		t.Fatalf("win should inc battle+win, got battles=%d wins=%d",
			repo.players[100].totalBattles, repo.players[100].totalWins)
	}
}

func TestUpdateMMR_Idempotent(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	first, _, err := uc.UpdateMMR(context.Background(), 100, 16, "win", "m1")
	if err != nil {
		t.Fatalf("first err: %v", err)
	}
	second, already, err := uc.UpdateMMR(context.Background(), 100, 16, "win", "m1")
	if err != nil {
		t.Fatalf("second err: %v", err)
	}
	if !already {
		t.Fatal("second call with same key should be idempotent hit")
	}
	if second != first {
		t.Fatalf("idempotent return should equal first: first=%d second=%d", first, second)
	}
	if repo.players[100].mmr != 1516 {
		t.Fatalf("mmr must not double-apply, got %d", repo.players[100].mmr)
	}
	if repo.players[100].totalBattles != 1 {
		t.Fatalf("battles must not double-count, got %d", repo.players[100].totalBattles)
	}
}

func TestUpdateMMR_RequiresKey(t *testing.T) {
	uc := newUC(newFakeRepo())
	_, _, err := uc.UpdateMMR(context.Background(), 100, 16, "win", "")
	if errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("empty idempotency_key should be ErrInvalidArg, got %v", err)
	}
}

func TestUpdateMMR_Floor(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	newMMR, _, err := uc.UpdateMMR(context.Background(), 100, -9999, "lose", "m1")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if newMMR != 0 {
		t.Fatalf("mmr should clamp to floor 0, got %d", newMMR)
	}
}

func TestUpdateMMR_LoseCountsBattleNotWin(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, _, err := uc.UpdateMMR(context.Background(), 100, -16, "lose", "m1"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if repo.players[100].totalBattles != 1 || repo.players[100].totalWins != 0 {
		t.Fatalf("lose: battle+1 win+0, got battles=%d wins=%d",
			repo.players[100].totalBattles, repo.players[100].totalWins)
	}
}

func TestUpdateMMR_AbandonNoBattleCount(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, _, err := uc.UpdateMMR(context.Background(), 100, 0, "abandon", "m1"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if repo.players[100].totalBattles != 0 {
		t.Fatalf("abandon should not count battle, got %d", repo.players[100].totalBattles)
	}
}

func TestGetMMR_NotFoundReturnsBase(t *testing.T) {
	uc := newUC(newFakeRepo())
	mmr, err := uc.GetMMR(context.Background(), 999)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if mmr != 1500 {
		t.Fatalf("unbuilt player should return base 1500, got %d", mmr)
	}
}

func TestUnlockHero_Idempotent(t *testing.T) {
	uc := newUC(newFakeRepo())
	if err := uc.UnlockHero(context.Background(), 100, 7, "reward"); err != nil {
		t.Fatalf("first unlock err: %v", err)
	}
	err := uc.UnlockHero(context.Background(), 100, 7, "reward")
	if errcode.As(err) != errcode.ErrPlayerHeroAlreadyOwn {
		t.Fatalf("second unlock should be ErrPlayerHeroAlreadyOwn, got %v", err)
	}
}

func TestUpdateNickname_Validation(t *testing.T) {
	uc := newUC(newFakeRepo())
	if err := uc.UpdateNickname(context.Background(), 100, "   "); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("blank nickname should be ErrInvalidArg, got %v", err)
	}
	if err := uc.UpdateNickname(context.Background(), 0, "ok"); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("zero player_id should be ErrInvalidArg, got %v", err)
	}
}

func TestBattleFlags(t *testing.T) {
	cases := []struct {
		reason              string
		wantBattle, wantWin bool
	}{
		{"win", true, true},
		{"lose", true, false},
		{"draw", true, false},
		{"abandon", false, false},
		{"rollback", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		b, w := battleFlags(c.reason)
		if b != c.wantBattle || w != c.wantWin {
			t.Fatalf("reason=%q: want (battle=%v win=%v), got (%v %v)", c.reason, c.wantBattle, c.wantWin, b, w)
		}
	}
}

// ── 出战养成 ──────────────────────────────────────────────────────────────────

func TestSelectHero_FeatureDisabled(t *testing.T) {
	uc := newUC(newFakeRepo()) // HeroSelectionEnabled=false
	err := uc.SelectHero(context.Background(), 100, 7)
	if errcode.As(err) != errcode.ErrPlayerFeatureDisabled {
		t.Fatalf("disabled toggle should be ErrPlayerFeatureDisabled, got %v", err)
	}
}

func TestSelectHero_NotOwned(t *testing.T) {
	uc := newUCHero(newFakeRepo())
	err := uc.SelectHero(context.Background(), 100, 7)
	if errcode.As(err) != errcode.ErrPlayerHeroLocked {
		t.Fatalf("unowned hero should be ErrPlayerHeroLocked, got %v", err)
	}
}

func TestSelectHero_Success(t *testing.T) {
	repo := newFakeRepo()
	uc := newUCHero(repo)
	if err := uc.UnlockHero(context.Background(), 100, 7, "reward"); err != nil {
		t.Fatalf("unlock err: %v", err)
	}
	if err := uc.SelectHero(context.Background(), 100, 7); err != nil {
		t.Fatalf("select err: %v", err)
	}
	hero, err := uc.GetActiveHero(context.Background(), 100)
	if err != nil {
		t.Fatalf("get active err: %v", err)
	}
	if hero != 7 {
		t.Fatalf("active hero want 7, got %d", hero)
	}
}

func TestGrantAttributePoints_Idempotent(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	first, err := uc.GrantAttributePoints(context.Background(), 100, 5, "lvlup-10")
	if err != nil {
		t.Fatalf("first grant err: %v", err)
	}
	if first != 5 {
		t.Fatalf("first grant unspent want 5, got %d", first)
	}
	second, err := uc.GrantAttributePoints(context.Background(), 100, 5, "lvlup-10")
	if err != nil {
		t.Fatalf("second grant err: %v", err)
	}
	if second != 5 {
		t.Fatalf("idempotent grant should not double-add, want 5, got %d", second)
	}
}

func TestGrantAttributePoints_RequiresKey(t *testing.T) {
	uc := newUC(newFakeRepo())
	if _, err := uc.GrantAttributePoints(context.Background(), 100, 5, ""); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("empty key should be ErrInvalidArg, got %v", err)
	}
	if _, err := uc.GrantAttributePoints(context.Background(), 100, 0, "k"); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("non-positive points should be ErrInvalidArg, got %v", err)
	}
}

func TestAllocateAttributePoints_Insufficient(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, err := uc.GrantAttributePoints(context.Background(), 100, 3, "g1"); err != nil {
		t.Fatalf("grant err: %v", err)
	}
	_, err := uc.AllocateAttributePoints(context.Background(), 100, []data.AttrAllocation{{Key: "str", Points: 5}})
	if errcode.As(err) != errcode.ErrPlayerInsufficientPoints {
		t.Fatalf("over-allocate should be ErrPlayerInsufficientPoints, got %v", err)
	}
}

func TestAllocateAttributePoints_SuccessThenReset(t *testing.T) {
	repo := newFakeRepo()
	uc := newUC(repo)
	if _, err := uc.GrantAttributePoints(context.Background(), 100, 10, "g1"); err != nil {
		t.Fatalf("grant err: %v", err)
	}
	unspent, err := uc.AllocateAttributePoints(context.Background(), 100, []data.AttrAllocation{{Key: "str", Points: 3}, {Key: "agi", Points: 2}})
	if err != nil {
		t.Fatalf("allocate err: %v", err)
	}
	if unspent != 5 {
		t.Fatalf("after allocate 5, unspent want 5, got %d", unspent)
	}
	attrs, u2, err := uc.GetAttributes(context.Background(), 100)
	if err != nil {
		t.Fatalf("get attrs err: %v", err)
	}
	if u2 != 5 || len(attrs) != 2 {
		t.Fatalf("want unspent=5 attrs=2, got unspent=%d attrs=%d", u2, len(attrs))
	}
	resetUnspent, err := uc.ResetAttributes(context.Background(), 100)
	if err != nil {
		t.Fatalf("reset err: %v", err)
	}
	if resetUnspent != 10 {
		t.Fatalf("after reset all points return, unspent want 10, got %d", resetUnspent)
	}
}

func TestAllocateAttributePoints_Validation(t *testing.T) {
	uc := newUC(newFakeRepo())
	if _, err := uc.AllocateAttributePoints(context.Background(), 100, nil); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("empty allocs should be ErrInvalidArg, got %v", err)
	}
	if _, err := uc.AllocateAttributePoints(context.Background(), 100, []data.AttrAllocation{{Key: "", Points: 1}}); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("empty key should be ErrInvalidArg, got %v", err)
	}
	if _, err := uc.AllocateAttributePoints(context.Background(), 100, []data.AttrAllocation{{Key: "str", Points: 0}}); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("non-positive points should be ErrInvalidArg, got %v", err)
	}
}

func TestGetLoadout_Snapshot(t *testing.T) {
	repo := newFakeRepo()
	uc := newUCHero(repo)
	if err := uc.UnlockHero(context.Background(), 100, 7, "reward"); err != nil {
		t.Fatalf("unlock err: %v", err)
	}
	if err := uc.SelectHero(context.Background(), 100, 7); err != nil {
		t.Fatalf("select err: %v", err)
	}
	if _, err := uc.GrantAttributePoints(context.Background(), 100, 4, "g1"); err != nil {
		t.Fatalf("grant err: %v", err)
	}
	if _, err := uc.AllocateAttributePoints(context.Background(), 100, []data.AttrAllocation{{Key: "str", Points: 1}}); err != nil {
		t.Fatalf("allocate err: %v", err)
	}
	loadout, err := uc.GetLoadout(context.Background(), 100)
	if err != nil {
		t.Fatalf("loadout err: %v", err)
	}
	if loadout.GetActiveHeroId() != 7 {
		t.Fatalf("loadout hero want 7, got %d", loadout.GetActiveHeroId())
	}
	if loadout.GetUnspentAttrPoints() != 3 {
		t.Fatalf("loadout unspent want 3, got %d", loadout.GetUnspentAttrPoints())
	}
	if len(loadout.GetAttributes()) != 1 {
		t.Fatalf("loadout attrs want 1, got %d", len(loadout.GetAttributes()))
	}
}

// ── 出战装备预设 / 天赋树(W5 ②)──────────────────────────────────────────────

func TestSetEquipment_FeatureDisabled(t *testing.T) {
	uc := newUCHero(newFakeRepo()) // LoadoutCustomizeEnabled=false
	err := uc.SetEquipment(context.Background(), 100, []data.EquipmentSlot{{Slot: 0, ItemConfigID: 1001}})
	if errcode.As(err) != errcode.ErrPlayerFeatureDisabled {
		t.Fatalf("disabled toggle should be ErrPlayerFeatureDisabled, got %v", err)
	}
}

func TestSetEquipment_DuplicateSlot(t *testing.T) {
	uc := newUCLoadout(newFakeRepo())
	err := uc.SetEquipment(context.Background(), 100, []data.EquipmentSlot{{Slot: 0, ItemConfigID: 1001}, {Slot: 0, ItemConfigID: 1002}})
	if errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("duplicate slot should be ErrInvalidArg, got %v", err)
	}
}

func TestSetEquipment_RequiresItemConfig(t *testing.T) {
	uc := newUCLoadout(newFakeRepo())
	err := uc.SetEquipment(context.Background(), 100, []data.EquipmentSlot{{Slot: 0, ItemConfigID: 0}})
	if errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("zero item_config_id should be ErrInvalidArg, got %v", err)
	}
}

func TestSetEquipment_SuccessThenGet(t *testing.T) {
	repo := newFakeRepo()
	uc := newUCLoadout(repo)
	if err := uc.SetEquipment(context.Background(), 100, []data.EquipmentSlot{{Slot: 0, ItemConfigID: 1001}, {Slot: 1, ItemConfigID: 1002}}); err != nil {
		t.Fatalf("set equipment err: %v", err)
	}
	slots, err := uc.GetEquipment(context.Background(), 100)
	if err != nil {
		t.Fatalf("get equipment err: %v", err)
	}
	if len(slots) != 2 {
		t.Fatalf("equipment want 2 slots, got %d", len(slots))
	}
}

func TestGrantTalentPoints_Idempotent(t *testing.T) {
	repo := newFakeRepo()
	uc := newUCLoadout(repo)
	first, err := uc.GrantTalentPoints(context.Background(), 100, 6, "lvlup-20")
	if err != nil {
		t.Fatalf("first grant err: %v", err)
	}
	if first != 6 {
		t.Fatalf("first talent grant unspent want 6, got %d", first)
	}
	second, err := uc.GrantTalentPoints(context.Background(), 100, 6, "lvlup-20")
	if err != nil {
		t.Fatalf("second grant err: %v", err)
	}
	if second != 6 {
		t.Fatalf("idempotent talent grant should not double-add, want 6, got %d", second)
	}
}

func TestSetTalents_Insufficient(t *testing.T) {
	repo := newFakeRepo()
	uc := newUCLoadout(repo)
	if _, err := uc.GrantTalentPoints(context.Background(), 100, 2, "g1"); err != nil {
		t.Fatalf("grant err: %v", err)
	}
	_, err := uc.SetTalents(context.Background(), 100, []data.TalentLevel{{TalentID: 5001, Level: 3}})
	if errcode.As(err) != errcode.ErrPlayerInsufficientPoints {
		t.Fatalf("over-spec should be ErrPlayerInsufficientPoints, got %v", err)
	}
}

func TestSetTalents_DuplicateTalent(t *testing.T) {
	repo := newFakeRepo()
	uc := newUCLoadout(repo)
	if _, err := uc.GrantTalentPoints(context.Background(), 100, 5, "g1"); err != nil {
		t.Fatalf("grant err: %v", err)
	}
	_, err := uc.SetTalents(context.Background(), 100, []data.TalentLevel{{TalentID: 5001, Level: 1}, {TalentID: 5001, Level: 1}})
	if errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("duplicate talent_id should be ErrInvalidArg, got %v", err)
	}
}

func TestSetTalents_SuccessThenResetAndLoadout(t *testing.T) {
	repo := newFakeRepo()
	uc := newUCLoadout(repo)
	if err := uc.UnlockHero(context.Background(), 100, 7, "reward"); err != nil {
		t.Fatalf("unlock err: %v", err)
	}
	if err := uc.SelectHero(context.Background(), 100, 7); err != nil {
		t.Fatalf("select err: %v", err)
	}
	if err := uc.SetEquipment(context.Background(), 100, []data.EquipmentSlot{{Slot: 0, ItemConfigID: 1001}}); err != nil {
		t.Fatalf("set equipment err: %v", err)
	}
	if _, err := uc.GrantTalentPoints(context.Background(), 100, 5, "g1"); err != nil {
		t.Fatalf("grant talent err: %v", err)
	}
	unspent, err := uc.SetTalents(context.Background(), 100, []data.TalentLevel{{TalentID: 5001, Level: 2}})
	if err != nil {
		t.Fatalf("set talents err: %v", err)
	}
	if unspent != 3 {
		t.Fatalf("after spec 2 of 5, talent unspent want 3, got %d", unspent)
	}

	loadout, err := uc.GetLoadout(context.Background(), 100)
	if err != nil {
		t.Fatalf("loadout err: %v", err)
	}
	if len(loadout.GetEquipment()) != 1 {
		t.Fatalf("loadout equipment want 1, got %d", len(loadout.GetEquipment()))
	}
	if len(loadout.GetTalents()) != 1 {
		t.Fatalf("loadout talents want 1, got %d", len(loadout.GetTalents()))
	}
	if loadout.GetUnspentTalentPoints() != 3 {
		t.Fatalf("loadout talent unspent want 3, got %d", loadout.GetUnspentTalentPoints())
	}

	resetUnspent, err := uc.ResetTalents(context.Background(), 100)
	if err != nil {
		t.Fatalf("reset talents err: %v", err)
	}
	if resetUnspent != 5 {
		t.Fatalf("after reset talents all return, want 5, got %d", resetUnspent)
	}
}
