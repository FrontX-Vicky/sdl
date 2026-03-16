# CLI Commands Reference

Two tools for MySQL data recovery:
- **`sql_import`** — Restore from a phpMyAdmin / mysqldump SQL file via upserts
- **`sdl_restore`** — Replay MongoDB audit events back to MySQL

---

## sql_import

Reads a `.sql` backup file and generates `INSERT ... ON DUPLICATE KEY UPDATE` statements.  
Handles all-databases dumps, both `INSERT ... (cols) VALUES` and column-less `INSERT ... VALUES` formats.  
Automatically coerces `NULL` to safe defaults for `NOT NULL` columns.

### Build

```sh
cd sdl_import
go build -o sql_import .
```

### Usage

```sh
sql_import --file <backup.sql> --db <DATABASE> [options]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--file` | **required** | Path to `.sql` backup file |
| `--db` | **required** | Target MySQL database name |
| `--output` | `import_<db>_<timestamp>.sql` | Output SQL file path |
| `--execute` | `false` | Execute SQL against MySQL directly |
| `--mysql-dsn` | *(from env)* | Override DSN: `user:pass@tcp(host:port)/dbname` |
| `--dry-run` | `false` | Print SQL to stdout; no file write or execution |
| `--continue-on-error` | `false` | Skip failing statements instead of stopping |
| `--skip-tables` | *(none)* | Comma-separated tables to skip |
| `--only-tables` | *(all)* | Comma-separated tables to import exclusively |
| `--skip-create` | `true` | Skip `CREATE TABLE` statements (tables must exist) |

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MYSQL_USER` | — | MySQL username |
| `MYSQL_PASS` | — | MySQL password |
| `MYSQL_ADDR` | `127.0.0.1:3306` | MySQL host:port |

`.env` is loaded from the current directory, parent directory, and the binary's directory.

### Examples

```sh
# Generate upsert SQL file
sql_import --file backup.sql --db myapp

# Execute directly against MySQL
sql_import --file backup.sql --db myapp --execute

# Preview without writing anything
sql_import --file backup.sql --db myapp --dry-run

# All-databases dump — filter to one DB + specific tables
sql_import --file all_dbs.sql --db pf_messenger \
  --only-tables 'users,chats,messages'

# Skip noisy/large tables
sql_import --file backup.sql --db myapp --skip-tables 'logs,sessions'

# Custom output path + explicit DSN
sql_import --file backup.sql --db myapp \
  --output out.sql \
  --mysql-dsn 'root:pass@tcp(127.0.0.1:3306)/myapp'
```

### How It Works

1. Connects to MySQL and loads full schema for the target database (PK columns, JSON columns, column order, `NOT NULL` constraints, defaults).
2. Streams the `.sql` file line by line (64 MB line buffer for huge files).
3. Tracks `USE db;` context so all-databases dumps are filtered to `--db`.
4. Supports both INSERT styles:
   - `INSERT INTO \`table\` (\`col1\`) VALUES (...)` — column list from dump
   - `INSERT INTO \`table\` VALUES (...)` — column order from MySQL `information_schema`
5. Also handles db-qualified form: `INSERT INTO \`db\`.\`table\` ...`
6. `NULL` values for `NOT NULL` columns are coerced to the column default, or a type-safe fallback (`0`, `''`, `CAST('null' AS JSON)`, etc.).
7. Columns not in the target schema are silently dropped.
8. Tables not in the target database are skipped with a warning.
9. Writes `.sql` output (or executes directly with `--execute`).

---

## sdl_restore

Replays MySQL row-change audit events stored in MongoDB (captured by the binlog logger) to reproduce `INSERT`, `UPDATE`, and `DELETE` operations as SQL.

### Build

```sh
cd sdl_restore
go build -o sdl_restore .
```

### Usage

```sh
sdl_restore --start-date <DATE> --db <DATABASE> [options]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--start-date` | **required** | Replay from this UTC time (`YYYY-MM-DD` or `YYYY-MM-DD HH:MM:SS`) |
| `--db` | **required** | MySQL database name to filter |
| `--end-date` | *(now)* | Stop at this UTC time (optional) |
| `--table` | *(all)* | Restrict to a single table |
| `--pk-column` | *(auto-detect)* | PK override — see formats below |
| `--output` | `restore_<db>_<timestamp>.sql` | Output SQL file path |
| `--execute` | `false` | Execute SQL against MySQL (in addition to file) |
| `--mysql-dsn` | *(from env)* | MySQL DSN: `user:pass@tcp(host:port)/dbname` |
| `--dry-run` | `false` | Print SQL to stdout; no file or execution |
| `--continue-on-error` | `false` | Skip failing statements instead of stopping |

### `--pk-column` Formats

| Format | Example | Meaning |
|--------|---------|---------|
| Single for all tables | `id` | Use `id` everywhere |
| Per-table | `users:user_id,orders:order_id` | Different PK per table |
| Composite PK | `tbl:col1+col2` | Multi-column PK |

If omitted, PKs are auto-detected from the first 50,000 events and confirmed via MySQL `information_schema`.

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MONGO_URI` | `mongodb://127.0.0.1:27017/` | MongoDB connection URI |
| `MONGO_DB` | `audit` | MongoDB database name |
| `MONGO_COLL` | `row_changes` | Collection name |
| `MYSQL_USER` | — | MySQL username |
| `MYSQL_PASS` | — | MySQL password |
| `MYSQL_ADDR` | `127.0.0.1:3306` | MySQL host:port |

### Examples

```sh
# All tables from a date
sdl_restore --start-date 2026-03-01 --db myapp

# Date range
sdl_restore --start-date 2026-03-01 --end-date 2026-03-14 --db myapp

# Single table
sdl_restore --start-date 2026-03-01 --db myapp --table users

# Preview only
sdl_restore --start-date "2026-03-01 14:30:00" --db myapp --dry-run

# Execute directly against MySQL
sdl_restore --start-date 2026-03-01 --db myapp \
  --execute --mysql-dsn 'root:pass@tcp(127.0.0.1:3306)/myapp'

# Override PK for all tables
sdl_restore --start-date 2026-03-01 --db myapp --pk-column id

# Per-table PK override
sdl_restore --start-date 2026-03-01 --db myapp \
  --pk-column 'users:user_id,orders:order_id'
```

### How It Works

1. **Phase 1 — Count**: MongoDB aggregation counts events by operation and table.
2. **Phase 2 — PK detection**: Scans up to 50,000 events to auto-detect PKs.
3. **Phase 3 — MySQL metadata**: Loads PKs, JSON columns, table columns, and table existence from `information_schema`.
4. **Phase 4 — Stream & write**: Events streamed in timestamp order, converted to SQL, written to file (and optionally executed in batches of 1,000 per transaction).

---

## Applying a Generated SQL File

Both tools produce a `.sql` file. Apply it with:

```sh
mysql -u root -p <database> < output.sql
```

Or re-run with `--execute` to apply inline.
