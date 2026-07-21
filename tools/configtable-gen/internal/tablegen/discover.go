package tablegen

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	configpb "github.com/luyuancpp/pandora/proto/gen/go/pandora/config/v1"
)

// 描述符驱动的表发现(做法对齐旧项目 protogen:proto 即单一事实源)。
// 生成器 import configpb 后,pandora.config.v1 包内全部 message 已在全局注册表;
// 打了 (excel_file) 注解的 <Name>TableData 容器即一张配置表,无需任何手写登记。

const configPkg = "pandora.config.v1"

// TableDef 一张配置表的完整定义,全部派生自 proto 描述符与 excel 注解。
type TableDef struct {
	Name      string // 表名 = manifest 注册键 = 文件名(去 .json),snake_case(由 GoName 派生)
	GoName    string // Go 标识前缀(容器名去 TableData 后缀,如 Level)
	RowType   string // 行 message Go 类型名(configpb 包内)
	DataType  string // 容器 message Go 类型名
	ProtoName string // 容器 message 全名(manifest.proto 字段)
	ExcelFile string // 源表相对路径((excel_file) 注解)
	KeyType   string // 主键 Go 类型(现约定 uint32,CLAUDE.md §5.6)

	container protoreflect.MessageType
	rowsField protoreflect.FieldDescriptor
	keyField  protoreflect.FieldDescriptor
	columns   []columnSpec
}

// columnSpec 一列的导表规格((excel_col) 及配套注解)。
type columnSpec struct {
	fd       protoreflect.FieldDescriptor
	header   string // (excel_col) 表头列名
	required bool   // (excel_required)
	def      string // (excel_default)
	prefix   string // (excel_prefix)
}

// Discover 遍历 pandora.config.v1 包,收集全部打了 (excel_file) 的整表容器。
// 返回按 ProtoName 排序(生成产物顺序稳定)。
func Discover() ([]TableDef, error) {
	var defs []TableDef
	var rangeErr error
	protoregistry.GlobalTypes.RangeMessages(func(mt protoreflect.MessageType) bool {
		desc := mt.Descriptor()
		if desc.ParentFile().Package() != configPkg {
			return true
		}
		opts, ok := desc.Options().(*descriptorpb.MessageOptions)
		if !ok || !proto.HasExtension(opts, configpb.E_ExcelFile) {
			return true
		}
		def, err := buildDef(mt, proto.GetExtension(opts, configpb.E_ExcelFile).(string))
		if err != nil {
			rangeErr = err
			return false
		}
		defs = append(defs, def)
		return true
	})
	if rangeErr != nil {
		return nil, rangeErr
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("包 %s 内没有任何打了 (excel_file) 注解的整表容器", configPkg)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].ProtoName < defs[j].ProtoName })
	return defs, nil
}

// buildDef 校验容器 / 行 message 约定并派生全部生成参数。
func buildDef(mt protoreflect.MessageType, excelFile string) (TableDef, error) {
	desc := mt.Descriptor()
	full := string(desc.FullName())
	dataType := string(desc.Name())

	if !strings.HasSuffix(dataType, "TableData") {
		return TableDef{}, fmt.Errorf("%s: 打了 (excel_file) 的容器必须命名为 <Name>TableData", full)
	}
	goName := strings.TrimSuffix(dataType, "TableData")
	if excelFile == "" {
		return TableDef{}, fmt.Errorf("%s: (excel_file) 不能为空", full)
	}

	rowsField := desc.Fields().ByName("rows")
	if rowsField == nil || !rowsField.IsList() || rowsField.Kind() != protoreflect.MessageKind {
		return TableDef{}, fmt.Errorf("%s: 容器必须只有 repeated <Name>Row rows = 1 一个字段", full)
	}
	rowDesc := rowsField.Message()

	keyField := rowDesc.Fields().ByName("id")
	if keyField == nil {
		return TableDef{}, fmt.Errorf("%s: 行 message 必须有名为 id 的主键字段", rowDesc.FullName())
	}
	if keyField.Kind() != protoreflect.Uint32Kind {
		return TableDef{}, fmt.Errorf("%s.id: 配置表主键必须 uint32(CLAUDE.md §5.6),实为 %s",
			rowDesc.FullName(), keyField.Kind())
	}

	var columns []columnSpec
	fields := rowDesc.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		fopts, ok := fd.Options().(*descriptorpb.FieldOptions)
		if !ok || !proto.HasExtension(fopts, configpb.E_ExcelCol) {
			continue // 无 (excel_col) 的字段不参与导表(允许服务端派生字段)
		}
		cs := columnSpec{
			fd:       fd,
			header:   proto.GetExtension(fopts, configpb.E_ExcelCol).(string),
			required: proto.GetExtension(fopts, configpb.E_ExcelRequired).(bool),
			def:      proto.GetExtension(fopts, configpb.E_ExcelDefault).(string),
			prefix:   proto.GetExtension(fopts, configpb.E_ExcelPrefix).(string),
		}
		if cs.header == "" {
			return TableDef{}, fmt.Errorf("%s.%s: (excel_col) 不能为空", rowDesc.FullName(), fd.Name())
		}
		if cs.required && cs.def != "" {
			return TableDef{}, fmt.Errorf("%s.%s: excel_required 与 excel_default 互斥", rowDesc.FullName(), fd.Name())
		}
		if fd.IsList() || fd.IsMap() {
			return TableDef{}, fmt.Errorf("%s.%s: 导表列暂不支持 repeated/map", rowDesc.FullName(), fd.Name())
		}
		columns = append(columns, cs)
	}
	if len(columns) == 0 {
		return TableDef{}, fmt.Errorf("%s: 行 message 没有任何 (excel_col) 列", rowDesc.FullName())
	}

	return TableDef{
		Name:      toSnake(goName),
		GoName:    goName,
		RowType:   string(rowDesc.Name()),
		DataType:  dataType,
		ProtoName: full,
		ExcelFile: excelFile,
		KeyType:   "uint32",
		container: mt,
		rowsField: rowsField,
		keyField:  keyField,
		columns:   columns,
	}, nil
}

// toSnake CamelCase → snake_case(Level→level,SkillPermission→skill_permission)。
func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
