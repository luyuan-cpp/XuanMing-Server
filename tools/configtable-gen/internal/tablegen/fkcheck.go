package tablegen

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ValidateFKs 批内引用完整性校验(hotreload doc §7.5,移植旧项目 exporter 的
// foreign_key.py 语义):所有 (excel_fk) 列的取值必须存在于目标表的 id 集合;
// 非必填列取值 0 = 无引用,跳过;必填列 0 也算非法。任一失败整批不产出。
// built:表 Name → 已构建的容器 message(必须包含全部被引用的目标表)。
func ValidateFKs(defs []TableDef, built map[string]proto.Message) error {
	idSets := make(map[string]map[uint64]bool)
	idsOf := func(name string) (map[uint64]bool, error) {
		if s, ok := idSets[name]; ok {
			return s, nil
		}
		def := defByName(defs, name)
		container, ok := built[name]
		if def == nil || !ok {
			return nil, fmt.Errorf("外键目标表 %q 不在本批构建结果中", name)
		}
		set := make(map[uint64]bool)
		rows := container.ProtoReflect().Get(def.rowsField).List()
		for i := 0; i < rows.Len(); i++ {
			set[rows.Get(i).Message().Get(def.keyField).Uint()] = true
		}
		idSets[name] = set
		return set, nil
	}

	for i := range defs {
		d := &defs[i]
		container, ok := built[d.Name]
		if !ok {
			continue
		}
		for _, c := range d.columns {
			if c.fkTarget == "" {
				continue
			}
			targetIDs, err := idsOf(c.fkTarget)
			if err != nil {
				return err
			}
			rows := container.ProtoReflect().Get(d.rowsField).List()
			for r := 0; r < rows.Len(); r++ {
				rm := rows.Get(r).Message()
				v := rm.Get(c.fd).Uint()
				if v == 0 {
					if c.required {
						return fkErr(d, c, rm, "必填外键不得为 0")
					}
					continue
				}
				if !targetIDs[v] {
					return fkErr(d, c, rm, fmt.Sprintf("引用 %d 在表 %s 中不存在", v, c.fkTarget))
				}
			}
		}
	}
	return nil
}

func fkErr(d *TableDef, c columnSpec, rm protoreflect.Message, msg string) error {
	return fmt.Errorf("%s 主键 %d 的 %s: %s", d.ExcelFile, rm.Get(d.keyField).Uint(), c.header, msg)
}

func defByName(defs []TableDef, name string) *TableDef {
	for i := range defs {
		if defs[i].Name == name {
			return &defs[i]
		}
	}
	return nil
}
