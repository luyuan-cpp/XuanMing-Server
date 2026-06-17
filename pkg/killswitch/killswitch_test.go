package killswitch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManager_Disabled_ExactMethod(t *testing.T) {
	m := NewManager()
	m.Replace(map[string]string{
		"pandora.login.v1.LoginService/Login": "fixing bug",
	})

	if ok, reason := m.Disabled("/pandora.login.v1.LoginService/Login"); !ok || reason != "fixing bug" {
		t.Fatalf("exact method should be disabled with reason, got ok=%v reason=%q", ok, reason)
	}
	// 同服务其它 method 不受影响
	if ok, _ := m.Disabled("/pandora.login.v1.LoginService/Logout"); ok {
		t.Fatalf("Logout should not be disabled")
	}
}

func TestManager_Disabled_ServiceWildcard(t *testing.T) {
	m := NewManager()
	m.Replace(map[string]string{
		"pandora.match.v1.MatchService/*": "match maintenance",
	})

	for _, op := range []string{
		"/pandora.match.v1.MatchService/StartMatch",
		"/pandora.match.v1.MatchService/CancelMatch",
	} {
		if ok, reason := m.Disabled(op); !ok || reason != "match maintenance" {
			t.Fatalf("%s should be disabled by service wildcard, got ok=%v reason=%q", op, ok, reason)
		}
	}
	// 别的服务不受影响
	if ok, _ := m.Disabled("/pandora.login.v1.LoginService/Login"); ok {
		t.Fatalf("login should not be disabled by match wildcard")
	}
}

func TestManager_Disabled_GlobalStar(t *testing.T) {
	m := NewManager()
	m.Replace(map[string]string{"*": ""})

	if ok, reason := m.Disabled("/pandora.anything.v1.AnyService/Any"); !ok || reason == "" {
		t.Fatalf("global star should disable everything with fallback reason, got ok=%v reason=%q", ok, reason)
	}
}

func TestManager_Disabled_Feature(t *testing.T) {
	RegisterFeature("trade",
		"pandora.trade.v1.TradeService/CreateOrder",
		"pandora.trade.v1.TradeService/ConfirmOrder",
	)
	m := NewManager()
	m.Replace(map[string]string{"feature/trade": "trade frozen"})

	if ok, reason := m.Disabled("/pandora.trade.v1.TradeService/CreateOrder"); !ok || reason != "trade frozen" {
		t.Fatalf("feature member CreateOrder should be disabled, got ok=%v reason=%q", ok, reason)
	}
	if ok, reason := m.Disabled("/pandora.trade.v1.TradeService/ConfirmOrder"); !ok || reason != "trade frozen" {
		t.Fatalf("feature member ConfirmOrder should be disabled, got ok=%v reason=%q", ok, reason)
	}
	// 不在 feature 组里的 RPC 放行
	if ok, _ := m.Disabled("/pandora.trade.v1.TradeService/GetOrder"); ok {
		t.Fatalf("GetOrder not in feature group, should pass")
	}
}

func TestManager_Disabled_EmptyAndNil(t *testing.T) {
	m := NewManager()
	if ok, _ := m.Disabled("/pandora.login.v1.LoginService/Login"); ok {
		t.Fatalf("empty manager should pass all")
	}
	var nilM *Manager
	if ok, _ := nilM.Disabled("/x/Y"); ok {
		t.Fatalf("nil manager should fail-open (pass)")
	}
}

func TestDefault_FailOpenWhenNil(t *testing.T) {
	SetDefault(nil)
	if ok, _ := Disabled("/pandora.login.v1.LoginService/Login"); ok {
		t.Fatalf("nil default should fail-open (pass)")
	}
}

func TestParseRules_NormalizesLeadingSlash(t *testing.T) {
	data := []byte(`
rules:
  "/pandora.login.v1.LoginService/Login": "bug"
  "pandora.match.v1.MatchService/*": "maint"
  "feature/trade": ""
`)
	rules, err := parseRules(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := rules["pandora.login.v1.LoginService/Login"]; !ok {
		t.Fatalf("leading slash should be stripped; got %v", rules)
	}
	if _, ok := rules["pandora.match.v1.MatchService/*"]; !ok {
		t.Fatalf("wildcard key missing; got %v", rules)
	}
	if _, ok := rules["feature/trade"]; !ok {
		t.Fatalf("feature key missing; got %v", rules)
	}
}

func TestFileSource_HotReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "killswitch.yaml")

	// 初始:空文件
	if err := os.WriteFile(path, []byte("rules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src, err := newFileSource(Config{Source: "file", FilePath: path})
	if err != nil {
		t.Fatalf("newFileSource: %v", err)
	}
	defer src.Close()

	mgr := src.Manager()
	if ok, _ := mgr.Disabled("/pandora.login.v1.LoginService/Login"); ok {
		t.Fatalf("initially should pass")
	}

	// 写入关停规则
	content := `
rules:
  "pandora.login.v1.LoginService/Login": "hotfix"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if !waitFor(t, 2*time.Second, func() bool {
		ok, reason := mgr.Disabled("/pandora.login.v1.LoginService/Login")
		return ok && reason == "hotfix"
	}) {
		t.Fatalf("login should be disabled after file update")
	}

	// 清空规则 → 恢复放行
	if err := os.WriteFile(path, []byte("rules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !waitFor(t, 2*time.Second, func() bool {
		ok, _ := mgr.Disabled("/pandora.login.v1.LoginService/Login")
		return !ok
	}) {
		t.Fatalf("login should be re-enabled after clearing rules")
	}
}

func TestFileSource_MissingFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.yaml")
	src, err := newFileSource(Config{Source: "file", FilePath: path})
	if err != nil {
		t.Fatalf("newFileSource on missing file should not error: %v", err)
	}
	defer src.Close()
	if ok, _ := src.Manager().Disabled("/x/Y"); ok {
		t.Fatalf("missing file should behave as empty (pass)")
	}
}

func TestSetup_DisabledClearsDefault(t *testing.T) {
	SetDefault(NewManager())
	src, err := Setup(Config{Enabled: false})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer src.Close()
	if Default() != nil {
		t.Fatalf("disabled setup should clear default")
	}
}

func TestSetup_UnregisteredSourceFailOpen(t *testing.T) {
	src, err := Setup(Config{Enabled: true, Source: "nonexistent", FailOpen: true})
	if err != nil {
		t.Fatalf("unregistered source should fail-open without error, got %v", err)
	}
	defer src.Close()
	if Default() != nil {
		t.Fatalf("unregistered source should leave default nil (pass all)")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}
