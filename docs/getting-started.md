# Getting Started

## Prerequisites

| Requirement | Version | Notes |
|-------------|---------|-------|
| Go | 1.18+ | For building all binaries |
| MySQL | 5.7+ | GTID mode + ROW binlog required |
| MongoDB | 4.0+ | Replica set required for transactions |

## .env Configuration

Create `.env` in the project root (or next to the binary):

```env
# MySQL
MYSQL_ADDR=127.0.0.1:3306
MYSQL_USER=repl_user
MYSQL_PASS=your_password
MYSQL_FLAVOR=mysql
MYSQL_SERVER_ID=2222

# MongoDB
MONGO_URI=mongodb://127.0.0.1:27017/?replicaSet=rs0&appName=audit
MONGO_DB=audit
MONGO_COLL=row_changes
MONGO_OFFSETS_COLL=binlog_offsets

# Filter (which tables to capture)
INCLUDE_REGEX=.*\..*
EXCLUDE_REGEX=^(mysql|performance_schema|information_schema|sys)\..*

# Timezone
TZ=Asia/Kolkata
```

```bash
chmod 600 .env   # protect credentials
```

## Build All Binaries

```bash
cd sdl

# Audit logger + live viewer (at repo root)
go build -o sdl_binary main.go
go build -o sdl_view   view.go

# TUI fetch/query tool
cd sdl_fetch  && go build -o sdl_fetch  . && cd ..

# Restore from MongoDB audit events
cd sdl_restore && go build -o sdl_restore . && cd ..

# Import from phpMyAdmin SQL dump
cd sdl_import  && go build -o sql_import  . && cd ..
```

## Quick Start

### 1. Start the audit logger

```bash
./sdl_binary
```

Logs to look for on first start:
- `Recovering 0 pending batches` — no leftover staging data (expected)
- `Connected to MongoDB`
- `Starting from GTID ...` or `Starting from current position`

### 2. Watch live events

```bash
./sdl_view

# With options
./sdl_view -history 50 -op u -table mydb.users -wide
```

Flags:

| Flag | Description |
|------|-------------|
| `-history N` | Show N recent events before live |
| `-op` | Filter by operation: `i` / `u` / `d` |
| `-table` | Filter by `db.table` |
| `-wide` | Wider CHANGES column |
| `-since` | Only events after RFC3339 timestamp |
| `-poll` | Polling interval when change streams unavailable |
| `-uri` | MongoDB URI override |
| `-db` | MongoDB database name override |
| `-coll` | Collection name override |

### 3. Query audit logs (TUI)

```bash
cd sdl_fetch && ./sdl_fetch
```

Keyboard shortcuts:

| Key | Action |
|-----|--------|
| `F1` | Set filters |
| `F5` | Refresh |
| `F9` | Export CSV/JSON |
| `F10` | Toggle auto-refresh |
| `Enter` | View event details |
| `?` | Help |
| `q` | Quit |

### 4. Restore data from audit events

See [commands.md](commands.md#sdl_restore) for full reference.

```bash
cd sdl_restore
./sdl_restore --start-date 2026-03-01 --db myapp --dry-run
```

### 5. Import from a SQL dump

See [commands.md](commands.md#sql_import) for full reference.

```bash
cd sdl_import
./sql_import --file backup.sql --db myapp --dry-run
```

## MongoDB Replica Set (Required for Logger)

```bash
# /etc/mongod.conf — add:
# replication:
#   replSetName: "rs0"

sudo systemctl restart mongod

mongosh --eval 'rs.initiate({ _id: "rs0", members: [{ _id: 0, host: "localhost:27017" }] })'
mongosh --eval 'rs.status()'   # stateStr should be "PRIMARY"
```

## MySQL Replication User

```sql
CREATE USER 'repl_user'@'%' IDENTIFIED BY 'secure_password';
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'repl_user'@'%';
GRANT SELECT ON your_database.* TO 'repl_user'@'%';
FLUSH PRIVILEGES;
```
