// experience_repo_test.go — AdvanceExperience 等级结算纯函数单测(实时成长)。
package data

import "testing"

// curve15 是需求口径的 14 项曲线(Lv1→Lv15),数值仅测试用。
func curve15() []uint64 {
	return []uint64{100, 200, 300, 400, 500, 600, 700, 800, 900, 1000, 1100, 1200, 1300, 1400}
}

func TestAdvanceExperience_SingleLevel(t *testing.T) {
	level, exp, gained := AdvanceExperience(1, 50, 60, curve15())
	if level != 2 || exp != 10 || gained != 1 {
		t.Fatalf("got level=%d exp=%d gained=%d, want 2/10/1", level, exp, gained)
	}
}

func TestAdvanceExperience_NoLevelUp(t *testing.T) {
	level, exp, gained := AdvanceExperience(3, 10, 20, curve15())
	if level != 3 || exp != 30 || gained != 0 {
		t.Fatalf("got level=%d exp=%d gained=%d, want 3/30/0", level, exp, gained)
	}
}

func TestAdvanceExperience_ExactBoundary(t *testing.T) {
	// 恰好到门槛 → 升级且级内经验归 0。
	level, exp, gained := AdvanceExperience(1, 0, 100, curve15())
	if level != 2 || exp != 0 || gained != 1 {
		t.Fatalf("got level=%d exp=%d gained=%d, want 2/0/1", level, exp, gained)
	}
}

func TestAdvanceExperience_MultiLevel(t *testing.T) {
	// 一次大额经验连升多级:100+200+300=600 → Lv1 加 650 → Lv4 余 50。
	level, exp, gained := AdvanceExperience(1, 0, 650, curve15())
	if level != 4 || exp != 50 || gained != 3 {
		t.Fatalf("got level=%d exp=%d gained=%d, want 4/50/3", level, exp, gained)
	}
}

func TestAdvanceExperience_ReachMaxClampsExp(t *testing.T) {
	// 升到满级瞬间级内经验清 0(满级不累加,MAX 显示)。
	level, exp, gained := AdvanceExperience(14, 0, 999_999, curve15())
	if level != 15 || exp != 0 || gained != 1 {
		t.Fatalf("got level=%d exp=%d gained=%d, want 15/0/1", level, exp, gained)
	}
}

func TestAdvanceExperience_AtMaxNoop(t *testing.T) {
	level, exp, gained := AdvanceExperience(15, 0, 500, curve15())
	if level != 15 || exp != 0 || gained != 0 {
		t.Fatalf("got level=%d exp=%d gained=%d, want 15/0/0", level, exp, gained)
	}
}

func TestAdvanceExperience_OverMaxDefensiveClamp(t *testing.T) {
	// 等级异常超上限(脏数据防御)→ 按满级处理。
	level, exp, gained := AdvanceExperience(99, 123, 500, curve15())
	if level != 15 || exp != 0 || gained != 0 {
		t.Fatalf("got level=%d exp=%d gained=%d, want 15/0/0", level, exp, gained)
	}
}

func TestAdvanceExperience_ZeroLevelDefensive(t *testing.T) {
	// 等级 <1(脏数据防御)→ 按 Lv1 起算。
	level, exp, gained := AdvanceExperience(0, 0, 100, curve15())
	if level != 2 || exp != 0 || gained != 1 {
		t.Fatalf("got level=%d exp=%d gained=%d, want 2/0/1", level, exp, gained)
	}
}

func TestAdvanceExperience_ZeroCurveEntryStops(t *testing.T) {
	// 非法曲线项 0(防御)→ 该级不可升,经验累在级内。
	curve := []uint64{100, 0, 300}
	level, exp, gained := AdvanceExperience(1, 0, 10_000, curve)
	if level != 2 || exp != 9_900 || gained != 1 {
		t.Fatalf("got level=%d exp=%d gained=%d, want 2/9900/1", level, exp, gained)
	}
}

func TestAdvanceExperience_WrapGuard(t *testing.T) {
	// uint64 回绕防御:按可升满处理,最终满级且级内 0。
	level, exp, gained := AdvanceExperience(1, ^uint64(0)-10, 100, curve15())
	if level != 15 || exp != 0 || gained != 14 {
		t.Fatalf("got level=%d exp=%d gained=%d, want 15/0/14", level, exp, gained)
	}
}
