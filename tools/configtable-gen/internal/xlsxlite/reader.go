// Package xlsxlite 最小 xlsx 读取器(stdlib 实现,archive/zip + encoding/xml)。
//
// 只服务本仓库配置表源表(Pandora-Client-SVN/Table/*.xlsx)的受控格式:
// 单元格支持共享字符串 / 内联字符串 / 数值 / 布尔 / 公式结果,其余一律报错(fail-closed)。
// 不做样式、日期、合并单元格——源表契约里没有这些;出现即说明表格式越界,报错定位。
package xlsxlite

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// Sheet 一张工作表的稠密网格:Rows[r][c] 为去首尾空白后的单元格文本,空格为 ""。
type Sheet struct {
	Name string
	Rows [][]string
}

// ReadFirstSheet 打开 xlsx,按工作簿顺序读第一张工作表。
// 配置源表约定单表单 sheet;若有多 sheet,只读第一张(策划备注 sheet 允许放在后面)。
func ReadFirstSheet(path string) (*Sheet, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("打开 xlsx 失败: %w", err)
	}
	defer zr.Close()

	wb, err := parseWorkbook(zr)
	if err != nil {
		return nil, err
	}
	if len(wb.sheets) == 0 {
		return nil, fmt.Errorf("工作簿没有任何 sheet")
	}
	first := wb.sheets[0]
	target, ok := wb.relTarget[first.RelID]
	if !ok {
		return nil, fmt.Errorf("sheet %q 缺少关系目标(r:id=%s)", first.Name, first.RelID)
	}
	shared, err := parseSharedStrings(zr)
	if err != nil {
		return nil, err
	}
	rows, err := parseWorksheet(zr, target, shared)
	if err != nil {
		return nil, fmt.Errorf("sheet %q: %w", first.Name, err)
	}
	return &Sheet{Name: first.Name, Rows: rows}, nil
}

// ---- workbook / 关系 ----

type workbookInfo struct {
	sheets    []sheetEntry
	relTarget map[string]string // r:id → zip 内 worksheet 路径
}

type sheetEntry struct {
	Name  string
	RelID string
}

func parseWorkbook(zr *zip.ReadCloser) (*workbookInfo, error) {
	var wbXML struct {
		Sheets struct {
			Sheet []struct {
				Name string `xml:"name,attr"`
				RID  string `xml:"id,attr"` // r:id,namespace 由 encoding/xml 剥离后按本地名匹配
			} `xml:"sheet"`
		} `xml:"sheets"`
	}
	if err := unmarshalZipXML(zr, "xl/workbook.xml", &wbXML); err != nil {
		return nil, err
	}
	var relsXML struct {
		Relationship []struct {
			ID     string `xml:"Id,attr"`
			Target string `xml:"Target,attr"`
		} `xml:"Relationship"`
	}
	if err := unmarshalZipXML(zr, "xl/_rels/workbook.xml.rels", &relsXML); err != nil {
		return nil, err
	}
	info := &workbookInfo{relTarget: make(map[string]string)}
	for _, s := range wbXML.Sheets.Sheet {
		info.sheets = append(info.sheets, sheetEntry{Name: s.Name, RelID: s.RID})
	}
	for _, r := range relsXML.Relationship {
		t := strings.TrimPrefix(r.Target, "/")
		if !strings.HasPrefix(t, "xl/") {
			t = "xl/" + t
		}
		info.relTarget[r.ID] = t
	}
	return info, nil
}

// ---- 共享字符串 ----

// parseSharedStrings 读 xl/sharedStrings.xml;<si> 取其下全部 <t> 文本拼接(兼容富文本 run)。
func parseSharedStrings(zr *zip.ReadCloser) ([]string, error) {
	var ss struct {
		SI []struct {
			T *string `xml:"t"`
			R []struct {
				T string `xml:"t"`
			} `xml:"r"`
		} `xml:"si"`
	}
	err := unmarshalZipXML(zr, "xl/sharedStrings.xml", &ss)
	if err != nil {
		if _, missing := err.(*fileNotFoundError); missing {
			return nil, nil // 没有共享字符串表 = 全表无字符串单元格,合法
		}
		return nil, err
	}
	out := make([]string, len(ss.SI))
	for i, si := range ss.SI {
		var b strings.Builder
		if si.T != nil {
			b.WriteString(*si.T)
		}
		for _, r := range si.R {
			b.WriteString(r.T)
		}
		out[i] = b.String()
	}
	return out, nil
}

// ---- worksheet ----

func parseWorksheet(zr *zip.ReadCloser, target string, shared []string) ([][]string, error) {
	var ws struct {
		SheetData struct {
			Row []struct {
				R int `xml:"r,attr"`
				C []struct {
					R  string  `xml:"r,attr"`
					T  string  `xml:"t,attr"`
					V  *string `xml:"v"`
					IS struct {
						T string `xml:"t"`
					} `xml:"is"`
				} `xml:"c"`
			} `xml:"row"`
		} `xml:"sheetData"`
	}
	if err := unmarshalZipXML(zr, target, &ws); err != nil {
		return nil, err
	}

	maxRow, maxCol := 0, 0
	type cellVal struct {
		row, col int
		val      string
	}
	var cells []cellVal
	for _, row := range ws.SheetData.Row {
		if row.R <= 0 {
			return nil, fmt.Errorf("行缺少 r 属性")
		}
		for _, c := range row.C {
			col, err := colIndex(c.R)
			if err != nil {
				return nil, err
			}
			val, err := cellText(c.T, c.V, c.IS.T, shared, c.R)
			if err != nil {
				return nil, err
			}
			cells = append(cells, cellVal{row: row.R - 1, col: col, val: strings.TrimSpace(val)})
			if row.R > maxRow {
				maxRow = row.R
			}
			if col+1 > maxCol {
				maxCol = col + 1
			}
		}
	}
	grid := make([][]string, maxRow)
	for i := range grid {
		grid[i] = make([]string, maxCol)
	}
	for _, cv := range cells {
		grid[cv.row][cv.col] = cv.val
	}
	return grid, nil
}

// cellText 按单元格类型取文本。未支持的类型(样式日期 / 错误值等)一律报错。
func cellText(typ string, v *string, inline string, shared []string, ref string) (string, error) {
	val := ""
	if v != nil {
		val = *v
	}
	switch typ {
	case "": // 数值(或空单元格)
		return val, nil
	case "n": // 显式数值
		return val, nil
	case "s": // 共享字符串
		var idx int
		if _, err := fmt.Sscanf(val, "%d", &idx); err != nil || idx < 0 || idx >= len(shared) {
			return "", fmt.Errorf("单元格 %s 共享字符串索引非法: %q", ref, val)
		}
		return shared[idx], nil
	case "inlineStr":
		return inline, nil
	case "str": // 公式计算结果字符串
		return val, nil
	case "b":
		return val, nil // "0" / "1",与源表布尔列取值一致
	default:
		return "", fmt.Errorf("单元格 %s 类型 %q 不在支持范围(源表格式越界)", ref, typ)
	}
}

// colIndex "B5" → 1。只取字母段。
func colIndex(ref string) (int, error) {
	n := 0
	seen := false
	for _, ch := range ref {
		if ch >= 'A' && ch <= 'Z' {
			n = n*26 + int(ch-'A'+1)
			seen = true
			continue
		}
		break
	}
	if !seen {
		return 0, fmt.Errorf("单元格引用 %q 缺少列字母", ref)
	}
	return n - 1, nil
}

// ---- zip 内 XML ----

type fileNotFoundError struct{ name string }

func (e *fileNotFoundError) Error() string { return "xlsx 内缺少 " + e.name }

func unmarshalZipXML(zr *zip.ReadCloser, name string, dst any) error {
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("读 %s 失败: %w", name, err)
		}
		defer rc.Close()
		raw, err := io.ReadAll(rc)
		if err != nil {
			return fmt.Errorf("读 %s 失败: %w", name, err)
		}
		if err := xml.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("解析 %s 失败: %w", name, err)
		}
		return nil
	}
	return &fileNotFoundError{name: name}
}
