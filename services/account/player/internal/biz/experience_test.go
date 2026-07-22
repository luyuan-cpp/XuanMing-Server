// experience_test.go — AddExperience 业务逻辑 + 推送出箱发布器单测(实时成长)。
package biz

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/pkg/errcode"
	playerv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/player/v1"
	"github.com/luyuancpp/pandora/services/account/player/internal/conf"
	"github.com/luyuancpp/pandora/services/account/player/internal/data"
)

func expUsecase(t *testing.T) (*PlayerUsecase, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	uc := NewPlayerUsecase(repo, conf.PlayerConf{ExperienceEnabled: true})
	uc.expLevels = staticExperienceLevels{curve: []uint64{100, 200, 300}} // MaxLevel = 4
	return uc, repo
}

type staticExperienceLevels struct{ curve []uint64 }

func (s staticExperienceLevels) ExperienceCurve() []uint64 {
	return append([]uint64(nil), s.curve...)
}

func TestAddExperience_Validation(t *testing.T) {
	uc, _ := expUsecase(t)
	ctx := context.Background()
	if _, _, err := uc.AddExperience(ctx, 0, 10, "quest", "k1"); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("player_id=0 want ErrInvalidArg, got %v", err)
	}
	if _, _, err := uc.AddExperience(ctx, 1, 0, "quest", "k1"); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("delta=0 want ErrInvalidArg, got %v", err)
	}
	if _, _, err := uc.AddExperience(ctx, 1, 10, "quest", ""); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("empty key want ErrInvalidArg, got %v", err)
	}
	if _, _, err := uc.AddExperience(ctx, 1, 1_000_001, "quest", "k1"); errcode.As(err) != errcode.ErrInvalidArg {
		t.Fatalf("over max_exp_per_grant want ErrInvalidArg, got %v", err)
	}
}

func TestAddExperience_DisabledWithoutLevelTable(t *testing.T) {
	uc := NewPlayerUsecase(newFakeRepo(), conf.PlayerConf{ExperienceEnabled: true})
	_, _, err := uc.AddExperience(context.Background(), 1, 10, "quest", "k1")
	if errcode.As(err) != errcode.ErrPlayerFeatureDisabled {
		t.Fatalf("missing level table want ErrPlayerFeatureDisabled, got %v", err)
	}
}

func TestAddExperience_DisabledByFeatureGate(t *testing.T) {
	uc := NewPlayerUsecase(newFakeRepo(), conf.PlayerConf{})
	uc.expLevels = staticExperienceLevels{curve: []uint64{100}}
	_, _, err := uc.AddExperience(context.Background(), 1, 10, "quest", "k1")
	if errcode.As(err) != errcode.ErrPlayerFeatureDisabled {
		t.Fatalf("experience_enabled=false want ErrPlayerFeatureDisabled, got %v", err)
	}
}

func TestAddExperience_MultiLevelAndOutbox(t *testing.T) {
	uc, repo := expUsecase(t)
	ctx := context.Background()
	// 350 = 100(Lv1→2) + 200(Lv2→3) + 余 50 → Lv3。
	st, already, err := uc.AddExperience(ctx, 7, 350, "monster_kill", "progress:1:5:7:exp")
	if err != nil || already {
		t.Fatalf("unexpected err=%v already=%v", err, already)
	}
	if st.Level != 3 || st.ExpInLevel != 50 || st.LevelsGained != 2 || st.IsMaxLevel {
		t.Fatalf("got %+v, want level=3 exp=50 gained=2", st)
	}
	// 出箱事件携带最终快照。
	if len(repo.pushOutbox) != 1 {
		t.Fatalf("push outbox rows = %d, want 1", len(repo.pushOutbox))
	}
	rec := repo.pushOutbox[0]
	if rec.EventType != uint32(playerv1.PlayerPushEventType_PLAYER_PUSH_EVENT_TYPE_EXPERIENCE) {
		t.Fatalf("event_type = %d, want EXPERIENCE", rec.EventType)
	}
	evt := &playerv1.PlayerExperienceEvent{}
	if err := proto.Unmarshal(rec.Payload, evt); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if evt.GetLevel() != 3 || evt.GetExpInLevel() != 50 || evt.GetLevelsGained() != 2 {
		t.Fatalf("event %+v, want level=3 exp=50 gained=2", evt)
	}
}

func TestAddExperience_Idempotent(t *testing.T) {
	uc, repo := expUsecase(t)
	ctx := context.Background()
	if _, _, err := uc.AddExperience(ctx, 7, 150, "quest", "quest:7:1001"); err != nil {
		t.Fatalf("first grant: %v", err)
	}
	st, already, err := uc.AddExperience(ctx, 7, 150, "quest", "quest:7:1001")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !already {
		t.Fatal("replay want already=true")
	}
	if st.Level != 2 || st.ExpInLevel != 50 {
		t.Fatalf("replay snapshot %+v, want level=2 exp=50", st)
	}
	if st.LevelsGained != 0 {
		t.Fatalf("replay levels_gained = %d, want 0", st.LevelsGained)
	}
	if len(repo.pushOutbox) != 1 {
		t.Fatalf("replay must not re-enqueue outbox, rows = %d", len(repo.pushOutbox))
	}
}

func TestAddExperience_MaxLevelNoop(t *testing.T) {
	uc, repo := expUsecase(t)
	ctx := context.Background()
	// 600 = 恰好升满(100+200+300)→ Lv4 MAX。
	st, _, err := uc.AddExperience(ctx, 7, 600, "quest", "k-full")
	if err != nil || !st.IsMaxLevel || st.Level != 4 || st.ExpInLevel != 0 {
		t.Fatalf("fill to max got %+v err=%v", st, err)
	}
	rows := len(repo.pushOutbox)
	// 满级后再加 → no-op:快照不变、无新出箱,但消费幂等键落 no-op 收据(首次 already=false)。
	st2, already, err := uc.AddExperience(ctx, 7, 100, "quest", "k-after-max")
	if err != nil || already {
		t.Fatalf("max noop err=%v already=%v", err, already)
	}
	if !st2.IsMaxLevel || st2.Level != 4 || st2.ExpInLevel != 0 || st2.LevelsGained != 0 {
		t.Fatalf("max noop snapshot %+v", st2)
	}
	if len(repo.pushOutbox) != rows {
		t.Fatalf("max noop must not enqueue outbox: %d → %d", rows, len(repo.pushOutbox))
	}
	// 同键重放 → 幂等命中收据,按 proto 契约 already=true(审计 P2:
	// "true = 幂等命中,本次未重复入账",满级 no-op 重放同样是幂等命中)。
	st3, already3, err := uc.AddExperience(ctx, 7, 100, "quest", "k-after-max")
	if err != nil || !already3 {
		t.Fatalf("max noop replay want already=true, err=%v already=%v", err, already3)
	}
	if !st3.IsMaxLevel || st3.Level != 4 || st3.ExpInLevel != 0 {
		t.Fatalf("max noop replay snapshot %+v", st3)
	}
	if len(repo.pushOutbox) != rows {
		t.Fatalf("max noop replay must not enqueue outbox: %d → %d", rows, len(repo.pushOutbox))
	}
}

func TestDecorateExperience(t *testing.T) {
	uc, _ := expUsecase(t)
	if exp, isMax := uc.DecorateExperience(4, 123); exp != 0 || !isMax {
		t.Fatalf("max level decorate got exp=%d isMax=%v, want 0/true", exp, isMax)
	}
	if exp, isMax := uc.DecorateExperience(2, 55); exp != 55 || isMax {
		t.Fatalf("mid level decorate got exp=%d isMax=%v, want 55/false", exp, isMax)
	}
	// 曲线未配置(功能关闭)→ 不标满级。
	off := NewPlayerUsecase(newFakeRepo(), conf.PlayerConf{})
	if exp, isMax := off.DecorateExperience(99, 7); exp != 7 || isMax {
		t.Fatalf("disabled decorate got exp=%d isMax=%v, want 7/false", exp, isMax)
	}
}

// capturePusher 收集投递;failFirst 模拟 kafka 首条失败(出箱行必须保留)。
type capturePusher struct {
	sent      []data.PushOutboxRecord
	failFirst bool
}

func (c *capturePusher) PushPlayerEvent(_ context.Context, playerID uint64, eventType uint32, payload []byte) error {
	if c.failFirst {
		c.failFirst = false
		return errors.New("kafka down")
	}
	c.sent = append(c.sent, data.PushOutboxRecord{PlayerID: playerID, EventType: eventType, Payload: payload})
	return nil
}

func TestPublishPushOutboxBatch(t *testing.T) {
	uc, repo := expUsecase(t)
	ctx := context.Background()
	if _, _, err := uc.AddExperience(ctx, 7, 50, "quest", "k1"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, _, err := uc.AddExperience(ctx, 8, 60, "quest", "k2"); err != nil {
		t.Fatalf("grant: %v", err)
	}

	// 首条投递失败 → 本轮中断,出箱行全保留(FIFO 保序,at-least-once)。
	p := &capturePusher{failFirst: true}
	uc.SetExperiencePusher(p)
	if n, err := uc.publishPushOutboxBatch(ctx); err == nil || n != 0 {
		t.Fatalf("first round want failure with 0 published, got n=%d err=%v", n, err)
	}
	if len(repo.pushOutbox) != 2 {
		t.Fatalf("failed round must keep outbox rows, got %d", len(repo.pushOutbox))
	}

	// 第二轮全部投出并删行。
	if n, err := uc.publishPushOutboxBatch(ctx); err != nil || n != 2 {
		t.Fatalf("second round want 2 published, got n=%d err=%v", n, err)
	}
	if len(repo.pushOutbox) != 0 {
		t.Fatalf("published rows must be deleted, got %d", len(repo.pushOutbox))
	}
	if len(p.sent) != 2 || p.sent[0].PlayerID != 7 || p.sent[1].PlayerID != 8 {
		t.Fatalf("sent order/content wrong: %+v", p.sent)
	}
}
