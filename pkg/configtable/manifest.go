// Package configtable 服务端配置表加载器(CLAUDE.md §9 不变量 15 的落地)。
//
// 移植自旧项目 mmorpg 的 go/shared/generated/table 读表,并按
// docs/design/config-table-hotreload.md §0 标准流水线加固,与旧实现的差异:
//   - 旧:每表各自 Load、普通指针赋值切换、失败 log.Fatalf 杀进程;
//   - 新:manifest 驱动整批加载,全表解析 + 校验通过才用 atomic.Pointer 一次性切换,
//     任一步失败返回 error、保留旧批次,批内跨表版本一致。
package configtable

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ManifestFileName 批次清单文件名(active / staging 目录下)。
const ManifestFileName = "manifest.json"

// Manifest 批次清单(config-table-hotreload.md §5),发布与热加载的唯一权威:
// 服务端只加载 Tables 列出的表,加载前逐表校验 checksum,version 单调防回退。
// 属流水线元数据而非业务数据结构,不建并行 proto(CLAUDE.md §5.8 约束的是业务结构)。
type Manifest struct {
	Version       uint64          `json:"version"`
	GeneratedAtMs uint64          `json:"generated_at_ms"`
	Generator     string          `json:"generator"`
	SourceRev     string          `json:"source_rev"`
	Tables        []ManifestTable `json:"tables"`
}

// ManifestTable 清单中一张表的条目。
type ManifestTable struct {
	Name     string `json:"name"`     // 表名 = 文件名(去 .json)= 注册键
	File     string `json:"file"`     // 必须恰为 "<name>.json",防路径逃逸
	Proto    string `json:"proto"`    // 容器 proto message 全名,与注册表比对防接错
	Checksum string `json:"checksum"` // "sha256:<hex64 小写>"
	Rows     uint32 `json:"rows"`     // 数据行数,加载后断言一致(防截断)
}

// ReadManifest 读取并解析 dir 下的 manifest.json,做结构合法性校验。
// 允许未知字段:滚动窗口内旧进程要能读带新元数据字段的清单。
func ReadManifest(dir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFileName))
	if err != nil {
		return nil, fmt.Errorf("读 manifest 失败: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("解析 manifest 失败: %w", err)
	}
	if m.Version == 0 {
		return nil, fmt.Errorf("manifest version 必须 > 0")
	}
	if len(m.Tables) == 0 {
		return nil, fmt.Errorf("manifest tables 为空")
	}
	seen := make(map[string]bool, len(m.Tables))
	for i, t := range m.Tables {
		if t.Name == "" {
			return nil, fmt.Errorf("manifest tables[%d] name 为空", i)
		}
		if seen[t.Name] {
			return nil, fmt.Errorf("manifest 表名重复 %q", t.Name)
		}
		seen[t.Name] = true
		// 文件名钉死为 <name>.json:同时消灭「文件名与表名漂移」和「../ 路径逃逸」。
		if t.File != t.Name+".json" {
			return nil, fmt.Errorf("表 %q 的 file 必须是 %q,实为 %q", t.Name, t.Name+".json", t.File)
		}
		if !strings.HasPrefix(t.Checksum, "sha256:") {
			return nil, fmt.Errorf("表 %q checksum 缺少 sha256: 前缀", t.Name)
		}
	}
	return &m, nil
}

// VerifyChecksum 校验内容哈希与清单声明一致(防发布拷贝截断 / 篡改)。
func VerifyChecksum(raw []byte, declared string) error {
	sum := sha256.Sum256(raw)
	got := "sha256:" + hex.EncodeToString(sum[:])
	if got != declared {
		return fmt.Errorf("checksum 不匹配: 声明 %s, 实际 %s", declared, got)
	}
	return nil
}
