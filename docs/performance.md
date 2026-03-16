# Performance & Indexes

## Required MongoDB Indexes

Run once after setting up the `audit` database:

```js
use audit

// 1. Primary time-based queries
db.row_changes.createIndex(
  { "ts": -1, "meta.db": 1, "meta.tbl": 1 },
  { name: "idx_ts_db_tbl", background: true }
)

// 2. DB + table filtering
db.row_changes.createIndex(
  { "meta.db": 1, "meta.tbl": 1, "ts": -1 },
  { name: "idx_db_tbl_ts", background: true }
)

// 3. Primary key lookups
db.row_changes.createIndex(
  { "meta.pk": 1, "ts": -1 },
  { name: "idx_pk_ts", background: true }
)

// 4. Operation type filtering
db.row_changes.createIndex(
  { "op": 1, "ts": -1 },
  { name: "idx_op_ts", background: true }
)

// 5. Full filter combination
db.row_changes.createIndex(
  { "meta.db": 1, "meta.tbl": 1, "meta.pk": 1, "ts": -1 },
  { name: "idx_db_tbl_pk_ts", background: true }
)

// Staging collection (required for logger)
db.row_changes_staging.createIndex({ "status": 1 })
db.row_changes_staging.createIndex(
  { "createdAt": 1 },
  { expireAfterSeconds: 604800 }   // 7-day auto-cleanup
)

// Verify
db.row_changes.getIndexes()
db.row_changes_staging.getIndexes()
```

## Expected Query Times (with indexes)

| Query | Documents | Time |
|-------|-----------|------|
| Last 100 events | 100 | < 50 ms |
| Filter by DB + table | 1 000 | < 100 ms |
| Filter by PK | 10 | < 10 ms |
| Time range (1 day) | 10 000 | < 200 ms |
| Time range (1 week) | 100 000 | < 500 ms |

## Performance Improvements Over Full Scan

| Scenario | Without indexes | With indexes | Gain |
|----------|----------------|-------------|------|
| Time range query | Full scan | Index scan | 50–100× |
| DB + table filter | Full scan | Index scan | 100–500× |
| PK lookup | Full scan | Index scan | 200–1 000× |

## sdl_fetch TUI Optimizations

The fetch tool applies these automatically:

| Optimization | Detail |
|-------------|--------|
| Index hints | Forces `{ ts: -1 }` index on time-range queries |
| Batch size | 1,000 docs per cursor batch |
| Field projection | Only fetches `_id`, `ts`, `op`, `meta`, `chg` |
| Query timeout | 30-second limit per query |
| Pre-allocated slices | `make([]EventDoc, 0, limit)` |
| Graph caching | Re-rendered only when data or dimensions change |

## Monitoring Index Usage

```js
// See which indexes are being hit
db.row_changes.aggregate([{ $indexStats: {} }])

// Explain a query
db.row_changes.find(
  { "meta.db": "myapp", "ts": { $gte: ISODate("2026-03-01") } }
).explain("executionStats")
```

## Deployment Checklist

- [ ] All 5 `row_changes` indexes created
- [ ] Staging indexes + TTL index created
- [ ] Verify with `db.row_changes.getIndexes()`
- [ ] Test performance with `.explain("executionStats")`
- [ ] Monitor with `$indexStats` after 24 hours of load
