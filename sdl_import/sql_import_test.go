package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseColumnList(t *testing.T) {
	input := "`id`, `action`, `body_id`, `no_of_days`, `time`, `request_type`"
	cols := parseColumnList(input)
	expected := []string{"id", "action", "body_id", "no_of_days", "time", "request_type"}
	if len(cols) != len(expected) {
		t.Fatalf("expected %d cols, got %d: %v", len(expected), len(cols), cols)
	}
	for i, c := range cols {
		if c != expected[i] {
			t.Errorf("col[%d]: expected %q, got %q", i, expected[i], c)
		}
	}
}

func TestParseValuesFromTuple(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect []string
	}{
		{
			name:   "simple ints",
			input:  "(18, 1, 12, 0)",
			expect: []string{"18", "1", "12", "0"},
		},
		{
			name:   "strings and ints",
			input:  "(18, 1, 12, 0, '09:00:00', 6, 2, 0, 0, '0000-00-00 00:00:00', 0, '2022-12-03 09:18:22')",
			expect: []string{"18", "1", "12", "0", "'09:00:00'", "6", "2", "0", "0", "'0000-00-00 00:00:00'", "0", "'2022-12-03 09:18:22'"},
		},
		{
			name:   "escaped quotes",
			input:  `(46154, 'Sishir  Ranga', 'they didn\'t see much change')`,
			expect: []string{"46154", "'Sishir  Ranga'", `'they didn\'t see much change'`},
		},
		{
			name:   "NULL values",
			input:  "(1, NULL, 'test', NULL)",
			expect: []string{"1", "NULL", "'test'", "NULL"},
		},
		{
			name:   "negative numbers",
			input:  "(36, 2, 2, -3, '09:00:00')",
			expect: []string{"36", "2", "2", "-3", "'09:00:00'"},
		},
		{
			name:   "float values",
			input:  "(1, 2.9, 35)",
			expect: []string{"1", "2.9", "35"},
		},
		{
			name:   "string with comma inside",
			input:  "(1, 'Anna Nagar, Chennai', 139)",
			expect: []string{"1", "'Anna Nagar, Chennai'", "139"},
		},
		{
			name:   "string with parens inside",
			input:  `(1, 'Some (text) here', 2)`,
			expect: []string{"1", "'Some (text) here'", "2"},
		},
		{
			name:   "empty string",
			input:  "(1, '', 2)",
			expect: []string{"1", "''", "2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseValuesFromTuple(tt.input)
			if len(got) != len(tt.expect) {
				t.Fatalf("expected %d values, got %d: %v", len(tt.expect), len(got), got)
			}
			for i, v := range got {
				if v != tt.expect[i] {
					t.Errorf("value[%d]: expected %q, got %q", i, tt.expect[i], v)
				}
			}
		})
	}
}

func TestExtractTuples(t *testing.T) {
	tests := []struct {
		name  string
		input string
		count int
		first string
	}{
		{
			name:  "single tuple with semicolon",
			input: "(18, 1, 12, 0);",
			count: 1,
			first: "(18, 1, 12, 0)",
		},
		{
			name:  "multiple tuples",
			input: "(1, 'a'), (2, 'b'), (3, 'c');",
			count: 3,
			first: "(1, 'a')",
		},
		{
			name:  "tuple with comma in string",
			input: "(1, 'a, b'), (2, 'c');",
			count: 2,
			first: "(1, 'a, b')",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTuples(tt.input)
			if len(got) != tt.count {
				t.Fatalf("expected %d tuples, got %d: %v", tt.count, len(got), got)
			}
			if got[0] != tt.first {
				t.Errorf("first tuple: expected %q, got %q", tt.first, got[0])
			}
		})
	}
}

func TestBuildUpsertSQL(t *testing.T) {
	cols := []string{"id", "action", "name"}
	vals := []string{"1", "2", "'test'"}
	pkCols := []string{"id"}

	stmt := buildUpsertSQL("mydb", "mytable", cols, vals, nil, pkCols, nil)

	if !strings.Contains(stmt, "INSERT INTO `mydb`.`mytable`") {
		t.Error("missing INSERT INTO")
	}
	if !strings.Contains(stmt, "ON DUPLICATE KEY UPDATE") {
		t.Error("missing ON DUPLICATE KEY UPDATE")
	}
	// PK column should NOT be in the UPDATE clause
	if strings.Contains(stmt, "`id` = VALUES(`id`)") {
		t.Error("PK column 'id' should not be in UPDATE clause")
	}
	// Non-PK columns SHOULD be in UPDATE clause
	if !strings.Contains(stmt, "`action` = VALUES(`action`)") {
		t.Error("missing action in UPDATE clause")
	}
	if !strings.Contains(stmt, "`name` = VALUES(`name`)") {
		t.Error("missing name in UPDATE clause")
	}
	fmt.Println("Generated statement:", stmt)
}

func TestBuildUpsertSQL_AllPK(t *testing.T) {
	cols := []string{"id", "type"}
	vals := []string{"1", "'test'"}
	pkCols := []string{"id", "type"}

	stmt := buildUpsertSQL("mydb", "mytable", cols, vals, nil, pkCols, nil)

	if !strings.Contains(stmt, "INSERT IGNORE") {
		t.Error("all-PK table should use INSERT IGNORE")
	}
	if strings.Contains(stmt, "ON DUPLICATE KEY") {
		t.Error("all-PK table should NOT have ON DUPLICATE KEY")
	}
}

func TestBuildUpsertSQL_JSONColumn(t *testing.T) {
	cols := []string{"id", "data"}
	vals := []string{"1", "''"}
	pkCols := []string{"id"}
	jsonCols := map[string]bool{"data": true}

	stmt := buildUpsertSQL("mydb", "mytable", cols, vals, nil, pkCols, jsonCols)

	if !strings.Contains(stmt, "CAST('null' AS JSON)") {
		t.Error("empty string in JSON column should be CAST('null' AS JSON)")
	}
}

func TestBuildUpsertSQL_ColumnFilter(t *testing.T) {
	cols := []string{"id", "action", "removed_col"}
	vals := []string{"1", "2", "'old'"}
	pkCols := []string{"id"}
	mysqlCols := map[string]bool{"id": true, "action": true}

	stmt := buildUpsertSQL("mydb", "mytable", cols, vals, mysqlCols, pkCols, nil)

	if strings.Contains(stmt, "removed_col") {
		t.Error("removed_col should be filtered out")
	}
	if !strings.Contains(stmt, "`id`") || !strings.Contains(stmt, "`action`") {
		t.Error("valid columns should be present")
	}
}

func TestReInsertHeaderMultiLine(t *testing.T) {
	// Multi-line: VALUES at end with no data
	line := "INSERT INTO `action_request` (`id`, `action`, `body_id`) VALUES"
	matches := reInsertHeader.FindStringSubmatch(line)
	if matches == nil {
		t.Fatal("should match multi-line INSERT header")
	}
	if matches[1] != "action_request" {
		t.Errorf("table: expected 'action_request', got %q", matches[1])
	}
	if strings.TrimSpace(matches[3]) != "" {
		t.Errorf("trailing data should be empty for multi-line, got %q", matches[3])
	}
}

func TestReInsertHeaderSingleLine(t *testing.T) {
	// Single-line: VALUES followed by tuples
	line := "INSERT INTO `test_table` (`id`, `name`) VALUES (1, 'test'), (2, 'test2');"
	matches := reInsertHeader.FindStringSubmatch(line)
	if matches == nil {
		t.Fatal("should match single-line INSERT")
	}
	if matches[1] != "test_table" {
		t.Errorf("table: expected 'test_table', got %q", matches[1])
	}
	trailing := strings.TrimSpace(matches[3])
	if trailing == "" {
		t.Error("trailing data should not be empty for single-line INSERT")
	}
}

func TestMultiLineParsingFlow(t *testing.T) {
	// Simulate what the main loop does:
	// Line 1: INSERT INTO `tbl` (`id`, `name`) VALUES
	// Line 2: (1, 'Alice'),
	// Line 3: (2, 'Bob');

	header := "INSERT INTO `tbl` (`id`, `name`) VALUES"
	matches := reInsertHeader.FindStringSubmatch(header)
	if matches == nil {
		t.Fatal("header not matched")
	}
	tableName := matches[1]
	colList := parseColumnList(matches[2])
	trailing := strings.TrimSpace(matches[3])

	if tableName != "tbl" {
		t.Errorf("table: %q", tableName)
	}
	if len(colList) != 2 || colList[0] != "id" || colList[1] != "name" {
		t.Errorf("cols: %v", colList)
	}
	if trailing != "" {
		t.Errorf("should be empty trailing: %q", trailing)
	}

	// Simulate reading tuple lines
	tupleLines := []string{
		"(1, 'Alice'),",
		"(2, 'Bob');",
	}

	for _, line := range tupleLines {
		trimmed := strings.TrimSpace(line)
		tupleLine := strings.TrimRight(trimmed, ",; ")
		vals := parseValuesFromTuple(tupleLine)
		if vals == nil {
			t.Errorf("failed to parse tuple from: %q", line)
			continue
		}
		if len(vals) != 2 {
			t.Errorf("expected 2 values, got %d from %q: %v", len(vals), line, vals)
		}
	}
}

func TestParseValuesComplexText(t *testing.T) {
	// Real-world tuple from the backup with escaped quotes, commas in text
	tuple := `(46154, 'Sishir  Ranga', 'Kinjal Sanghvi ', 'Batul  Trainer', 'Other - Reason for sishir to not continue is.  His family is little orthodox. They had issues with his attention span , concentration power etc at school, they didn\'t see much change in him, they said they shall get back in a month.', '2025-07-01', '2025-07-31', 'OM_Franchisee', 'B80', 'Anna Nagar, Chennai', 139, 0, 1, '2019-06-26', 6.3, 74)`
	vals := parseValuesFromTuple(tuple)
	if vals == nil {
		t.Fatal("failed to parse complex tuple")
	}
	// Should be 16 values
	if len(vals) != 16 {
		t.Fatalf("expected 16 values, got %d: %v", len(vals), vals)
	}
	if vals[0] != "46154" {
		t.Errorf("val[0] = %q, want 46154", vals[0])
	}
	if vals[1] != "'Sishir  Ranga'" {
		t.Errorf("val[1] = %q", vals[1])
	}
	// The long text with commas, apostrophes should be one value
	if !strings.HasPrefix(vals[4], "'Other - Reason") {
		t.Errorf("val[4] should be the long text, got: %q", vals[4])
	}
	// Anna Nagar, Chennai (with comma inside) should be one value
	if vals[9] != "'Anna Nagar, Chennai'" {
		t.Errorf("val[9] = %q, want 'Anna Nagar, Chennai'", vals[9])
	}
	if vals[15] != "74" {
		t.Errorf("val[15] = %q, want 74", vals[15])
	}
}
