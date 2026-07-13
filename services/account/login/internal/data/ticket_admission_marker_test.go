package data

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/luyuancpp/pandora/pkg/errcode"
)

func newAdmissionMarkerRepo(t *testing.T) (*miniredis.Miniredis, *RedisTicketJTIRepo) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, NewRedisTicketJTIRepo(rdb)
}

func TestAdmissionMarkerResponseUnknownAndRotationIdempotent(t *testing.T) {
	mr, repo := newAdmissionMarkerRepo(t)
	const jti = "player-ticket-jti"
	attempt := strings.Repeat("a", 64)
	firstCredential := strings.Repeat("b", 64)
	rotatedCredential := strings.Repeat("c", 64)

	status, err := repo.PeekAdmission(context.Background(), jti, attempt)
	if err != nil || status != AdmissionMarkerMissing {
		t.Fatalf("initial peek status=%v err=%v", status, err)
	}
	status, err = repo.MarkUsedByAdmission(context.Background(), jti, attempt, firstCredential, 5*time.Minute)
	if err != nil || status != AdmissionMarkerCreated {
		t.Fatalf("first mark status=%v err=%v", status, err)
	}
	original, _ := mr.Get(ticketKey(jti))

	// 模拟 SET 已应用但响应丢失；重试时 active credential 已轮换。same attempt 仍可确认，
	// 且首次 accepted_credential_hash 永不被新 hash 覆盖。
	status, err = repo.MarkUsedByAdmission(context.Background(), jti, attempt, rotatedCredential, 5*time.Minute)
	if err != nil || status != AdmissionMarkerExisting {
		t.Fatalf("rotation retry status=%v err=%v", status, err)
	}
	after, _ := mr.Get(ticketKey(jti))
	if after != original || !strings.Contains(after, firstCredential) || strings.Contains(after, rotatedCredential) {
		t.Fatalf("existing marker was overwritten: before=%q after=%q", original, after)
	}
	if status, err = repo.PeekAdmission(context.Background(), jti, attempt); err != nil || status != AdmissionMarkerExisting {
		t.Fatalf("same owner peek status=%v err=%v", status, err)
	}
}

func TestAdmissionMarkerDifferentOwnerAndLegacyRejected(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prevalue string
	}{
		{"different-attempt", admissionMarkerVersion + "|" + strings.Repeat("d", 64) + "|" + strings.Repeat("b", 64) + "|1|9999999999999"},
		{"legacy-one", "1"},
		{"malformed", admissionMarkerVersion + "|bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mr, repo := newAdmissionMarkerRepo(t)
			const jti = "player-ticket-jti"
			mr.Set(ticketKey(jti), tc.prevalue)
			attempt := strings.Repeat("a", 64)
			if status, err := repo.PeekAdmission(context.Background(), jti, attempt); err != nil || status != AdmissionMarkerConflict {
				t.Fatalf("peek status=%v err=%v", status, err)
			}
			if status, err := repo.MarkUsedByAdmission(context.Background(), jti, attempt, strings.Repeat("b", 64), time.Minute); status != AdmissionMarkerConflict || errcode.As(err) != errcode.ErrLoginTicketReplayed {
				t.Fatalf("mark status=%v code=%v err=%v", status, errcode.As(err), err)
			}
			got, _ := mr.Get(ticketKey(jti))
			if got != tc.prevalue {
				t.Fatal("conflicting marker was mutated")
			}
		})
	}
}

func TestAdmissionMarkerSameAttemptWindowExpiresWithoutShorteningJTIKey(t *testing.T) {
	mr, repo := newAdmissionMarkerRepo(t)
	baseTime := time.Unix(1_800_000_000, 0)
	mr.SetTime(baseTime)
	repo.admissionReplayWindow = 30 * time.Second
	attempt := strings.Repeat("a", 64)
	credential := strings.Repeat("b", 64)
	const ttl = 5 * time.Minute
	if status, err := repo.MarkUsedByAdmission(context.Background(), "window-jti", attempt, credential, ttl); err != nil || status != AdmissionMarkerCreated {
		t.Fatalf("create status=%v err=%v", status, err)
	}
	before := mr.TTL(ticketKey("window-jti"))
	mr.SetTime(baseTime.Add(31 * time.Second))
	mr.FastForward(31 * time.Second)
	if status, err := repo.PeekAdmission(context.Background(), "window-jti", attempt); err != nil || status != AdmissionMarkerConflict {
		t.Fatalf("expired peek status=%v err=%v", status, err)
	}
	if status, err := repo.MarkUsedByAdmission(context.Background(), "window-jti", attempt, credential, ttl); status != AdmissionMarkerConflict || errcode.As(err) != errcode.ErrLoginTicketReplayed {
		t.Fatalf("expired mark status=%v code=%v err=%v", status, errcode.As(err), err)
	}
	if !mr.Exists(ticketKey("window-jti")) {
		t.Fatal("idempotency window expiry must not delete the full-TTL jti key")
	}
	after := mr.TTL(ticketKey("window-jti"))
	if after >= before || after > ttl-30*time.Second {
		t.Fatalf("same-owner rejection refreshed TTL: before=%s after=%s", before, after)
	}
}

func TestAdmissionMarkerConcurrentSameAttempt(t *testing.T) {
	_, repo := newAdmissionMarkerRepo(t)
	const workers = 32
	var created, existing, failed atomic.Int32
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			status, err := repo.MarkUsedByAdmission(context.Background(), "concurrent-jti",
				strings.Repeat("a", 64), strings.Repeat("b", 64), time.Minute)
			if err != nil {
				failed.Add(1)
				return
			}
			switch status {
			case AdmissionMarkerCreated:
				created.Add(1)
			case AdmissionMarkerExisting:
				existing.Add(1)
			default:
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 || existing.Load() != workers-1 || failed.Load() != 0 {
		t.Fatalf("created=%d existing=%d failed=%d", created.Load(), existing.Load(), failed.Load())
	}
}
