package conf

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLocalDSExeEnvFallback 锁住跨机器兜底的 dev 安全不变量:
//  1. 配置里的可执行路径在本机存在时,即使设了 PANDORA_DS_EXE 也不覆盖(dev 机 F:\ 路径照用);
//  2. 配置路径不存在(策划机:写死的盘符不在本机)时,回退到 PANDORA_DS_EXE / PANDORA_DS_DIR。
func TestLocalDSExeEnvFallback(t *testing.T) {
	dir := t.TempDir()
	realExe := filepath.Join(dir, "PandoraServer.exe")
	if err := os.WriteFile(realExe, []byte("stub"), 0o600); err != nil {
		t.Fatalf("写桩文件失败: %v", err)
	}
	envExe := filepath.Join(dir, "FromEnv.exe")
	envDir := filepath.Join(dir, "envwd")

	t.Run("配置路径存在时不被环境变量覆盖", func(t *testing.T) {
		t.Setenv("PANDORA_DS_EXE", envExe)
		t.Setenv("PANDORA_DS_DIR", envDir)
		c := &Config{}
		c.LocalDS.ExecutablePath = realExe
		c.LocalDS.WorkingDir = dir
		c.Defaults()
		if c.LocalDS.ExecutablePath != realExe {
			t.Fatalf("存在的配置路径被覆盖了: got %q want %q", c.LocalDS.ExecutablePath, realExe)
		}
		if c.LocalDS.WorkingDir != dir {
			t.Fatalf("WorkingDir 被意外覆盖: got %q want %q", c.LocalDS.WorkingDir, dir)
		}
	})

	t.Run("配置路径不存在时回退到环境变量", func(t *testing.T) {
		t.Setenv("PANDORA_DS_EXE", envExe)
		t.Setenv("PANDORA_DS_DIR", envDir)
		c := &Config{}
		c.LocalDS.ExecutablePath = filepath.Join(dir, "missing", "PandoraServer.exe")
		c.Defaults()
		if c.LocalDS.ExecutablePath != envExe {
			t.Fatalf("缺失路径未回退到环境变量: got %q want %q", c.LocalDS.ExecutablePath, envExe)
		}
		if c.LocalDS.WorkingDir != envDir {
			t.Fatalf("WorkingDir 未回退到环境变量: got %q want %q", c.LocalDS.WorkingDir, envDir)
		}
	})

	t.Run("未设环境变量时缺失路径保持原样", func(t *testing.T) {
		os.Unsetenv("PANDORA_DS_EXE")
		os.Unsetenv("PANDORA_DS_DIR")
		missing := filepath.Join(dir, "missing2", "PandoraServer.exe")
		c := &Config{}
		c.LocalDS.ExecutablePath = missing
		c.Defaults()
		if c.LocalDS.ExecutablePath != missing {
			t.Fatalf("无环境变量时路径不应改动: got %q want %q", c.LocalDS.ExecutablePath, missing)
		}
	})

	t.Run("正斜杠路径归一化为本机分隔符", func(t *testing.T) {
		os.Unsetenv("PANDORA_DS_EXE")
		os.Unsetenv("PANDORA_DS_DIR")
		// 策划在 yaml 里用正斜杠写,无需 \\ 转义。
		slashExe := filepath.ToSlash(realExe)
		c := &Config{}
		c.LocalDS.ExecutablePath = slashExe
		c.Defaults()
		if c.LocalDS.ExecutablePath != realExe {
			t.Fatalf("正斜杠路径未归一化: got %q want %q", c.LocalDS.ExecutablePath, realExe)
		}
	})
}
