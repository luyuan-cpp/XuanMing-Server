// schema.go — 启动前建表存在性检查(2026-07-08)。
//
// 背景:deploy/mysql-init/*.sql 只在 MySQL volume **首次初始化**时执行;既有 volume / k8s PVC
// 升级后不会自动重放 init SQL,新增的表(如 player_roles / player_item_instance)会缺失,
// 运行期首条 SQL 才炸(对客户端表现为全量内部错误)。服务启动期显式检查依赖表,
// 缺表 fail-fast,错误信息直接指向迁移 SQL(init SQL 全部 CREATE TABLE IF NOT EXISTS,
// 可对既有库安全重放补齐)。
package mysqlx

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// CheckTables 校验当前连接所在库(DATABASE())中给定表是否全部存在。
//
// 缺表返回错误,消息列出缺失表名 + migrationHint(指向 deploy/mysql-init 下对应 SQL 文件,
// 运维对既有库重放该文件即可补齐)。tables 为空时直接返回 nil。
//
// 用法(服务 main 启动期,连上 MySQL 后):
//
//	if err := mysqlx.CheckTables(ctx, db, "deploy/mysql-init/02-account-tables.sql", "player_roles"); err != nil {
//	    helper.Errorw("msg", "mysql_schema_check_failed", "err", err)
//	    os.Exit(1)
//	}
func CheckTables(ctx context.Context, db *sql.DB, migrationHint string, tables ...string) error {
	if len(tables) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(tables)), ",")
	args := make([]any, len(tables))
	for i, t := range tables {
		args[i] = t
	}
	rows, err := db.QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name IN (`+placeholders+`)`,
		args...)
	if err != nil {
		return fmt.Errorf("query information_schema.tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	present := make(map[string]struct{}, len(tables))
	for rows.Next() {
		var name string
		if serr := rows.Scan(&name); serr != nil {
			return fmt.Errorf("scan table_name: %w", serr)
		}
		present[strings.ToLower(name)] = struct{}{}
	}
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("iterate information_schema.tables: %w", rerr)
	}

	var missing []string
	for _, t := range tables {
		if _, ok := present[strings.ToLower(t)]; !ok {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少数据库表 %v:既有 MySQL volume / PVC 不会自动重放 init SQL,请对当前库重放 %s(全部 CREATE TABLE IF NOT EXISTS,可安全重复执行)后再启动", missing, migrationHint)
	}
	return nil
}

// CheckColumns 校验当前连接所在库(DATABASE())中给定表是否包含全部指定列(R8 收口)。
//
// 背景:CheckTables 只按表名判存在,识别不出「表存在但缺新增列」的半旧 schema——
// 例如 player_session_generations 在早期版本无 generation 列,旧库只补建过旧版表时
// 启动期表名检查通过,运行期首条含新列的 SQL 才炸。依赖新增列的服务应在启动期
// 用本函数把列缺失也 fail-fast,错误信息指向对应迁移。
//
// columns 为空时直接返回 nil。列名比较不区分大小写。
func CheckColumns(ctx context.Context, db *sql.DB, migrationHint, table string, columns ...string) error {
	if len(columns) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?`,
		table)
	if err != nil {
		return fmt.Errorf("query information_schema.columns for %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	present := make(map[string]struct{}, len(columns))
	for rows.Next() {
		var name string
		if serr := rows.Scan(&name); serr != nil {
			return fmt.Errorf("scan column_name: %w", serr)
		}
		present[strings.ToLower(name)] = struct{}{}
	}
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("iterate information_schema.columns: %w", rerr)
	}

	var missing []string
	for _, c := range columns {
		if _, ok := present[strings.ToLower(c)]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("表 %s 缺少列 %v:schema 是旧版本(表存在但未跑新增列迁移),请对当前库执行 %s 后再启动", table, missing, migrationHint)
	}
	return nil
}

// ColumnSpec 描述一列的期望形状(R9 复审 P2 收口:只查列名不验类型/约束,识别不出
// 「列在但类型错/可空性错/键缺失」的畸形 schema,运行期写入才炸或静默截断)。
// 空字符串字段表示不校验该维度(渐进接入,不强迫一次写全)。
type ColumnSpec struct {
	Name string
	// DataType 对照 information_schema.columns.DATA_TYPE(小写,如 "bigint" / "varchar" / "datetime")。
	DataType string
	// Nullable 对照 IS_NULLABLE:"NO" / "YES"。
	Nullable string
	// Key 对照 COLUMN_KEY:"PRI" / "UNI" / "MUL" / ""(空串=不校验;要求非键列请勿依赖本维度)。
	Key string
}

// CheckColumnSpecs 校验表中给定列存在且类型/可空性/键形状符合预期(R9 复审 P2)。
//
// 与 CheckColumns 的关系:CheckColumns 只判列存在;本函数额外对照 DATA_TYPE、
// IS_NULLABLE、COLUMN_KEY,识别「旧库手工建过同名列但形状不对」的半旧 schema。
// 比较不区分大小写。specs 为空时直接返回 nil。
func CheckColumnSpecs(ctx context.Context, db *sql.DB, migrationHint, table string, specs ...ColumnSpec) error {
	if len(specs) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT column_name, data_type, is_nullable, column_key FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?`,
		table)
	if err != nil {
		return fmt.Errorf("query information_schema.columns for %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	type colShape struct {
		dataType string
		nullable string
		key      string
	}
	present := make(map[string]colShape)
	for rows.Next() {
		var name, dataType, nullable, key string
		if serr := rows.Scan(&name, &dataType, &nullable, &key); serr != nil {
			return fmt.Errorf("scan column shape: %w", serr)
		}
		present[strings.ToLower(name)] = colShape{
			dataType: strings.ToLower(dataType),
			nullable: strings.ToUpper(nullable),
			key:      strings.ToUpper(key),
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("iterate information_schema.columns: %w", rerr)
	}

	var problems []string
	for _, spec := range specs {
		shape, ok := present[strings.ToLower(spec.Name)]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: 列缺失", spec.Name))
			continue
		}
		if spec.DataType != "" && shape.dataType != strings.ToLower(spec.DataType) {
			problems = append(problems, fmt.Sprintf("%s: 类型 %s ≠ 期望 %s", spec.Name, shape.dataType, strings.ToLower(spec.DataType)))
		}
		if spec.Nullable != "" && shape.nullable != strings.ToUpper(spec.Nullable) {
			problems = append(problems, fmt.Sprintf("%s: 可空性 %s ≠ 期望 %s", spec.Name, shape.nullable, strings.ToUpper(spec.Nullable)))
		}
		if spec.Key != "" && shape.key != strings.ToUpper(spec.Key) {
			problems = append(problems, fmt.Sprintf("%s: 键形状 %q ≠ 期望 %q", spec.Name, shape.key, strings.ToUpper(spec.Key)))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("表 %s 列形状不符 [%s]:schema 与迁移产物不一致(旧库手工改过表/迁移未跑全),请对照 %s 修复后再启动", table, strings.Join(problems, "; "), migrationHint)
	}
	return nil
}
