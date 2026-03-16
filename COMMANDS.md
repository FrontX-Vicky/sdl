# SDL Tool Commands

Two CLI tools for MySQL data operations: **sql_import** (restore from phpMyAdmin SQL dumps) and **sdl_restore** (replay audit events from MongoDB).

---

## sql_import

Reads a phpMyAdmin `.sql` backup file and generates upsert (`INSERT ... ON DUPLICATE KEY UPDATE`) statements for an existing MySQL database. Handles schema differences by skipping columns and tables that don't exist in the target.

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
| `--mysql-dsn` | *(from env)* | MySQL DSN override: `user:pass@tcp(host:port)/dbname` |
| `--dry-run` | `false` | Print SQL to stdout; do not write file or execute |
| `--continue-on-error` | `false` | Continue past statement failures during `--execute` |
| `--skip-tables` | *(none)* | Comma-separated list of tables to skip |
| `--only-tables` | *(all)* | Comma-separated list of tables to import exclusively |
| `--skip-create` | `true` | Skip `CREATE TABLE` statements (target tables must already exist) |

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

# Import only specific tables
sql_import --file backup.sql --db myapp --only-tables 'users,orders'

# Skip noisy tables
sql_import --file backup.sql --db myapp --skip-tables 'logs,sessions'

# Custom output path + explicit DSN
sql_import --file backup.sql --db myapp --output out.sql --mysql-dsn 'root:pass@tcp(127.0.0.1:3306)/myapp'
```

### How It Works

1. Connects to MySQL and loads the schema (PKs, JSON columns, column list) for the target database.
2. Streams the `.sql` file line by line — supports huge files via a 64 MB line buffer.
3. For each `INSERT INTO` statement, generates `INSERT ... ON DUPLICATE KEY UPDATE` (upsert).
4. Columns not present in the target schema are silently dropped.
5. Tables not present in the target database are skipped with a warning.
6. JSON columns: empty string values are replaced with `CAST('null' AS JSON)`.
7. Writes a `.sql` output file (or executes directly with `--execute`).

---

## sdl_restore

Replays MySQL row-change audit events stored in MongoDB, generating SQL (`INSERT`, `UPDATE`, `DELETE`) to restore a database to a point in time. Events are read from a MongoDB `audit` collection populated by a binlog listener.

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
| `--start-date` | **required** | Replay events from this UTC date/time (`YYYY-MM-DD` or `YYYY-MM-DD HH:MM:SS`) |
| `--db` | **required** | MySQL database name to filter events by |
| `--end-date` | *(now)* | Stop at this UTC date/time (optional) |
| `--table` | *(all)* | Restrict to a single table |
| `--pk-column` | *(auto-detect)* | PK column override (see formats below) |
| `--output` | `restore_<db>_<timestamp>.sql` | Output SQL file path |
| `--execute` | `false` | Execute SQL against MySQL directly (in addition to writing the file) |
| `--mysql-dsn` | *(from env)* | MySQL DSN: `user:pass@tcp(host:port)/dbname` |
| `--dry-run` | `false` | Print SQL to stdout; do not write file or execute |
| `--continue-on-error` | `false` | Continue past statement failures during `--execute` |

### `--pk-column` Formats

| Format | Example | Meaning |
|--------|---------|---------|
| Single column for all tables | `id` | Use `id` as PK everywhere |
| Per-table | `users:user_id,orders:order_id` | Different PK per table |
| Composite PK | `tbl:col1+col2` | Multi-column PK |

If omitted, PKs are auto-detected by scanning the first 50,000 events and confirmed against the MySQL `information_schema`.

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `MONGO_URI` | `mongodb://127.0.0.1:27017/` | MongoDB connection URI |
| `MONGO_DB` | `audit` | MongoDB database name |
| `MONGO_COLL` | `row_changes` | MongoDB collection name |
| `MYSQL_USER` | — | MySQL username |
| `MYSQL_PASS` | — | MySQL password |
| `MYSQL_ADDR` | `127.0.0.1:3306` | MySQL host:port |

`.env` is loaded from the current directory, parent directory, and the binary's directory.

### Examples

```sh
# Generate SQL for all tables from a date
sdl_restore --start-date 2026-03-01 --db myapp

# Restrict to a date range
sdl_restore --start-date 2026-03-01 --end-date 2026-03-14 --db myapp

# Single table only
sdl_restore --start-date 2026-03-01 --db myapp --table users

# Preview without writing
sdl_restore --start-date "2026-03-01 14:30:00" --db myapp --dry-run

# Execute directly against MySQL
sdl_restore --start-date 2026-03-01 --db myapp --execute --mysql-dsn 'root:pass@tcp(127.0.0.1:3306)/myapp'

# Override PK detection
sdl_restore --start-date 2026-03-01 --db myapp --pk-column id
sdl_restore --start-date 2026-03-01 --db myapp --pk-column 'users:user_id,orders:order_id'
```

### How It Works

1. **Phase 1 — Count**: MongoDB aggregation counts events by operation type and table.
2. **Phase 2 — PK detection**: Scans up to 50,000 events to auto-detect primary key columns by matching `meta.pk` values against column data.
3. **Phase 3 — MySQL metadata**: Queries `information_schema` for PKs, JSON columns, column lists, and table existence.
4. **Phase 4 — Stream & write**: Streams all matching events in timestamp order, converts each to SQL, and writes to the output file (and optionally executes in batches of 1,000 statements per transaction).

Events are processed in chronological order to preserve referential integrity. Missing tables are skipped and reported in a summary.

---

## Applying a Generated SQL File

Both tools produce a `.sql` file that can be applied with:

```sh
mysql -u root -p <database> < output.sql
```

Or re-run the tool with `--execute` to apply directly.
