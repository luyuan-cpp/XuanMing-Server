// configtable-gen 配置表生成器入口(描述符驱动,做法对齐旧项目 protogen)。
//
// 用法(仓库根目录):
//
//	go run ./tools/configtable-gen -tables F:\work\Pandora-Client-SVN\Table [-out configtable/dist] [-go-out pkg/configtable] [-source-rev svn-r123] [-version N]
//
// 表清单零手写登记:pandora.config.v1 包内打了 (excel_file) 注解的 <Name>TableData
// 容器即一张配置表(见 excel.proto),生成器经 protoreflect 自动发现。一次产出:
//  1. 数据:xlsx → excel 注解驱动的通用构建器(§7 严格校验,失败整批不产出)
//     → dist/*.json + manifest.json(version 单调 + sha256;内容不变幂等跳过);
//  2. 代码:pkg/configtable 的 <name>_table.gen.go + tables.gen.go(内容不变不重写),
//     伴生文件 <name>.go 缺失时创建一次空钩子桩(此后归人维护,不覆盖)。
//
// 加新表:写 proto(带 excel 注解)→ pwsh tools/scripts/proto_gen.ps1 → 跑本工具。
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/luyuancpp/pandora/tools/configtable-gen/internal/gogen"
	"github.com/luyuancpp/pandora/tools/configtable-gen/internal/tablegen"
	"github.com/luyuancpp/pandora/tools/configtable-gen/internal/xlsxlite"
)

func main() {
	tablesRoot := flag.String("tables", "", "源表根目录(必填,如 F:\\work\\Pandora-Client-SVN\\Table)")
	outDir := flag.String("out", filepath.Join("configtable", "dist"), "数据产物输出目录")
	goOut := flag.String("go-out", filepath.Join("pkg", "configtable"), "Go 表代码输出目录(空 = 跳过代码生成)")
	bitStateDir := flag.String("bitindex-state", filepath.Join("configtable", "bitindex_state"),
		"位序状态目录((excel_bit_index) 表;git 跟踪,丢失会导致已落库位图错位)")
	sourceRev := flag.String("source-rev", "unknown", "源表版本标注(如 svn-r123)")
	forceVersion := flag.Uint64("version", 0, "强制指定版本号(默认自动单调递增)")
	flag.Parse()

	if *tablesRoot == "" {
		fmt.Fprintln(os.Stderr, "缺少 -tables 源表根目录")
		flag.Usage()
		os.Exit(2)
	}

	defs, err := tablegen.Discover()
	if err != nil {
		fatalf("发现配置表失败: %v", err)
	}

	// 1. 数据产物:xlsx → 通用构建器(excel 注解驱动)→ dist
	var generated []tablegen.Generated
	built := make(map[string]proto.Message, len(defs))
	for i := range defs {
		def := &defs[i]
		path := filepath.Join(*tablesRoot, filepath.FromSlash(def.ExcelFile))
		sheet, err := xlsxlite.ReadFirstSheet(path)
		if err != nil {
			fatalf("读 %s 失败: %v", path, err)
		}
		data, rows, err := def.Build(sheet.Rows)
		if err != nil {
			fatalf("校验失败,整批不产出:\n  %v", err)
		}
		generated = append(generated, tablegen.Generated{
			Name: def.Name, ProtoName: def.ProtoName, Data: data, RowCount: rows,
		})
		built[def.Name] = data
		fmt.Printf("[OK] %-8s %3d 行  ← %s\n", def.Name, rows, def.ExcelFile)
	}

	// 1.5 批内引用完整性((excel_fk),§7.5):全表构建完成后统一校验,失败整批不产出。
	if err := tablegen.ValidateFKs(defs, built); err != nil {
		fatalf("外键校验失败,整批不产出:\n  %v", err)
	}

	res, err := tablegen.WriteBatch(generated, tablegen.Options{
		OutDir:       *outDir,
		SourceRev:    *sourceRev,
		ForceVersion: *forceVersion,
		Now:          time.Now(),
	})
	if err != nil {
		fatalf("写数据产物失败: %v", err)
	}
	if res.Unchanged {
		fmt.Printf("数据与上一批一致,保持 version %d,未写盘\n", res.Version)
	} else {
		fmt.Printf("数据批次 version %d → %s(%d 张表)\n", res.Version, *outDir, len(res.Tables))
	}

	// 2. Go 表代码:内容比对,不变不写(重复跑无 git 噪音)
	if *goOut == "" {
		return
	}
	files, err := gogen.Files(defs)
	if err != nil {
		fatalf("%v", err)
	}

	// 2.5 位序映射((excel_bit_index) 表):状态文件保证稳定分配(新 ID 追加、删 ID 保位)。
	for _, def := range defs {
		if !def.BitIndex {
			continue
		}
		statePath := filepath.Join(*bitStateDir, def.Name+".json")
		state, err := tablegen.LoadBitState(statePath)
		if err != nil {
			fatalf("表 %s: %v", def.Name, err)
		}
		live, changed := state.Assign(def.RowIDs(built[def.Name]))
		if changed {
			if err := tablegen.SaveBitState(statePath, state); err != nil {
				fatalf("表 %s 写位序状态失败: %v", def.Name, err)
			}
			fmt.Printf("[BIT] %s 位序状态更新 → %s\n", def.Name, statePath)
		}
		raw, err := gogen.BitIndexFile(def, live, state.BitCount())
		if err != nil {
			fatalf("表 %s 生成位序映射失败: %v", def.Name, err)
		}
		files[def.Name+"_bitindex.gen.go"] = raw
	}
	changed := 0
	for name, raw := range files {
		path := filepath.Join(*goOut, name)
		if prev, err := os.ReadFile(path); err == nil && bytes.Equal(prev, raw) {
			continue
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			fatalf("写 %s 失败: %v", path, err)
		}
		changed++
		fmt.Printf("[GEN] %s\n", path)
	}
	if changed == 0 {
		fmt.Printf("Go 表代码无变化(%s,%d 个文件)\n", *goOut, len(files))
	}

	// 3. 伴生桩:<name>.go 缺失时创建一次(空 validate 钩子),此后归人维护不覆盖
	for _, def := range defs {
		path := filepath.Join(*goOut, def.Name+".go")
		if _, err := os.Stat(path); err == nil {
			continue
		}
		stub, err := gogen.CompanionStub(def)
		if err != nil {
			fatalf("生成 %s 伴生桩失败: %v", def.Name, err)
		}
		if err := os.WriteFile(path, stub, 0o644); err != nil {
			fatalf("写 %s 失败: %v", path, err)
		}
		fmt.Printf("[NEW] %s(伴生桩,补业务校验 / 域方法后归人维护)\n", path)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
