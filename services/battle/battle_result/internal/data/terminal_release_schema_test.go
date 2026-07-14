package data

import (
	"strings"
	"testing"
)

func TestValidateTerminalReleaseSchemaSnapshotExact(t *testing.T) {
	columns := append([]terminalReleaseColumn(nil), expectedTerminalReleaseColumns...)
	indexes := append([]terminalReleaseIndex(nil), expectedTerminalReleaseIndexes...)
	if err := validateTerminalReleaseColumns(columns); err != nil {
		t.Fatalf("canonical columns rejected: %v", err)
	}
	if err := validateTerminalReleaseIndexes(indexes); err != nil {
		t.Fatalf("canonical indexes rejected: %v", err)
	}
	if err := validateTerminalReleaseTable(expectedTerminalReleaseTable); err != nil {
		t.Fatalf("canonical table metadata rejected: %v", err)
	}

	columns[11].ColumnType = "bigint unsigned"
	if err := validateTerminalReleaseColumns(columns); err == nil {
		t.Fatal("writer_epoch type drift was accepted")
	}
	columns = append([]terminalReleaseColumn(nil), expectedTerminalReleaseColumns...)
	columns[14].Default = ""
	if err := validateTerminalReleaseColumns(columns); err == nil {
		t.Fatal("released_at_ms zero default drift was accepted")
	}
	for _, mutant := range []terminalReleaseTable{
		{Engine: "myisam", Collation: expectedTerminalReleaseTable.Collation},
		{Engine: expectedTerminalReleaseTable.Engine, Collation: "utf8mb4_general_ci"},
	} {
		if err := validateTerminalReleaseTable(mutant); err == nil {
			t.Fatalf("unsafe table metadata accepted: %+v", mutant)
		}
	}
	indexes = append([]terminalReleaseIndex(nil), expectedTerminalReleaseIndexes...)
	indexes[2].Column = "match_id"
	if err := validateTerminalReleaseIndexes(indexes); err == nil {
		t.Fatal("due index drift was accepted")
	}
}

func TestTerminalReleaseDeleteSQLRequiresDurableReleasedPhase(t *testing.T) {
	normalized := strings.Join(strings.Fields(strings.ToLower(deleteTerminalReleaseOutboxSQL)), " ")
	if normalized != "delete from terminal_release_outbox where id = ? and released_at_ms > 0" {
		t.Fatalf("unsafe terminal outbox delete SQL: %q", normalized)
	}
}
