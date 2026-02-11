# Critical Failure Scenarios Analysis

## Current Implementation Status

### ✓ WORKING SCENARIOS
1. **Graceful Shutdown (SIGTERM/SIGINT)** - Batch flushed before exit
2. **MongoDB Transient Failures** - Retry with exponential backoff (5 retries)
3. **Duplicate Events** - Duplicate key errors (11000) are ignored
4. **Virtual Columns** - Properly handled with bounds checking

---

## ⚠️ CRITICAL GAPS - DATA LOSS RISKS

### 1. **MySQL FAILS** ❌
**Current State:** No explicit MySQL reconnection handling
**Risk Level:** HIGH

**What happens:**
- Canal library handles reconnection internally, BUT:
  - In-memory batch (< 100 events) stays in memory
  - If service crashes before flush, batch is lost
  - GTID offset is NOT saved for events in current batch

**Required Fix:**
- Persist uncommitted batch to MongoDB immediately (with batch ID marker)
- OR: Flush batch more frequently (every N seconds, not just every 100 events)
- OR: Implement WAL (Write-Ahead Log) on disk for uncommitted events

---

### 2. **MONGODB PERSISTENT FAILURE** ❌
**Current State:** Retry logic exists, but behavior on persistent failure is unclear
**Risk Level:** CRITICAL

**What happens:**
- After 5 retries fail, `writeBatchWithGTID()` returns error
- `addDoc()` returns error
- `OnRow()` returns error → **Canal STOPS processing**
- Events are queued in MySQL binlog, batch stays in memory
- If service crashes, batch is lost

**Required Fix:**
- Add circuit breaker pattern: if MongoDB fails for X seconds, flush to local WAL
- Implement persistent queue (SQLite/RocksDB) as fallback
- Don't stop canal immediately, buffer events locally

---

### 3. **SERVICE STOPS UNEXPECTEDLY** ❌
**Current State:** In-memory batch vulnerable
**Risk Level:** CRITICAL

**What happens:**
```
If batch has 50 events and service crashes:
- Batch is lost (not in MongoDB)
- GTID offset NOT saved
- On restart: Replication resumes from last GTID
- Events 51-100 (that were lost) are re-fetched from MySQL
- But MongoDB has duplicates (first 50 events might be there from previous run)
```

**Scenarios:**
- Kill -9 (no signal handling)
- Server reboot
- OOM killer
- Segfault

**Required Fix:**
- Persist batch to MongoDB WAL collection immediately
- Use "two-phase commit":
  1. Write to `batch_staging` collection
  2. Write to `row_changes` + `offsets` in transaction
  3. Clean up `batch_staging`

---

### 4. **MULTIPLE RESTARTS/CASCADING FAILURES** ❌
**Current State:** GTID tracking is correct, but idempotency not guaranteed
**Risk Level:** CRITICAL

**What happens:**
```
Run 1: Events A,B,C written to MongoDB, GTID=100 saved
Run 1: Service crashes before flushing next 50 events (D,E,F)
Run 2: Load GTID=100, resume from 101
Run 2: Events D,E,F fetched again
Run 2: MongoDB write succeeds, GTID=103 saved
Run 2: Service crashes, events G,H in memory
Run 3: Load GTID=103, resume from 104
Run 3: Events G,H fetched again → duplicates in MongoDB!
```

**Issue:** Events G,H have same doc IDs as from Run 2 (makeID is deterministic)
→ Duplicate key error → ignored → seems OK, but:
- If another error happens, partial batch might be lost
- Event timing is slightly different → doc might look different

**Required Fix:**
- Use idempotent event IDs (include batch counter/sequence)
- OR: Mark events with batch_id + sequence_number
- OR: Use upsert instead of insert for event deduplication

---

### 5. **SCHEMA CHANGES** ⚠️
**Current State:** Bounds checking exists, but schema migration handling missing
**Risk Level:** MEDIUM-HIGH

**What happens:**
```
Schema: id, name, email (3 cols)
Binlog row data: 3 values

Column added: id, name, email, age (4 cols in schema, but not in binlog)
Solution: Code handles by using min(colNames, rowLength)

BUT: If column is DROPPED:
Schema: id, name, email, age (4 cols)
Binlog row data: 3 values (old schema)
→ Column index mismatch!
→ "email" might be read as "age" value
```

**Required Fix:**
- Add schema change detection handler (OnTableChanged)
- Store schema version with each event
- Handle schema evolution in change tracking

---

### 6. **BINLOG UNAVAILABLE/ROTATED** ❌
**Current State:** Fallback to master's GTID exists, but timing gap
**Risk Level:** HIGH

**What happens:**
```
GTID saved: 8707a53a-19b3-11eb-9300-62b5e2617066:1-100

If binlog is purged before service reconnects:
- GTID 1-100 no longer exists in binlog
- StartFromGTID fails
- Fallback: Get master's current GTID (e.g., 1-500)
- Start from 1-500: Events 101-500 are LOST!
```

**Required Fix:**
- Save not just GTID but also binlog file + position
- Add logic: if GTID not found, check if binlog file still exists
- If not: Alert and require manual intervention (don't silently skip)
- Implement binlog purge detection/alerting

---

## SUMMARY TABLE

| Scenario | Current | Risk | Impact |
|----------|---------|------|--------|
| MySQL fails | Canal retry (implicit) | HIGH | Batch in memory lost |
| MongoDB transient fails | Retry + backoff | MEDIUM | Works as designed |
| MongoDB persistent fails | Stops replication | CRITICAL | Data loss |
| Service crash (unexpected) | Batch lost | CRITICAL | Data loss |
| Multiple restarts | GTID tracking OK | MEDIUM | Potential duplicates |
| Schema changes | Handled (partial) | MEDIUM | Column mismatch possible |
| Binlog rotation | Fallback exists | CRITICAL | Silent data loss |

---

## RECOMMENDED FIXES (Priority Order)

1. **[CRITICAL]** Implement persistent batch WAL (Write-Ahead Log)
   - Flush batch to MongoDB `batch_staging` immediately
   - Atomic transaction to promote from staging to final

2. **[CRITICAL]** MongoDB persistence strategy for failure
   - Circuit breaker pattern
   - Local queue (SQLite/RocksDB) fallback
   - Async reconciliation when MongoDB recovers

3. **[CRITICAL]** Binlog rotation detection
   - Monitor binlog file age
   - Alert when GTID not found
   - Require manual recovery

4. **[HIGH]** Schema change handling
   - Track schema version per table
   - Store with each event
   - Validate column indices

5. **[MEDIUM]** Event deduplication enhancement
   - Use batch_id + sequence instead of just doc ID
   - Enable upsert mode for safety

6. **[MEDIUM]** Circuit breaker for MongoDB
   - When N retries fail in X seconds → trigger fallback
   - Graceful degradation instead of hard stop

