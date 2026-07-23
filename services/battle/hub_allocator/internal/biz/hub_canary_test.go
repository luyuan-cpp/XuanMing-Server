package biz

import (
	"context"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/pkg/releasetrack"
)

type staticTrackFleet struct{ candidates []ShardCandidate }

func (f *staticTrackFleet) ListShards(context.Context, string) ([]ShardCandidate, error) {
	return append([]ShardCandidate(nil), f.candidates...), nil
}

func trackCandidate(pod, track string, shardID uint32) ShardCandidate {
	return ShardCandidate{
		PodName: pod, Addr: "127.0.0.1:7777", Region: "global", ShardID: shardID,
		Capacity: 10, ReleaseTrack: track, TokenReady: true,
	}
}

func newTrackUsecase(t *testing.T, percent uint32, fleet *staticTrackFleet) (*HubUsecase, *fakeRepo) {
	t.Helper()
	cfg := testConf()
	repo := newFakeRepo()
	uc := NewHubUsecase(repo, fleet, &fakeSigner{}, cfg)
	policy, err := releasetrack.New(percent, "hub-canary-test")
	if err != nil {
		t.Fatal(err)
	}
	uc.SetReleaseTrackPolicy(policy)
	return uc, repo
}

func TestAssignHubCanaryPersistsActualTrackAndFallback(t *testing.T) {
	t.Run("canary cohort selects and persists actual canary", func(t *testing.T) {
		fleet := &staticTrackFleet{candidates: []ShardCandidate{
			trackCandidate("hub-stable", releasetrack.Stable, 1),
			trackCandidate("hub-canary", releasetrack.Canary, 2),
		}}
		uc, repo := newTrackUsecase(t, 100, fleet)
		if _, err := uc.AssignHub(context.Background(), 1001, "global", 0, 0, 0, ""); err != nil {
			t.Fatal(err)
		}
		assignment, found, _ := repo.GetAssignment(context.Background(), 1001)
		if !found || assignment.GetHubPodName() != "hub-canary" || assignment.GetReleaseTrack() != releasetrack.Canary {
			t.Fatalf("assignment=%+v", assignment)
		}
	})

	t.Run("canary no capacity falls back stable and remains sticky", func(t *testing.T) {
		fleet := &staticTrackFleet{candidates: []ShardCandidate{
			trackCandidate("hub-stable", releasetrack.Stable, 1),
		}}
		uc, repo := newTrackUsecase(t, 100, fleet)
		if _, err := uc.AssignHub(context.Background(), 1002, "global", 0, 0, 0, ""); err != nil {
			t.Fatal(err)
		}
		assignment, _, _ := repo.GetAssignment(context.Background(), 1002)
		if assignment.GetReleaseTrack() != releasetrack.Stable {
			t.Fatalf("fallback assignment=%+v", assignment)
		}
		// 后续 canary 出现也不能重算 cohort 把已有 stable assignment 搬轨。
		fleet.candidates = append(fleet.candidates, trackCandidate("hub-canary", releasetrack.Canary, 2))
		if _, err := uc.AssignHub(context.Background(), 1002, "global", 0, 0, 0, ""); err != nil {
			t.Fatal(err)
		}
		again, _, _ := repo.GetAssignment(context.Background(), 1002)
		if again.GetHubPodName() != "hub-stable" || again.GetReleaseTrack() != releasetrack.Stable {
			t.Fatalf("sticky assignment=%+v", again)
		}
	})
}

func TestAssignHubStableNeverFallsForwardToCanary(t *testing.T) {
	fleet := &staticTrackFleet{candidates: []ShardCandidate{
		trackCandidate("hub-canary", releasetrack.Canary, 2),
	}}
	uc, _ := newTrackUsecase(t, 0, fleet)
	_, err := uc.AssignHub(context.Background(), 1003, "global", 0, 0, 0, "")
	if errcode.As(err) != errcode.ErrHubNoAvailable {
		t.Fatalf("err=%v", err)
	}
}

var _ HubFleetProvider = (*staticTrackFleet)(nil)
