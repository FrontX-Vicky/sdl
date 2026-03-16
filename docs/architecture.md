# Architecture

## System Overview

```
MySQL (binlog)
     │  ROW events via canal
     ▼
main.go  ──staging──▶  MongoDB row_changes_staging  (crash buffer)
     │                         │
     └──transaction──▶  MongoDB row_changes  +  binlog_offsets
                               │
              ┌────────────────┼────────────────┐
              ▼                ▼                ▼
          view.go          sdl_fetch        sdl_restore
       (live tail)        (TUI query)     (replay → MySQL)

                     sql_import
                  (dump file → MySQL)
```

## Key Data Structures

### EventDoc (stored in MongoDB)

```go
type EventDoc struct {
    ID    string           `bson:"_id"`      // SHA1 deterministic hash
    TS    time.Time        `bson:"ts"`       // UTC timestamp
    OP    string           `bson:"op"`       // "i" | "u" | "d"
    Meta  Meta             `bson:"meta"`     // DB, table, PK
    Seq   int64            `bson:"seq"`
    Chg   map[string]Delta `bson:"chg"`      // Column changes
    Src   map[string]any   `bson:"src"`      // Binlog coordinates
    TSIST string           `bson:"ts_ist"`   // IST timestamp string
}

type Meta struct {
    DB  string `bson:"db"`
    Tbl string `bson:"tbl"`
    PK  any    `bson:"pk"`
}

type Delta struct {
    F any `bson:"f,omitempty"`   // from (old value)
    T any `bson:"t,omitempty"`   // to  (new value)
}
```

Example document:
```json
{
  "_id": "a3f2...sha1hash",
  "ts": "2026-03-01T10:30:00Z",
  "op": "u",
  "meta": { "db": "myapp", "tbl": "users", "pk": 42 },
  "chg": {
    "email": { "f": "old@example.com", "t": "new@example.com" }
  },
  "src": { "binlog": { "file": "mysql-bin.000012", "pos": 98765 } },
  "ts_ist": "2026-03-01 16:00:00"
}
```

### MongoSink

```go
type MongoSink struct {
    client    *mongo.Client
    events    *mongo.Collection   // row_changes
    offsets   *mongo.Collection   // binlog_offsets
    staging   *mongo.Collection   // row_changes_staging
    loc       *time.Location
    failCount int
    lastErr   error
}
```

### Handler (binlog processor)

```go
type Handler struct {
    canal.DummyEventHandler
    sink         *MongoSink
    batch        []EventDoc
    lastGTID     string
    tableSchemas map[string][]string
    // batch position tracking, etc.
}
```

---

## Write Pipeline (Zero Data Loss)

Every batch goes through a two-phase commit:

```
OnRow() event arrives
        │
        ▼
Append to in-memory batch
        │
  (batch full or flush)
        │
        ▼
Phase 1: Write batch → staging  (status: "pending")
        │              ← crash here: recovery finds it on restart
        ▼
Phase 2: MongoDB transaction:
          INSERT batch into row_changes
          UPDATE binlog_offsets (GTID)
          Mark staging (status: "committed")
        │              ← crash here: idempotent retry on restart
        ▼
Continue
```

### Crash Recovery

On startup, `RecoverPendingBatches()` scans `row_changes_staging` for `status: "pending"` documents and reprocesses them. Because event IDs are deterministic (SHA1 of source + ts + db + tbl + pk + op), duplicate key errors (11000) are silently ignored.

### Retry Policy

Transient MongoDB errors trigger exponential backoff:

| Attempt | Wait |
|---------|------|
| 1 | 100 ms |
| 2 | 200 ms |
| 3 | 400 ms |
| 4 | 800 ms |
| 5 | 1.6 s |

Non-retryable errors (auth failure, invalid document) fail immediately.

---

## Failure Scenarios

### MySQL Connection Lost
- Canal library pauses event delivery.
- In-memory batch persists in staging on next flush.
- On reconnect, Canal resumes from last saved GTID.

### MongoDB Transient Failure
- Retry with backoff (see above).
- If all retries fail, batch stays in staging — recovered on next restart.

### Service Crash (kill -9 / OOM)
- If crash before Phase 1: events re-fetched from binlog on restart (GTID tracking).
- If crash after Phase 1: staging recovery replays the batch idempotently.

### Schema Change (ALTER TABLE)
- `OnTableChanged()` clears the cached column schema for that table.
- Current in-memory batch is flushed before processing the next event.
- Prevents index-out-of-range panics when column count changes.

### Duplicate Events
- Deterministic `_id` = same event always gets same hash.
- MongoDB duplicate key (11000) ignored silently.

---

## Idempotent Event IDs

```go
func makeID(source string, ts time.Time, db, tbl string, pk any, op string) string {
    h := sha1.New()
    fmt.Fprintf(h, "%s:%v:%s.%s:%v:%s", source, ts.Unix(), db, tbl, pk, op)
    return hex.EncodeToString(h.Sum(nil))
}
```

Restarting the logger after a crash and reprocessing the same binlog range will produce the same IDs — MongoDB will reject duplicates silently, preserving integrity.

---

## System Guarantees

**Zero data loss guaranteed when:**
- MongoDB replica set with journaling enabled
- MySQL binlog retention ≥ 14 days
- Service receives SIGTERM for graceful shutdown
- MongoDB recovers within retry window (~30 s)
- MySQL recovers within retry window (~10 min)

**Known limitations:**
- Persistent MongoDB outage > 30 s requires manual recovery from staging
- Binlog purged before reconnect causes a gap (ensure 14-day retention)
