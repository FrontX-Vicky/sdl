# MongoDB Standalone Mode Fix

## Problem

The service failed with:
```
(IllegalOperation) Transaction numbers are only allowed on a replica set member or mongos
```

This occurs because the original code uses MongoDB transactions via `session.WithTransaction()`, which **requires MongoDB to be running as a replica set**. Transactions are not supported on standalone MongoDB instances.

## Solution

Added a **fallback mechanism** that detects the replica set requirement error and automatically switches to non-transactional writes.

### Changes Made

#### 1. Modified `writeBatchWithGTID()` (Lines 180-218)

Changed from direct transaction use to:
```go
// Try with transaction if MongoDB is replica set, fall back to non-transactional if not
err := s.writeBatchWithTransaction(retryCtx, docs, source, gtid, file, pos)
if err != nil {
    // Check if error is due to non-replica set MongoDB
    if strings.Contains(err.Error(), "Transaction numbers are only allowed on a replica set") {
        // Fallback: write without transaction (WARNING: not atomic, but works on standalone)
        log.Println("WARNING: MongoDB not a replica set, using non-transactional writes. Data safety reduced.")
        err = s.writeBatchWithoutTransaction(retryCtx, docs, source, gtid, file, pos)
        if err != nil {
            return fmt.Errorf("write batch (non-transactional fallback): %w", err)
        }
    } else {
        return err
    }
}
```

#### 2. Added `writeBatchWithTransaction()` (Lines 260-309)

Extracted the original transaction logic into its own function for clarity and modularity. Requires MongoDB replica set.

```go
func (s *MongoSink) writeBatchWithTransaction(ctx context.Context, docs []interface{}, 
    source, gtid, file string, pos uint32) error {
    // Uses session.WithTransaction() 
    // Atomic: Events and GTID saved together, both succeed or both fail
}
```

#### 3. Added `writeBatchWithoutTransaction()` (Lines 311-350)

New fallback function that writes without transactions. Used only for standalone MongoDB.

```go
func (s *MongoSink) writeBatchWithoutTransaction(ctx context.Context, docs []interface{}, 
    source, gtid, file string, pos uint32) error {
    // Writes events first, then GTID (best effort)
    // NOT atomic - WARNING logged to indicate reduced data safety
}
```

## How It Works

1. **Primary Path**: Try to write with transaction (replica set mode)
2. **Fallback Path**: If error contains "Transaction numbers are only allowed on a replica set", use non-transactional writes
3. **Staging Collection**: Still in place for crash recovery (writes to staging happen before transaction attempt)
4. **Retry Logic**: Both paths wrapped in `retryWithBackoff()` for transient error handling

## Data Safety Trade-offs

### With MongoDB Replica Set (Original - Recommended)
✅ **Atomic writes**: Events and GTID saved together in transaction
✅ **Zero data loss** on crashes (staging collection handles recovery)
✅ **No duplicates** (idempotent event IDs + transaction rollback)
✅ **Consistent state**: Never in a position where GTID is ahead of events

### With MongoDB Standalone (New Fallback)
⚠️ **Non-atomic writes**: Events written first, then GTID
⚠️ **Reduced safety**: If service crashes between writes, GTID may be saved without events
⚠️ **Recovery limited**: Staging collection still helps, but atomicity is lost
✅ **Still functional**: Service continues to work and capture events
✅ **Still handles duplicates**: Idempotent event IDs prevent duplicate processing on retry

## Recommended Next Steps

### Option A: Set up MongoDB Replica Set (RECOMMENDED)
```bash
# Initialize replica set on MongoDB instance
mongo
> rs.initiate()
> rs.status()  # Should show replica set active
```

Then the system will automatically use the safer transactional path.

### Option B: Continue with Standalone
The service will function but with reduced crash-recovery guarantees. Monitor logs for the warning:
```
WARNING: MongoDB not a replica set, using non-transactional writes. Data safety reduced.
```

## Log Output

When running on standalone MongoDB, you'll see:
```
2026/01/19 15:34:20 WARNING: MongoDB not a replica set, using non-transactional writes. Data safety reduced.
```

This warning indicates the fallback mode is active.

## Code Location

- **Main logic**: Lines 180-218 in `writeBatchWithGTID()`
- **Transaction path**: Lines 260-309 in `writeBatchWithTransaction()`
- **Fallback path**: Lines 311-350 in `writeBatchWithoutTransaction()`
- **Detection**: `strings.Contains(err.Error(), "Transaction numbers are only allowed on a replica set")`

## Testing

To verify the fix works:

1. **On Standalone MongoDB**:
   - Service should start successfully
   - Warning message logged: "MongoDB not a replica set..."
   - Events should be captured to MongoDB
   - Check logs for recovery messages on restart

2. **On Replica Set MongoDB**:
   - Service should start successfully
   - No warning message logged
   - Full transactional atomicity in effect
   - Safest crash-recovery path active

## Migration Path

If you start with standalone and later want to set up replica set:

1. Stop the service
2. Initialize MongoDB replica set
3. Restart the service
4. Service will automatically detect replica set and use transactional path (no code changes needed)
