package data

import (
	"reflect"
	"testing"
)

func TestAuthoritativeRecoveryPlayerIDsAreCopiedFromCredentialSnapshot(t *testing.T) {
	rec := &TerminalReleaseRecord{PlayerIDs: []uint64{2, 1}}
	got := authoritativeRecoveryPlayerIDs(rec)
	if !reflect.DeepEqual(got, []uint64{2, 1}) {
		t.Fatalf("got=%v", got)
	}
	got[0] = 99
	if rec.PlayerIDs[0] != 2 {
		t.Fatalf("caller mutated credential snapshot: %+v", rec.PlayerIDs)
	}
	if authoritativeRecoveryPlayerIDs(nil) != nil || authoritativeRecoveryPlayerIDs(&TerminalReleaseRecord{}) != nil {
		t.Fatal("missing authority must not synthesize a roster")
	}
}
