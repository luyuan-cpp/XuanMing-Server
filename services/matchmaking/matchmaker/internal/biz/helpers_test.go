package biz

import (
	"testing"

	matchv1 "github.com/luyuancpp/pandora/proto/gen/go/pandora/match/v1"
)

func TestCombatFactionsFromMembersSupportsMultipleTeamsPerFactionAndMultipleFactions(t *testing.T) {
	members := []*matchv1.MatchMemberStorageRecord{
		{PlayerId: 101, TeamId: 7001, Side: 4},
		{PlayerId: 102, TeamId: 7002, Side: 4},
		{PlayerId: 201, TeamId: 7003, Side: 9},
	}
	factions, err := combatFactionsFromMembers(members)
	if err != nil {
		t.Fatal(err)
	}
	if factions[101] != 4 || factions[102] != 4 || factions[201] != 9 {
		t.Fatalf("combat factions=%v", factions)
	}
}

func TestCombatFactionsFromMembersRejectsNegativeSide(t *testing.T) {
	_, err := combatFactionsFromMembers([]*matchv1.MatchMemberStorageRecord{{PlayerId: 101, Side: -1}})
	if err == nil {
		t.Fatal("negative match side must be rejected")
	}
}
