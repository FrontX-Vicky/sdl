# Operations & Deployment

## Production Checklist

### MySQL
- [ ] `gtid_mode = ON`
- [ ] `binlog_format = ROW`
- [ ] `binlog_row_image = FULL`
- [ ] `sync_binlog = 1`
- [ ] `innodb_flush_log_at_trx_commit = 1`
- [ ] `binlog_expire_logs_seconds = 1209600` (14 days)
- [ ] Replication user created

### MongoDB
- [ ] Running as replica set (`rs0`)
- [ ] Journaling enabled
- [ ] Version 4.0+
- [ ] Indexes created (see [performance.md](performance.md))

### System
- [ ] `.env` created and `chmod 600`
- [ ] Binaries built
- [ ] Systemd service configured (optional)

---

## MySQL Configuration

Edit `/etc/mysql/mysql.conf.d/mysqld.cnf` under `[mysqld]`:

```ini
server-id                      = 1

log_bin                        = /var/log/mysql/mysql-bin.log
binlog_format                  = ROW
binlog_row_image               = FULL

gtid_mode                      = ON
enforce_gtid_consistency       = ON

binlog_expire_logs_seconds     = 1209600    # 14 days — CRITICAL
sync_binlog                    = 1
innodb_flush_log_at_trx_commit = 1

binlog_cache_size              = 4M
max_binlog_size                = 512M
binlog_rows_query_log_events   = ON
log_slave_updates              = ON

transaction_isolation          = READ-COMMITTED
max_connections                = 500
max_allowed_packet             = 64M
```

Apply and verify:
```bash
sudo systemctl restart mysql

mysql -u root -p -e "
  SHOW VARIABLES LIKE 'binlog_format';
  SHOW VARIABLES LIKE 'gtid_mode';
  SHOW VARIABLES LIKE 'sync_binlog';
  SHOW VARIABLES LIKE 'binlog_expire_logs_seconds';
  SHOW MASTER STATUS;
"
```

**Safety vs. Performance trade-offs:**

| Setting | Max Safety | Balanced | Dev Only |
|---------|-----------|----------|----------|
| `sync_binlog` | `1` | `10` | `0` |
| `innodb_flush_log_at_trx_commit` | `1` | `2` | `0` |
| `binlog_expire_logs_seconds` | `1209600` (14d) | `604800` (7d) | `86400` (1d) |

---

## MongoDB Configuration

### Initialize Replica Set (required for transactions)

```bash
# /etc/mongod.conf — add:
# replication:
#   replSetName: "rs0"
# storage:
#   journal:
#     enabled: true

sudo systemctl restart mongod

mongosh --eval '
rs.initiate({ _id: "rs0", members: [{ _id: 0, host: "localhost:27017" }] })
'
# Wait ~5 seconds
mongosh --eval 'rs.status()'    # stateStr should be "PRIMARY"
```

### Staging Collection Cleanup (auto-expire)

```js
use audit
db.row_changes_staging.createIndex(
  { "createdAt": 1 },
  { expireAfterSeconds: 604800 }   // auto-delete after 7 days
)
```

---

## Systemd Service

```bash
sudo tee /etc/systemd/system/sdl.service << 'EOF'
[Unit]
Description=MySQL Binlog to MongoDB Audit Logger
After=network.target mongod.service mysql.service

[Service]
Type=simple
User=your_user
WorkingDirectory=/path/to/sdl
ExecStart=/path/to/sdl/sdl_binary
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=sdl
LimitNOFILE=65536
MemoryLimit=2G
TimeoutStopSec=30
KillSignal=SIGTERM

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable sdl.service
sudo systemctl start  sdl.service
```

Verify:
```bash
sudo systemctl status sdl.service
sudo journalctl -u sdl.service -f
```

---

## Monitoring

### Daily health checks

```bash
# Latest events
mongosh audit --eval "db.row_changes.find().sort({ts:-1}).limit(5).pretty()"

# GTID progression
mongosh audit --eval "db.binlog_offsets.find().pretty()"

# Pending staging (should be 0 or near-0 under normal operation)
mongosh audit --eval 'db.row_changes_staging.countDocuments({status:"pending"})'

# Event counts by operation
mongosh audit --eval '
db.row_changes.aggregate([
  { $group: { _id: "$op", count: { $sum: 1 } } }
])
'
```

### Slow query profiling (MongoDB)

```js
use audit
db.setProfilingLevel(1, { slowms: 100 })
db.system.profile.find().sort({ ts: -1 }).limit(5).pretty()
```

---

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|-------------|-----|
| `Recovering X pending batches` on start | Previous crash | Normal — events are replayed automatically |
| No events in MongoDB | Binlog not enabled or wrong user | Check `SHOW MASTER STATUS` and replication user grants |
| `Canal reconnect` errors in logs | MySQL restarted or network blip | Wait — canal reconnects automatically (10 retries) |
| `MongoDB ping failed` | MongoDB down or wrong URI | Check `MONGO_URI` and `mongod` status |
| `duplicate key error` in logs | Expected — duplicate event IDs | Safe to ignore; events are idempotent |
| Binlog position lost | Binlog rotated before GTID saved | Ensure 14-day retention; check `binlog_expire_logs_seconds` |

---

## Emergency Recovery

### Manual staging replay

```bash
# Find unprocessed staging batches
mongosh audit --eval 'db.row_changes_staging.find({status:"pending"}).pretty()'

# Simply restart the service — it auto-recovers on startup
sudo systemctl restart sdl.service
```

### Restore data to a point-in-time

```bash
cd sdl_restore
./sdl_restore --start-date 2026-01-01 --db myapp --dry-run
# Review output, then:
./sdl_restore --start-date 2026-01-01 --db myapp --execute
```

### Import from a SQL dump

```bash
cd sdl_import
./sql_import --file backup.sql --db myapp --dry-run
# Review output, then:
./sql_import --file backup.sql --db myapp --execute --continue-on-error
```
