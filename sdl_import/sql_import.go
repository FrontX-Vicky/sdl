package main

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"
)

// ─── helpers (shared patterns from restore.go) ───

func getenvImport(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func buildMySQLDSNImport(user, pass, addr, dbName string) string {
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = pass
	cfg.Net = "tcp"
	cfg.Addr = addr
	cfg.DBName = dbName
	cfg.ParseTime = true
	cfg.MultiStatements = true
	return cfg.FormatDSN()
}

func logMemoryImport(label string) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	log.Printf("[mem] %s: alloc=%d MB, sys=%d MB", label, m.Alloc/1024/1024, m.Sys/1024/1024)
}

// ─── MySQL schema introspection ───

func detectPKColumnsMySQL(db *sql.DB, dbName string) map[string][]string {
	result := make(map[string][]string)
	rows, err := db.Query(
		`SELECT TABLE_NAME, COLUMN_NAME
		 FROM information_schema.key_column_usage
		 WHERE TABLE_SCHEMA = ? AND CONSTRAINT_NAME = 'PRIMARY'
		 ORDER BY TABLE_NAME, ORDINAL_POSITION`, dbName)
	if err != nil {
		log.Printf("WARNING: Cannot query info_schema for PKs: %v", err)
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			continue
		}
		result[tbl] = append(result[tbl], col)
	}
	return result
}

func detectJSONColumnsMySQL(db *sql.DB, dbName string) map[string]map[string]bool {
	result := make(map[string]map[string]bool)
	rows, err := db.Query(
		`SELECT TABLE_NAME, COLUMN_NAME
		 FROM information_schema.columns
		 WHERE TABLE_SCHEMA = ? AND DATA_TYPE = 'json'`, dbName)
	if err != nil {
		log.Printf("WARNING: Cannot query info_schema for JSON columns: %v", err)
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			continue
		}
		if result[tbl] == nil {
			result[tbl] = make(map[string]bool)
		}
		result[tbl][col] = true
	}
	return result
}

func detectTableColumnsMySQL(db *sql.DB, dbName string) map[string]map[string]bool {
	result := make(map[string]map[string]bool)
	rows, err := db.Query(
		`SELECT TABLE_NAME, COLUMN_NAME
		 FROM information_schema.columns
		 WHERE TABLE_SCHEMA = ?`, dbName)
	if err != nil {
		log.Printf("WARNING: Cannot query info_schema for columns: %v", err)
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			continue
		}
		if result[tbl] == nil {
			result[tbl] = make(map[string]bool)
		}
		result[tbl][col] = true
	}
	return result
}

func detectTableColumnOrderMySQL(db *sql.DB, dbName string) map[string][]string {
	result := make(map[string][]string)
	rows, err := db.Query(
		`SELECT TABLE_NAME, COLUMN_NAME
		 FROM information_schema.columns
		 WHERE TABLE_SCHEMA = ?
		 ORDER BY TABLE_NAME, ORDINAL_POSITION`, dbName)
	if err != nil {
		log.Printf("WARNING: Cannot query info_schema for ordered columns: %v", err)
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			continue
		}
		result[tbl] = append(result[tbl], col)
	}
	return result
}

type columnMeta struct {
	IsNullable bool
	HasDefault bool
	DefaultVal string
	DataType   string
	Extra      string
}

func detectColumnMetaMySQL(db *sql.DB, dbName string) map[string]map[string]columnMeta {
	result := make(map[string]map[string]columnMeta)
	rows, err := db.Query(
		`SELECT TABLE_NAME, COLUMN_NAME, IS_NULLABLE, COLUMN_DEFAULT, DATA_TYPE, EXTRA
		 FROM information_schema.columns
		 WHERE TABLE_SCHEMA = ?`, dbName)
	if err != nil {
		log.Printf("WARNING: Cannot query info_schema for column metadata: %v", err)
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, col, isNullable, dataType, extra string
		var def sql.NullString
		if err := rows.Scan(&tbl, &col, &isNullable, &def, &dataType, &extra); err != nil {
			continue
		}
		if result[tbl] == nil {
			result[tbl] = make(map[string]columnMeta)
		}
		result[tbl][col] = columnMeta{
			IsNullable: strings.EqualFold(isNullable, "YES"),
			HasDefault: def.Valid,
			DefaultVal: def.String,
			DataType:   strings.ToLower(dataType),
			Extra:      strings.ToLower(extra),
		}
	}
	return result
}

func detectExistingTables(db *sql.DB, dbName string) map[string]bool {
	result := make(map[string]bool)
	rows, err := db.Query(
		`SELECT TABLE_NAME FROM information_schema.tables WHERE TABLE_SCHEMA = ?`, dbName)
	if err != nil {
		log.Printf("WARNING: Cannot query tables: %v", err)
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var tbl string
		if err := rows.Scan(&tbl); err == nil {
			result[tbl] = true
		}
	}
	return result
}

// ─── SQL parsing ───

// reInsertHeader matches both:
// 1) INSERT INTO `table` (`col1`, `col2`) VALUES ...
// 2) INSERT INTO `table` VALUES ...
// and db-qualified variants: INSERT INTO `db`.`table` ...
var reInsertHeader = regexp.MustCompile(`(?i)^INSERT INTO\s+` + "`" + `(?:([^` + "`" + `]+)` + "`" + `\.)?([^` + "`" + `]+)` + "`" + `(?:\s*\(([^)]*)\))?\s+VALUES\s*(.*)$`)

// reUseDB matches: USE `db`; or USE db;
var reUseDB = regexp.MustCompile(`(?i)^USE\s+` + "`" + `?([^` + "`" + `\s;]+)` + "`" + `?\s*;?\s*$`)

type insertHeader struct {
	sourceDB   string
	table      string
	columns    []string
	hasColumns bool
	trailing   string
}

func parseInsertHeader(line string) (insertHeader, bool) {
	m := reInsertHeader.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return insertHeader{}, false
	}
	h := insertHeader{
		sourceDB: strings.TrimSpace(m[1]),
		table:    strings.TrimSpace(m[2]),
		trailing: strings.TrimSpace(m[4]),
	}
	if strings.TrimSpace(m[3]) != "" {
		h.columns = parseColumnList(m[3])
		h.hasColumns = true
	}
	return h, true
}

func parseUseDB(line string) string {
	m := reUseDB.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// parseColumnList extracts column names from "`col1`, `col2`, ..."
func parseColumnList(raw string) []string {
	parts := strings.Split(raw, ",")
	cols := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "`")
		if p != "" {
			cols = append(cols, p)
		}
	}
	return cols
}

// parseValuesFromTuple extracts individual values from "(val1, val2, val3)"
// Returns string values exactly as-is from SQL (e.g., "'text'", "123", "NULL")
func parseValuesFromTuple(tuple string) []string {
	// Remove outer parentheses
	tuple = strings.TrimSpace(tuple)
	if len(tuple) < 2 || tuple[0] != '(' || tuple[len(tuple)-1] != ')' {
		return nil
	}
	inner := tuple[1 : len(tuple)-1]

	var values []string
	inString := false
	stringChar := byte(0)
	escaped := false
	start := 0

	for i := 0; i < len(inner); i++ {
		c := inner[i]

		if escaped {
			escaped = false
			continue
		}

		if c == '\\' && inString {
			escaped = true
			continue
		}

		if inString {
			if c == stringChar {
				inString = false
			}
			continue
		}

		switch c {
		case '\'', '"':
			inString = true
			stringChar = c
		case ',':
			values = append(values, strings.TrimSpace(inner[start:i]))
			start = i + 1
		}
	}
	// Last value
	values = append(values, strings.TrimSpace(inner[start:]))
	return values
}

// extractTuples parses "(v1,v2),(v3,v4);" or "(v1,v2),(v3,v4)," from inline data.
// Handles quoted strings, escaped chars, and nested parentheses correctly.
func extractTuples(data string) []string {
	data = strings.TrimRight(data, "; \t\r\n")

	var tuples []string
	depth := 0
	inString := false
	stringChar := byte(0)
	escaped := false
	start := -1

	for i := 0; i < len(data); i++ {
		c := data[i]

		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if inString {
			if c == stringChar {
				inString = false
			}
			continue
		}
		switch c {
		case '\'', '"':
			inString = true
			stringChar = c
		case '(':
			if depth == 0 {
				start = i
			}
			depth++
		case ')':
			depth--
			if depth == 0 && start >= 0 {
				tuples = append(tuples, data[start:i+1])
				start = -1
			}
		}
	}
	return tuples
}

func escapeSQLLiteralImport(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "\\'")
	s = strings.ReplaceAll(s, "\x00", "\\0")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\x1a", "\\Z")
	return s
}

func isNumericTypeImport(t string) bool {
	switch t {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "numeric", "float", "double", "real", "bit", "bool", "boolean":
		return true
	default:
		return false
	}
}

func defaultLiteralImport(meta columnMeta) string {
	if meta.HasDefault {
		d := strings.TrimSpace(meta.DefaultVal)
		if strings.EqualFold(d, "NULL") {
			return ""
		}
		ld := strings.ToLower(d)
		if strings.HasPrefix(ld, "current_timestamp") || strings.HasPrefix(ld, "now()") || strings.HasPrefix(ld, "uuid()") {
			return d
		}
		if isNumericTypeImport(meta.DataType) {
			if _, err := strconv.ParseFloat(d, 64); err == nil {
				return d
			}
		}
		return "'" + escapeSQLLiteralImport(d) + "'"
	}

	if strings.Contains(meta.Extra, "auto_increment") {
		return "NULL"
	}

	switch meta.DataType {
	case "json":
		return "CAST('null' AS JSON)"
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "numeric", "float", "double", "real", "bit", "bool", "boolean":
		return "0"
	case "date":
		return "'1970-01-01'"
	case "datetime", "timestamp":
		return "'1970-01-01 00:00:00'"
	case "time":
		return "'00:00:00'"
	case "year":
		return "1970"
	default:
		return "''"
	}
}

func coerceNullValueImport(col string, val string, colMeta map[string]columnMeta) string {
	if !strings.EqualFold(strings.TrimSpace(val), "NULL") {
		return val
	}
	if colMeta == nil {
		return val
	}
	meta, ok := colMeta[col]
	if !ok || meta.IsNullable {
		return val
	}
	replacement := defaultLiteralImport(meta)
	if replacement == "" {
		return val
	}
	return replacement
}

// buildUpsertSQL generates INSERT ... ON DUPLICATE KEY UPDATE for a single row.
// backupCols is the full column list from the dump.
// values is the full value list from the dump.
// mysqlCols filters which columns exist in target MySQL.
// pkCols lists PK columns (excluded from the UPDATE clause).
func buildUpsertSQL(dbName, tableName string, backupCols []string, values []string,
	mysqlCols map[string]bool, pkCols []string, jsonCols map[string]bool, colMeta map[string]columnMeta) string {

	pkSet := make(map[string]bool, len(pkCols))
	for _, pk := range pkCols {
		pkSet[pk] = true
	}

	var colParts, valParts, updateParts []string
	for i, col := range backupCols {
		if i >= len(values) {
			break
		}
		// Skip columns that don't exist in MySQL
		if mysqlCols != nil && !mysqlCols[col] {
			continue
		}
		val := values[i]
		val = coerceNullValueImport(col, val, colMeta)
		// For JSON columns, empty string is invalid — use CAST('null' AS JSON)
		if jsonCols[col] && val == "''" {
			val = "CAST('null' AS JSON)"
		}
		colParts = append(colParts, "`"+col+"`")
		valParts = append(valParts, val)
		if !pkSet[col] {
			updateParts = append(updateParts, fmt.Sprintf("`%s` = VALUES(`%s`)", col, col))
		}
	}

	if len(colParts) == 0 {
		return ""
	}

	if len(updateParts) == 0 {
		// All columns are PK — just INSERT IGNORE
		return fmt.Sprintf("INSERT IGNORE INTO `%s`.`%s` (%s) VALUES (%s);",
			dbName, tableName,
			strings.Join(colParts, ", "),
			strings.Join(valParts, ", "))
	}

	return fmt.Sprintf("INSERT INTO `%s`.`%s` (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s;",
		dbName, tableName,
		strings.Join(colParts, ", "),
		strings.Join(valParts, ", "),
		strings.Join(updateParts, ", "))
}

// ─── main ───

func main() {
	sqlFile := flag.String("file", "", "Path to .sql backup file [required]")
	dbName := flag.String("db", "", "Target MySQL database name [required]")
	output := flag.String("output", "", "Output SQL file path (default: import_<db>_<timestamp>.sql)")
	execute := flag.Bool("execute", false, "Execute SQL against MySQL directly")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN override: user:pass@tcp(host:port)/dbname")
	dryRun := flag.Bool("dry-run", false, "Preview SQL on stdout without writing file or executing")
	continueOnErr := flag.Bool("continue-on-error", false, "Continue if a statement fails during --execute")
	skipTables := flag.String("skip-tables", "", "Comma-separated table names to skip")
	onlyTables := flag.String("only-tables", "", "Comma-separated table names to import (empty = all)")
	skipCreate := flag.Bool("skip-create", true, "Skip CREATE TABLE statements (default: true, tables must exist)")
	flag.Parse()

	// Load .env
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../.env")
	if exe, err := os.Executable(); err == nil {
		dir := exe[:strings.LastIndex(exe, "/")+1]
		if dir == "" {
			dir = exe[:strings.LastIndex(exe, "\\")+1]
		}
		_ = godotenv.Load(dir + ".env")
	}

	if *sqlFile == "" || *dbName == "" {
		fmt.Println("Usage: sql_import --file <backup.sql> --db <DATABASE> [options]")
		fmt.Println()
		flag.PrintDefaults()
		fmt.Println("\nExamples:")
		fmt.Println("  sql_import --file backup.sql --db pf_TickleRight_9210")
		fmt.Println("  sql_import --file backup.sql --db pf_TickleRight_9210 --execute")
		fmt.Println("  sql_import --file backup.sql --db pf_TickleRight_9210 --dry-run")
		fmt.Println("  sql_import --file backup.sql --db pf_TickleRight_9210 --only-tables 'users,orders'")
		fmt.Println("  sql_import --file backup.sql --db pf_TickleRight_9210 --skip-tables 'logs,sessions'")
		os.Exit(1)
	}

	// Parse table filters
	skipTableSet := make(map[string]bool)
	if *skipTables != "" {
		for _, t := range strings.Split(*skipTables, ",") {
			skipTableSet[strings.TrimSpace(t)] = true
		}
	}
	onlyTableSet := make(map[string]bool)
	if *onlyTables != "" {
		for _, t := range strings.Split(*onlyTables, ",") {
			onlyTableSet[strings.TrimSpace(t)] = true
		}
	}

	// Open SQL file
	f, err := os.Open(*sqlFile)
	if err != nil {
		log.Fatalf("Cannot open SQL file: %v", err)
	}
	defer f.Close()

	fi, _ := f.Stat()
	log.Printf("SQL file: %s (%.2f MB)", *sqlFile, float64(fi.Size())/1024/1024)

	// ─── Connect to MySQL and load schema ───
	var dsn string
	if *mysqlDSN != "" {
		dsn = *mysqlDSN
	} else {
		user := getenvImport("MYSQL_USER", "")
		pass := os.Getenv("MYSQL_PASS")
		addr := getenvImport("MYSQL_ADDR", "127.0.0.1:3306")
		if user != "" {
			dsn = buildMySQLDSNImport(user, pass, addr, *dbName)
		}
	}

	if dsn == "" {
		log.Fatal("MySQL connection required. Set MYSQL_USER/MYSQL_PASS/MYSQL_ADDR env vars or use --mysql-dsn")
	}

	log.Printf("Connecting to MySQL...")
	mysqlDB, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("MySQL open: %v", err)
	}
	defer mysqlDB.Close()
	if err := mysqlDB.Ping(); err != nil {
		log.Fatalf("MySQL ping: %v", err)
	}

	// Load schema
	log.Printf("Loading MySQL schema for %s...", *dbName)
	pkColumns := detectPKColumnsMySQL(mysqlDB, *dbName)
	jsonCols := detectJSONColumnsMySQL(mysqlDB, *dbName)
	tableCols := detectTableColumnsMySQL(mysqlDB, *dbName)
	tableColOrder := detectTableColumnOrderMySQL(mysqlDB, *dbName)
	tableColMeta := detectColumnMetaMySQL(mysqlDB, *dbName)
	existingTables := detectExistingTables(mysqlDB, *dbName)

	{
		pkCount, jsonCount, colCount := 0, 0, 0
		for _, cols := range pkColumns {
			pkCount += len(cols)
		}
		for _, cols := range jsonCols {
			jsonCount += len(cols)
		}
		for _, cols := range tableCols {
			colCount += len(cols)
		}
		log.Printf("Schema loaded: %d tables, %d PK columns, %d JSON columns, %d total columns",
			len(existingTables), pkCount, jsonCount, colCount)
		if len(tableColOrder) == 0 {
			log.Printf("WARNING: Could not load ordered column metadata; VALUES-only INSERT statements may be skipped")
		}
		if len(tableColMeta) == 0 {
			log.Printf("WARNING: Could not load NOT NULL/default metadata; NULL coercion is disabled")
		}
	}

	// ─── Output setup ───
	outPath := *output
	if outPath == "" {
		outPath = fmt.Sprintf("import_%s_%s.sql", *dbName, time.Now().Format("20060102_150405"))
	}

	var writer *bufio.Writer
	var outFile *os.File

	if *dryRun {
		writer = bufio.NewWriterSize(os.Stdout, 256*1024)
	} else {
		outFile, err = os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			log.Fatalf("Create output file: %v", err)
		}
		defer outFile.Close()
		writer = bufio.NewWriterSize(outFile, 8*1024*1024)
	}

	// Header
	fmt.Fprintf(writer, "-- ============================================================\n")
	fmt.Fprintf(writer, "-- SQL Import — Upsert from phpMyAdmin backup\n")
	fmt.Fprintf(writer, "-- Generated: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(writer, "-- Source:    %s\n", *sqlFile)
	fmt.Fprintf(writer, "-- Database:  %s\n", *dbName)
	fmt.Fprintf(writer, "-- ============================================================\n\n")
	fmt.Fprintf(writer, "SET @OLD_SQL_MODE = @@SQL_MODE;\n")
	fmt.Fprintf(writer, "SET SQL_MODE = 'NO_AUTO_VALUE_ON_ZERO';\n")
	fmt.Fprintf(writer, "SET @OLD_FOREIGN_KEY_CHECKS = @@FOREIGN_KEY_CHECKS;\n")
	fmt.Fprintf(writer, "SET FOREIGN_KEY_CHECKS = 0;\n")
	fmt.Fprintf(writer, "SET NAMES utf8mb4;\n\n")

	// ─── MySQL execution setup ───
	if *execute {
		if _, err := mysqlDB.Exec("SET SQL_MODE = 'NO_AUTO_VALUE_ON_ZERO'"); err != nil {
			log.Printf("WARNING: SET SQL_MODE: %v", err)
		}
		if _, err := mysqlDB.Exec("SET FOREIGN_KEY_CHECKS = 0"); err != nil {
			log.Fatalf("Disable FK checks: %v", err)
		}
		if _, err := mysqlDB.Exec("SET NAMES utf8mb4"); err != nil {
			log.Printf("WARNING: SET NAMES utf8mb4: %v", err)
		}
		if _, err := mysqlDB.Exec("START TRANSACTION"); err != nil {
			log.Printf("WARNING: START TRANSACTION: %v", err)
		}
	}

	// ─── Stream SQL file ───
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024*1024), 64*1024*1024) // 64MB line buffer for huge INSERT lines

	var (
		totalRows       int
		totalStmts      int
		execApplied     int
		execFailed      int
		skippedRows     int
		skippedTables   = make(map[string]bool)
		missingTables   = make(map[string]bool)
		tablesImported  = make(map[string]int)
		dbSkipped       = make(map[string]bool)
		noColsTables    = make(map[string]bool)
		missingOrder    = make(map[string]bool)
		lineNum         int
		inCreateTable   bool
		createDepth     int
		currentSourceDB string
	)

	const txBatchSize = 1000

	// processTuple handles one value tuple for a given INSERT context
	processTuple := func(tuple string, tblName string, filteredCols []string, validIdxs []int,
		pkCols []string, jsonColSet map[string]bool) {

		allVals := parseValuesFromTuple(tuple)
		if allVals == nil {
			skippedRows++
			return
		}

		// Filter values to only valid column indices
		var vals []string
		for _, idx := range validIdxs {
			if idx < len(allVals) {
				vals = append(vals, allVals[idx])
			}
		}

		stmt := buildUpsertSQL(*dbName, tblName, filteredCols, vals, nil, pkCols, jsonColSet, tableColMeta[tblName])
		if stmt == "" {
			skippedRows++
			return
		}

		fmt.Fprintf(writer, "%s\n", stmt)
		totalStmts++
		totalRows++
		tablesImported[tblName]++

		// Execute
		if *execute {
			if _, err := mysqlDB.Exec(stmt); err != nil {
				execFailed++
				if execFailed <= 50 {
					log.Printf("ERROR [%s] row %d: %v", tblName, totalRows, err)
				}
				if !*continueOnErr {
					mysqlDB.Exec("COMMIT")
					writer.Flush()
					log.Fatalf("Stopping. Use --continue-on-error to skip. Failed at line %d", lineNum)
				}
			} else {
				execApplied++
				if execApplied%txBatchSize == 0 {
					mysqlDB.Exec("COMMIT")
					mysqlDB.Exec("START TRANSACTION")
				}
			}
		}
	}

	// Multi-line INSERT state
	var (
		inInsert     bool     // currently reading value tuples
		curTable     string   // current INSERT table
		curCols      []string // filtered column list
		curValidIdxs []int    // valid column indices
		curPKCols    []string // PK columns for current table
		curJSONCols  map[string]bool
		insertSkip   bool // skip this INSERT block (table filtered/missing)
	)

	startTime := time.Now()

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		// Skip empty lines and comments (but only if not reading INSERT tuples)
		trimmed := strings.TrimSpace(line)
		if !inInsert {
			if trimmed == "" || strings.HasPrefix(trimmed, "--") || strings.HasPrefix(trimmed, "/*") {
				continue
			}

			if useDB := parseUseDB(trimmed); useDB != "" {
				currentSourceDB = useDB
				continue
			}

			// Track and skip CREATE TABLE blocks if --skip-create
			if *skipCreate {
				if strings.HasPrefix(trimmed, "CREATE TABLE") {
					inCreateTable = true
					createDepth = 0
				}
				if inCreateTable {
					createDepth += strings.Count(trimmed, "(") - strings.Count(trimmed, ")")
					if strings.HasSuffix(trimmed, ";") || (createDepth <= 0 && strings.Contains(trimmed, ")")) {
						inCreateTable = false
					}
					continue
				}
			}

			// Skip SET, USE, CREATE DATABASE and other non-INSERT statements
			upper := strings.ToUpper(trimmed)
			if strings.HasPrefix(upper, "SET ") ||
				strings.HasPrefix(upper, "CREATE DATABASE") ||
				strings.HasPrefix(upper, "/*!") ||
				strings.HasPrefix(upper, "DROP TABLE") ||
				strings.HasPrefix(upper, "LOCK TABLES") ||
				strings.HasPrefix(upper, "UNLOCK TABLES") ||
				strings.HasPrefix(upper, "ALTER TABLE") {
				continue
			}

			// Try to match INSERT INTO ... VALUES header
			if !strings.HasPrefix(upper, "INSERT INTO") {
				continue
			}

			h, ok := parseInsertHeader(trimmed)
			if !ok {
				continue
			}

			tblName := h.table
			trailing := h.trailing // data after VALUES (may be empty)
			sourceDB := h.sourceDB
			if sourceDB == "" {
				sourceDB = currentSourceDB
			}

			// Apply table filters
			insertSkip = false
			if sourceDB != "" && !strings.EqualFold(sourceDB, *dbName) {
				insertSkip = true
				if !dbSkipped[sourceDB] {
					log.Printf("Skipping source database %s (target is %s)", sourceDB, *dbName)
					dbSkipped[sourceDB] = true
				}
			}
			if len(onlyTableSet) > 0 && !onlyTableSet[tblName] {
				insertSkip = true
			}
			if skipTableSet[tblName] {
				if !skippedTables[tblName] {
					log.Printf("Skipping table %s (--skip-tables)", tblName)
					skippedTables[tblName] = true
				}
				insertSkip = true
			}
			if !existingTables[tblName] {
				if !missingTables[tblName] {
					log.Printf("WARNING: Table %s does not exist in MySQL — skipping", tblName)
					missingTables[tblName] = true
				}
				insertSkip = true
			}

			// Prepare column mapping
			curTable = tblName
			curPKCols = pkColumns[tblName]
			curJSONCols = jsonCols[tblName]
			mysqlColSet := tableCols[tblName]

			curValidIdxs = nil
			curCols = nil
			if h.hasColumns {
				droppedCols := 0
				for i, col := range h.columns {
					if mysqlColSet != nil && !mysqlColSet[col] {
						droppedCols++
						continue
					}
					curValidIdxs = append(curValidIdxs, i)
					curCols = append(curCols, col)
				}
				if droppedCols > 0 && tablesImported[tblName] == 0 {
					log.Printf("  Table %s: dropped %d column(s) not in MySQL schema", tblName, droppedCols)
				}
			} else {
				curCols = append(curCols, tableColOrder[tblName]...)
				if len(curCols) == 0 {
					insertSkip = true
					if !missingOrder[tblName] {
						log.Printf("WARNING: Table %s has VALUES-only INSERT but ordered columns are unavailable — skipping", tblName)
						missingOrder[tblName] = true
					}
				}
				for i := range curCols {
					curValidIdxs = append(curValidIdxs, i)
				}
				if len(curCols) > 0 && !noColsTables[tblName] {
					log.Printf("  Table %s: using MySQL ordinal column mapping for VALUES-only INSERT", tblName)
					noColsTables[tblName] = true
				}
			}

			if len(curCols) == 0 {
				insertSkip = true
			}

			if !h.hasColumns && mysqlColSet != nil {
				// curCols already from MySQL schema order; keep mysqlColSet referenced for consistency
				_ = mysqlColSet
			}

			// If there's data after VALUES on the same line (single-line format)
			if trailing != "" && !insertSkip {
				// Extract tuples from inline data
				tuples := extractTuples(trailing)
				for _, tuple := range tuples {
					processTuple(tuple, curTable, curCols, curValidIdxs,
						curPKCols, curJSONCols)
				}
				// If trailing ends with ; we're done with this INSERT
				if strings.HasSuffix(strings.TrimSpace(trailing), ";") {
					continue
				}
			}

			// Enter multi-line INSERT mode
			inInsert = true
			continue
		}

		// === In multi-line INSERT VALUES mode ===
		// Each line is a value tuple: (val1, val2, ...),  or  (val1, val2, ...);
		if trimmed == "" {
			continue
		}

		// Check for end of INSERT block
		isLast := strings.HasSuffix(trimmed, ";")

		if !insertSkip {
			// Clean trailing comma or semicolon from the tuple line
			tupleLine := strings.TrimRight(trimmed, ",; ")
			if tupleLine != "" {
				processTuple(tupleLine, curTable, curCols, curValidIdxs,
					curPKCols, curJSONCols)
			}
		}

		if isLast {
			inInsert = false
		}

		// Progress
		if totalRows%100000 == 0 && totalRows > 0 {
			elapsed := time.Since(startTime)
			logMemoryImport(fmt.Sprintf("line %d, %d rows", lineNum, totalRows))
			log.Printf("  Progress: %d rows processed (%d stmts, %d tables) [%s elapsed]",
				totalRows, totalStmts, len(tablesImported), elapsed.Round(time.Second))
			writer.Flush()
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("WARNING: Scanner error at line %d: %v (file may be truncated)", lineNum, err)
	}

	// Footer
	fmt.Fprintf(writer, "\nSET FOREIGN_KEY_CHECKS = @OLD_FOREIGN_KEY_CHECKS;\n")
	fmt.Fprintf(writer, "SET SQL_MODE = @OLD_SQL_MODE;\n")
	fmt.Fprintf(writer, "-- Import complete: %d rows, %d statements, %d tables\n", totalRows, totalStmts, len(tablesImported))
	writer.Flush()

	// Finalize MySQL
	if *execute {
		mysqlDB.Exec("COMMIT")
		mysqlDB.Exec("SET FOREIGN_KEY_CHECKS = @OLD_FOREIGN_KEY_CHECKS")
		mysqlDB.Exec("SET SQL_MODE = @OLD_SQL_MODE")
	}

	elapsed := time.Since(startTime)

	if *dryRun {
		log.Printf("Dry run complete: %d rows parsed", totalRows)
		return
	}

	log.Printf("")
	log.Printf("========== IMPORT SUMMARY ==========")
	log.Printf("Source file:   %s", *sqlFile)
	log.Printf("Target DB:     %s", *dbName)
	log.Printf("Rows parsed:   %d", totalRows)
	log.Printf("Rows skipped:  %d", skippedRows)
	log.Printf("Statements:    %d", totalStmts)
	log.Printf("Tables:        %d", len(tablesImported))
	if len(missingTables) > 0 {
		log.Printf("Missing tables: %d (not in MySQL, skipped)", len(missingTables))
		names := make([]string, 0, len(missingTables))
		for t := range missingTables {
			names = append(names, t)
		}
		sort.Strings(names)
		for _, t := range names {
			log.Printf("  - %s", t)
		}
	}
	if *execute {
		log.Printf("Executed:      %d applied, %d failed", execApplied, execFailed)
	} else {
		log.Printf("SQL file:      %s", outPath)
		log.Printf("To apply: mysql -u root -p %s < %s", *dbName, outPath)
		log.Printf("Or re-run with: --execute")
	}
	log.Printf("Elapsed:       %s", elapsed.Round(time.Second))
	log.Printf("====================================")
	logMemoryImport("done")
}
