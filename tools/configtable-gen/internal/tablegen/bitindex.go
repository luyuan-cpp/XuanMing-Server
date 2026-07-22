package tablegen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// 稳定「ID → 位序」分配(移植旧项目 exporter 的 bit_index 机制,含
// mapping/table_index_mapping 状态文件语义):位序一经分配永不变更、永不复用——
// 新 ID 追加到最大位之后,删除的 ID 在状态里保留占位,保证已落库的进度 / 解锁
// 位图不因改表(删行 / 插行)而错位。状态文件必须 git 跟踪,丢失 = 位图数据全部作废。

// BitState 一张表的位序状态(configtable/bitindex_state/<name>.json)。
type BitState struct {
	// Entries 全部已分配位(含已从表里删除的 ID,按 bit 升序落盘)。
	Entries []BitEntry `json:"entries"`
}

// BitEntry 一条分配记录。
type BitEntry struct {
	ID  uint32 `json:"id"`
	Bit uint32 `json:"bit"`
}

// LoadBitState 读状态文件。文件不存在时只有 allowBootstrap=true 才返回全新空状态:
// 已发布过的表若状态文件丢失被静默当首次初始化,重新分配的位序会让历史落库位图
// 全部错位重释义(审计 P1)。丢失属事故:从 git 恢复,或人工确认无位图数据落库后
// 显式带 -bitindex-bootstrap 重建。
func LoadBitState(path string, allowBootstrap bool) (*BitState, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if !allowBootstrap {
			return nil, fmt.Errorf("位序状态文件缺失: %s(丢失会导致已落库位图错位;从 git 恢复,或确认表从未发布后用 -bitindex-bootstrap 显式初始化)", path)
		}
		return &BitState{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读位序状态失败: %w", err)
	}
	var s BitState
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("位序状态损坏(不可手改;丢失会导致已落库位图错位): %w", err)
	}
	seenID, seenBit := map[uint32]bool{}, map[uint32]bool{}
	for _, e := range s.Entries {
		if seenID[e.ID] || seenBit[e.Bit] {
			return nil, fmt.Errorf("位序状态损坏: id=%d bit=%d 重复", e.ID, e.Bit)
		}
		seenID[e.ID], seenBit[e.Bit] = true, true
	}
	return &s, nil
}

// Assign 为当前表的 id 集合分配位序:已有的沿用,新 id 按升序追加到最大位之后;
// 状态里已删除的 id 保留占位。返回「仅当前表内 id」的映射与状态是否变更。
func (s *BitState) Assign(ids []uint32) (live []BitEntry, changed bool) {
	byID := make(map[uint32]uint32, len(s.Entries))
	next := uint32(0)
	for _, e := range s.Entries {
		byID[e.ID] = e.Bit
		if e.Bit+1 > next {
			next = e.Bit + 1
		}
	}
	sorted := append([]uint32(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for _, id := range sorted {
		if _, ok := byID[id]; ok {
			continue
		}
		byID[id] = next
		s.Entries = append(s.Entries, BitEntry{ID: id, Bit: next})
		next++
		changed = true
	}
	sort.Slice(s.Entries, func(i, j int) bool { return s.Entries[i].Bit < s.Entries[j].Bit })
	for _, id := range sorted {
		live = append(live, BitEntry{ID: id, Bit: byID[id]})
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Bit < live[j].Bit })
	return live, changed
}

// BitCount 位图长度 = 最大已分配位 + 1(含已删除 ID 的保留位,存储侧按此定容)。
func (s *BitState) BitCount() uint32 {
	max := uint32(0)
	for _, e := range s.Entries {
		if e.Bit+1 > max {
			max = e.Bit + 1
		}
	}
	return max
}

// SaveBitState 确定性落盘(按 bit 升序,带缩进,尾随换行)。
// 临时文件 + 原子重命名(审计 P1:位序状态是已落库位图的唯一权威,直接 WriteFile
// 崩溃可截断;rename 同目录原子替换,旧状态要么完整保留要么完整替换)。
func SaveBitState(path string, s *BitState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ParseJSON 把 dist 产物 JSON 读回容器 message(工具 / 测试用,口径同 marshalDeterministic)。
func (d *TableDef) ParseJSON(raw []byte) (proto.Message, error) {
	container := d.container.New().Interface()
	if err := protojson.Unmarshal(raw, container); err != nil {
		return nil, fmt.Errorf("表 %s 解析 dist JSON 失败: %w", d.Name, err)
	}
	return container, nil
}

// RowIDs 从已构建容器中取全部主键(bitindex 分配与 FK 集合共用)。
func (d *TableDef) RowIDs(container proto.Message) []uint32 {
	rows := container.ProtoReflect().Get(d.rowsField).List()
	out := make([]uint32, 0, rows.Len())
	for i := 0; i < rows.Len(); i++ {
		out = append(out, uint32(rows.Get(i).Message().Get(d.keyField).Uint()))
	}
	return out
}
