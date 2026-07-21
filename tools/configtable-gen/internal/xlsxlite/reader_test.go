package xlsxlite

import (
	"archive/zip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildXlsx 生成一个最小合法 xlsx:共享字符串 + 数值 + 内联字符串混排,含跳空单元格。
func buildXlsx(t *testing.T, path string, sheetXML string, sharedXML string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	files := map[string]string{
		"xl/workbook.xml": `<?xml version="1.0"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"
          xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/><sheet name="备注" sheetId="2" r:id="rId2"/></sheets>
</workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet2.xml"/>
</Relationships>`,
		"xl/worksheets/sheet1.xml": sheetXML,
		"xl/worksheets/sheet2.xml": `<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData/></worksheet>`,
	}
	if sharedXML != "" {
		files["xl/sharedStrings.xml"] = sharedXML
	}
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadFirstSheet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.xlsx")
	shared := `<?xml version="1.0"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="3" uniqueCount="3">
  <si><t>ID</t></si>
  <si><r><t>关卡</t></r><r><t>名称</t></r></si>
  <si><r><t>MOBA</t></r><r><t>战斗</t></r></si>
</sst>`
	sheet := `<?xml version="1.0"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <sheetData>
    <row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>
    <row r="3"><c r="A3"><v>6</v></c><c r="B3" t="s"><v>2</v></c><c r="D3" t="inlineStr"><is><t>内联</t></is></c></row>
    <row r="4"><c r="A4" t="b"><v>1</v></c><c r="B4"><v> 7 </v></c></row>
  </sheetData>
</worksheet>`
	buildXlsx(t, path, sheet, shared)

	s, err := ReadFirstSheet(path)
	if err != nil {
		t.Fatalf("ReadFirstSheet: %v", err)
	}
	if s.Name != "Sheet1" {
		t.Fatalf("应读工作簿顺序第一张 sheet,得到 %q", s.Name)
	}
	if len(s.Rows) != 4 {
		t.Fatalf("行数=%d", len(s.Rows))
	}
	if s.Rows[0][0] != "ID" || s.Rows[0][1] != "关卡名称" {
		t.Fatalf("表头=%v(富文本 run 应拼接)", s.Rows[0])
	}
	if len(s.Rows[1]) != 4 {
		t.Fatalf("空行应补齐列宽: %v", s.Rows[1])
	}
	if s.Rows[2][0] != "6" || s.Rows[2][1] != "MOBA战斗" || s.Rows[2][2] != "" || s.Rows[2][3] != "内联" {
		t.Fatalf("第 3 行=%v(数值 / 共享 run / 跳空 / 内联)", s.Rows[2])
	}
	if s.Rows[3][0] != "1" || s.Rows[3][1] != "7" {
		t.Fatalf("第 4 行=%v(布尔 / 数值应去空白)", s.Rows[3])
	}
}

func TestReadFirstSheetUnsupportedCellType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.xlsx")
	sheet := `<?xml version="1.0"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <sheetData><row r="1"><c r="A1" t="e"><v>#REF!</v></c></row></sheetData>
</worksheet>`
	buildXlsx(t, path, sheet, "")
	_, err := ReadFirstSheet(path)
	if err == nil || !strings.Contains(err.Error(), "不在支持范围") {
		t.Fatalf("错误值单元格应 fail-closed: %v", err)
	}
}

func TestReadFirstSheetBadSharedIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.xlsx")
	sheet := `<?xml version="1.0"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <sheetData><row r="1"><c r="A1" t="s"><v>99</v></c></row></sheetData>
</worksheet>`
	buildXlsx(t, path, sheet, "")
	if _, err := ReadFirstSheet(path); err == nil {
		t.Fatal("共享字符串索引越界应报错")
	}
}

func TestReadFirstSheetMissingFile(t *testing.T) {
	if _, err := ReadFirstSheet(filepath.Join(t.TempDir(), "nope.xlsx")); err == nil {
		t.Fatal("文件缺失应报错")
	}
}

// TestRealTableIfPresent 本机存在真实源表时顺带跑一遍(CI / 他机无 SVN 仓库则跳过)。
func TestRealTableIfPresent(t *testing.T) {
	real := `F:\work\Pandora-Client-SVN\Table\关卡\g_关卡.xlsx`
	if _, err := os.Stat(real); err != nil {
		t.Skipf("真实源表不存在,跳过: %v", err)
	}
	s, err := ReadFirstSheet(real)
	if err != nil {
		t.Fatalf("读真实 g_关卡.xlsx 失败: %v", err)
	}
	if len(s.Rows) < 5 {
		t.Fatalf("真实表行数异常: %d", len(s.Rows))
	}
	fmt.Println("真实表首行:", s.Rows[0])
}
