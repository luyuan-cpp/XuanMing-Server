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
	RowType   string // 行 message Go 类型名(pb 包内)
	DataType  string // 容器 message Go 类型名
	ProtoName string // 容器 message 全名(manifest.proto 字段)
	ExcelFile string // 源表相对路径((excel_file) 注解)
	KeyType   string // 主键 Go 类型(现约定 uint32,CLAUDE.md §5.6)
	BitIndex  bool   // (excel_bit_index):生成稳定 ID→位序映射
	DataStart int    // 数据区起始 Excel 行号(1 基,(excel_data_start_row),默认 5)

	container protoreflect.MessageType
	rowsField protoreflect.FieldDescriptor
	keyField  protoreflect.FieldDescriptor
	columns   []columnSpec

	fks []FKGen // Discover 末尾跨表解析后填充
}

// columnSpec 一列的导表规格((excel_col) 及配套注解)。
type columnSpec struct {
	fd       protoreflect.FieldDescriptor
	header   string // (excel_col) 表头列名
	required bool   // (excel_required)
	def      string // (excel_default)
	prefix   string // (excel_prefix)
	unique   bool   // (excel_key) 唯一二级键
	multi    bool   // (excel_multi_key) 非唯一索引
	fkTarget string // (excel_fk) 目标表 Name(引用其 id)
}

// KeyGen 供代码生成消费的二级键 / 索引视图。
type KeyGen struct {
	GoField string // Go 字段名(SceneId)
	GoType  string // 键 Go 类型(uint32 / string / configpb.XXX)
	Header  string // 源表列名(注释用)
}

// FKGen 供代码生成消费的外键视图。
type FKGen struct {
	GoField      string
	Header       string
	Required     bool   // 必填外键:0 也算非法(validateCrossTables 用)
	TargetName   string // 目标表 Name(level)
	TargetGoName string // 目标表 GoName(Level)
	TargetRow    string // 目标行类型(LevelRow)
}

// UniqueKeys 全部唯一二级键((excel_key))。
func (d TableDef) UniqueKeys() []KeyGen {
	return d.keysWhere(func(c columnSpec) bool { return c.unique })
}

// MultiKeys 全部非唯一索引((excel_multi_key) + (excel_fk) 隐含的反查索引)。
func (d TableDef) MultiKeys() []KeyGen {
	return d.keysWhere(func(c columnSpec) bool { return c.multi || c.fkTarget != "" })
}

// FKs 全部外键((excel_fk),含已解析的目标表信息)。
func (d TableDef) FKs() []FKGen { return d.fks }

func (d TableDef) keysWhere(pred func(columnSpec) bool) []KeyGen {
	var out []KeyGen
	for _, c := range d.columns {
		if pred(c) {
			out = append(out, KeyGen{GoField: goCamel(string(c.fd.Name())), GoType: goType(c.fd), Header: c.header})
		}
	}
	return out
}

// Discover 遍历 pandora.config.v1 包,收集全部打了 (excel_file) 的整表容器。
func Discover() ([]TableDef, error) { return DiscoverPackage(configPkg) }

// DiscoverPackage 按包名发现配置表(生产只用 pandora.config.v1;
// pandora.configtest.v1 夹具包仅供生成器单测,见 proto/pandora/configtest/v1)。
// 返回按 ProtoName 排序(生成产物顺序稳定);末尾统一解析 (excel_fk) 目标表。
func DiscoverPackage(pkg string) ([]TableDef, error) {
	var defs []TableDef
	var rangeErr error
	protoregistry.GlobalTypes.RangeMessages(func(mt protoreflect.MessageType) bool {
		desc := mt.Descriptor()
		if string(desc.ParentFile().Package()) != pkg {
			return true
		}
		opts, ok := desc.Options().(*descriptorpb.MessageOptions)
		if !ok || !proto.HasExtension(opts, configpb.E_ExcelFile) {
			return true
		}
		def, err := buildDef(mt, opts)
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
		return nil, fmt.Errorf("包 %s 内没有任何打了 (excel_file) 注解的整表容器", pkg)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].ProtoName < defs[j].ProtoName })
	if err := resolveFKs(defs); err != nil {
		return nil, err
	}
	return defs, nil
}

// resolveFKs 把每列的 (excel_fk) 目标表名解析成目标表定义(不存在 = 配置错误)。
func resolveFKs(defs []TableDef) error {
	byName := make(map[string]*TableDef, len(defs))
	for i := range defs {
		byName[defs[i].Name] = &defs[i]
	}
	for i := range defs {
		d := &defs[i]
		for _, c := range d.columns {
			if c.fkTarget == "" {
				continue
			}
			target, ok := byName[c.fkTarget]
			if !ok {
				return fmt.Errorf("%s.%s: (excel_fk) 目标表 %q 不存在(同包内已发现: %v)",
					d.ProtoName, c.fd.Name(), c.fkTarget, tableNames(defs))
			}
			if target.Name == d.Name {
				return fmt.Errorf("%s.%s: (excel_fk) 不支持自引用", d.ProtoName, c.fd.Name())
			}
			d.fks = append(d.fks, FKGen{
				GoField:      goCamel(string(c.fd.Name())),
				Header:       c.header,
				Required:     c.required,
				TargetName:   target.Name,
				TargetGoName: target.GoName,
				TargetRow:    target.RowType,
			})
		}
	}
	return nil
}

// buildDef 校验容器 / 行 message 约定并派生全部生成参数。
func buildDef(mt protoreflect.MessageType, mopts *descriptorpb.MessageOptions) (TableDef, error) {
	desc := mt.Descriptor()
	full := string(desc.FullName())
	dataType := string(desc.Name())
	excelFile := proto.GetExtension(mopts, configpb.E_ExcelFile).(string)
	dataStart := int(proto.GetExtension(mopts, configpb.E_ExcelDataStartRow).(uint32))
	if dataStart == 0 {
		dataStart = 5
	}

	if !strings.HasSuffix(dataType, "TableData") {
		return TableDef{}, fmt.Errorf("%s: 打了 (excel_file) 的容器必须命名为 <Name>TableData", full)
	}
	goName := strings.TrimSuffix(dataType, "TableData")
	if excelFile == "" {
		return TableDef{}, fmt.Errorf("%s: (excel_file) 不能为空", full)
	}
	if dataStart < 2 {
		return TableDef{}, fmt.Errorf("%s: (excel_data_start_row) 必须 >= 2,实为 %d", full, dataStart)
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
			unique:   proto.GetExtension(fopts, configpb.E_ExcelKey).(bool),
			multi:    proto.GetExtension(fopts, configpb.E_ExcelMultiKey).(bool),
			fkTarget: proto.GetExtension(fopts, configpb.E_ExcelFk).(string),
		}
		if err := checkColumn(rowDesc, fd, cs); err != nil {
			return TableDef{}, err
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
		BitIndex:  proto.GetExtension(mopts, configpb.E_ExcelBitIndex).(bool),
		DataStart: dataStart,
		container: mt,
		rowsField: rowsField,
		keyField:  keyField,
		columns:   columns,
	}, nil
}

// checkColumn 单列注解合法性(注解互斥 / 类型约束)。
func checkColumn(rowDesc protoreflect.MessageDescriptor, fd protoreflect.FieldDescriptor, cs columnSpec) error {
	at := func() string { return fmt.Sprintf("%s.%s", rowDesc.FullName(), fd.Name()) }
	if cs.header == "" {
		return fmt.Errorf("%s: (excel_col) 不能为空", at())
	}
	if cs.required && cs.def != "" {
		return fmt.Errorf("%s: excel_required 与 excel_default 互斥", at())
	}
	if fd.IsList() || fd.IsMap() {
		return fmt.Errorf("%s: 导表列暂不支持 repeated/map", at())
	}
	marks := 0
	for _, on := range []bool{cs.unique, cs.multi, cs.fkTarget != ""} {
		if on {
			marks++
		}
	}
	if marks > 1 {
		return fmt.Errorf("%s: excel_key / excel_multi_key / excel_fk 互斥(fk 自带反查索引)", at())
	}
	if fd.Name() == "id" && marks > 0 {
		return fmt.Errorf("%s: 主键 id 天然唯一,不得再标 key/multi_key/fk", at())
	}
	if (cs.unique || cs.multi) && fd.Kind() == protoreflect.BoolKind {
		return fmt.Errorf("%s: 布尔列不支持作键 / 索引", at())
	}
	if cs.fkTarget != "" && fd.Kind() != protoreflect.Uint32Kind {
		return fmt.Errorf("%s: (excel_fk) 列必须 uint32(引用目标表 id,CLAUDE.md §5.6),实为 %s", at(), fd.Kind())
	}
	return nil
}

func tableNames(defs []TableDef) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.Name
	}
	return out
}

// goType 键 / 索引列的 Go 类型(枚举 → pb 包内类型,模板统一 configpb 别名)。
func goType(fd protoreflect.FieldDescriptor) string {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return "string"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "uint32"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "uint64"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "int32"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "int64"
	case protoreflect.EnumKind:
		return "configpb." + string(fd.Enum().Name())
	default:
		return "unsupported_" + fd.Kind().String()
	}
}

// goCamel proto 字段名 → protoc-gen-go 风格 Go 字段名(scene_id→SceneId,uint32_key→Uint32Key)。
func goCamel(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		if r == '_' {
			up = true
			continue
		}
		if up && r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		up = false
		b.WriteRune(r)
	}
	return b.String()
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
