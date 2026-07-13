package dsauthrecord

import "testing"

func TestBattleResultReceiptRoundTripAndIdentity(t *testing.T) {
	r := NewBattleResultReceipt(9, "alloc", "battle-9", "uid", 3, 7, "jti", 2000, "kid", "hash", 2, 1000)
	raw, err := MarshalBattleResultReceipt(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalBattleResultReceipt(raw)
	if err != nil || !got.Valid(1500) || !got.SameCredential(r) {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	got.JTI = "other"
	if got.SameCredential(r) {
		t.Fatal("different credential accepted")
	}
	// receipt 是已发生事件的证明；签发它的 token 后续过期不应抹掉该事实。
	if !r.Valid(3000) {
		t.Fatal("receipt became invalid only because recorded credential expired")
	}
}
