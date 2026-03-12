package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
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
		return "X'" + hex.EncodeToString(val.Data) + "'"
	case primitive.ObjectID:
		return "'" + val.Hex() + "'"
	case []byte:
		if len(val) == 0 {
			return "''"
		}
		return "X'" + hex.EncodeToString(val) + "'"
	default:
		s := fmt.Sprintf("%v", val)
		return "'" + escapeString(s) + "'"
	}
}

// detectPKColumns analyzes INSERT events to determine PK column names per table.
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

	// Auto-detect from INSERT events
	candidates := make(map[string]map[string]int) // table -> column -> match count

	for _, event := range events {
		if event.OP != "i" {
			continue
		}
		tableKey := event.Meta.DB + "." + event.Meta.Tbl
		if _, exists := result[tableKey]; exists {
			continue // Already resolved via override
		}

		pkStr := fmt.Sprint(event.Meta.PK)
		if _, ok := candidates[tableKey]; !ok {
			candidates[tableKey] = make(map[string]int)
		}

		if strings.Contains(pkStr, "|") {
			// Composite PK
			parts := strings.Split(pkStr, "|")
			for col, delta := range event.Chg {
				tStr := fmt.Sprint(delta.T)
				for _, part := range parts {
					if tStr == part {
						candidates[tableKey][col]++
						break
					}
				}
			}
		} else {
			// Simple PK
			for col, delta := range event.Chg {
				if fmt.Sprint(delta.T) == pkStr {
					candidates[tableKey][col]++
				}
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

		// Check if composite PK from a sample INSERT event
		for _, event := range events {
			if event.OP == "i" && event.Meta.DB+"."+event.Meta.Tbl == table {
				pkStr := fmt.Sprint(event.Meta.PK)
				if strings.Contains(pkStr, "|") {
					n := strings.Count(pkStr, "|") + 1
					pkCols := make([]string, 0, n)
					for i := 0; i < n && i < len(sorted); i++ {
						pkCols = append(pkCols, sorted[i].name)
					}
					result[table] = pkCols
				} else {
					result[table] = []string{sorted[0].name}
				}
				break
			}
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

// eventToSQL converts an EventDoc to a SQL statement
func eventToSQL(event EventDoc, pkCols []string) (string, error) {
	db := event.Meta.DB
	tbl := event.Meta.Tbl

	switch event.OP {
	case "i":
		return insertSQL(db, tbl, event.Chg)
	case "u":
		return updateSQL(db, tbl, event.Chg, pkCols, event.Meta.PK)
	case "d":
		return deleteSQL(db, tbl, pkCols, event.Meta.PK, event.Chg)
	default:
		return "", fmt.Errorf("unknown operation: %s", event.OP)
	}
}

// insertSQL generates an INSERT ... ON DUPLICATE KEY UPDATE statement
func insertSQL(db, tbl string, chg map[string]Delta) (string, error) {
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
		values = append(values, sqlValue(chg[col].T))
		updates = append(updates, fmt.Sprintf("`%s` = VALUES(`%s`)", col, col))
	}

	return fmt.Sprintf("INSERT INTO `%s`.`%s` (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s;",
		db, tbl,
		strings.Join(colNames, ", "),
		strings.Join(values, ", "),
		strings.Join(updates, ", ")), nil
}

// updateSQL generates an UPDATE ... SET ... WHERE pk = ... statement
func updateSQL(db, tbl string, chg map[string]Delta, pkCols []string, pk any) (string, error) {
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
		setClauses = append(setClauses, fmt.Sprintf("`%s` = %s", col, sqlValue(chg[col].T)))
	}

	return fmt.Sprintf("UPDATE `%s`.`%s` SET %s WHERE %s;",
		db, tbl, strings.Join(setClauses, ", "), where), nil
}

// deleteSQL generates a DELETE ... WHERE pk = ... statement
func deleteSQL(db, tbl string, pkCols []string, pk any, chg map[string]Delta) (string, error) {
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
				conditions = append(conditions, fmt.Sprintf("`%s` = %s", col, sqlValue(chg[col].F)))
			}
			return fmt.Sprintf("DELETE FROM `%s`.`%s` WHERE %s LIMIT 1;",
				db, tbl, strings.Join(conditions, " AND ")), nil
		}
		return "", fmt.Errorf("unknown PK column for DELETE — use --pk-column")
	}

	where := buildWhereClause(pkCols, pk)
	return fmt.Sprintf("DELETE FROM `%s`.`%s` WHERE %s;", db, tbl, where), nil
}

func main() {
	// --- Flags ---
	startDate := flag.String("start-date", "", "Start date UTC (YYYY-MM-DD or YYYY-MM-DD HH:MM:SS) [required]")
	endDate := flag.String("end-date", "", "End date UTC (optional)")
	dbName := flag.String("db", "", "MySQL database name to filter [required]")
	tableName := flag.String("table", "", "Table name (empty = all tables)")
	pkColumn := flag.String("pk-column", "", "PK column override. Examples: 'id' | 'users:user_id,orders:order_id' | 'tbl:col1+col2'")
	output := flag.String("output", "", "Output SQL file path (default: restore_<db>_<timestamp>.sql)")
	execute := flag.Bool("execute", false, "Execute SQL against MySQL directly (in addition to writing file)")
	mysqlDSN := flag.String("mysql-dsn", "", "MySQL DSN for --execute: user:pass@tcp(host:port)/dbname")
	dryRun := flag.Bool("dry-run", false, "Preview SQL on stdout without writing file or executing")
	continueOnErr := flag.Bool("continue-on-error", false, "Continue if a statement fails during --execute")
	flag.Parse()

	_ = godotenv.Load(".env")

	// --- Validate ---
	if *startDate == "" || *dbName == "" {
		fmt.Println("Usage: sdl_restore --start-date <DATE> --db <DATABASE> [--table <TABLE>] [options]")
		fmt.Println()
		flag.PrintDefaults()
		fmt.Println("\nExamples:")
		fmt.Println("  sdl_restore --start-date 2026-03-01 --db myapp")
		fmt.Println("  sdl_restore --start-date 2026-03-01 --db myapp --table users")
		fmt.Println("  sdl_restore --start-date '2026-03-01 14:30:00' --db myapp --dry-run")
		fmt.Println("  sdl_restore --start-date 2026-03-01 --db myapp --execute --mysql-dsn 'root:pass@tcp(127.0.0.1:3306)/myapp'")
		fmt.Println("  sdl_restore --start-date 2026-03-01 --db myapp --pk-column id")
		fmt.Println("  sdl_restore --start-date 2026-03-01 --db myapp --pk-column 'users:user_id,orders:order_id'")
		os.Exit(1)
	}

	startTime, err := parseDate(*startDate)
	if err != nil {
		log.Fatalf("Invalid --start-date: %v", err)
	}

	var endTime time.Time
	if *endDate != "" {
		endTime, err = parseDate(*endDate)
		if err != nil {
			log.Fatalf("Invalid --end-date: %v", err)
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

	// --- Fetch events (chronological ascending for replay) ---
	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer fetchCancel()

	cursor, err := coll.Find(fetchCtx, filter,
		options.Find().
			SetSort(bson.D{{Key: "ts", Value: 1}}).
			SetBatchSize(5000))
	if err != nil {
		log.Fatalf("MongoDB query: %v", err)
	}
	defer cursor.Close(fetchCtx)

	var events []EventDoc
	if err := cursor.All(fetchCtx, &events); err != nil {
		log.Fatalf("Decode events: %v", err)
	}

	if len(events) == 0 {
		log.Println("No events found matching filters. Nothing to restore.")
		return
	}

	// --- Summary ---
	opCounts := map[string]int{}
	tables := map[string]bool{}
	for _, e := range events {
		opCounts[e.OP]++
		tables[e.Meta.DB+"."+e.Meta.Tbl] = true
	}
	log.Printf("Found %d events to replay", len(events))
	log.Printf("  INSERT: %d | UPDATE: %d | DELETE: %d | Tables: %d",
		opCounts["i"], opCounts["u"], opCounts["d"], len(tables))

	// --- Detect PK columns ---
	pkColumns := detectPKColumns(events, *pkColumn)
	for table, cols := range pkColumns {
		log.Printf("  PK: %s → [%s]", table, strings.Join(cols, ", "))
	}

	// --- Generate SQL ---
	var sb strings.Builder
	sb.WriteString("-- ============================================================\n")
	sb.WriteString("-- SDL Restore Script — Replay binlog events from MongoDB audit\n")
	sb.WriteString(fmt.Sprintf("-- Generated: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("-- Source:    MongoDB %s.%s\n", mongoDB, mongoColl))
	sb.WriteString(fmt.Sprintf("-- Database:  %s\n", *dbName))
	sb.WriteString(fmt.Sprintf("-- Table:     %s\n", tblDisplay))
	sb.WriteString(fmt.Sprintf("-- Period:    %s → %s\n", startTime.Format("2006-01-02 15:04:05"), endDisplay))
	sb.WriteString(fmt.Sprintf("-- Events:    %d (INSERT:%d UPDATE:%d DELETE:%d)\n",
		len(events), opCounts["i"], opCounts["u"], opCounts["d"]))
	sb.WriteString("-- ============================================================\n\n")

	sb.WriteString("SET @OLD_FOREIGN_KEY_CHECKS = @@FOREIGN_KEY_CHECKS;\n")
	sb.WriteString("SET FOREIGN_KEY_CHECKS = 0;\n")
	sb.WriteString("SET NAMES utf8mb4;\n\n")

	applied, skipped := 0, 0
	for i, event := range events {
		tableKey := event.Meta.DB + "." + event.Meta.Tbl
		pkCols := pkColumns[tableKey]

		stmt, err := eventToSQL(event, pkCols)
		if err != nil {
			log.Printf("WARNING: Skip event #%d (%s %s pk=%v): %v",
				i+1, opName(event.OP), tableKey, event.Meta.PK, err)
			sb.WriteString(fmt.Sprintf("-- SKIPPED #%d: %s %s pk=%v — %v\n\n",
				i+1, opName(event.OP), tableKey, event.Meta.PK, err))
			skipped++
			continue
		}

		sb.WriteString(fmt.Sprintf("-- #%d %s %s pk=%v @ %s\n",
			i+1, opName(event.OP), tableKey, event.Meta.PK,
			event.TS.Format("2006-01-02 15:04:05 UTC")))
		sb.WriteString(stmt + "\n\n")
		applied++
	}

	sb.WriteString("\nSET FOREIGN_KEY_CHECKS = @OLD_FOREIGN_KEY_CHECKS;\n")
	sb.WriteString(fmt.Sprintf("-- Restore complete: %d applied, %d skipped\n", applied, skipped))

	fullSQL := sb.String()

	// --- Dry run: print to stdout ---
	if *dryRun {
		fmt.Print(fullSQL)
		log.Printf("Dry run: %d statements generated, %d skipped", applied, skipped)
		return
	}

	// --- Write SQL file ---
	outPath := *output
	if outPath == "" {
		outPath = fmt.Sprintf("restore_%s_%s.sql", *dbName, time.Now().Format("20060102_150405"))
	}
	if err := os.WriteFile(outPath, []byte(fullSQL), 0600); err != nil {
		log.Fatalf("Write SQL file: %v", err)
	}
	log.Printf("SQL file saved: %s (%d statements, %d skipped)", outPath, applied, skipped)

	// --- Execute against MySQL (optional) ---
	if !*execute {
		log.Printf("To apply, run:  mysql -u root -p %s < %s", *dbName, outPath)
		log.Printf("Or re-run with: --execute --mysql-dsn 'user:pass@tcp(host:port)/%s'", *dbName)
		return
	}

	dsn := *mysqlDSN
	if dsn == "" {
		user := getenv("MYSQL_USER", "")
		pass := os.Getenv("MYSQL_PASS")
		addr := getenv("MYSQL_ADDR", "127.0.0.1:3306")
		if user == "" {
			log.Fatal("--mysql-dsn required for --execute, or set MYSQL_USER/MYSQL_PASS/MYSQL_ADDR env vars")
		}
		dsn = fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true", user, pass, addr, *dbName)
	}

	log.Printf("Connecting to MySQL...")
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("MySQL open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatalf("MySQL ping: %v", err)
	}

	// Disable FK checks for the session
	if _, err := db.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		log.Fatalf("Disable FK checks: %v", err)
	}
	if _, err := db.ExecContext(ctx, "SET NAMES utf8mb4"); err != nil {
		log.Printf("WARNING: SET NAMES utf8mb4: %v", err)
	}

	execApplied, execFailed := 0, 0
	log.Printf("Executing %d statements against MySQL...", applied)

	for i, event := range events {
		tableKey := event.Meta.DB + "." + event.Meta.Tbl
		pkCols := pkColumns[tableKey]

		stmt, err := eventToSQL(event, pkCols)
		if err != nil {
			execFailed++
			continue
		}

		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			log.Printf("ERROR #%d (%s %s pk=%v): %v",
				i+1, opName(event.OP), tableKey, event.Meta.PK, err)
			execFailed++
			if !*continueOnErr {
				log.Fatal("Stopping. Use --continue-on-error to skip failed statements.")
			}
			continue
		}

		execApplied++
		if execApplied%100 == 0 {
			log.Printf("  Progress: %d/%d executed...", execApplied, applied)
		}
	}

	// Re-enable FK checks
	if _, err := db.ExecContext(context.Background(), "SET FOREIGN_KEY_CHECKS = @OLD_FOREIGN_KEY_CHECKS"); err != nil {
		log.Printf("WARNING: Re-enable FK checks: %v", err)
	}

	log.Printf("Execution complete: %d applied, %d failed, %d skipped", execApplied, execFailed, skipped)
}
