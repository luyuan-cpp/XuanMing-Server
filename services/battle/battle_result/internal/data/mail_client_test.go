// mail_client_test.go — groupInstanceAttachments 纯函数单测:
// 逐件展开的配置 ID 聚合成 instance 形态附件(oneof),保持首见顺序。
package data

import (
	"testing"
)

func TestGroupInstanceAttachments(t *testing.T) {
	atts := groupInstanceAttachments([]uint32{5001, 5002, 5001, 5001})
	if len(atts) != 2 {
		t.Fatalf("want 2 grouped attachments, got %d", len(atts))
	}
	first, second := atts[0].GetInstance(), atts[1].GetInstance()
	if first == nil || second == nil {
		t.Fatalf("attachments must be instance kind, got %+v", atts)
	}
	if first.GetItemConfigId() != 5001 || first.GetCount() != 3 {
		t.Fatalf("first group wrong: %+v", first)
	}
	if second.GetItemConfigId() != 5002 || second.GetCount() != 1 {
		t.Fatalf("second group wrong: %+v", second)
	}
	// 溢出装备绝不能被拼成可堆叠形态(守住装备=唯一实例)。
	if atts[0].GetStack() != nil || atts[1].GetStack() != nil {
		t.Fatal("overflow equipment must not be stack kind")
	}
}

func TestGroupInstanceAttachmentsEmpty(t *testing.T) {
	if got := groupInstanceAttachments(nil); len(got) != 0 {
		t.Fatalf("empty input must yield no attachments, got %d", len(got))
	}
}
