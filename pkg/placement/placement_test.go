package placement

import "testing"

func TestParseModeRejectsTypo(t *testing.T) {
	if _, err := ParseMode("enfore"); err == nil {
		t.Fatal("misspelled enforce must not fall back to off")
	}
}

func TestBindingAllOrNothing(t *testing.T) {
	valid := Binding{Version: 7, OperationID: "9849ab5b-2ecf-4fc3-983d-2d8df53cc009", SourceMatchID: 9}
	if !valid.Complete() || valid.ValidateOptional() != nil {
		t.Fatalf("complete binding rejected: %+v", valid)
	}
	if err := (Binding{Version: 7}).ValidateOptional(); err == nil {
		t.Fatal("partial binding accepted")
	}
	bootstrap := Binding{Version: 1, OperationID: "9849ab5b-2ecf-4fc3-983d-2d8df53cc009"}
	if !bootstrap.Complete() {
		t.Fatal("initial HUB binding must allow source_match_id=0")
	}
}
