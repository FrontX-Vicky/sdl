# Performance Optimization - MongoDB Indexes

## Required Indexes for Optimal Performance

Run these commands in MongoDB to create the necessary indexes:

```javascript
// Connect to MongoDB
use audit

// 1. Primary compound index for time-based queries
db.row_changes.createIndex(
  { "ts": -1, "meta.db": 1, "meta.tbl": 1 },
  { name: "idx_ts_db_tbl", background: true }
)

// 2. Index for database + table queries
db.row_changes.createIndex(
  { "meta.db": 1, "meta.tbl": 1, "ts": -1 },
  { name: "idx_db_tbl_ts", background: true }
)

// 3. Index for primary key lookups
db.row_changes.createIndex(
  { "meta.pk": 1, "ts": -1 },
  { name: "idx_pk_ts", background: true }
)

// 4. Index for operation type queries
db.row_changes.createIndex(
  { "op": 1, "ts": -1 },
  { name: "idx_op_ts", background: true }
)

// 5. Compound index for common filter combinations
db.row_changes.createIndex(
  { "meta.db": 1, "meta.tbl": 1, "meta.pk": 1, "ts": -1 },
  { name: "idx_db_tbl_pk_ts", background: true }
)

// Verify indexes
db.row_changes.getIndexes()
```

## Expected Performance Improvements

| Scenario | Before | After | Improvement |
|----------|--------|-------|-------------|
| Time range query | Full scan | Index scan | **50-100x faster** |
| DB + Table filter | Full scan | Index scan | **100-500x faster** |
| PK lookup | Full scan | Index scan | **200-1000x faster** |
| Large result sets | Slow cursor | Batch optimized | **2-5x faster** |

## Query Optimization Features Added

### 1. **Index Hints**
```go
SetHint(bson.D{{Key: "ts", Value: -1}})
```
Forces MongoDB to use the timestamp index for better performance.

### 2. **Batch Size Optimization**
```go
SetBatchSize(1000)
```
Retrieves documents in efficient batches of 1000.

### 3. **Query Timeout Reduction**
```go
context.WithTimeout(context.Background(), 30*time.Second)
```
Changed from 10 minutes to 30 seconds for better responsiveness.

### 4. **Field Projection**
```go
SetProjection(bson.M{
    "_id": 1, "ts": 1, "op": 1, "meta": 1, "chg": 1
})
```
Only fetches needed fields, reducing network transfer.

### 5. **Pre-allocated Slices**
```go
events := make([]EventDoc, 0, int(params.Limit))
```
Reduces memory allocations and garbage collection.

## Graph Visualization Improvements

### 1. **Increased Resolution**
- Bucket count: 30 → **60 minutes** for finer granularity
- Minimum height: 4 → **6 lines** for better readability
- Minimum width: 20 → **30 characters** for clearer trends

### 2. **Enhanced Legend**
```
● INS:1234  ● UPD:567  ● DEL:89  Max/min: 45
```
Shows totals and max value at a glance.

### 3. **Better Y-axis Scaling**
- Values > 1000 shown as "K" (e.g., "1.2K")
- Improved label formatting for readability

### 4. **Caching**
- Graph rendered once and cached
- Only recomputed when data changes
- Saves CPU on rapid UI updates

## UI Performance Optimizations

### 1. **Table Rendering**
- Pre-allocated color maps for operations
- Batch row removal instead of sequential
- Optimized string formatting

### 2. **Graph Caching**
```go
cachedGraph string
cacheValid  bool
```
Graph only re-rendered when data changes or dimensions change.

### 3. **Smart Redraws**
- Status updates don't trigger full graph recalculation
- Auto-refresh invalidates cache efficiently

## Monitoring Performance

### Check Index Usage
```javascript
// See which indexes are being used
db.row_changes.aggregate([
  { $indexStats: {} }
])

// Check slow queries
db.setProfilingLevel(1, { slowms: 100 })
db.system.profile.find().limit(5).sort({ ts: -1 }).pretty()
```

### Expected Query Times (with indexes)

| Operation | Documents | Time (avg) |
|-----------|-----------|------------|
| Last 100 events | 100 | < 50ms |
| Filter by DB+Table | 1,000 | < 100ms |
| Filter by PK | 10 | < 10ms |
| Time range (1 day) | 10,000 | < 200ms |
| Time range (1 week) | 100,000 | < 500ms |

## Deployment Checklist

- [ ] Create all 5 indexes in MongoDB
- [ ] Verify indexes with `db.row_changes.getIndexes()`
- [ ] Test query performance with `explain()`
- [ ] Monitor index usage with `$indexStats`
- [ ] Rebuild binary: `go build -o fetch.exe fetch.go`
- [ ] Deploy updated binary to server

---

**Note**: Creating indexes on large collections may take time. Use `background: true` to avoid blocking other operations.
