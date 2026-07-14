package data

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// terminalReleaseColumn 是 information_schema 中安全相关列的规范快照。
type terminalReleaseColumn struct {
	Name       string
	ColumnType string
	Nullable   string
	Charset    string
	Collation  string
	Default    string
	Extra      string
}

type terminalReleaseIndex struct {
	Name      string
	NonUnique int
	Sequence  int
	Column    string
}

type terminalReleaseTable struct {
	Engine    string
	Collation string
}

var expectedTerminalReleaseTable = terminalReleaseTable{
	Engine: "innodb", Collation: "utf8mb4_0900_ai_ci",
}

var expectedTerminalReleaseColumns = []terminalReleaseColumn{
	{Name: "id", ColumnType: "bigint unsigned", Nullable: "NO", Extra: "auto_increment"},
	{Name: "match_id", ColumnType: "bigint unsigned", Nullable: "NO"},
	{Name: "allocation_id", ColumnType: "char(36)", Nullable: "NO", Charset: "ascii", Collation: "ascii_bin"},
	{Name: "ds_pod_name", ColumnType: "varchar(253)", Nullable: "NO", Charset: "ascii", Collation: "ascii_bin"},
	{Name: "gameserver_uid", ColumnType: "varchar(64)", Nullable: "NO", Charset: "ascii", Collation: "ascii_bin"},
	{Name: "instance_epoch", ColumnType: "int unsigned", Nullable: "NO"},
	{Name: "auth_gen", ColumnType: "bigint unsigned", Nullable: "NO"},
	{Name: "auth_jti", ColumnType: "varchar(256)", Nullable: "NO", Charset: "ascii", Collation: "ascii_bin"},
	{Name: "auth_exp_ms", ColumnType: "bigint", Nullable: "NO"},
	{Name: "auth_kid", ColumnType: "varchar(128)", Nullable: "NO", Charset: "ascii", Collation: "ascii_bin"},
	{Name: "auth_token_sha256", ColumnType: "char(64)", Nullable: "NO", Charset: "ascii", Collation: "ascii_bin"},
	{Name: "auth_writer_epoch", ColumnType: "int unsigned", Nullable: "NO"},
	{Name: "authorized_at_ms", ColumnType: "bigint", Nullable: "NO"},
	{Name: "release_after_ms", ColumnType: "bigint", Nullable: "NO"},
	{Name: "released_at_ms", ColumnType: "bigint", Nullable: "NO", Default: "0"},
	{Name: "created_at_ms", ColumnType: "bigint", Nullable: "NO"},
}

var expectedTerminalReleaseIndexes = []terminalReleaseIndex{
	{Name: "PRIMARY", NonUnique: 0, Sequence: 1, Column: "id"},
	{Name: "idx_terminal_release_due", NonUnique: 1, Sequence: 1, Column: "release_after_ms"},
	{Name: "idx_terminal_release_due", NonUnique: 1, Sequence: 2, Column: "id"},
	{Name: "uk_terminal_release_match", NonUnique: 0, Sequence: 1, Column: "match_id"},
}

// ValidateTerminalReleaseSchema 是 Model-B battle_result 注册 capability 前的机械迁移门。
// 它不只检查“表存在”，还精确核对全部列与两个 fencing 索引，避免半迁移实例先接结算。
func (r *MySQLBattleRepo) ValidateTerminalReleaseSchema(ctx context.Context) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("terminal release schema: nil mysql repo")
	}
	columns, err := loadTerminalReleaseColumns(ctx, r.db)
	if err != nil {
		return err
	}
	if err := validateTerminalReleaseColumns(columns); err != nil {
		return err
	}
	table, err := loadTerminalReleaseTable(ctx, r.db)
	if err != nil {
		return err
	}
	if err := validateTerminalReleaseTable(table); err != nil {
		return err
	}
	indexes, err := loadTerminalReleaseIndexes(ctx, r.db)
	if err != nil {
		return err
	}
	return validateTerminalReleaseIndexes(indexes)
}

func loadTerminalReleaseTable(ctx context.Context, db *sql.DB) (terminalReleaseTable, error) {
	const q = `SELECT COALESCE(engine, ''), COALESCE(table_collation, '')
FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_name = 'terminal_release_outbox'`
	var table terminalReleaseTable
	if err := db.QueryRowContext(ctx, q).Scan(&table.Engine, &table.Collation); err != nil {
		return terminalReleaseTable{}, fmt.Errorf("terminal release schema query table: %w", err)
	}
	table.Engine = strings.ToLower(table.Engine)
	table.Collation = strings.ToLower(table.Collation)
	return table, nil
}

func loadTerminalReleaseColumns(ctx context.Context, db *sql.DB) ([]terminalReleaseColumn, error) {
	const q = `SELECT column_name, column_type, is_nullable,
COALESCE(character_set_name, ''), COALESCE(collation_name, ''),
COALESCE(column_default, ''), extra
FROM information_schema.columns
WHERE table_schema = DATABASE() AND table_name = 'terminal_release_outbox'
ORDER BY ordinal_position`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("terminal release schema query columns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []terminalReleaseColumn
	for rows.Next() {
		var c terminalReleaseColumn
		if err := rows.Scan(&c.Name, &c.ColumnType, &c.Nullable, &c.Charset, &c.Collation, &c.Default, &c.Extra); err != nil {
			return nil, fmt.Errorf("terminal release schema scan column: %w", err)
		}
		c.Name = strings.ToLower(c.Name)
		c.ColumnType = strings.ToLower(c.ColumnType)
		c.Nullable = strings.ToUpper(c.Nullable)
		c.Charset = strings.ToLower(c.Charset)
		c.Collation = strings.ToLower(c.Collation)
		c.Default = strings.ToLower(c.Default)
		c.Extra = strings.ToLower(c.Extra)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("terminal release schema iterate columns: %w", err)
	}
	return out, nil
}

func loadTerminalReleaseIndexes(ctx context.Context, db *sql.DB) ([]terminalReleaseIndex, error) {
	const q = `SELECT index_name, non_unique, seq_in_index, column_name
FROM information_schema.statistics
WHERE table_schema = DATABASE() AND table_name = 'terminal_release_outbox'
ORDER BY CASE WHEN index_name = 'PRIMARY' THEN 0 ELSE 1 END,
         BINARY index_name, seq_in_index`
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("terminal release schema query indexes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []terminalReleaseIndex
	for rows.Next() {
		var idx terminalReleaseIndex
		if err := rows.Scan(&idx.Name, &idx.NonUnique, &idx.Sequence, &idx.Column); err != nil {
			return nil, fmt.Errorf("terminal release schema scan index: %w", err)
		}
		out = append(out, idx)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("terminal release schema iterate indexes: %w", err)
	}
	return out, nil
}

func validateTerminalReleaseColumns(got []terminalReleaseColumn) error {
	if len(got) != len(expectedTerminalReleaseColumns) {
		return fmt.Errorf("terminal release schema columns=%d want=%d", len(got), len(expectedTerminalReleaseColumns))
	}
	for i, want := range expectedTerminalReleaseColumns {
		if got[i] != want {
			return fmt.Errorf("terminal release schema column[%d]=%+v want=%+v", i, got[i], want)
		}
	}
	return nil
}

func validateTerminalReleaseTable(got terminalReleaseTable) error {
	if got != expectedTerminalReleaseTable {
		return fmt.Errorf("terminal release schema table=%+v want=%+v", got, expectedTerminalReleaseTable)
	}
	return nil
}

func validateTerminalReleaseIndexes(got []terminalReleaseIndex) error {
	if len(got) != len(expectedTerminalReleaseIndexes) {
		return fmt.Errorf("terminal release schema indexes=%d want=%d", len(got), len(expectedTerminalReleaseIndexes))
	}
	for i, want := range expectedTerminalReleaseIndexes {
		if got[i] != want {
			return fmt.Errorf("terminal release schema index[%d]=%+v want=%+v", i, got[i], want)
		}
	}
	return nil
}
