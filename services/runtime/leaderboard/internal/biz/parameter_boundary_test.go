package biz

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/services/runtime/leaderboard/internal/data"
)

type leaderboardArgumentSpy struct {
	data.BoardStore
	rangeOffset  int64
	rangeLimit   int
	aroundRadius int
	submitMode   int32
	submitOpt    data.Options
}

func (s *leaderboardArgumentSpy) Range(ctx context.Context, board data.BoardKey, offset int64, limit int, ascending bool) ([]data.Entry, error) {
	s.rangeOffset = offset
	s.rangeLimit = limit
	return s.BoardStore.Range(ctx, board, offset, limit, ascending)
}

func (s *leaderboardArgumentSpy) Around(ctx context.Context, board data.BoardKey, entityID uint64, radius int, ascending bool) ([]data.Entry, bool, error) {
	s.aroundRadius = radius
	return s.BoardStore.Around(ctx, board, entityID, radius, ascending)
}

func (s *leaderboardArgumentSpy) Submit(ctx context.Context, board data.BoardKey, entityID uint64, score int64, mode int32, opt data.Options, tsMs int64) (int64, int64, error) {
	s.submitMode = mode
	s.submitOpt = opt
	return s.BoardStore.Submit(ctx, board, entityID, score, mode, opt, tsMs)
}

// TestLeaderboardParametersAreNormalizedBeforeStorage 固化读窗口和默认上报模式的边界，
// 防止负 offset、无限 limit/radius 或未指定 mode 直接进入 Redis Lua/范围命令。
func TestLeaderboardParametersAreNormalizedBeforeStorage(t *testing.T) {
	uc, _, _, _ := newTestUsecase(t)
	spy := &leaderboardArgumentSpy{BoardStore: uc.board}
	uc.board = spy
	uc.cfg.DefaultListLimit = 2
	uc.cfg.MaxListLimit = 3
	uc.cfg.DefaultAroundRadius = 1
	uc.cfg.DefaultEstimateBucketWidth = 25
	ctx := context.Background()

	if _, _, err := uc.GetRange(ctx, globalBoard, -99, 0); err != nil {
		t.Fatalf("GetRange defaults: %v", err)
	}
	if spy.rangeOffset != 0 || spy.rangeLimit != 2 {
		t.Fatalf("default range args=%d/%d want=0/2", spy.rangeOffset, spy.rangeLimit)
	}

	if _, _, err := uc.GetRange(ctx, globalBoard, 4, 10_000); err != nil {
		t.Fatalf("GetRange clamp: %v", err)
	}
	if spy.rangeOffset != 4 || spy.rangeLimit != 3 {
		t.Fatalf("clamped range args=%d/%d want=4/3", spy.rangeOffset, spy.rangeLimit)
	}

	if _, _, err := uc.GetAround(ctx, globalBoard, 7, 0); err != nil {
		t.Fatalf("GetAround default: %v", err)
	}
	if spy.aroundRadius != 1 {
		t.Fatalf("default radius=%d want=1", spy.aroundRadius)
	}
	if _, _, err := uc.GetAround(ctx, globalBoard, 7, 10_000); err != nil {
		t.Fatalf("GetAround clamp: %v", err)
	}
	if spy.aroundRadius != 3 {
		t.Fatalf("clamped radius=%d want=3", spy.aroundRadius)
	}

	if _, _, err := uc.SubmitScore(ctx, globalBoard, 7, 100, 0, data.Options{}); err != nil {
		t.Fatalf("SubmitScore defaults: %v", err)
	}
	if spy.submitMode != data.ModeSetIfHigher {
		t.Fatalf("unspecified mode normalized to=%d want=%d", spy.submitMode, data.ModeSetIfHigher)
	}
	if spy.submitOpt.EstimateBucketWidth != 25 {
		t.Fatalf("default estimate bucket=%d want=25", spy.submitOpt.EstimateBucketWidth)
	}
}
