# MySQL Binlog to MongoDB Audit Logger

This project contains two Go scripts for MySQL database auditing:

1. **script.go** - Captures MySQL binlog events and stores them in MongoDB
2. **fetch.go** - Queries and retrieves audit logs from MongoDB

## Setup

### Prerequisites

- Go 1.18 or higher
- MySQL with binlog enabled (GTID mode recommended)
- MongoDB server

### Install Dependencies

```bash
go mod init sdl
go get github.com/joho/godotenv
go get github.com/go-mysql-org/go-mysql/canal
go get github.com/go-mysql-org/go-mysql/mysql
go get github.com/go-mysql-org/go-mysql/replication
go get go.mongodb.org/mongo-driver/bson
go get go.mongodb.org/mongo-driver/mongo
go get go.mongodb.org/mongo-driver/mongo/options
```

### Configuration

Create a `.env` file in the same directory:

```env
# MySQL Configuration
MYSQL_ADDR=127.0.0.1:3306
MYSQL_USER=repl
MYSQL_PASS=your_password
MYSQL_FLAVOR=mysql
MYSQL_SERVER_ID=2222

# MongoDB Configuration
MONGO_URI=mongodb://127.0.0.1:27017/?appName=audit
MONGO_DB=audit
MONGO_COLL=row_changes
MONGO_OFFSETS_COLL=binlog_offsets

# Include/Exclude Patterns
INCLUDE_REGEX=.*\..*
EXCLUDE_REGEX=^(mysql|performance_schema|information_schema|sys)\..*

# Timezone
TZ=Asia/Kolkata
```

## Usage

### 1. Start the Audit Logger (script.go)

This will continuously capture MySQL changes and store them in MongoDB:

```bash
go run script.go
```

### 2. Query Audit Logs (fetch.go)

Launch the interactive TUI to view and analyze audit logs:

```bash
go run fetch.go
```

**TUI Features:**
- **Real-time activity graphs** (60-minute window, INS/UPD/DEL per minute)
- **Advanced filtering** by database, table, primary key, date range
- **Auto-refresh** every 1 second (F10 to toggle)
- **Export** to CSV/JSON (F9)
- **Event details** view (Enter on event)
- **Paste support** in input fields (Ctrl+V, Shift+Insert, Right-click)
- **Loading indicators** for better UX
- **Optimized queries** with MongoDB index hints
- **Graph caching** for smooth performance

**Performance:**
- Queries optimized with proper indexes (see PERFORMANCE_INDEXES.md)
- Batch retrieval (1000 docs at a time)
- Field projection to minimize data transfer
- Cached graph rendering for smooth UI
- 30-second query timeout for responsiveness

**Keyboard Shortcuts:**
- `F1` - Set filters
- `F5` - Refresh data
- `F9` - Export to file
- `F10` - Toggle auto-refresh
- `?` - Help
- `q` - Quit
- `Enter` - View event details
- `ESC` - Close dialog

## fetch.go Examples

The script includes several query examples:

### Example 1: Fetch all events for a specific table
```go
events, err := fetchEvents(coll, QueryParams{
    Database: "your_database",
    Table:    "users",
    Limit:    10,
})
```

### Example 2: Fetch events for a specific primary key
```go
events, err := fetchEvents(coll, QueryParams{
    Database: "your_database",
    Table:    "users",
    PK:       "123",
    Limit:    5,
})
```

### Example 3: Fetch only specific operations
```go
events, err := fetchEvents(coll, QueryParams{
    Database:  "your_database",
    Table:     "users",
    Operation: "u",  // "i"=insert, "u"=update, "d"=delete
    Limit:     5,
})
```

### Example 4: Fetch events within a time range
```go
events, err := fetchEvents(coll, QueryParams{
    Database:  "your_database",
    StartTime: yesterday,
    EndTime:   now,
    Limit:     10,
})
```

### Example 5: Export to JSON
```go
events, err := fetchEvents(coll, QueryParams{
    Database: "your_database",
    Table:    "users",
    Limit:    100,
})
exportToJSON(events, "audit_export.json")
```

## Customizing fetch.go

You can modify the `main()` function in `fetch.go` to query specific data:

```go
func main() {
    coll, err := connectMongo()
    if err != nil {
        log.Fatalf("Failed to connect to MongoDB: %v", err)
    }

    // Your custom query
    events, err := fetchEvents(coll, QueryParams{
        Database: "my_database",
        Table:    "my_table",
        PK:       "specific_id",
        Limit:    20,
    })
    
    if err != nil {
        log.Fatalf("Error: %v", err)
    }
    
    printEvents(events)
}
```

## Event Document Structure

Each audit event in MongoDB has the following structure:

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

## Operations

- `i` - INSERT
- `u` - UPDATE
- `d` - DELETE

## Troubleshooting

### Connection Issues

If you can't connect to MongoDB or MySQL, verify:
- Connection strings in `.env`
- Network connectivity
- Credentials

### No Events Found

- Check that `script.go` is running
- Verify MySQL binlog is enabled
- Check include/exclude regex patterns

### Dependencies Not Found

Run:
```bash
go mod tidy
```

## Notes

- The audit logger (`script.go`) should run continuously as a service
- The fetch script (`fetch.go`) is for ad-hoc queries and analysis
- Adjust the `INCLUDE_REGEX` and `EXCLUDE_REGEX` to filter specific tables
- Binlog position and GTID are automatically tracked for resumability
