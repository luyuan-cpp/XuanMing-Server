package tablegen

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// GeneratorName 写进 manifest.generator,便于追溯产物来源。
const GeneratorName = "configtable-gen@0.1.0"

// Generated 一张已构建并通过校验的表。
type Generated struct {
	Name      string        // 表名 = 文件名(去 .json)= 服务端注册键
	ProtoName string        // 容器 message 全名(manifest.proto 字段)
	Data      proto.Message // 容器实例
	RowCount  int
}

// manifest 结构与 pkg/configtable.Manifest 的 JSON 契约一致。
// 生成器独立 module,不 import pkg(避免工具链拖上服务端框架依赖);
// 两端以 docs/design/config-table-hotreload.md §5 为共同契约,改字段两边同步。
type manifestFile struct {
	Version       uint64          `json:"version"`
	GeneratedAtMs uint64          `json:"generated_at_ms"`
	Generator     string          `json:"generator"`
	SourceRev     string          `json:"source_rev"`
	Tables        []manifestEntry `json:"tables"`
}

type manifestEntry struct {
	Name     string `json:"name"`
	File     string `json:"file"`
	Proto    string `json:"proto"`
	Checksum string `json:"checksum"`
	Rows     uint32 `json:"rows"`
}

// Result 一次生成的结果摘要。
type Result struct {
	Version   uint64
	Unchanged bool // true = 内容与上一批完全一致,未写任何文件、未升版本
	Tables    []manifestEntry
}

// Options 生成参数。
type Options struct {
	OutDir       string
	SourceRev    string    // 源表版本标注(如 svn-r123)
	ForceVersion uint64    // 非 0 = 显式指定版本(仍须高于上一批)
	Now          time.Time // 版本号与 generated_at_ms 的时钟(测试可注入)
}

// WriteBatch 输出一批产物:先逐表写 <name>.json,最后写 manifest.json。
//
// 幂等:所有表内容 checksum 与 dist 现有 manifest 完全一致时不写盘、不升版本
// (重复跑生成器不产生 git 噪音);--version 强制指定时同样受「须高于上一批」约束。
// 版本号规则:自动模式 = YYYYMMDD*1000+1 起步,同日重复发布在上一版本上 +1,
// 保证单调递增(hotreload doc §5 不变量)。
func WriteBatch(tables []Generated, opts Options) (*Result, error) {
	if len(tables) == 0 {
		return nil, fmt.Errorf("没有任何表可输出")
	}
	entries := make([]manifestEntry, 0, len(tables))
	raws := make(map[string][]byte, len(tables))
	for _, t := range tables {
		raw, err := marshalDeterministic(t.Data)
		if err != nil {
			return nil, fmt.Errorf("表 %s 序列化失败: %w", t.Name, err)
		}
		if err := strictRoundTrip(raw, t.Data); err != nil {
			return nil, fmt.Errorf("表 %s 回读校验失败: %w", t.Name, err)
		}
		sum := sha256.Sum256(raw)
		entries = append(entries, manifestEntry{
			Name:     t.Name,
			File:     t.Name + ".json",
			Proto:    t.ProtoName,
			Checksum: "sha256:" + hex.EncodeToString(sum[:]),
			Rows:     uint32(t.RowCount),
		})
		raws[t.Name] = raw
	}

	prev, err := readPrevManifest(opts.OutDir)
	if err != nil {
		return nil, err
	}
	if prev != nil && sameContent(prev.Tables, entries) && opts.ForceVersion == 0 {
		return &Result{Version: prev.Version, Unchanged: true, Tables: entries}, nil
	}
	version, err := nextVersion(prev, opts)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, err
	}
	for _, e := range entries {
		if err := os.WriteFile(filepath.Join(opts.OutDir, e.File), raws[e.Name], 0o644); err != nil {
			return nil, fmt.Errorf("写 %s 失败: %w", e.File, err)
		}
	}
	m := manifestFile{
		Version:       version,
		GeneratedAtMs: uint64(opts.Now.UnixMilli()),
		Generator:     GeneratorName,
		SourceRev:     opts.SourceRev,
		Tables:        entries,
	}
	rawManifest, err := json.MarshalIndent(&m, "", "  ")
	if err != nil {
		return nil, err
	}
	rawManifest = append(rawManifest, '\n')
	if err := os.WriteFile(filepath.Join(opts.OutDir, "manifest.json"), rawManifest, 0o644); err != nil {
		return nil, fmt.Errorf("写 manifest.json 失败: %w", err)
	}
	return &Result{Version: version, Tables: entries}, nil
}

// marshalDeterministic protojson 产出的空白不稳定(库内故意注入),
// 走 Compact → Indent 归一化,保证同内容字节级一致(checksum 稳定、git diff 干净)。
// 口径:proto 原名(snake_case)+ 枚举数字 + 零值字段省略(protojson 默认)。
func marshalDeterministic(msg proto.Message) ([]byte, error) {
	raw, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true}.Marshal(msg)
	if err != nil {
		return nil, err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// strictRoundTrip 生成阶段严格回读(不 DiscardUnknown)+ 等价断言,钉死 §8 的字段对齐。
func strictRoundTrip(raw []byte, src proto.Message) error {
	clone := proto.Clone(src)
	proto.Reset(clone)
	if err := protojson.Unmarshal(raw, clone); err != nil {
		return err
	}
	if !proto.Equal(src, clone) {
		return fmt.Errorf("回读结果与源数据不等价")
	}
	return nil
}

func readPrevManifest(outDir string) (*manifestFile, error) {
	raw, err := os.ReadFile(filepath.Join(outDir, "manifest.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读现有 manifest 失败: %w", err)
	}
	var m manifestFile
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("现有 manifest 损坏(如需重建请删除后重跑): %w", err)
	}
	return &m, nil
}

func sameContent(a, b []manifestEntry) bool {
	if len(a) != len(b) {
		return false
	}
	byName := make(map[string]manifestEntry, len(a))
	for _, e := range a {
		byName[e.Name] = e
	}
	for _, e := range b {
		p, ok := byName[e.Name]
		if !ok || p.Checksum != e.Checksum || p.Rows != e.Rows || p.Proto != e.Proto {
			return false
		}
	}
	return true
}

func nextVersion(prev *manifestFile, opts Options) (uint64, error) {
	var prevVersion uint64
	if prev != nil {
		prevVersion = prev.Version
	}
	if opts.ForceVersion != 0 {
		if opts.ForceVersion <= prevVersion {
			return 0, fmt.Errorf("--version %d 须高于上一批 %d(version 单调递增)", opts.ForceVersion, prevVersion)
		}
		return opts.ForceVersion, nil
	}
	base := uint64(opts.Now.Year()*10000+int(opts.Now.Month())*100+opts.Now.Day())*1000 + 1
	if prevVersion >= base {
		return prevVersion + 1, nil
	}
	return base, nil
}
