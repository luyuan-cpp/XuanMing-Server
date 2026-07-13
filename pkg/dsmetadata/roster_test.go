package dsmetadata

import "testing"

func TestCanonicalRoster(t *testing.T) {
	ids, annotation, err := CanonicalRoster([]uint64{9, 2, 9, 1})
	if err != nil || annotation != "1,2,9" || len(ids) != 3 || ids[0] != 1 || ids[2] != 9 {
		t.Fatalf("ids=%v annotation=%q err=%v", ids, annotation, err)
	}
	if _, _, err := CanonicalRoster(nil); err == nil {
		t.Fatal("empty roster must fail")
	}
	if _, _, err := CanonicalRoster([]uint64{0, 1}); err == nil {
		t.Fatal("zero player must fail")
	}
	tooMany := make([]uint64, MaxBattleRosterPlayers+1)
	for i := range tooMany {
		tooMany[i] = uint64(i + 1)
	}
	if _, _, err := CanonicalRoster(tooMany); err == nil {
		t.Fatal("oversized roster must fail")
	}
}
