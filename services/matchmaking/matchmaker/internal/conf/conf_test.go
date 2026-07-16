// conf_test.go — 校验 etc/ 下各部署配置能被 main.go 同款加载路径解析,且关键字段符合部署语义。
// 防止改配置文件时手滑(缩进 / 字段名拼错)直到服务启动才发现。
package conf_test

import (
	"path/filepath"
	"testing"

	kconfig "github.com/go-kratos/kratos/v2/config"
	"github.com/go-kratos/kratos/v2/config/file"

	"github.com/luyuancpp/pandora/services/matchmaking/matchmaker/internal/conf"
)

// loadConfig 复刻 main.go 的加载方式:kratos file source → Scan → Defaults。
func loadConfig(t *testing.T, rel string) conf.Config {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("abs %s: %v", rel, err)
	}
	c := kconfig.New(kconfig.WithSource(file.NewSource(path)))
	defer c.Close()
	if err := c.Load(); err != nil {
		t.Fatalf("load %s: %v", rel, err)
	}
	var cfg conf.Config
	if err := c.Scan(&cfg); err != nil {
		t.Fatalf("scan %s: %v", rel, err)
	}
	cfg.Defaults()
	return cfg
}

// PVP 撮合实例:默认部署,走排队撮合(非 solo)。
func TestConfig_DevPVP(t *testing.T) {
	cfg := loadConfig(t, "etc/matchmaker-dev.yaml")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Match.EnableSoloMatch {
		t.Fatalf("PVP 实例必须走撮合(enable_solo_match=false),否则每张票都单独开局")
	}
	if cfg.Match.GameMode == "" {
		t.Fatalf("game_mode 不能为空(撮合池命名空间)")
	}
}

// PVE 直进实例:与 PVP 同二进制不同部署;单人/整队直接开局,不撮合。
// 两实例必须错开 gRPC 端口与 snowflake node_id(match_id 全局唯一)。
func TestConfig_PVE(t *testing.T) {
	pve := loadConfig(t, "etc/matchmaker-pve.yaml")
	pvp := loadConfig(t, "etc/matchmaker-dev.yaml")
	if err := pve.Validate(); err != nil {
		t.Fatalf("PVE Validate: %v", err)
	}

	if !pve.Match.EnableSoloMatch {
		t.Fatalf("PVE 实例必须 enable_solo_match=true(组好队/单人直进副本)")
	}
	if pve.Match.GameMode == pvp.Match.GameMode {
		t.Fatalf("PVE 与 PVP game_mode 相同(%q),撮合池会串", pve.Match.GameMode)
	}
	if pve.Match.MatchResumeAuthAudience == pvp.Match.MatchResumeAuthAudience {
		t.Fatal("PVE 与 PVP Match resume audience 必须隔离，防签名跨部署重放")
	}
	if pve.Server.Grpc.Addr == pvp.Server.Grpc.Addr {
		t.Fatalf("PVE 与 PVP gRPC 端口相同(%q),同机部署会撞端口", pve.Server.Grpc.Addr)
	}
	if pve.Node.NodeId == pvp.Node.NodeId {
		t.Fatalf("PVE 与 PVP node_id 相同(%d),snowflake match_id 会撞", pve.Node.NodeId)
	}
	if pve.Match.TeamSize <= 0 {
		t.Fatalf("team_size 必须 > 0")
	}
}

func TestMatchResumeAuthSecretMustBeIndependent(t *testing.T) {
	cfg := loadConfig(t, "etc/matchmaker-dev.yaml")
	cfg.Match.MatchResumeAuthSecret = cfg.JWT.Secret
	if err := cfg.Validate(); err == nil {
		t.Fatal("player JWT and match resume service identity must not share a key")
	}
}

func TestAllocationAbortAuthMustBeDedicatedForRealAllocator(t *testing.T) {
	base := loadConfig(t, "etc/matchmaker-dev.yaml")
	for name, reused := range map[string]string{
		"player-jwt":   base.JWT.Secret,
		"login-resume": base.Match.MatchResumeAuthSecret,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			cfg.Match.AllocationAbortAuthSecret = reused
			if err := cfg.Validate(); err == nil {
				t.Fatalf("allocation abort key reused %s trust domain", name)
			}
		})
	}
	cfg := base
	cfg.Match.AllocationAbortAuthSecret = "short"
	if err := cfg.Validate(); err == nil {
		t.Fatal("short allocation abort key accepted")
	}
	cfg = base
	cfg.Match.AllocationAbortAuthAudience = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("empty allocation abort audience accepted")
	}
}
