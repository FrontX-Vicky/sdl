# MySQL Binlog to MongoDB Audit Logger

A production-grade system that captures MySQL database changes via binlog replication and stores them as audit events in MongoDB with **zero data loss guarantee**.

## Overview

This project provides comprehensive MySQL database auditing with:
- ✅ **Zero data loss** - Crash recovery, atomic transactions, retry logic
- ✅ **Real-time replication** - Captures INSERT, UPDATE, DELETE operations
- ✅ **Multiple tools** - Logger, query tool, and real-time viewer
- ✅ **Production-ready** - Graceful shutdown, monitoring, error handling

## Project Components

### 1. main.go - Audit Logger
Continuously captures MySQL binlog events and stores them in MongoDB with:
- Staging collection for crash recovery
- Atomic transactions (batch + GTID offset together)
- Exponential backoff retry (5 attempts over ~30 seconds)
- Canal reconnection for protocol errors (10 attempts over ~10 minutes)
- Schema change detection
- Graceful shutdown on SIGTERM/SIGINT

### 2. fetch.go - Query Tool
Retrieve and analyze audit logs from MongoDB with:
- Flexible filtering (database, table, primary key, operation, time range)
- Export to JSON or CSV
- Summary statistics and change tracking

### 3. view.go - Real-Time Viewer
Live-tail audit events with:
- Change streams (MongoDB 3.6+) or polling fallback
- Historical replay with configurable limit
- Filtering by operation type, table, timestamp

## Quick Start

### Prerequisites

- **Go 1.18+**
- **MySQL 5.7+** with:
  - GTID mode enabled (`gtid_mode = ON`)
  - ROW binlog format (`binlog_format = ROW`)
  - Binlog retention ≥ 14 days (`binlog_expire_logs_seconds = 1209600`)
- **MongoDB 4.0+** with:
  - Replica set (required for transactions)
  - Journaling enabled

### Installation

```bash
# Clone or download the project
cd sdl

# Dependencies are already in go.mod
go mod download

# Build binaries
go build -o sdl_binary main.go
go build -o sdl_fetch fetch.go
go build -o sdl_view view.go
```

### Configuration

Create `.env` file:

```env
# MySQL Configuration
MYSQL_ADDR=127.0.0.1:3306
MYSQL_USER=repl_user
MYSQL_PASS=your_password
MYSQL_FLAVOR=mysql
MYSQL_SERVER_ID=2222

# MongoDB Configuration (use replica set URI)
MONGO_URI=mongodb://127.0.0.1:27017/?replicaSet=rs0&appName=audit
MONGO_DB=audit
MONGO_COLL=row_changes
MONGO_OFFSETS_COLL=binlog_offsets

# Include/Exclude Patterns
INCLUDE_REGEX=.*\..*
EXCLUDE_REGEX=^(mysql|performance_schema|information_schema|sys)\..*

# Timezone
TZ=Asia/Kolkata
```

### Setup MongoDB Indexes

```bash
mongosh << 'EOF'
use audit

// Events collection
db.row_changes.createIndex({ "ts": 1 })
db.row_changes.createIndex({ "meta.pk": 1, "meta.db": 1, "meta.tbl": 1 })

// Staging collection (7-day auto-cleanup)
db.row_changes_staging.createIndex({ "status": 1 })
db.row_changes_staging.createIndex({ "createdAt": 1 }, { expireAfterSeconds: 604800 })
EOF
```

## Usage

### Start Audit Logger

```bash
# Run in foreground
./sdl_binary

# Or as systemd service
sudo systemctl start sdl.service
sudo journalctl -u sdl.service -f
```

### Query Audit Logs

```bash
# Run fetch tool
go run fetch.go
```

**Example queries in fetch.go:**

```go
// All events for a table
events, err := fetchEvents(coll, QueryParams{
    Database: "mydb",
    Table:    "users",
    Limit:    10,
})

// Events for specific primary key
events, err := fetchEvents(coll, QueryParams{
    Database: "mydb",
    Table:    "users",
    PK:       "123",
    Limit:    5,
})

// Filter by operation type
events, err := fetchEvents(coll, QueryParams{
    Database:  "mydb",
    Table:     "users",
    Operation: "u",  // "i"=insert, "u"=update, "d"=delete
    Limit:     5,
})

// Time range query
events, err := fetchEvents(coll, QueryParams{
    Database:  "mydb",
    StartTime: yesterday,
    EndTime:   now,
    Limit:     10,
})

// Export to JSON
exportToJSON(events, "audit_export.json")

// Export to CSV
exportToCSV(events, "audit_export.csv")
```

### View Real-Time Events

```bash
# Basic live tail
./sdl_view

# With options
./sdl_view -history 50 -op u -table mydb.users -wide

# Custom MongoDB connection
./sdl_view -uri mongodb://host:27017 -db audit -coll row_changes
```

**Available flags:**
- `-history N` - Show N recent events before live tail
- `-op` - Filter by operation: i/u/d
- `-table` - Filter by table: database.table
- `-wide` - Wider CHANGES column display
- `-since` - Only show events after RFC3339 timestamp
- `-poll` - Polling interval (if change streams unavailable)

## Event Document Structure

Each audit event stored in MongoDB:

```json
{
  "_id": "unique_hash",
  "ts": "2025-12-13T10:30:00Z",
  "op": "u",
  "meta": {
    "db": "database_name",
    "tbl": "table_name",
    "pk": "primary_key_value"
  },
  "chg": {
    "column_name": {
      "f": "old_value",
      "t": "new_value"
    }
  },
  "src": {
    "binlog": {
      "file": "mysql-bin.000001",
      "pos": 12345
    },
    "gtid": "..."
  },
  "ts_ist": "2025-12-13 16:00:00"
}
```

**Operations:**
- `i` - INSERT
- `u` - UPDATE
- `d` - DELETE

## System Architecture

### Zero Data Loss Protection

**Failure Scenarios Covered:**

1. **MySQL Connection Lost** ✓
   - Batch persisted in staging collection
   - Automatic retry with exponential backoff
   - Recovery on restart

2. **MongoDB Transient Failure** ✓
   - 5 automatic retries (100ms → 10s backoff)
   - Transient error detection
   - Batch preserved in staging if all retries fail

3. **Service Crash** ✓
   - Two-phase commit with staging
   - Recovery scans for pending batches on startup
   - Idempotent event IDs prevent duplicates

4. **Multiple Cascading Failures** ✓
   - GTID offset tracking
   - Atomic batch + GTID updates
   - Deterministic event hashing

5. **Schema Changes** ✓
   - Schema change detection
   - Automatic batch flushing
   - Bounds checking for array access

6. **Binlog Rotation** ⚠️
   - Requires proper MySQL configuration
   - Binlog retention ≥ 14 days recommended

### Key Components

- **Staging Collection** - Crash recovery checkpoint
- **Atomic Transactions** - Batch + GTID written together
- **Retry Logic** - Exponential backoff for transient errors
- **Recovery Function** - Processes pending batches on startup
- **Schema Tracking** - Detects and handles schema changes
- **Graceful Shutdown** - Flushes remaining events on SIGTERM

## Monitoring

### Key Metrics

```javascript
// Staging backlog (should be ~0)
db.row_changes_staging.countDocuments({ status: "pending" })

// Latest GTID
db.binlog_offsets.findOne()

// Event count (last hour)
db.row_changes.countDocuments({ 
  ts: { $gte: new Date(Date.now() - 3600000) } 
})

// Events by operation
db.row_changes.aggregate([
  { $group: { _id: "$op", count: { $sum: 1 } } }
])
```

### Service Logs

```bash
# Watch logs
sudo journalctl -u sdl.service -f

# Check errors
sudo journalctl -u sdl.service -p err -n 50

# Service status
sudo systemctl status sdl.service
```

## Troubleshooting

### Service Won't Start
1. Check MongoDB replica set: `mongosh` → `rs.status()`
2. Verify MySQL connection and GTID mode
3. Check logs: `journalctl -u sdl.service -n 100`
4. Verify `.env` configuration

### Events Not Flowing
1. Check MySQL binlog: `SHOW MASTER STATUS;`
2. Verify MongoDB connection and transactions
3. Check staging: `db.row_changes_staging.find()`
4. Monitor GTID progression

### Batch Stuck in Staging
1. Check pending batches: `db.row_changes_staging.find({status: "pending"})`
2. If MongoDB issue resolved, restart service
3. Force archive if needed (see OPERATIONS.md)

## Documentation

- **[README.md](README.md)** - This file (setup and usage)
- **[ARCHITECTURE.md](ARCHITECTURE.md)** - System design, failure analysis, implementation
- **[OPERATIONS.md](OPERATIONS.md)** - Deployment, monitoring, troubleshooting

## Configuration Files

- `.env` - Service configuration (MySQL, MongoDB, patterns)
- `go.mod` - Go dependencies
- `systemd/sdl.service` - Systemd service definition (see OPERATIONS.md)

## License

This is a production system for critical data auditing. Ensure proper testing before deployment.

## Support

For detailed information on:
- **Deployment** → See [OPERATIONS.md](OPERATIONS.md)
- **Architecture** → See [ARCHITECTURE.md](ARCHITECTURE.md)
- **Failure Scenarios** → See [ARCHITECTURE.md#failure-scenarios--protection](ARCHITECTURE.md#failure-scenarios--protection)
- **MySQL Configuration** → See [OPERATIONS.md#mysql-configuration](OPERATIONS.md#mysql-configuration)
- **MongoDB Setup** → See [OPERATIONS.md#mongodb-configuration](OPERATIONS.md#mongodb-configuration)
- **Monitoring** → See [OPERATIONS.md#monitoring--alerts](OPERATIONS.md#monitoring--alerts)
