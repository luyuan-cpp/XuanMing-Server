module github.com/luyuancpp/pandora/tools/configtable-gen

go 1.26.5

// 配置表生成器:读 Pandora-Client-SVN/Table 源表(xlsx)→ 严格校验 →
// 输出 configtable/dist/*.json + manifest.json。
// 契约:docs/design/config-table-hotreload.md §3/§5/§7/§8。
// xlsx 解析用 stdlib(archive/zip + encoding/xml)自实现最小读取器,
// 源表格式受本仓库契约钉死,读取器 fail-closed,不引第三方 xlsx 依赖。

require (
	github.com/luyuancpp/pandora/proto v0.0.0-00010101000000-000000000000
	google.golang.org/protobuf v1.36.11
)

require (
	golang.org/x/net v0.54.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
	google.golang.org/grpc v1.79.3 // indirect
)

replace github.com/luyuancpp/pandora/proto => ../../proto
