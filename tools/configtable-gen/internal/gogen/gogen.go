// Package gogen 生成服务端 Go 表访问代码(pkg/configtable 的 *.gen.go)。
//
// 做法对齐旧项目 protogen:proto 描述符驱动(输入 = tablegen.Discover() 的 TableDef,
// 全部派生自 excel.proto 注解)+ 独立模板文件(template/*.tmpl,go:embed)+ gofmt。
// 移植对应关系:
//   - table.go.tmpl   ← 旧 go_config.go.j2(TableManager 去单例化);
//   - tables.go.tmpl  ← 旧 go_all_table.go.j2(LoadTables → specByName 注册,
//     加载语义统一到 store.go:manifest 驱动 / 失败保留旧表);
//   - companion.go.tmpl:伴生桩,仅在缺失时创建一次(同 protogen 的 instance 文件模式)。
// 未移植 comp(ECS 组件)与 fk(外键 helper)模板:Go 侧无 ECS 消费方、
// 现有表无外键列,出现真实需求再加(CLAUDE.md §15.3)。
package gogen

import (
	"bytes"
	"embed"
	"fmt"
	"go/format"
	"text/template"

	"github.com/luyuancpp/pandora/tools/configtable-gen/internal/tablegen"
)

//go:embed template/*.tmpl
var templateFS embed.FS

// configpbImport 生成代码引用的 pb 包(与 pkg/configtable 手写代码一致)。
const configpbImport = "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"

var tmpls = template.Must(template.ParseFS(templateFS, "template/*.tmpl"))

type tableCtx struct {
	tablegen.TableDef
	PbImport string
}

// TableFile 生成一张表的 <name>_table.gen.go 内容(gofmt 后)。
func TableFile(def tablegen.TableDef) ([]byte, error) {
	return render("table.go.tmpl", tableCtx{TableDef: def, PbImport: configpbImport})
}

// RegistryFile 生成 tables.gen.go 内容(gofmt 后)。
func RegistryFile(defs []tablegen.TableDef) ([]byte, error) {
	return render("tables.go.tmpl", struct {
		Tables   []tablegen.TableDef
		PbImport string
	}{Tables: defs, PbImport: configpbImport})
}

// CompanionStub 生成 <name>.go 伴生桩(空校验钩子)。只应在目标文件不存在时落盘一次。
func CompanionStub(def tablegen.TableDef) ([]byte, error) {
	return render("companion.go.tmpl", tableCtx{TableDef: def, PbImport: configpbImport})
}

// Files 全部常规生成产物(不含伴生桩):文件名 → 内容。加新表后重跑即增量覆盖。
func Files(defs []tablegen.TableDef) (map[string][]byte, error) {
	out := make(map[string][]byte, len(defs)+1)
	for _, def := range defs {
		raw, err := TableFile(def)
		if err != nil {
			return nil, fmt.Errorf("生成 %s 表代码失败: %w", def.Name, err)
		}
		out[def.Name+"_table.gen.go"] = raw
	}
	reg, err := RegistryFile(defs)
	if err != nil {
		return nil, fmt.Errorf("生成 tables.gen.go 失败: %w", err)
	}
	out["tables.gen.go"] = reg
	return out, nil
}

func render(name string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpls.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, err
	}
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("gofmt 失败(模板产物不合法): %w\n----\n%s", err, buf.String())
	}
	return formatted, nil
}
