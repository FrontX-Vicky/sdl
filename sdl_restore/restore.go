package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Delta represents a column change with from/to values
type Delta struct {
	F any `bson:"f,omitempty"`
	T any `bson:"t,omitempty"`
}

// Meta holds event metadata
type Meta struct {
	DB  string `bson:"db"`
	Tbl string `bson:"tbl"`
	PK  any    `bson:"pk"`
}

// EventDoc represents a single audit event from MongoDB
type EventDoc struct {
	ID   string           `bson:"_id"`
	TS   time.Time        `bson:"ts"`
	OP   string           `bson:"op"`
	Meta Meta             `bson:"meta"`
	Chg  map[string]Delta `bson:"chg,omitempty"`
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// buildMySQLDSN safely constructs a DSN that handles special characters in passwords
func buildMySQLDSN(user, pass, addr, dbName string) string {
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = pass
	cfg.Net = "tcp"
	cfg.Addr = addr
	cfg.DBName = dbName
	cfg.ParseTime = true
	return cfg.FormatDSN()
}

func parseDate(s string) (time.Time, error) {
	formats := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse date %q (use YYYY-MM-DD or YYYY-MM-DD HH:MM:SS)", s)
}

// mergeDateTime replaces the time portion of base with the HH:MM:SS in timeStr.
// base must be non-zero. timeStr must be in HH:MM:SS format.
// The returned time is in UTC, matching the UTC normalisation applied by parseDate.
func mergeDateTime(base time.Time, timeStr string) (time.Time, error) {
	if base.IsZero() {
		return time.Time{}, fmt.Errorf("base date must not be zero")
	}
	combined := base.UTC().Format("2006-01-02") + " " + timeStr
	t, err := parseDate(combined)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time %q: must be HH:MM:SS", timeStr)
	}
	return t, nil
}

func opName(op string) string {
	switch op {
	case "i":
		return "INSERT"
	case "u":
		return "UPDATE"
	case "d":
		return "DELETE"
	default:
		return strings.ToUpper(op)
	}
}

// escapeString escapes a string for safe embedding in MySQL SQL literals
func escapeString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "\\'")
	s = strings.ReplaceAll(s, "\x00", "\\0")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\x1a", "\\Z")
	return s
}

// sqlValue converts a Go/BSON value to its MySQL SQL literal representation
func sqlValue(v any) string {
	if v == nil {
		return "NULL"
	}
	switch val := v.(type) {
	case string:
		return "'" + escapeString(val) + "'"
	case int:
		return fmt.Sprintf("%d", val)
	case int32:
		return fmt.Sprintf("%d", val)
	case int64:
		return fmt.Sprintf("%d", val)
	case float32:
		return fmt.Sprintf("%g", val)
	case float64:
		return fmt.Sprintf("%g", val)
	case bool:
		if val {
			return "1"
		}
		return "0"
	case time.Time:
		return "'" + val.Format("2006-01-02 15:04:05") + "'"
	case primitive.DateTime:
		return "'" + val.Time().Format("2006-01-02 15:04:05") + "'"
	case primitive.Binary:
		if len(val.Data) == 0 {
			return "''"
		}
		// If binary data is valid UTF-8, treat as string (handles JSON columns)
		if utf8.Valid(val.Data) {
			return "'" + escapeString(string(val.Data)) + "'"
		}
		return "X'" + hex.EncodeToString(val.Data) + "'"
	case primitive.ObjectID:
		return "'" + val.Hex() + "'"
	case []byte:
		if len(val) == 0 {
			return "''"
		}
		if utf8.Valid(val) {
			return "'" + escapeString(string(val)) + "'"
		}
		return "X'" + hex.EncodeToString(val) + "'"
	case bson.M, bson.D, bson.A:
		// BSON documents/arrays → JSON string (for MySQL JSON columns)
		b, err := json.Marshal(val)
		if err != nil {
			return "'" + escapeString(fmt.Sprintf("%v", val)) + "'"
		}
		return "'" + escapeString(string(b)) + "'"
	case map[string]any:
		b, err := json.Marshal(val)
		if err != nil {
			return "'" + escapeString(fmt.Sprintf("%v", val)) + "'"
		}
		return "'" + escapeString(string(b)) + "'"
	case []any:
		b, err := json.Marshal(val)
		if err != nil {
			return "'" + escapeString(fmt.Sprintf("%v", val)) + "'"
		}
		return "'" + escapeString(string(b)) + "'"
	default:
		s := fmt.Sprintf("%v", val)
		return "'" + escapeString(s) + "'"
	}
}

// detectPKColumnsFromMySQL queries information_schema for real PK columns (most reliable)
func detectPKColumnsFromMySQL(db *sql.DB, dbName string) map[string][]string {
	result := make(map[string][]string)

	rows, err := db.Query(
		`SELECT TABLE_NAME, COLUMN_NAME
		 FROM information_schema.key_column_usage
		 WHERE TABLE_SCHEMA = ? AND CONSTRAINT_NAME = 'PRIMARY'
		 ORDER BY TABLE_NAME, ORDINAL_POSITION`, dbName)
	if err != nil {
		log.Printf("WARNING: Cannot query information_schema for PKs: %v", err)
		return result
	}
	defer rows.Close()

	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			continue
		}
		key := dbName + "." + tbl
		result[key] = append(result[key], col)
	}

	return result
}

// detectJSONColumns queries information_schema for columns with JSON type
func detectJSONColumns(db *sql.DB, dbName string) map[string]map[string]bool {
	result := make(map[string]map[string]bool)

	rows, err := db.Query(
		`SELECT TABLE_NAME, COLUMN_NAME
		 FROM information_schema.columns
		 WHERE TABLE_SCHEMA = ? AND DATA_TYPE = 'json'`, dbName)
	if err != nil {
		log.Printf("WARNING: Cannot query information_schema for JSON columns: %v", err)
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

// detectTableColumns queries information_schema for all column names per table
func detectTableColumns(db *sql.DB, dbName string) map[string]map[string]bool {
	result := make(map[string]map[string]bool)

	rows, err := db.Query(
		`SELECT TABLE_NAME, COLUMN_NAME
		 FROM information_schema.columns
		 WHERE TABLE_SCHEMA = ?`, dbName)
	if err != nil {
		log.Printf("WARNING: Cannot query information_schema for columns: %v", err)
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

// detectPKColumns analyzes events to determine PK column names per table.
// It scans INSERT events (T values), DELETE events (F values), and UPDATE events (F/T values).
// override format: "id" (all tables), "tbl1:id,tbl2:uid" (per-table), "tbl:col1+col2" (composite)
func detectPKColumns(events []EventDoc, override string) map[string][]string {
	result := make(map[string][]string)

	// Handle manual override
	if override != "" {
		if strings.Contains(override, ":") {
			// Per-table: "tbl1:id,tbl2:uid+vid"
			pairs := strings.Split(override, ",")
			for _, pair := range pairs {
				parts := strings.SplitN(strings.TrimSpace(pair), ":", 2)
				if len(parts) == 2 {
					table := strings.TrimSpace(parts[0])
					cols := strings.Split(strings.TrimSpace(parts[1]), "+")
					for i := range cols {
						cols[i] = strings.TrimSpace(cols[i])
					}
					result[table] = cols
				}
			}
		} else {
			// Single column name for all tables
			result["*"] = []string{strings.TrimSpace(override)}
		}
	}

	// Auto-detect from ALL event types (INSERT, DELETE, UPDATE)
	candidates := make(map[string]map[string]int) // table -> column -> match count

	for _, event := range events {
		tableKey := event.Meta.DB + "." + event.Meta.Tbl
		if _, exists := result[tableKey]; exists {
			continue // Already resolved via override
		}

		pkStr := fmt.Sprint(event.Meta.PK)
		if pkStr == "" || pkStr == "<nil>" {
			continue
		}
		if _, ok := candidates[tableKey]; !ok {
			candidates[tableKey] = make(map[string]int)
		}

		// Collect candidate values to match against meta.pk
		// INSERT: T values contain the inserted data (including PK)
		// DELETE: F values contain the deleted row data (including PK)
		// UPDATE: both F and T may contain PK if it's in the change set
		matchValue := func(col string, val any) {
			if val == nil {
				return
			}
			valStr := fmt.Sprint(val)
			if strings.Contains(pkStr, "|") {
				// Composite PK
				for _, part := range strings.Split(pkStr, "|") {
					if valStr == part {
						candidates[tableKey][col]++
						return
					}
				}
			} else {
				if valStr == pkStr {
					candidates[tableKey][col]++
				}
			}
		}

		for col, delta := range event.Chg {
			switch event.OP {
			case "i":
				matchValue(col, delta.T) // INSERT: new values
			case "d":
				matchValue(col, delta.F) // DELETE: old values (all columns present)
			case "u":
				matchValue(col, delta.F) // UPDATE: old value of changed columns
				matchValue(col, delta.T) // UPDATE: new value of changed columns
			}
		}
	}

	// Resolve: pick column(s) with highest match count
	for table, cols := range candidates {
		if _, exists := result[table]; exists {
			continue
		}

		type colCount struct {
			name  string
			count int
		}
		var sorted []colCount
		for name, count := range cols {
			sorted = append(sorted, colCount{name, count})
		}
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].count > sorted[j].count
		})

		if len(sorted) == 0 {
			continue
		}

		// Check if composite PK from any event
		isComposite := false
		for _, event := range events {
			if event.Meta.DB+"."+event.Meta.Tbl == table {
				pkStr := fmt.Sprint(event.Meta.PK)
				if strings.Contains(pkStr, "|") {
					isComposite = true
					n := strings.Count(pkStr, "|") + 1
					pkCols := make([]string, 0, n)
					for i := 0; i < n && i < len(sorted); i++ {
						pkCols = append(pkCols, sorted[i].name)
					}
					result[table] = pkCols
				}
				break
			}
		}
		if !isComposite {
			result[table] = []string{sorted[0].name}
		}
	}

	// Apply wildcard override to tables not yet resolved
	if wildcard, ok := result["*"]; ok {
		for _, event := range events {
			tableKey := event.Meta.DB + "." + event.Meta.Tbl
			if _, exists := result[tableKey]; !exists {
				result[tableKey] = wildcard
			}
		}
		delete(result, "*")
	}

	// Warn about tables with no PK detection; try common column names as fallback
	seen := make(map[string]bool)
	for _, event := range events {
		tableKey := event.Meta.DB + "." + event.Meta.Tbl
		if seen[tableKey] {
			continue
		}
		seen[tableKey] = true

		if _, exists := result[tableKey]; !exists {
			// Try common PK column names from any event for this table
			for _, ev := range events {
				if ev.Meta.DB+"."+ev.Meta.Tbl != tableKey || len(ev.Chg) == 0 {
					continue
				}
				for _, common := range []string{"id", "ID", "Id"} {
					if _, has := ev.Chg[common]; has {
						result[tableKey] = []string{common}
						log.Printf("WARNING: Auto-detect PK failed for %s, guessing '%s'. Use --pk-column to override.", tableKey, common)
						break
					}
				}
				if _, exists := result[tableKey]; exists {
					break
				}
			}
			if _, exists := result[tableKey]; !exists {
				log.Printf("WARNING: Cannot detect PK column for %s. UPDATE/DELETE may fail. Use --pk-column.", tableKey)
			}
		}
	}

	return result
}

// buildWhereClause constructs a WHERE clause for PK matching
func buildWhereClause(pkCols []string, pkValue any) string {
	if len(pkCols) == 0 {
		return ""
	}

	pkStr := fmt.Sprint(pkValue)

	if len(pkCols) == 1 {
		return fmt.Sprintf("`%s` = %s", pkCols[0], sqlValue(pkValue))
	}

	// Composite PK: meta.pk is "val1|val2|..."
	parts := strings.Split(pkStr, "|")
	var conditions []string
	for i, col := range pkCols {
		val := ""
		if i < len(parts) {
			val = parts[i]
		}
		conditions = append(conditions, fmt.Sprintf("`%s` = '%s'", col, escapeString(val)))
	}
	return strings.Join(conditions, " AND ")
}

// sqlValueForCol returns the SQL literal for a value, adjusting for JSON columns
func sqlValueForCol(tbl, col string, v any, jsonCols map[string]map[string]bool) string {
	val := sqlValue(v)
	// For JSON columns, convert empty string to JSON null literal ('' is invalid JSON, SQL NULL may violate NOT NULL)
	if val == "''" && jsonCols[tbl] != nil && jsonCols[tbl][col] {
		return "CAST('null' AS JSON)"
	}
	return val
}

// filterColumns removes columns not present in the actual MySQL table schema.
// If tableCols is empty (no MySQL connection), all columns pass through.
func filterColumns(chg map[string]Delta, tbl string, tableCols map[string]map[string]bool) map[string]Delta {
	if len(tableCols) == 0 || tableCols[tbl] == nil {
		return chg
	}
	valid := tableCols[tbl]
	filtered := make(map[string]Delta, len(chg))
	for col, delta := range chg {
		if valid[col] {
			filtered[col] = delta
		}
	}
	return filtered
}

// eventToSQL converts an EventDoc to a SQL statement
func eventToSQL(event EventDoc, pkCols []string, jsonCols map[string]map[string]bool, tableCols map[string]map[string]bool) (string, error) {
	db := event.Meta.DB
	tbl := event.Meta.Tbl
	chg := filterColumns(event.Chg, tbl, tableCols)

	switch event.OP {
	case "i":
		return insertSQL(db, tbl, chg, jsonCols)
	case "u":
		return updateSQL(db, tbl, chg, pkCols, event.Meta.PK, jsonCols)
	case "d":
		return deleteSQL(db, tbl, pkCols, event.Meta.PK, chg, jsonCols)
	default:
		return "", fmt.Errorf("unknown operation: %s", event.OP)
	}
}

// insertSQL generates an INSERT ... ON DUPLICATE KEY UPDATE statement
func insertSQL(db, tbl string, chg map[string]Delta, jsonCols map[string]map[string]bool) (string, error) {
	if len(chg) == 0 {
		return "", fmt.Errorf("no columns in INSERT event")
	}

	// Sort columns for deterministic output
	cols := make([]string, 0, len(chg))
	for col := range chg {
		cols = append(cols, col)
	}
	sort.Strings(cols)

	var colNames, values, updates []string
	for _, col := range cols {
		colNames = append(colNames, "`"+col+"`")
		values = append(values, sqlValueForCol(tbl, col, chg[col].T, jsonCols))
		updates = append(updates, fmt.Sprintf("`%s` = VALUES(`%s`)", col, col))
	}

	return fmt.Sprintf("INSERT INTO `%s`.`%s` (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s;",
		db, tbl,
		strings.Join(colNames, ", "),
		strings.Join(values, ", "),
		strings.Join(updates, ", ")), nil
}

// updateSQL generates an UPDATE ... SET ... WHERE pk = ... statement
func updateSQL(db, tbl string, chg map[string]Delta, pkCols []string, pk any, jsonCols map[string]map[string]bool) (string, error) {
	if len(chg) == 0 {
		return "", fmt.Errorf("no columns in UPDATE event")
	}
	if len(pkCols) == 0 {
		return "", fmt.Errorf("unknown PK column for UPDATE — use --pk-column")
	}

	where := buildWhereClause(pkCols, pk)

	cols := make([]string, 0, len(chg))
	for col := range chg {
		cols = append(cols, col)
	}
	sort.Strings(cols)

	var setClauses []string
	for _, col := range cols {
		setClauses = append(setClauses, fmt.Sprintf("`%s` = %s", col, sqlValueForCol(tbl, col, chg[col].T, jsonCols)))
	}

	return fmt.Sprintf("UPDATE `%s`.`%s` SET %s WHERE %s;",
		db, tbl, strings.Join(setClauses, ", "), where), nil
}

// deleteSQL generates a DELETE ... WHERE pk = ... statement
func deleteSQL(db, tbl string, pkCols []string, pk any, chg map[string]Delta, jsonCols map[string]map[string]bool) (string, error) {
	if len(pkCols) == 0 {
		// Fallback: use ALL columns from chg (F values) in the WHERE clause
		if len(chg) > 0 {
			cols := make([]string, 0, len(chg))
			for col := range chg {
				cols = append(cols, col)
			}
			sort.Strings(cols)

			var conditions []string
			for _, col := range cols {
				conditions = append(conditions, fmt.Sprintf("`%s` = %s", col, sqlValueForCol(tbl, col, chg[col].F, jsonCols)))
			}
			return fmt.Sprintf("DELETE FROM `%s`.`%s` WHERE %s LIMIT 1;",
				db, tbl, strings.Join(conditions, " AND ")), nil
		}
		return "", fmt.Errorf("unknown PK column for DELETE — use --pk-column")
	}

	where := buildWhereClause(pkCols, pk)
	return fmt.Sprintf("DELETE FROM `%s`.`%s` WHERE %s;", db, tbl, where), nil
}

const maxMemoryBytes = 12 * 1024 * 1024 * 1024 // 12 GB hard limit (server has 16GB)

// checkMemory logs and panics if memory usage exceeds the limit
func checkMemory() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if m.Alloc > maxMemoryBytes {
		log.Fatalf("MEMORY LIMIT EXCEEDED: using %d MB (limit %d MB). Aborting to protect system.",
			m.Alloc/1024/1024, maxMemoryBytes/1024/1024)
	}
}

// logMemory periodically logs current memory usage
func logMemory(label string) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	log.Printf("[mem] %s: alloc=%d MB, sys=%d MB", label, m.Alloc/1024/1024, m.Sys/1024/1024)
}

// PK detection pass: lightweight scan using only meta fields
type pkScanDoc struct {
	OP   string           `bson:"op"`
	Meta Meta             `bson:"meta"`
	Chg  map[string]Delta `bson:"chg,omitempty"`
}

func main() {
	// --- Flags ---
	startDate := flag.String("start-date", "2026-05-27 09:30:00", "Start date UTC (YYYY-MM-DD or YYYY-MM-DD HH:MM:SS) [required]")
	startTimeFlag := flag.String("start-time", "", "Start time UTC (HH:MM:SS), combined with --start-date (overrides any time in --start-date)")
	endDate := flag.String("end-date", "2026-05-27 11:00:00", "End date UTC (YYYY-MM-DD or YYYY-MM-DD HH:MM:SS, optional)")
	endTimeFlag := flag.String("end-time", "", "End time UTC (HH:MM:SS), combined with --end-date (overrides any time in --end-date)")
	dbName := flag.String("db", "", "MySQL database name to filter [required]")
	tableName := flag.String("table", "", "Table name (empty = all tables)")
	pkColumn := flag.String("pk-column", "", "PK column override. Examples: 'id' | 'users:user_id,orders:order_id' | 'tbl:col1+col2'")
	output := flag.String("output", "", "Output SQL file path (default: restore_<db>_<timestamp>.sql)")
	execute := flag.Bool("execute", false, "Execute SQL against MySQL directly (in addition to writing file)")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN for --execute: user:pass@tcp(host:port)/dbname")
	dryRun := flag.Bool("dry-run", false, "Preview SQL on stdout without writing file or executing")
	continueOnErr := flag.Bool("continue-on-error", false, "Continue if a statement fails during --execute")
	flag.Parse()

	// Try .env in current dir, parent dir, and binary's dir
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../.env")
	if exe, err := os.Executable(); err == nil {
		dir := exe[:strings.LastIndex(exe, "/")+1]
		if dir == "" {
			dir = exe[:strings.LastIndex(exe, "\\")+1]
		}
		_ = godotenv.Load(dir + ".env")
	}

	// --- Validate ---
	if *startDate == "" || *dbName == "" {
		fmt.Println("Usage: sdl_restore --start-date <DATE> --db <DATABASE> [--table <TABLE>] [options]")
		fmt.Println()
		flag.PrintDefaults()
		fmt.Println("\nExamples:")
		fmt.Println("  sdl_restore --start-date 2026-03-01 --db myapp")
		fmt.Println("  sdl_restore --start-date 2026-03-01 --db myapp --table users")
		fmt.Println("  sdl_restore --start-date '2026-03-01 14:30:00' --db myapp --dry-run")
		fmt.Println("  sdl_restore --start-date 2026-03-01 --start-time 09:00:00 --end-date 2026-03-01 --end-time 17:30:00 --db myapp")
		fmt.Println("  sdl_restore --start-date 2026-03-01 --start-time 00:00:00 --end-date 2026-03-01 --end-time 23:59:59 --db myapp --dry-run")
		fmt.Println("  sdl_restore --start-date 2026-03-01 --db myapp --execute --mysql-dsn 'root:pass@tcp(127.0.0.1:3306)/myapp'")
		fmt.Println("  sdl_restore --start-date 2026-03-01 --db myapp --pk-column id")
		fmt.Println("  sdl_restore --start-date 2026-03-01 --db myapp --pk-column 'users:user_id,orders:order_id'")
		os.Exit(1)
	}

	startTime, err := parseDate(*startDate)
	if err != nil {
		log.Fatalf("Invalid --start-date: %v", err)
	}
	// Apply --start-time override (merges date from --start-date with the given HH:MM:SS)
	if *startTimeFlag != "" {
		startTime, err = mergeDateTime(startTime, *startTimeFlag)
		if err != nil {
			log.Fatalf("Invalid --start-time: %v", err)
		}
	}

	var endTime time.Time
	if *endDate != "" {
		endTime, err = parseDate(*endDate)
		if err != nil {
			log.Fatalf("Invalid --end-date: %v", err)
		}
	}
	// Apply --end-time override (merges date from --end-date with the given HH:MM:SS)
	if *endTimeFlag != "" {
		if *endDate == "" {
			// Default to same date as start when end-date is omitted
			endTime = startTime
		}
		endTime, err = mergeDateTime(endTime, *endTimeFlag)
		if err != nil {
			log.Fatalf("Invalid --end-time: %v", err)
		}
	}

	// --- Connect to MongoDB ---
	mongoURI := getenv("MONGO_URI", "mongodb://127.0.0.1:27017/?appName=audit")
	mongoDB := getenv("MONGO_DB", "audit")
	mongoColl := getenv("MONGO_COLL", "row_changes")

	log.Printf("Connecting to MongoDB %s ...", mongoURI)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().
		ApplyURI(mongoURI).
		SetConnectTimeout(10*time.Second))
	if err != nil {
		log.Fatalf("MongoDB connect: %v", err)
	}
	defer client.Disconnect(context.Background())

	if err := client.Ping(ctx, nil); err != nil {
		log.Fatalf("MongoDB ping: %v", err)
	}

	coll := client.Database(mongoDB).Collection(mongoColl)

	// --- Build MongoDB filter ---
	filter := bson.M{
		"meta.db": *dbName,
		"ts":      bson.M{"$gte": startTime},
	}
	if *tableName != "" {
		filter["meta.tbl"] = *tableName
	}
	if !endTime.IsZero() {
		filter["ts"].(bson.M)["$lte"] = endTime
	}

	tblDisplay := *tableName
	if tblDisplay == "" {
		tblDisplay = "ALL"
	}
	endDisplay := "now"
	if !endTime.IsZero() {
		endDisplay = endTime.Format("2006-01-02 15:04:05")
	}
	log.Printf("Fetching events: db=%s table=%s from=%s to=%s",
		*dbName, tblDisplay, startTime.Format("2006-01-02 15:04:05"), endDisplay)

	// =====================================================
	// PHASE 1: Fast counts via MongoDB aggregation
	// =====================================================
	log.Printf("Phase 1: Counting events via aggregation...")
	logMemory("before phase 1")

	aggCtx, aggCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer aggCancel()

	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: filter}},
		{{Key: "$group", Value: bson.M{
			"_id":   bson.M{"op": "$op", "tbl": "$meta.tbl"},
			"count": bson.M{"$sum": 1},
		}}},
	}
	aggCursor, err := coll.Aggregate(aggCtx, pipeline)
	if err != nil {
		log.Fatalf("MongoDB aggregation: %v", err)
	}

	opCounts := map[string]int{}
	tables := map[string]bool{}
	scanned := 0
	for aggCursor.Next(aggCtx) {
		var result struct {
			ID struct {
				OP  string `bson:"op"`
				Tbl string `bson:"tbl"`
			} `bson:"_id"`
			Count int `bson:"count"`
		}
		if err := aggCursor.Decode(&result); err != nil {
			continue
		}
		opCounts[result.ID.OP] += result.Count
		tables[*dbName+"."+result.ID.Tbl] = true
		scanned += result.Count
	}
	aggCursor.Close(aggCtx)

	if scanned == 0 {
		log.Println("No events found matching filters. Nothing to restore.")
		return
	}

	log.Printf("Found %d events to replay", scanned)
	log.Printf("  INSERT: %d | UPDATE: %d | DELETE: %d | Tables: %d",
		opCounts["i"], opCounts["u"], opCounts["d"], len(tables))

	// =====================================================
	// PHASE 2: PK detection (scan first 50K events only)
	// =====================================================
	log.Printf("Phase 2: Scanning first 50K events for PK detection...")

	scanCtx, scanCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer scanCancel()

	scanCursor, err := coll.Find(scanCtx, filter, options.Find().
		SetSort(bson.D{{Key: "ts", Value: 1}}).
		SetBatchSize(5000).
		SetProjection(bson.M{"op": 1, "meta": 1, "chg": 1}).
		SetLimit(50000))
	if err != nil {
		log.Fatalf("MongoDB PK scan query: %v", err)
	}

	var pkDetectEvents []EventDoc
	for scanCursor.Next(scanCtx) {
		var doc pkScanDoc
		if err := scanCursor.Decode(&doc); err != nil {
			continue
		}
		pkDetectEvents = append(pkDetectEvents, EventDoc{OP: doc.OP, Meta: doc.Meta, Chg: doc.Chg})
	}
	scanCursor.Close(scanCtx)
	log.Printf("  Scanned %d events for PK detection", len(pkDetectEvents))

	pkColumns := detectPKColumns(pkDetectEvents, *pkColumn)
	pkDetectEvents = nil
	runtime.GC()

	// =====================================================
	// PHASE 3: MySQL metadata (PKs, JSON columns, table existence)
	// =====================================================
	jsonCols := make(map[string]map[string]bool)
	tableCols := make(map[string]map[string]bool)
	existingTables := make(map[string]bool)

	var mysqlDB *sql.DB // kept open for --execute mode
	{
		var dsn string
		if *mysqlDSN != "" {
			dsn = *mysqlDSN
		} else {
			user := getenv("MYSQL_USER", "")
			pass := os.Getenv("MYSQL_PASS")
			addr := getenv("MYSQL_ADDR", "127.0.0.1:3306")
			if user != "" {
				dsn = buildMySQLDSN(user, pass, addr, *dbName)
			}
		}
		if dsn != "" {
			log.Printf("Querying MySQL for PKs, JSON columns, and table list...")
			mysqlDB, err = sql.Open("mysql", dsn)
			if err != nil {
				log.Printf("WARNING: MySQL open: %v (using event-based detection only)", err)
				mysqlDB = nil
			} else if err := mysqlDB.Ping(); err != nil {
				log.Printf("WARNING: MySQL ping: %v (using event-based detection only)", err)
				mysqlDB.Close()
				mysqlDB = nil
			}
			if mysqlDB != nil {
				// PK columns
				mysqlPKs := detectPKColumnsFromMySQL(mysqlDB, *dbName)
				enriched := 0
				for table, cols := range mysqlPKs {
					if _, exists := pkColumns[table]; !exists {
						pkColumns[table] = cols
						enriched++
						log.Printf("  PK (from MySQL): %s → [%s]", table, strings.Join(cols, ", "))
					}
				}
				if enriched > 0 {
					log.Printf("Resolved %d additional PK column(s) from MySQL", enriched)
				}
				// JSON columns
				jsonCols = detectJSONColumns(mysqlDB, *dbName)
				if len(jsonCols) > 0 {
					count := 0
					for _, cols := range jsonCols {
						count += len(cols)
					}
					log.Printf("Detected %d JSON column(s) across tables", count)
				}
				// All table columns (to filter out columns not in MySQL)
				tableCols = detectTableColumns(mysqlDB, *dbName)
				if len(tableCols) > 0 {
					count := 0
					for _, cols := range tableCols {
						count += len(cols)
					}
					log.Printf("Loaded schema: %d column(s) across %d tables", count, len(tableCols))
				}
				// Table existence
				rows, err := mysqlDB.Query(
					"SELECT TABLE_NAME FROM information_schema.tables WHERE TABLE_SCHEMA = ?", *dbName)
				if err == nil {
					for rows.Next() {
						var tName string
						if err := rows.Scan(&tName); err == nil {
							existingTables[*dbName+"."+tName] = true
						}
					}
					rows.Close()
					log.Printf("Found %d existing tables in MySQL", len(existingTables))
				}

				// If not executing, close MySQL now
				if !*execute {
					mysqlDB.Close()
					mysqlDB = nil
				}
			}
		} else {
			log.Printf("NOTE: No MySQL connection available. Set MYSQL_USER/MYSQL_PASS/MYSQL_ADDR or use --mysql-dsn.")
		}
	}

	for table, cols := range pkColumns {
		log.Printf("  PK: %s → [%s]", table, strings.Join(cols, ", "))
	}
	logMemory("after metadata")

	// =====================================================
	// PHASE 4: Single-pass stream → write SQL + execute
	// =====================================================
	log.Printf("Phase 4: Streaming events → SQL file%s...", func() string {
		if *execute {
			return " + MySQL execution"
		}
		return ""
	}())

	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer fetchCancel()

	cursor, err := coll.Find(fetchCtx, filter,
		options.Find().
			SetSort(bson.D{{Key: "ts", Value: 1}}).
			SetBatchSize(5000))
	if err != nil {
		log.Fatalf("MongoDB query: %v", err)
	}
	defer cursor.Close(fetchCtx)

	// Determine output target
	outPath := *output
	if outPath == "" {
		outPath = fmt.Sprintf("restore_%s_%s.sql", *dbName, time.Now().Format("20060102_150405"))
	}

	// For dry-run, write to stdout; otherwise write to file via buffered writer
	var writer *bufio.Writer
	var outFile *os.File

	if *dryRun {
		writer = bufio.NewWriterSize(os.Stdout, 256*1024)
	} else {
		outFile, err = os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			log.Fatalf("Create SQL file: %v", err)
		}
		defer outFile.Close()
		writer = bufio.NewWriterSize(outFile, 8*1024*1024) // 8MB write buffer
	}

	// Write header
	fmt.Fprintf(writer, "-- ============================================================\n")
	fmt.Fprintf(writer, "-- SDL Restore Script — Replay binlog events from MongoDB audit\n")
	fmt.Fprintf(writer, "-- Generated: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(writer, "-- Source:    MongoDB %s.%s\n", mongoDB, mongoColl)
	fmt.Fprintf(writer, "-- Database:  %s\n", *dbName)
	fmt.Fprintf(writer, "-- Table:     %s\n", tblDisplay)
	fmt.Fprintf(writer, "-- Period:    %s → %s\n", startTime.Format("2006-01-02 15:04:05"), endDisplay)
	fmt.Fprintf(writer, "-- Events:    %d (INSERT:%d UPDATE:%d DELETE:%d)\n",
		scanned, opCounts["i"], opCounts["u"], opCounts["d"])
	fmt.Fprintf(writer, "-- ============================================================\n\n")
	fmt.Fprintf(writer, "SET @OLD_FOREIGN_KEY_CHECKS = @@FOREIGN_KEY_CHECKS;\n")
	fmt.Fprintf(writer, "SET FOREIGN_KEY_CHECKS = 0;\n")
	fmt.Fprintf(writer, "SET NAMES utf8mb4;\n\n")

	// --- Setup MySQL execution if --execute ---
	if *execute && mysqlDB != nil {
		defer mysqlDB.Close()
		if _, err := mysqlDB.ExecContext(context.Background(), "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
			log.Fatalf("Disable FK checks: %v", err)
		}
		if _, err := mysqlDB.ExecContext(context.Background(), "SET NAMES utf8mb4"); err != nil {
			log.Printf("WARNING: SET NAMES utf8mb4: %v", err)
		}
		// Start first transaction batch
		if _, err := mysqlDB.ExecContext(context.Background(), "START TRANSACTION"); err != nil {
			log.Printf("WARNING: START TRANSACTION: %v", err)
		}
	} else if *execute && mysqlDB == nil {
		log.Fatal("--execute requires MySQL connection. Set MYSQL_USER/MYSQL_PASS/MYSQL_ADDR or use --mysql-dsn")
	}

	applied, skipped := 0, 0
	execApplied, execFailed, execSkippedMissing := 0, 0, 0
	missingTables := make(map[string]bool)
	eventNum := 0
	const txBatchSize = 1000 // Commit every N statements for speed

	for cursor.Next(fetchCtx) {
		var event EventDoc
		if err := cursor.Decode(&event); err != nil {
			log.Printf("WARNING: Decode error at event %d: %v", eventNum+1, err)
			skipped++
			eventNum++
			continue
		}
		eventNum++

		tableKey := event.Meta.DB + "." + event.Meta.Tbl
		pkCols := pkColumns[tableKey]

		stmt, err := eventToSQL(event, pkCols, jsonCols, tableCols)
		if err != nil {
			if skipped < 100 {
				log.Printf("WARNING: Skip event #%d (%s %s pk=%v): %v",
					eventNum, opName(event.OP), tableKey, event.Meta.PK, err)
			}
			fmt.Fprintf(writer, "-- SKIPPED #%d: %s %s pk=%v — %v\n\n",
				eventNum, opName(event.OP), tableKey, event.Meta.PK, err)
			skipped++
			continue
		}

		// Write to SQL file (always)
		fmt.Fprintf(writer, "-- #%d %s %s pk=%v @ %s\n",
			eventNum, opName(event.OP), tableKey, event.Meta.PK,
			event.TS.Format("2006-01-02 15:04:05 UTC"))
		fmt.Fprintf(writer, "%s\n\n", stmt)
		applied++

		// Execute against MySQL (if --execute)
		if *execute && mysqlDB != nil {
			// Skip missing tables
			if len(existingTables) > 0 && !existingTables[tableKey] {
				if !missingTables[tableKey] {
					log.Printf("Skipping table %s (does not exist in MySQL)", tableKey)
					missingTables[tableKey] = true
				}
				execSkippedMissing++
			} else {
				if _, err := mysqlDB.ExecContext(context.Background(), stmt); err != nil {
					log.Printf("ERROR #%d (%s %s pk=%v): %v",
						eventNum, opName(event.OP), tableKey, event.Meta.PK, err)
					execFailed++
					if !*continueOnErr {
						// Commit what we have before stopping
						mysqlDB.ExecContext(context.Background(), "COMMIT")
						log.Fatal("Stopping. Use --continue-on-error to skip failed statements.")
					}
				} else {
					execApplied++
					// Commit in batches for throughput
					if execApplied%txBatchSize == 0 {
						mysqlDB.ExecContext(context.Background(), "COMMIT")
						mysqlDB.ExecContext(context.Background(), "START TRANSACTION")
					}
				}
			}
		}

		// Periodic progress + memory check
		if eventNum%100000 == 0 {
			checkMemory()
			logMemory(fmt.Sprintf("phase 4: %d events processed", eventNum))
			log.Printf("  Progress: %d/%d events processed (%d applied, %d skipped)...",
				eventNum, scanned, applied, skipped)
			writer.Flush()
		}
	}

	fmt.Fprintf(writer, "\nSET FOREIGN_KEY_CHECKS = @OLD_FOREIGN_KEY_CHECKS;\n")
	fmt.Fprintf(writer, "-- Restore complete: %d applied, %d skipped\n", applied, skipped)
	writer.Flush()

	if *dryRun {
		log.Printf("Dry run: %d statements generated, %d skipped", applied, skipped)
		return
	}

	log.Printf("SQL file saved: %s (%d statements, %d skipped)", outPath, applied, skipped)

	// Finalize MySQL execution
	if *execute && mysqlDB != nil {
		// Commit final batch
		mysqlDB.ExecContext(context.Background(), "COMMIT")

		// Re-enable FK checks
		if _, err := mysqlDB.ExecContext(context.Background(), "SET FOREIGN_KEY_CHECKS = @OLD_FOREIGN_KEY_CHECKS"); err != nil {
			log.Printf("WARNING: Re-enable FK checks: %v", err)
		}

		// Print missing tables summary
		if len(missingTables) > 0 {
			log.Printf("")
			log.Printf("========== MISSING TABLES ==========")
			log.Printf("%d table(s) do not exist in MySQL and were skipped:", len(missingTables))
			missingList := make([]string, 0, len(missingTables))
			for t := range missingTables {
				missingList = append(missingList, t)
			}
			sort.Strings(missingList)
			for _, t := range missingList {
				log.Printf("  - %s", t)
			}
			log.Printf("Skipped %d events for missing tables.", execSkippedMissing)
			log.Printf("Create these tables first, then re-run to restore their data.")
			log.Printf("====================================")
		}

		log.Printf("Execution complete: %d applied, %d failed, %d skipped (missing table: %d, other: %d)",
			execApplied, execFailed, skipped+execSkippedMissing, execSkippedMissing, skipped)
	} else {
		log.Printf("To apply, run:  mysql -u root -p %s < %s", *dbName, outPath)
		log.Printf("Or re-run with: --execute --mysql-dsn 'user:pass@tcp(host:port)/%s'", *dbName)
	}

	logMemory("done")
}
