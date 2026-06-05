// duration_test.go — Duration 包装类型单测(W3 ⑥,2026-06-05)。
//
// 覆盖:
//   - UnmarshalJSON 字符串路径:"5s" / "1h30m" / "0s" / "500ms" / "-2m"
//   - UnmarshalJSON 数字路径:5000000000 → 5s(向后兼容)
//   - UnmarshalJSON 空串 "" → 0
//   - UnmarshalJSON null → 零值
//   - UnmarshalJSON 非法值 "abc" / "5"(无单位)/ {} → error
//   - MarshalJSON → 带引号字符串
//   - Round-trip:Marshal → Unmarshal → 等值
//   - 嵌套结构 e2e:Kratos config 走 file source → Scan → 字段值正确
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"
)

func TestDuration_UnmarshalJSON_String(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{`"5s"`, 5 * time.Second},
		{`"1h30m"`, 90 * time.Minute},
		{`"0s"`, 0},
		{`"500ms"`, 500 * time.Millisecond},
		{`"-2m"`, -2 * time.Minute},
		{`""`, 0},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			var d Duration
			if err := json.Unmarshal([]byte(c.in), &d); err != nil {
				t.Fatalf("Unmarshal %q: %v", c.in, err)
			}
			if d.Std() != c.want {
				t.Errorf("Unmarshal %q: got %v, want %v", c.in, d.Std(), c.want)
			}
		})
	}
}

func TestDuration_UnmarshalJSON_Numeric(t *testing.T) {
	// 向后兼容:旧 yaml/json 写 ns 数字。
	var d Duration
	if err := json.Unmarshal([]byte("5000000000"), &d); err != nil {
		t.Fatalf("Unmarshal numeric: %v", err)
	}
	if d.Std() != 5*time.Second {
		t.Errorf("numeric: got %v, want 5s", d.Std())
	}
}

func TestDuration_UnmarshalJSON_Null(t *testing.T) {
	d := Duration(7 * time.Second)
	if err := json.Unmarshal([]byte("null"), &d); err != nil {
		t.Fatalf("Unmarshal null: %v", err)
	}
	// null 应保持原值不动(本实现选择 no-op,跟标准库 *time.Time 行为对齐)
	if d.Std() != 7*time.Second {
		t.Errorf("null should not overwrite: got %v", d.Std())
	}
}

func TestDuration_UnmarshalJSON_Invalid(t *testing.T) {
	cases := []string{
		`"abc"`,   // 非法格式
		`"5"`,     // 无单位
		`{}`,      // 错误类型
		`[1,2,3]`, // 错误类型
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			var d Duration
			if err := json.Unmarshal([]byte(c), &d); err == nil {
				t.Fatalf("expected error for %q, got nil (parsed as %v)", c, d.Std())
			}
		})
	}
}

func TestDuration_MarshalJSON(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{5 * time.Second, `"5s"`},
		{90 * time.Minute, `"1h30m0s"`},
		{0, `"0s"`},
		{500 * time.Millisecond, `"500ms"`},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			b, err := json.Marshal(Duration(c.in))
			if err != nil {
				t.Fatalf("Marshal %v: %v", c.in, err)
			}
			if string(b) != c.want {
				t.Errorf("Marshal %v: got %s, want %s", c.in, string(b), c.want)
			}
		})
	}
}

func TestDuration_RoundTrip(t *testing.T) {
	for _, in := range []time.Duration{
		5 * time.Second,
		1500 * time.Millisecond,
		24 * time.Hour,
		0,
	} {
		b, err := json.Marshal(Duration(in))
		if err != nil {
			t.Fatalf("Marshal %v: %v", in, err)
		}
		var out Duration
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("Unmarshal %s: %v", string(b), err)
		}
		if out.Std() != in {
			t.Errorf("round-trip %v: got %v", in, out.Std())
		}
	}
}

func TestDuration_String(t *testing.T) {
	if got := Duration(5 * time.Second).String(); got != "5s" {
		t.Errorf("String: got %q, want %q", got, "5s")
	}
}

// TestDuration_E2E_KratosConfig 验证 Kratos config 实际链路:
//
//	yaml file → file.NewSource → kconfig.New → Scan into struct
//
// 模拟下游服务的 conf 结构(嵌套 Duration 字段),写一个 yaml fixture,
// 用 Kratos config 加载,断言能解出 "5s" 字符串。这是本 Plan 的核心验证点
// (此前所有 yaml 都靠 "不写 duration 字段" 注释绕开,W3 ⑥ 后可放心写)。
func TestDuration_E2E_KratosConfig(t *testing.T) {
	type RedisConfDemo struct {
		Host        string   `yaml:"host" json:"host"`
		DialTimeout Duration `yaml:"dial_timeout" json:"dial_timeout"`
		DefaultTTL  Duration `yaml:"default_ttl" json:"default_ttl"`
	}
	type DemoCfg struct {
		Redis RedisConfDemo `yaml:"redis" json:"redis"`
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yaml")
	body := []byte(`redis:
  host: "127.0.0.1:6380"
  dial_timeout: "2s"
  default_ttl: "30s"
`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	c := kconfig.New(kconfig.WithSource(file.NewSource(path)))
	if err := c.Load(); err != nil {
		t.Fatalf("kratos config Load: %v", err)
	}
	defer func() { _ = c.Close() }()

	var cfg DemoCfg
	if err := c.Scan(&cfg); err != nil {
		t.Fatalf("kratos config Scan: %v", err)
	}

	if cfg.Redis.Host != "127.0.0.1:6380" {
		t.Errorf("host: got %q", cfg.Redis.Host)
	}
	if cfg.Redis.DialTimeout.Std() != 2*time.Second {
		t.Errorf("dial_timeout: got %v, want 2s", cfg.Redis.DialTimeout.Std())
	}
	if cfg.Redis.DefaultTTL.Std() != 30*time.Second {
		t.Errorf("default_ttl: got %v, want 30s", cfg.Redis.DefaultTTL.Std())
	}
}
