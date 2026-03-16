# sdl_fetch (TUI Query Tool)

` sdl_fetch ` is the terminal UI for querying and exporting audit events stored in MongoDB.

## Build

```bash
cd sdl_fetch
go build -o sdl_fetch .
```

## Run

```bash
cd sdl_fetch
./sdl_fetch
```

## Features

- Real-time activity graphs (60-minute window for INS/UPD/DEL)
- Filter by database, table, primary key, operation, and date range
- Auto-refresh (toggle on/off)
- Export results to CSV or JSON
- Event detail viewer
- Paste support in input fields
- Optimized MongoDB queries with index hints and projection

## Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `F1` | Set filters |
| `F5` | Refresh data |
| `F9` | Export current view |
| `F10` | Toggle auto-refresh |
| `Enter` | View selected event details |
| `ESC` | Close dialog |
| `?` | Help |
| `q` | Quit |

## Query Examples (code-level)

```go
// All events for a table
events, err := fetchEvents(coll, QueryParams{
    Database: "my_database",
    Table:    "users",
    Limit:    10,
})

// Events for a specific primary key
events, err := fetchEvents(coll, QueryParams{
    Database: "my_database",
    Table:    "users",
    PK:       "123",
    Limit:    5,
})

// Filter by operation type
events, err := fetchEvents(coll, QueryParams{
    Database:  "my_database",
    Table:     "users",
    Operation: "u", // i/u/d
    Limit:     5,
})

// Time range query
events, err := fetchEvents(coll, QueryParams{
    Database:  "my_database",
    StartTime: yesterday,
    EndTime:   now,
    Limit:     10,
})

// Export JSON
events, err := fetchEvents(coll, QueryParams{
    Database: "my_database",
    Table:    "users",
    Limit:    100,
})
exportToJSON(events, "audit_export.json")
```

## Event Shape

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

## Troubleshooting

- Connection issues: verify `MONGO_URI` / MySQL settings in `.env`
- No events: confirm logger is running and include/exclude regex allow your tables
- Dependency errors: run `go mod tidy`

## Related Docs

- Main docs index: [README.md](README.md)
- Performance/indexes: [performance.md](performance.md)
- Operations: [operations.md](operations.md)
