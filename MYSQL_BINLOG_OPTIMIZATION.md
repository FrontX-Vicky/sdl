# MySQL Binlog Configuration for 100% Efficiency

## Critical Settings for Zero Data Loss

### 1. Edit MySQL Configuration

```bash
sudo nano /etc/mysql/mysql.conf.d/mysqld.cnf
```

Add/modify these settings under `[mysqld]`:

```ini
[mysqld]
# Binary Logging Configuration
server-id                      = 1
log_bin                        = /var/log/mysql/mysql-bin.log
binlog_format                  = ROW
binlog_row_image               = FULL

# GTID Mode (CRITICAL - Already enabled)
gtid_mode                      = ON
enforce_gtid_consistency       = ON

# Binlog Retention (CRITICAL FOR DATA SAFETY)
binlog_expire_logs_seconds     = 1209600    # 14 days (2 weeks)
# OR for older MySQL versions:
# expire_logs_days             = 14

# Sync Settings (100% Durability - Performance Impact!)
sync_binlog                    = 1          # Sync every transaction (safest)
innodb_flush_log_at_trx_commit = 1          # Sync InnoDB logs every commit

# Binlog Cache (Performance Optimization)
binlog_cache_size              = 4M         # Per-connection binlog cache
max_binlog_cache_size          = 512M       # Max cache per connection
max_binlog_size                = 512M       # Rotate at 512MB

# Row Event Optimization
binlog_rows_query_log_events   = ON         # Include original SQL in binlog
log_slave_updates              = ON         # If using replication

# Transaction Isolation
transaction_isolation          = READ-COMMITTED

# Connection Settings
max_connections                = 500
max_allowed_packet             = 64M
```

### 2. Apply Configuration

```bash
# Restart MySQL
sudo systemctl restart mysql

# Verify settings
mysql -u root -p
```

### 3. Verify in MySQL

```sql
-- Check binlog format
SHOW VARIABLES LIKE 'binlog_format';
-- Should be: ROW

-- Check GTID mode
SHOW VARIABLES LIKE 'gtid_mode';
-- Should be: ON

-- Check sync settings
SHOW VARIABLES LIKE 'sync_binlog';
-- Should be: 1

SHOW VARIABLES LIKE 'innodb_flush_log_at_trx_commit';
-- Should be: 1

-- Check binlog retention
SHOW VARIABLES LIKE 'binlog_expire_logs_seconds';
-- Should be: 1209600 (14 days)

-- Check current binlogs
SHOW BINARY LOGS;

-- Check binlog position
SHOW MASTER STATUS;
```

---

## Performance vs Safety Trade-offs

### Maximum Safety (RECOMMENDED for Data-Critical)
```ini
sync_binlog                    = 1    # Every transaction synced to disk
innodb_flush_log_at_trx_commit = 1    # Every commit flushed
binlog_expire_logs_seconds     = 1209600  # 14 days retention
```
**Impact**: ~10-20% write performance reduction, but **zero data loss** on crashes

### Balanced (Acceptable Risk)
```ini
sync_binlog                    = 10   # Sync every 10 transactions
innodb_flush_log_at_trx_commit = 2    # Flush every second
binlog_expire_logs_seconds     = 604800  # 7 days retention
```
**Impact**: Better performance, but ~1 second of data loss on OS crash

### High Performance (Not Recommended for Critical Data)
```ini
sync_binlog                    = 0    # OS decides when to sync
innodb_flush_log_at_trx_commit = 0    # Flush every second
binlog_expire_logs_seconds     = 259200  # 3 days retention
```
**Impact**: Best performance, but risk of several seconds data loss on crashes

---

## Data Safety: Full Stack Configuration

### 1. MongoDB as Regular Collection (Not Time-Series)

**Current Issue**: Time-series collections don't support transactions

**Fix**: Convert to regular collection (see previous instructions)

**Result**: 
- ✅ Atomic writes (events + GTID together)
- ✅ Zero data loss on service crashes
- ✅ No duplicate events

### 2. MySQL Binlog Settings

```ini
sync_binlog                    = 1
innodb_flush_log_at_trx_commit = 1
binlog_expire_logs_seconds     = 1209600  # 14 days
```

**Result**:
- ✅ Zero data loss on MySQL crashes
- ✅ Binlog available for 14 days (service can be down for 2 weeks and still recover)

### 3. Service Configuration (Already Implemented)

- ✅ Staging collection for crash recovery
- ✅ Exponential backoff retry
- ✅ Graceful shutdown (signal handlers)
- ✅ GTID-based position tracking
- ✅ Idempotent event IDs (no duplicates on retry)

---

## Monitoring Queries

### Check Binlog Disk Usage
```bash
du -sh /var/log/mysql/
```

### Check Oldest Binlog
```sql
SHOW BINARY LOGS;
```
Should show binlogs going back 14 days.

### Check Replication Lag (Service Side)
```javascript
// In MongoDB
use audit
db.row_changes.find().sort({ts: -1}).limit(1)
```
Compare `ts` field to current time. Lag should be < 1 second under normal load.

### Check Service Position
```javascript
use audit
db.binlog_offsets.find()
```
Compare GTID to MySQL's current GTID:
```sql
-- In MySQL
SELECT @@GLOBAL.gtid_executed;
```

---

## Binlog Rotation Strategy

### Automatic Rotation
```ini
max_binlog_size = 512M  # Rotate when file reaches 512MB
```

### Manual Rotation (if needed)
```sql
FLUSH BINARY LOGS;
```

### Purge Old Binlogs (automatic with retention setting)
```sql
-- Check what will be purged
SHOW BINARY LOGS;

-- Manual purge (be careful!)
-- Only purge if service has caught up
PURGE BINARY LOGS BEFORE NOW() - INTERVAL 7 DAY;
```

---

## Full Data Safety Checklist

✅ **MySQL Configuration**
- [ ] `sync_binlog = 1`
- [ ] `innodb_flush_log_at_trx_commit = 1`
- [ ] `binlog_expire_logs_seconds = 1209600` (14 days)
- [ ] `binlog_format = ROW`
- [ ] `gtid_mode = ON`

✅ **MongoDB Configuration**
- [ ] Replica set initialized (`rs.initiate()`)
- [ ] Collection is regular (NOT time-series)
- [ ] Indexes created on `_id`, `ts`, `meta.db`, `meta.tbl`

✅ **Service Configuration**
- [ ] Staging collection enabled (already in code)
- [ ] Graceful shutdown handlers (already in code)
- [ ] Retry logic active (already in code)

✅ **Monitoring**
- [ ] Check replication lag daily
- [ ] Monitor binlog disk usage
- [ ] Alert on service errors (`journalctl -u sdl.service`)
- [ ] Verify no gaps in `row_changes` collection

---

## Disk Space Requirements

### Binlog Retention Calculation
```
Average binlog size per day = (writes per second) × (average row size) × 86400
```

**Example**:
- 100 writes/sec
- 500 bytes per row
- 14 days retention

```
100 × 500 × 86400 × 14 = ~60 GB
```

**Recommendation**: Allocate **at least 100 GB** for binlog partition if using 14-day retention.

### MongoDB Storage
```
Events per day × Average document size × Retention period
```

**Example**:
- 8.6M events/day (100 writes/sec)
- 1 KB per document
- Keep forever (audit trail)

```
First year: 8.6M × 1KB × 365 = ~3.1 TB
```

**Recommendation**: Plan for **5+ TB** MongoDB storage with compression.

---

## Quick Commands Reference

### MySQL
```bash
# Check binlog status
mysql -e "SHOW BINARY LOGS;"

# Check current GTID
mysql -e "SELECT @@GLOBAL.gtid_executed;"

# Monitor live changes
mysqlbinlog --read-from-remote-server --host=127.0.0.1 --port=3306 --user=developer --password=yourpass --stop-never mysql-bin.999999
```

### MongoDB
```bash
# Check collection type
mongosh audit --eval "db.row_changes.stats().timeseries"

# Check latest events
mongosh audit --eval "db.row_changes.find().sort({ts: -1}).limit(10)"

# Check service position
mongosh audit --eval "db.binlog_offsets.find().pretty()"

# Check staging (should be mostly empty or committed)
mongosh audit --eval 'db.row_changes_staging.find({status: "pending"})'
```

### Service
```bash
# Check service status
sudo systemctl status sdl.service

# Watch live logs
sudo journalctl -u sdl.service -f

# Restart service
sudo systemctl restart sdl.service

# Check service errors
sudo journalctl -u sdl.service -p err -n 100
```

---

## Final Recommendation for 100% Data Safety

### Priority 1: Convert MongoDB to Regular Collection
```javascript
// This is THE MOST IMPORTANT step
use audit
db.row_changes.drop()
db.createCollection("row_changes")
db.row_changes.createIndex({ "_id": 1 })
db.row_changes.createIndex({ "ts": -1 })
```

### Priority 2: MySQL Binlog Settings
```ini
sync_binlog = 1
innodb_flush_log_at_trx_commit = 1
binlog_expire_logs_seconds = 1209600
```

### Priority 3: Monitor Daily
```bash
# Check lag
mongosh audit --eval "db.row_changes.find().sort({ts: -1}).limit(1)"

# Check errors
sudo journalctl -u sdl.service -p err --since today
```

With these settings, you'll have **maximum data safety** at the cost of ~10-20% MySQL write performance.
