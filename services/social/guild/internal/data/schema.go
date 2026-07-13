package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// RequiredSchemaVersion 是 guild 新计数写路径要求的 pandora_social 最低迁移版本。
const RequiredSchemaVersion uint = 2

type schemaColumnMetadata struct {
	DataType   string
	ColumnType string
	Nullable   string
	Default    sql.NullString
}

type requiredSchemaMetadata struct {
	Columns                  map[string]schemaColumnMetadata
	PlayerGroupCountsPrimary []string
}

// ValidateRequiredSchema 在服务装配前检查计数列 / 计数表的完整物理契约，避免只凭同名列放行
// 错误类型、signedness、NULL/default 或复合主键，直到首个业务请求才暴露溢出或锁失效。
// fresh schema 无需 schema_migrations 表，因此这里校验实际结构，而不是只信迁移版本号。
func ValidateRequiredSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("校验 pandora_social schema: db 为空")
	}
	rows, err := db.QueryContext(ctx, `
SELECT TABLE_NAME, COLUMN_NAME, DATA_TYPE, COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT
FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = DATABASE()
  AND (
    (TABLE_NAME = 'guilds' AND COLUMN_NAME = 'pending_request_count')
    OR
    (TABLE_NAME = 'player_group_counts' AND COLUMN_NAME IN ('player_id', 'group_count'))
  )`)
	if err != nil {
		return fmt.Errorf("读取 pandora_social schema 元数据: %w", err)
	}
	metadata := requiredSchemaMetadata{
		Columns: make(map[string]schemaColumnMetadata, 3),
	}
	for rows.Next() {
		var tableName, columnName string
		var column schemaColumnMetadata
		if err := rows.Scan(
			&tableName,
			&columnName,
			&column.DataType,
			&column.ColumnType,
			&column.Nullable,
			&column.Default,
		); err != nil {
			_ = rows.Close()
			return fmt.Errorf("读取 pandora_social schema 列: %w", err)
		}
		metadata.Columns[tableName+"."+columnName] = column
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("遍历 pandora_social schema 列: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭 pandora_social schema 列结果: %w", err)
	}

	pkRows, err := db.QueryContext(ctx, `
SELECT COLUMN_NAME
FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = DATABASE()
  AND TABLE_NAME = 'player_group_counts'
  AND INDEX_NAME = 'PRIMARY'
ORDER BY SEQ_IN_INDEX`)
	if err != nil {
		return fmt.Errorf("读取 player_group_counts 主键: %w", err)
	}
	for pkRows.Next() {
		var columnName string
		if err := pkRows.Scan(&columnName); err != nil {
			_ = pkRows.Close()
			return fmt.Errorf("读取 player_group_counts 主键列: %w", err)
		}
		metadata.PlayerGroupCountsPrimary = append(metadata.PlayerGroupCountsPrimary, columnName)
	}
	if err := pkRows.Err(); err != nil {
		_ = pkRows.Close()
		return fmt.Errorf("遍历 player_group_counts 主键: %w", err)
	}
	if err := pkRows.Close(); err != nil {
		return fmt.Errorf("关闭 player_group_counts 主键结果: %w", err)
	}
	return validateRequiredSchemaMetadata(metadata)
}

func validateRequiredSchemaMetadata(metadata requiredSchemaMetadata) error {
	var violations []string
	checkRequiredColumn := func(
		key, dataType string,
		unsigned bool,
		defaultValid bool,
		defaultValue string,
	) {
		column, ok := metadata.Columns[key]
		if !ok {
			violations = append(violations, key+" 缺失")
			return
		}
		if !strings.EqualFold(column.DataType, dataType) {
			violations = append(violations,
				fmt.Sprintf("%s DATA_TYPE=%s，期望 %s", key, column.DataType, dataType))
		}
		actualUnsigned := strings.Contains(strings.ToLower(column.ColumnType), "unsigned")
		if actualUnsigned != unsigned {
			want := "signed"
			if unsigned {
				want = "unsigned"
			}
			violations = append(violations,
				fmt.Sprintf("%s COLUMN_TYPE=%s，期望 %s", key, column.ColumnType, want))
		}
		if !strings.EqualFold(column.Nullable, "NO") {
			violations = append(violations,
				fmt.Sprintf("%s IS_NULLABLE=%s，期望 NO", key, column.Nullable))
		}
		if column.Default.Valid != defaultValid ||
			(defaultValid && strings.TrimSpace(column.Default.String) != defaultValue) {
			actual := "NULL"
			if column.Default.Valid {
				actual = column.Default.String
			}
			want := "NULL"
			if defaultValid {
				want = defaultValue
			}
			violations = append(violations,
				fmt.Sprintf("%s COLUMN_DEFAULT=%s，期望 %s", key, actual, want))
		}
	}

	checkRequiredColumn("guilds.pending_request_count", "int", false, true, "0")
	checkRequiredColumn("player_group_counts.player_id", "bigint", true, false, "")
	checkRequiredColumn("player_group_counts.group_count", "int", false, true, "0")
	if len(metadata.PlayerGroupCountsPrimary) != 1 ||
		!strings.EqualFold(metadata.PlayerGroupCountsPrimary[0], "player_id") {
		violations = append(violations,
			fmt.Sprintf("player_group_counts PRIMARY KEY=%v，期望 [player_id]", metadata.PlayerGroupCountsPrimary))
	}

	if len(violations) == 0 {
		return nil
	}
	sort.Strings(violations)
	return fmt.Errorf(
		"pandora_social schema 不兼容：%s；请先执行 pandora_social 迁移至 version=%d",
		strings.Join(violations, "; "),
		RequiredSchemaVersion,
	)
}
