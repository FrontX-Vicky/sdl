# Operations & Deployment Guide

## Table of Contents
1. [Quick Reference](#quick-reference)
2. [Production Deployment](#production-deployment)
3. [MySQL Configuration](#mysql-configuration)
4. [MongoDB Configuration](#mongodb-configuration)
5. [Monitoring & Alerts](#monitoring--alerts)
6. [Troubleshooting](#troubleshooting)
7. [Emergency Procedures](#emergency-procedures)

---

## Quick Reference

### What's Protected

#### Scenario 1: MySQL Connection Lost
**Protection:** Batch stays in MongoDB staging until committed
```
Event → Staging (persistent!) → Transaction → Committed
```
**If crash:** Restart finds batch in staging → Recover or retry

#### Scenario 2: MongoDB Transient Failure  
**Protection:** Automatic retry with exponential backoff
```
Retry 1 (100ms) → Retry 2 (200ms) → Retry 3 (400ms) → ... → Fail safely
```
**If persistent:** Batch in staging, recoverable on restart

#### Scenario 3: Service Crash
**Protection:** Startup recovery scans staging collection
```
Service crash → Staging has uncommitted batch → Restart → Recover
```
**Result:** Zero data loss, events re-processed safely

#### Scenario 4: Multiple Cascading Failures
**Protection:** GTID offset + idempotent event IDs
```
Crash #1 → Batch A in staging
Crash #2 → Batch A retried (duplicate key handled)
Crash #3 → Batch B in staging
Result: Each crash independent, GTID always advancing
```

#### Scenario 5: Schema Changes
**Protection:** Bounds checking + schema invalidation
```
Schema change detected → Flush current batch → Clear schema cache
```
**Result:** No index out of range errors

#### Scenario 6: Binlog Rotation
**Status:** Documented limitation, requires binlog retention
```
Keep binlog ≥ 14 days to prevent silent data loss
```

---

## Production Deployment

### Prerequisites Checklist

#### MongoDB
- [ ] Running as replica set (required for transactions)
- [ ] Journaling enabled (durability)
- [ ] Version 4.0+ (transaction support)
- [ ] Sufficient disk space (5+ TB recommended for audit trail)
- [ ] Indexes created (see below)

#### MySQL
- [ ] GTID mode enabled (`gtid_mode = ON`)
- [ ] Binlog format ROW (`binlog_format = ROW`)
- [ ] Binlog retention 14+ days (`binlog_expire_logs_seconds = 1209600`)
- [ ] Replication user created with appropriate privileges
- [ ] Sufficient disk space for binlogs (100+ GB recommended)

#### System
- [ ] Go 1.18+ installed
- [ ] Systemd (optional but recommended)
- [ ] Signal handling support (SIGTERM/SIGINT)
- [ ] Sufficient memory (2GB+ recommended)

### Step-by-Step Deployment

#### 1. Build the Binary
```bash
cd /path/to/sdl
go build -o sdl_binary main.go

# Verify build
./sdl_binary --version  # Should show Go version info
```

#### 2. Create .env Configuration
```bash
cat > .env << 'EOF'
# MySQL Configuration
MYSQL_ADDR=127.0.0.1:3306
MYSQL_USER=repl_user
MYSQL_PASS=secure_password
MYSQL_FLAVOR=mysql
MYSQL_SERVER_ID=2222

# MongoDB Configuration
MONGO_URI=mongodb://127.0.0.1:27017/?replicaSet=rs0&appName=audit
MONGO_DB=audit
MONGO_COLL=row_changes
MONGO_OFFSETS_COLL=binlog_offsets

# Include/Exclude Patterns
INCLUDE_REGEX=.*\..*
EXCLUDE_REGEX=^(mysql|performance_schema|information_schema|sys)\..*

# Timezone
TZ=Asia/Kolkata
EOF

chmod 600 .env  # Protect credentials
```

#### 3. Create Database Indexes
```bash
mongosh << 'MONGOEOF'
use audit

// Events collection
db.row_changes.createIndex({ "ts": 1 })
db.row_changes.createIndex({ "meta.pk": 1, "meta.db": 1, "meta.tbl": 1 })
db.row_changes.createIndex({ "_id": 1 }, { unique: true })

// Offsets collection
db.binlog_offsets.createIndex({ "_id": 1 }, { unique: true })

// Staging collection (with 7-day auto-cleanup)
db.row_changes_staging.createIndex({ "status": 1 })
db.row_changes_staging.createIndex({ "createdAt": 1 }, { 
  expireAfterSeconds: 604800 
})

// Verify indexes
db.row_changes.getIndexes()
db.row_changes_staging.getIndexes()
MONGOEOF
```

#### 4. Create Systemd Service (Optional)
```bash
sudo tee /etc/systemd/system/sdl.service << 'EOF'
[Unit]
Description=MySQL Binlog to MongoDB Audit Logger
After=network.target mongod.service mysql.service
Wants=mongod.service mysql.service

[Service]
Type=simple
User=your_user
Group=your_group
WorkingDirectory=/path/to/sdl
ExecStart=/path/to/sdl/sdl_binary
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=sdl

# Resource limits
LimitNOFILE=65536
MemoryLimit=2G

# Graceful shutdown (30 second timeout)
TimeoutStopSec=30
KillMode=mixed
KillSignal=SIGTERM

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable sdl.service
```

#### 5. Initial Start
```bash
# Test run (foreground)
./sdl_binary

# Check logs for:
# - "Recovering X pending batches" (should be 0 on first run)
# - "Connected to MongoDB"
# - "Starting from GTID" or "Starting from current position"
# - No errors

# If good, stop with Ctrl+C and start as service
sudo systemctl start sdl.service
sudo systemctl status sdl.service
```

#### 6. Post-Deployment Verification
```bash
# Check logs
sudo journalctl -u sdl.service -f

# Verify MongoDB events
mongosh audit --eval "db.row_changes.find().sort({ts: -1}).limit(5).pretty()"

# Check GTID progression
mongosh audit --eval "db.binlog_offsets.find().pretty()"

# Monitor staging (should be empty or have only "committed" status)
mongosh audit --eval 'db.row_changes_staging.find({status: "pending"}).pretty()'

# Test graceful shutdown
sudo systemctl stop sdl.service
# Check logs for "Flushing X remaining events" and "Shutdown complete"
```

---

## MySQL Configuration

### Critical Settings for Zero Data Loss

Edit MySQL configuration:
```bash
sudo nano /etc/mysql/mysql.conf.d/mysqld.cnf
```

Add/modify under `[mysqld]`:
```ini
[mysqld]
# Server identification
server-id                      = 1

# Binary Logging
log_bin                        = /var/log/mysql/mysql-bin.log
binlog_format                  = ROW
binlog_row_image               = FULL

# GTID Mode (CRITICAL)
gtid_mode                      = ON
enforce_gtid_consistency       = ON

# Binlog Retention (CRITICAL FOR DATA SAFETY)
binlog_expire_logs_seconds     = 1209600    # 14 days
# OR for MySQL < 8.0:
# expire_logs_days             = 14

# Sync Settings (100% Durability)
sync_binlog                    = 1          # Sync every transaction
innodb_flush_log_at_trx_commit = 1          # Sync InnoDB logs every commit

# Binlog Cache (Performance)
binlog_cache_size              = 4M
max_binlog_cache_size          = 512M
max_binlog_size                = 512M

# Row Event Optimization
binlog_rows_query_log_events   = ON
log_slave_updates              = ON

# Transaction Isolation
transaction_isolation          = READ-COMMITTED

# Connection Settings
max_connections                = 500
max_allowed_packet             = 64M
```

### Apply Configuration
```bash
# Restart MySQL
sudo systemctl restart mysql

# Verify settings
mysql -u root -p << 'EOF'
SHOW VARIABLES LIKE 'binlog_format';              -- Should be: ROW
SHOW VARIABLES LIKE 'gtid_mode';                  -- Should be: ON
SHOW VARIABLES LIKE 'sync_binlog';                -- Should be: 1
SHOW VARIABLES LIKE 'innodb_flush_log_at_trx_commit'; -- Should be: 1
SHOW VARIABLES LIKE 'binlog_expire_logs_seconds'; -- Should be: 1209600
SHOW BINARY LOGS;
SHOW MASTER STATUS;
EOF
```

### Performance vs Safety Trade-offs

**Maximum Safety (Recommended):**
```ini
sync_binlog                    = 1
innodb_flush_log_at_trx_commit = 1
binlog_expire_logs_seconds     = 1209600  # 14 days
```
**Impact:** ~10-20% write performance reduction, **zero data loss** on crashes

**Balanced (If performance critical):**
```ini
sync_binlog                    = 10       # Sync every 10 transactions
innodb_flush_log_at_trx_commit = 2        # OS handles flushing
binlog_expire_logs_seconds     = 604800   # 7 days
```
**Impact:** Better performance, small risk of 1-10 transactions lost on crash

**Minimum (Development only):**
```ini
sync_binlog                    = 0
innodb_flush_log_at_trx_commit = 0
binlog_expire_logs_seconds     = 86400    # 1 day
```
**Impact:** Best performance, significant data loss risk

### Create Replication User
```sql
-- Create user
CREATE USER 'repl_user'@'%' IDENTIFIED BY 'secure_password';

-- Grant replication privileges
GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'repl_user'@'%';

-- Grant read access to replicated databases
GRANT SELECT ON your_database.* TO 'repl_user'@'%';

FLUSH PRIVILEGES;

-- Test connection
-- mysql -u repl_user -p -h 127.0.0.1
```

---

## MongoDB Configuration

### Initialize Replica Set (REQUIRED)

**Standalone MongoDB:**
```bash
# Edit MongoDB config
sudo nano /etc/mongod.conf
```

Add replication settings:
```yaml
replication:
  replSetName: "rs0"

storage:
  journal:
    enabled: true
```

Restart and initialize:
```bash
sudo systemctl restart mongod

mongosh << 'EOF'
rs.initiate({
  _id: "rs0",
  members: [
    { _id: 0, host: "localhost:27017" }
  ]
})

// Wait a few seconds, then verify
rs.status()
// Should show: "stateStr" : "PRIMARY"
EOF
```

**Multi-Node Replica Set:**
```bash
mongosh << 'EOF'
rs.initiate({
  _id: "rs0",
  members: [
    { _id: 0, host: "mongo1.example.com:27017" },
    { _id: 1, host: "mongo2.example.com:27017" },
    { _id: 2, host: "mongo3.example.com:27017" }
  ]
})

rs.status()
EOF
```

### Verify Replica Set
```javascript
// Check status
rs.status()

// Check configuration
rs.conf()

// Test transaction support
use test
db.test.insertOne({test: 1})
session = db.getMongo().startSession()
session.startTransaction()
session.getDatabase("test").test.insertOne({test: 2})
session.commitTransaction()
```

### MongoDB Performance Tuning
```yaml
# /etc/mongod.conf

# Storage
storage:
  dbPath: /var/lib/mongodb
  journal:
    enabled: true
  engine: wiredTiger
  wiredTiger:
    engineConfig:
      cacheSizeGB: 8  # 50% of RAM
      journalCompressor: snappy
    collectionConfig:
      blockCompressor: snappy

# Replication
replication:
  replSetName: rs0
  oplogSizeMB: 10240  # 10GB oplog

# Network
net:
  port: 27017
  bindIp: 0.0.0.0
  maxIncomingConnections: 1000

# Security
security:
  authorization: enabled
```

---

## Monitoring & Alerts

### Key Metrics to Monitor

#### 1. Staging Collection Backlog
```javascript
// Should be near 0 in normal operation
use audit
db.row_changes_staging.countDocuments({ status: "pending" })

// Alert if > 100
```

#### 2. GTID Progression
```javascript
// Check latest GTID
use audit
db.binlog_offsets.find().pretty()

// Compare with MySQL
// mysql: SELECT @@global.gtid_executed;

// Alert if no update > 5 minutes
```

#### 3. Event Ingestion Rate
```javascript
// Events per minute
use audit
db.row_changes.countDocuments({ 
  ts: { $gte: new Date(Date.now() - 60000) } 
})

// Alert if drops to 0 unexpectedly
```

#### 4. Event Lag
```javascript
// Time difference between latest event and now
use audit
db.row_changes.find().sort({ts: -1}).limit(1).pretty()

// Alert if lag > 5 minutes
```

#### 5. Storage Usage
```bash
# MongoDB disk usage
du -sh /var/lib/mongodb

# MySQL binlog disk usage
du -sh /var/log/mysql

# Alert if > 80% capacity
```

#### 6. Service Health
```bash
# Check service status
systemctl is-active sdl.service

# Check for errors in last hour
journalctl -u sdl.service --since "1 hour ago" -p err

# Alert if service down or errors present
```

### Monitoring Dashboard Queries

#### MongoDB Queries
```javascript
// Staging status distribution
use audit
db.row_changes_staging.aggregate([
  { $group: { 
      _id: "$status", 
      count: { $sum: 1 } 
  }}
])

// Batch size distribution
db.row_changes_staging.aggregate([
  { $group: { 
      _id: null, 
      avgSize: { $avg: { $size: "$events" } },
      maxSize: { $max: { $size: "$events" } }
  }}
])

// Events by operation type
db.row_changes.aggregate([
  { $group: { 
      _id: "$op", 
      count: { $sum: 1 } 
  }}
])

// Events by table (top 10)
db.row_changes.aggregate([
  { $group: { 
      _id: { db: "$meta.db", tbl: "$meta.tbl" }, 
      count: { $sum: 1 } 
  }},
  { $sort: { count: -1 } },
  { $limit: 10 }
])

// Storage statistics
db.row_changes.stats()
db.row_changes_staging.stats()
```

#### System Queries
```bash
# Service uptime
systemctl show sdl.service | grep ActiveEnterTimestamp

# Memory usage
ps aux | grep sdl_binary | awk '{print $4, $6}'

# Log errors in last 24 hours
journalctl -u sdl.service --since "24 hours ago" -p err | wc -l

# Recent log entries
journalctl -u sdl.service -n 50 --no-pager
```

### Alert Conditions

Create alerts for these conditions:

| Condition | Severity | Threshold |
|-----------|----------|-----------|
| Staging pending count > 100 | WARNING | MongoDB issue |
| Staging pending count > 1000 | CRITICAL | MongoDB down |
| No GTID update > 5 minutes | WARNING | Replication lag |
| No GTID update > 15 minutes | CRITICAL | Service stopped |
| Event lag > 5 minutes | WARNING | Processing slow |
| Event lag > 30 minutes | CRITICAL | Severe lag |
| Service down | CRITICAL | Immediate action |
| Disk usage > 80% | WARNING | Need cleanup |
| Disk usage > 95% | CRITICAL | Service may fail |
| Error log entries | WARNING | Check logs |

---

## Troubleshooting

### Service Won't Start

#### Check 1: MongoDB Connection
```bash
mongosh "mongodb://127.0.0.1:27017"

# Verify replica set
rs.status()

# Check for errors in MongoDB logs
sudo journalctl -u mongod -n 50
```

#### Check 2: MySQL Connection
```bash
mysql -u repl_user -p -h 127.0.0.1

# Verify GTID mode
SHOW VARIABLES LIKE 'gtid_mode';

# Check binlog position
SHOW MASTER STATUS;
```

#### Check 3: Service Logs
```bash
# Recent logs
sudo journalctl -u sdl.service -n 100

# Follow logs
sudo journalctl -u sdl.service -f

# Errors only
sudo journalctl -u sdl.service -p err
```

#### Check 4: Configuration
```bash
# Verify .env file exists
ls -la /path/to/sdl/.env

# Check permissions
ls -la /path/to/sdl/sdl_binary

# Test manual start
cd /path/to/sdl
./sdl_binary  # Run in foreground to see errors
```

### Events Not Being Written

#### Check 1: Verify MySQL Events Flowing
```sql
-- Check recent changes
SELECT * FROM information_schema.processlist 
WHERE command = 'Binlog Dump';

-- Check binlog events
SHOW BINLOG EVENTS IN 'mysql-bin.000001' LIMIT 10;
```

#### Check 2: Check MongoDB Connection
```bash
mongosh audit << 'EOF'
db.adminCommand({ ping: 1 })
rs.status()
EOF
```

#### Check 3: Check Staging Collection
```javascript
use audit
db.row_changes_staging.find().pretty()

// If many "pending" status for hours → MongoDB transaction issue
```

#### Check 4: Check GTID Progression
```javascript
use audit
db.binlog_offsets.findOne()

// Compare with MySQL: SELECT @@global.gtid_executed;
```

### Batch Stuck in Staging

#### Identify Issue
```javascript
use audit

// Find pending batches
db.row_changes_staging.find({status: "pending"}).pretty()

// Check age
db.row_changes_staging.find({
  status: "pending",
  createdAt: { $lt: new Date(Date.now() - 3600000) }  // Older than 1 hour
}).pretty()
```

#### Resolution
```javascript
// Option 1: Force archive (if MongoDB issue resolved)
db.row_changes_staging.updateMany(
  {status: "pending"},
  {$set: {
    status: "archived",
    archivedAt: new Date(),
    note: "Manually archived due to recovery"
  }}
)

// Option 2: Delete old staging (extreme case)
db.row_changes_staging.deleteMany({
  createdAt: { $lt: new Date(Date.now() - 604800000) }  // Older than 7 days
})

// Restart service
```

### Duplicate Events

**Note:** Duplicates are normal and expected due to idempotent hashing.

#### Find Duplicates
```javascript
use audit

// Count documents with duplicate _id (should be 0 due to unique index)
db.row_changes.aggregate([
  {$group: {
    _id: "$_id",
    count: {$sum: 1}
  }},
  {$match: {
    count: {$gt: 1}
  }}
])

// This should return empty array
// Unique index prevents actual duplicates
```

#### If Duplicate Key Errors in Logs
```bash
# Check logs for duplicate key errors
journalctl -u sdl.service | grep "duplicate key error"

# This is normal and expected:
# - Same event processed twice after crash
# - MongoDB unique index prevents duplicate
# - Error is silently ignored in code
# - No action needed
```

### High Memory Usage

#### Check Memory
```bash
# Service memory usage
ps aux | grep sdl_binary

# MongoDB memory
ps aux | grep mongod
```

#### Tune Batch Size
```go
// In main.go, OnRow() function, reduce batch size:
if len(h.batch) >= 50 {  // Was 100
    h.sink.writeBatchWithGTID(...)
}
```

#### Tune MongoDB Cache
```yaml
# /etc/mongod.conf
storage:
  wiredTiger:
    engineConfig:
      cacheSizeGB: 4  # Reduce from default 50% of RAM
```

### High Disk Usage

#### Check Disk Usage
```bash
# MongoDB
du -sh /var/lib/mongodb/*

# MySQL binlogs
du -sh /var/log/mysql/*

# Overall
df -h
```

#### Clean Up MongoDB
```javascript
use audit

// Drop old staging documents (TTL should handle this)
db.row_changes_staging.deleteMany({
  status: "archived",
  archivedAt: { $lt: new Date(Date.now() - 604800000) }  // 7+ days old
})

// If needed, archive old events (move to different collection)
db.row_changes.aggregate([
  {$match: {
    ts: { $lt: new Date("2024-01-01") }
  }},
  {$out: "row_changes_archive_2023"}
])

// Then delete archived events
db.row_changes.deleteMany({
  ts: { $lt: new Date("2024-01-01") }
})

// Compact collection
db.runCommand({compact: "row_changes"})
```

#### Clean Up MySQL Binlogs
```sql
-- Check binlog size
SHOW BINARY LOGS;

-- Purge old binlogs (CAREFUL!)
-- Only purge if service has caught up
PURGE BINARY LOGS BEFORE NOW() - INTERVAL 7 DAY;

-- Or purge to specific binlog
PURGE BINARY LOGS TO 'mysql-bin.000100';
```

---

## Emergency Procedures

### Complete System Failure Recovery

#### Step 1: Assess Damage
```bash
# Check service status
systemctl status sdl.service mongod.service mysql.service

# Check logs
journalctl -u sdl.service -n 100
journalctl -u mongod -n 100
journalctl -u mysql -n 100

# Check disk space
df -h
```

#### Step 2: Verify Data Integrity
```javascript
// MongoDB
use audit

// Check latest event
db.row_changes.find().sort({ts: -1}).limit(1).pretty()

// Check GTID
db.binlog_offsets.findOne()

// Check staging
db.row_changes_staging.countDocuments({status: "pending"})
```

```sql
-- MySQL
SHOW MASTER STATUS;
SELECT @@global.gtid_executed;
SHOW BINARY LOGS;
```

#### Step 3: Recovery Actions

**If MongoDB Corrupted:**
```bash
# Restore from backup
mongorestore --drop /path/to/backup/audit

# Verify indexes
mongosh audit << 'EOF'
db.row_changes.getIndexes()
db.row_changes_staging.getIndexes()
db.binlog_offsets.getIndexes()
EOF

# Rebuild indexes if needed
mongosh audit << 'EOF'
db.row_changes.reIndex()
db.row_changes_staging.reIndex()
EOF
```

**If MySQL Binlogs Missing:**
```bash
# Check available binlogs
mysql -e "SHOW BINARY LOGS;"

# If binlog gap exists, manual data sync needed:
# 1. Identify missing event range
# 2. Extract from database backup
# 3. Manually insert into MongoDB
# 4. Update GTID offset
```

**If Service Data Loss Suspected:**
```bash
# 1. Stop service
systemctl stop sdl.service

# 2. Check staging for pending batches
mongosh audit --eval 'db.row_changes_staging.find({status: "pending"}).pretty()'

# 3. If batches found, restart service (recovery will handle)
systemctl start sdl.service

# 4. Monitor recovery in logs
journalctl -u sdl.service -f
# Look for: "Recovering X pending batches"

# 5. Verify GTID progression
mongosh audit --eval 'db.binlog_offsets.findOne()'
```

### Data Validation After Recovery

```javascript
// Compare counts
use audit

// Get event count by date
db.row_changes.aggregate([
  {$group: {
    _id: {$dateToString: {format: "%Y-%m-%d", date: "$ts"}},
    count: {$sum: 1}
  }},
  {$sort: {_id: 1}}
])

// Check for gaps in sequence
db.row_changes.aggregate([
  {$sort: {ts: 1}},
  {$project: {
    ts: 1,
    timeDiff: {$subtract: ["$ts", "$prevTs"]},
    prevTs: "$ts"
  }},
  {$match: {
    timeDiff: {$gt: 300000}  // 5 minute gaps
  }}
])
```

### Manual GTID Reset (Extreme Case Only)

⚠️ **WARNING:** This will cause re-processing of events. Only use if absolutely necessary.

```javascript
// Backup current offset
use audit
db.binlog_offsets.find().pretty()

// Save to file
mongoexport -d audit -c binlog_offsets -o backup_offsets.json

// Reset to specific GTID
db.binlog_offsets.updateOne(
  {_id: "mysql://127.0.0.1:3306"},
  {$set: {
    gtid: "your-new-gtid-here",
    updatedAt: new Date(),
    note: "Manually reset due to recovery"
  }}
)

// Or delete to start fresh
db.binlog_offsets.deleteMany({})
```

```bash
# Restart service
systemctl restart sdl.service

# Monitor for duplicates (will be ignored)
journalctl -u sdl.service -f | grep "duplicate key"
```

---

## Performance Tuning

### Batch Size Adjustment
```go
// In main.go, OnRow() function
if len(h.batch) >= 200 {  // Increased from 100
    h.sink.writeBatchWithGTID(...)
}
```

**Trade-offs:**
- Larger (200-500): Higher throughput, more memory, higher latency
- Smaller (10-50): Lower latency, less memory, more MongoDB writes

### Retry Configuration
```go
// In writeBatchWithGTID()
retryWithBackoff(ctx, fn, 10, 50*time.Millisecond)
// Was: 5 retries, 100ms initial delay
// Now: 10 retries, 50ms initial delay (for low-latency networks)
```

### Add Time-Based Flushing
```go
// Add to main()
flushTicker := time.NewTicker(5 * time.Second)
defer flushTicker.Stop()

go func() {
    for range flushTicker.C {
        if len(h.batch) > 0 {
            h.Flush(context.Background())
        }
    }
}()
```

---

*For architecture and implementation details, see [ARCHITECTURE.md](ARCHITECTURE.md)*
