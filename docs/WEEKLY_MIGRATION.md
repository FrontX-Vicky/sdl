# Weekly Audit DB Migration Guide

Move audit data from **Main Server → Windows Backup Machine** every week.

> **SDL will be stopped for the entire procedure — do steps in strict order.**
> Never skip a verification step. Each one is a safety gate.

---

## Prerequisites

| What | Where | Notes |
|------|-------|-------|
| MongoDB + mongodump | Main Server | Already running |
| MongoDB Database Tools | Windows Backup Machine | `mongorestore.exe` available |
| SSH access | Main Server | `user@MAIN_SERVER_IP` |

---

## Step 1 — SSH into the Main Server

```bash
ssh user@MAIN_SERVER_IP
```

---

## Step 2 — Stop SDL

```bash
# Stop gracefully — SDL flushes any in-flight batch before exiting
sudo systemctl stop sdl.service

# Wait for full shutdown
sleep 10

# Must show "inactive (dead)" before continuing
sudo systemctl status sdl.service
```

> ❌ **Do NOT continue until status shows `inactive (dead)`.**

---

## Step 3 — Dump the audit database

```bash
# Create backup directory
mkdir -p /mnt/volume_1/backup/mongodb_backup

# Dump directly into a single compressed archive
# Streams: MongoDB → gzip → file  (no temp folder, minimal disk usage)
mongodump \
  --uri="mongodb://127.0.0.1:27017" \
  --db=audit \
  --gzip \
  --archive=/mnt/volume_1/backup/mongodb_backup/sdl_weekly_dump.gz

# Verify — must show a non-zero file size
ls -lh /mnt/volume_1/backup/mongodb_backup/sdl_weekly_dump.gz
```

> ❌ **If file is missing or 0 bytes — do NOT continue. Restart SDL and investigate.**
> ```bash
> sudo systemctl start sdl.service
> ```
> ✅ Because SDL is stopped in Step 2, `row_changes_staging` should not keep increasing during the dump.

---

## Step 4 — Verify dump integrity (before dropping anything)

```bash
# Do a real restore into a temporary validation database.
# This is more reliable than --dryRun for archive validation because it gives real counts.
mongosh --eval "db.getSiblingDB('audit_verify').dropDatabase()"

mongorestore \
  --uri="mongodb://127.0.0.1:27017" \
  --gzip \
  --archive=/mnt/volume_1/backup/mongodb_backup/sdl_weekly_dump.gz \
  --nsInclude='audit.*' \
  --nsFrom='audit.*' \
  --nsTo='audit_verify.*' \
  2>&1

# Cross-check restored counts against live MongoDB counts
mongosh --eval "
  var live = db.getSiblingDB('audit');
  var verify = db.getSiblingDB('audit_verify');
  print('Live row_changes         :', live.row_changes.countDocuments());
  print('Verify row_changes       :', verify.row_changes.countDocuments());
  print('Live row_changes_staging :', live.row_changes_staging.countDocuments());
  print('Verify row_changes_staging:', verify.row_changes_staging.countDocuments());
"
```

The restore output will show lines like:
```
finished restoring audit.row_changes (65991767 documents, 0 failures)
```

> ❌ **If restore shows errors or the `audit_verify` counts do not match live counts — do NOT continue. Restart SDL.**
> ✅ **`audit_verify.row_changes` and `audit_verify.row_changes_staging` must match the live counts.**

---

## Step 5 — Drop ONLY data collections (NOT binlog_offsets)

> ⚠️ **`binlog_offsets` must NEVER be dropped.**
> It holds the MySQL GTID position. Dropping it means SDL loses its place
> in the binlog → permanent gap in audit logs after restart.

```bash
mongosh --eval "
  db = db.getSiblingDB('audit');
  db.row_changes.drop();
  db.row_changes_staging.drop();

  var cols = db.getCollectionNames();
  print('row_changes still exists    :', cols.includes('row_changes'));
  print('staging still exists        :', cols.includes('row_changes_staging'));
  print('binlog_offsets still exists :', cols.includes('binlog_offsets'));
  print('binlog_offsets doc count    :', db.binlog_offsets.countDocuments());
"
```

Expected output:
```
row_changes still exists    : false   ✅
staging still exists        : false   ✅
binlog_offsets still exists : true    ✅
binlog_offsets doc count    : 1       ✅
```

> ❌ **If `binlog_offsets doc count` is 0 — stop. Something is wrong.**

---

## Step 6 — Remove temporary validation database

```bash
mongosh --eval "db.getSiblingDB('audit_verify').dropDatabase()"
```

---

## Step 7 — Transfer dump to Windows Backup Machine

**Exit the SSH session**, then copy the dump to your **Windows machine**.

```powershell
# Example target path
# D:\backups\sdl_weekly_dump.gz
```

> ❌ **Do NOT delete the server copy yet.**
> **Do NOT restart SDL yet.**

---

## Step 8 — Import into Local MongoDB on Windows

```powershell
& "C:\Program Files\MongoDB\Tools\100\bin\mongorestore.exe" `
  --uri="mongodb://127.0.0.1:27017" `
  --gzip `
  --archive="D:\backups\sdl_weekly_dump.gz" `
  --nsInclude="audit.row_changes" `
  --nsInclude="audit.row_changes_staging"

$exitCode = $LASTEXITCODE
Write-Host "Restore exit code: $exitCode"
```

> ❌ **Exit code must be `0`. Anything else = restore failed.**
> **Server copy is still safe — retry this step after fixing the issue.**

---

## Step 9 — Verify Import on Windows

```powershell
mongosh mongodb://127.0.0.1:27017 --eval "
  db = db.getSiblingDB('audit');
  print('row_changes count  :', db.row_changes.countDocuments());
  print('staging count      :', db.row_changes_staging.countDocuments());
  print('');
  print('Oldest event:');
  printjson(db.row_changes.findOne(
    {},
    { ts:1, op:1, 'meta.db':1, 'meta.tbl':1, _id:0 },
    { sort: { ts: 1 } }
  ));
  print('Latest event:');
  printjson(db.row_changes.findOne(
    {},
    { ts:1, op:1, 'meta.db':1, 'meta.tbl':1, _id:0 },
    { sort: { ts: -1 } }
  ));
"
```

Confirm:
- `row_changes count` ≥ count validated in Step 4
- Oldest and Latest timestamps look correct (not null, not epoch)

> ❌ **If counts are wrong — do NOT delete server copy. Investigate first.**

---

## Step 10 — Cleanup (only after Step 9 passes)

```powershell
# Delete from server
ssh user@MAIN_SERVER_IP \
  "rm -f /mnt/volume_1/backup/mongodb_backup/sdl_weekly_dump.gz && echo 'Server copy deleted'"

# Delete from Windows
Remove-Item "D:\backups\sdl_weekly_dump.gz"
Write-Host "Windows copy deleted"
```

---

## Step 11 — Restart SDL on Main Server

```bash
ssh user@MAIN_SERVER_IP "sudo systemctl start sdl.service"

# Verify it picked up from the saved GTID in binlog_offsets
ssh user@MAIN_SERVER_IP "sudo journalctl -u sdl.service -n 30"
```

Look for `canal started` or `processing` — confirms SDL resumed from where it stopped, no gap.

---

## Step 12 — Open the Dashboard

```bash
cd /Users/vicky/Documents/SDL/sdl-dashboard
npm run dev
```

Open **http://localhost:3000** ✅

---

## Quick Reference Checklist

```
WEEK N — CHECKLIST
─────────────────────────────────────────────────────────────────────
[ ] SSH into main server
[ ] systemctl stop sdl.service → confirm "inactive (dead)"
[ ] mongodump --gzip --archive → sdl_weekly_dump.gz
[ ] ls -lh → file size > 0
[ ] Confirm row_changes_staging is not growing during dump
[ ] mongorestore --archive --nsFrom audit.* --nsTo audit_verify.* → 0 failures
[ ] audit_verify counts match live DB
[ ] Drop row_changes + row_changes_staging
[ ] Confirm: row_changes=false, staging=false, binlog_offsets=true, count=1
[ ] Drop audit_verify database
[ ] Copy dump to Windows machine
[ ] mongorestore.exe --archive --nsInclude ... on Windows → exit code 0
[ ] Verify count on Windows ≥ Step 4 validated count, timestamps look correct
[ ] Delete dump from server + Windows
[ ] systemctl start sdl.service → confirm canal started
─────────────────────────────────────────────────────────────────────
```

---

## Failure Recovery

### mongodump failed or file is 0 bytes
```bash
# SDL is still stopped — restart immediately, nothing was dropped
sudo systemctl start sdl.service
# Fix the issue, retry from Step 3
```

### dryRun shows errors or wrong counts
```bash
# Do NOT drop anything — restart SDL, investigate dump
sudo systemctl start sdl.service
# The original data is still intact in MongoDB
```

### mongorestore on Windows failed
```powershell
# Server copy still exists at:
# /mnt/volume_1/backup/mongodb_backup/sdl_weekly_dump.gz
# SDL is still stopped — fix the issue, retry Step 7
```

### `gzip: invalid header` on Windows
```powershell
# Use the exact archive form below. In PowerShell, prefer --archive="<path>"
# instead of separating --archive and the path.
& "C:\Program Files\MongoDB\Tools\100\bin\mongorestore.exe" `
  --uri="mongodb://127.0.0.1:27017" `
  --gzip `
  --archive="D:\backups\sdl_weekly_dump.gz" `
  --nsInclude="audit.row_changes" `
  --nsInclude="audit.row_changes_staging"

# If it still fails, verify the copied file:
Get-Item "D:\backups\sdl_weekly_dump.gz" | Select-Object FullName,Length,LastWriteTime
```

If `gzip: invalid header` still appears, the copied file is likely incomplete,
corrupted, or not the same file produced by `mongodump --gzip --archive`.

### SDL won't start after Step 10
```bash
ssh user@MAIN_SERVER_IP "sudo journalctl -u sdl.service -n 50"

# Check binlog_offsets is intact
ssh user@MAIN_SERVER_IP \
  "mongosh --eval \"db.getSiblingDB('audit').binlog_offsets.find().pretty()\""
```

### "mongodump/mongorestore: command not found"
```bash
# Ubuntu server:
sudo apt-get install -y mongodb-database-tools
```
