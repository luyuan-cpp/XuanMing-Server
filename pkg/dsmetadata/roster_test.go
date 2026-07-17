package dsmetadata

import (
	"strings"
	"testing"
)

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

func TestCanonicalCombatFactions(t *testing.T) {
	players, annotation, err := CanonicalCombatFactions(
		[]uint64{99, 7, 42},
		map[uint64]uint32{7: 3, 42: 3, 99: 9},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := annotation, "7=3,42=3,99=9"; got != want {
		t.Fatalf("annotation=%q want=%q", got, want)
	}
	if len(players) != 3 || players[0] != 7 || players[1] != 42 || players[2] != 99 {
		t.Fatalf("canonical players=%v", players)
	}
}

func TestCanonicalCombatFactionsRejectsNonExactOrOutOfRange(t *testing.T) {
	tests := []struct {
		name     string
		players  []uint64
		factions map[uint64]uint32
		want     string
	}{
		{name: "missing", players: []uint64{1, 2}, factions: map[uint64]uint32{1: 0}, want: "exactly cover"},
		{name: "extra", players: []uint64{1}, factions: map[uint64]uint32{1: 0, 2: 0}, want: "exactly cover"},
		{name: "wrong member", players: []uint64{1, 2}, factions: map[uint64]uint32{1: 0, 3: 1}, want: "missing"},
		{name: "overflow", players: []uint64{1}, factions: map[uint64]uint32{1: MaxCombatFactionID + 1}, want: "range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := CanonicalCombatFactions(tt.players, tt.factions)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v want substring %q", err, tt.want)
			}
		})
	}
}
