package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/go-mysql-org/go-mysql/canal"
	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Delta struct {
	F any `bson:"f,omitempty"`
	T any `bson:"t,omitempty"`
}
type Meta struct {
	DB, Tbl string
	PK      any `bson:"pk"`
}
type EventDoc struct {
	ID    string           `bson:"_id"`
	TS    time.Time        `bson:"ts"` // UTC
	OP    string           `bson:"op"` // "i","u","d"
	Meta  Meta             `bson:"meta"`
	Seq   int64            `bson:"seq"` // optional if you have it
	Chg   map[string]Delta `bson:"chg,omitempty"`
	Src   map[string]any   `bson:"src,omitempty"`    // binlog coords/gtid
	TSIST string           `bson:"ts_ist,omitempty"` // convenience string
}

type MongoSink struct {
	client            *mongo.Client
	events            *mongo.Collection
	offsets           *mongo.Collection
	staging           *mongo.Collection // Batch staging for crash recovery
	loc               *time.Location
	failCount         int // Consecutive failure count
	lastErr           error
	noTxWarningLogged bool // Log warning once only
}

func newMongoSink(uri, db, coll, offsets string, loc *time.Location) (*MongoSink, error) {
	c, err := mongo.Connect(context.Background(), options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	return &MongoSink{
		client:            c,
		events:            c.Database(db).Collection(coll),
		offsets:           c.Database(db).Collection(offsets),
		staging:           c.Database(db).Collection(coll + "_staging"),
		loc:               loc,
		noTxWarningLogged: false,
	}, nil
}

func toS(v any) string {
	return fmt.Sprint(v)
}

func isDuplicateOnlyWriteError(err error) bool {
	if err == nil {
		return false
	}

	var bwe *mongo.BulkWriteException
	if errors.As(err, &bwe) {
		if len(bwe.WriteErrors) == 0 {
			return false
		}
		for _, we := range bwe.WriteErrors {
			if we.Code != 11000 {
				return false
			}
		}
		return true
	}

	var we mongo.WriteException
	if errors.As(err, &we) {
		if len(we.WriteErrors) == 0 {
			return false
		}
		for _, writeErr := range we.WriteErrors {
			if writeErr.Code != 11000 {
				return false
			}
		}
		return true
	}

	return false
}

// retryWithBackoff executes fn with exponential backoff retry on transient errors
// maxRetries: maximum number of retry attempts (default 5)
// initialDelay: initial delay between retries (default 100ms)
func retryWithBackoff(ctx context.Context, fn func(context.Context) error, maxRetries int, initialDelay time.Duration) error {
	if maxRetries <= 0 {
		maxRetries = 5
	}
	if initialDelay <= 0 {
		initialDelay = 100 * time.Millisecond
	}

	var lastErr error
	delay := initialDelay

	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Try the operation
		err := fn(ctx)
		if err == nil {
			return nil
		}

		lastErr = err

		// Don't retry on context cancellation
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		// Check if it's a transient error we should retry
		var cmdErr *mongo.CommandError
		var netErr interface{ Temporary() bool }

		isTransient := false
		if errors.As(err, &cmdErr) {
			// Transient MongoDB errors: write concern timeout, read preference error
			isTransient = cmdErr.Code == 64 || // WriteConcernFailed
				cmdErr.Code == 10107 || // NotMaster
				cmdErr.Code == 13435 // NotMasterOrSecondary
		} else if errors.As(err, &netErr) {
			isTransient = netErr.Temporary()
		}

		// If we've exhausted retries or it's not transient, return error
		if attempt >= maxRetries || !isTransient {
			return lastErr
		}

		// Wait before retrying
		select {
		case <-time.After(delay):
			// Continue to next retry
		case <-ctx.Done():
			return ctx.Err()
		}

		// Exponential backoff: double the delay for next retry
		delay *= 2
		// Cap at 10 seconds per retry
		if delay > 10*time.Second {
			delay = 10 * time.Second
		}
	}

	return lastErr
}

func (s *MongoSink) writeBatch(ctx context.Context, docs []EventDoc) error {
	if len(docs) == 0 {
		return nil
	}
	ws := make([]mongo.WriteModel, 0, len(docs))
	for i := range docs {
		ws = append(ws, mongo.NewInsertOneModel().SetDocument(docs[i]))
	}
	_, err := s.events.BulkWrite(ctx, ws, options.BulkWrite().SetOrdered(false))
	if err != nil {
		if isDuplicateOnlyWriteError(err) {
			return nil
		}
		return err
	}
	return err
}

// writeBatchWithGTID writes batch and GTID atomically with crash recovery via staging
func (s *MongoSink) writeBatchWithGTID(ctx context.Context, docs []EventDoc, source, gtid, file string, pos uint32) error {
	if len(docs) == 0 {
		return nil
	}

	// Create staging document to protect against crashes
	batchID := fmt.Sprintf("%s_%d_%s", source, time.Now().UnixNano(), gtid)
	stagingDoc := bson.M{
		"_id":       batchID,
		"events":    docs,
		"source":    source,
		"gtid":      gtid,
		"file":      file,
		"pos":       pos,
		"createdAt": time.Now().UTC(),
		"status":    "pending", // pending -> committed -> archived
	}

	// Use retryWithBackoff to handle transient failures
	return retryWithBackoff(ctx, func(retryCtx context.Context) error {
		// First, write to staging (crash recovery point)
		if _, err := s.staging.InsertOne(retryCtx, stagingDoc); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				// A previous attempt already staged this exact batch.
				// Continue so the data write + GTID checkpoint can converge.
			} else {
				return fmt.Errorf("staging insert: %w", err)
			}
		}

		// Try with transaction if MongoDB supports it, fall back to non-transactional if not
		err := s.writeBatchWithTransaction(retryCtx, docs, source, gtid, file, pos)
		if err != nil {
			// Check if error is due to transaction limitations (replica set requirement or time-series collection)
			errStr := err.Error()
			if strings.Contains(errStr, "Transaction numbers are only allowed on a replica set") ||
				strings.Contains(errStr, "Cannot insert into a time-series collection in a multi-document transaction") {
				// Fallback: write without transaction (WARNING: not atomic, but works)
				// Keep functionality; suppress non-error logging
				err = s.writeBatchWithoutTransaction(retryCtx, docs, source, gtid, file, pos)
				if err != nil {
					return fmt.Errorf("write batch (non-transactional fallback): %w", err)
				}
			} else {
				return err
			}
		}

		// Mark staging as committed (for recovery)
		_, _ = s.staging.UpdateByID(retryCtx, batchID, bson.M{"$set": bson.M{"status": "committed", "committedAt": time.Now().UTC()}})
		return nil
	}, 5, 100*time.Millisecond)
}

func (s *MongoSink) saveGTID(ctx context.Context, source, gtid string, file string, pos uint32) error {
	_, err := s.offsets.UpdateByID(ctx, source, bson.M{
		"$set": bson.M{
			"source": source, "gtid": gtid,
			"file": file, "pos": pos,
			"updatedAt": time.Now().UTC(),
		},
	}, options.Update().SetUpsert(true))
	return err
}

func (s *MongoSink) loadGTID(ctx context.Context, source string) (string, bool, error) {
	var doc struct {
		GTID string `bson:"gtid"`
	}

	// Use retry logic for initial GTID load
	err := retryWithBackoff(ctx, func(retryCtx context.Context) error {
		err := s.offsets.FindOne(retryCtx, bson.M{"_id": source}).Decode(&doc)
		if err != nil {
			// try by 'source' field too (older upsert)
			_ = s.offsets.FindOne(retryCtx, bson.M{"source": source}).Decode(&doc)
		}
		if err != nil && err != mongo.ErrNoDocuments {
			return err
		}
		return nil
	}, 5, 100*time.Millisecond)

	if err != nil && err != mongo.ErrNoDocuments {
		return "", false, err
	}

	if doc.GTID == "" {
		return "", false, nil
	}
	return doc.GTID, true, nil
}

// writeBatchWithTransaction writes batch and GTID within a transaction (requires replica set)
func (s *MongoSink) writeBatchWithTransaction(ctx context.Context, docs []EventDoc, source, gtid, file string, pos uint32) error {
	session, err := s.client.StartSession()
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	defer session.EndSession(ctx)

	_, err = session.WithTransaction(ctx, func(sessCtx mongo.SessionContext) (interface{}, error) {
		// Write events batch
		ws := make([]mongo.WriteModel, 0, len(docs))
		for i := range docs {
			ws = append(ws, mongo.NewInsertOneModel().SetDocument(docs[i]))
		}
		_, err := s.events.BulkWrite(sessCtx, ws, options.BulkWrite().SetOrdered(false))
		if err != nil {
			if !isDuplicateOnlyWriteError(err) {
				return nil, err
			}
			// All duplicates, continue to save GTID
		}

		// Save GTID offset
		_, err = s.offsets.UpdateByID(sessCtx, source, bson.M{
			"$set": bson.M{
				"source":    source,
				"gtid":      gtid,
				"file":      file,
				"pos":       pos,
				"updatedAt": time.Now().UTC(),
			},
		}, options.Update().SetUpsert(true))
		if err != nil {
			return nil, fmt.Errorf("save GTID: %w", err)
		}

		return nil, nil
	})

	return err
}

// writeBatchWithoutTransaction writes batch and GTID without transaction (fallback for standalone MongoDB)
// WARNING: This is NOT atomic - if service crashes between writes, GTID may be saved without events or vice versa
// Only used when MongoDB is not a replica set
func (s *MongoSink) writeBatchWithoutTransaction(ctx context.Context, docs []EventDoc, source, gtid, file string, pos uint32) error {
	// Write events batch first
	ws := make([]mongo.WriteModel, 0, len(docs))
	for i := range docs {
		ws = append(ws, mongo.NewInsertOneModel().SetDocument(docs[i]))
	}
	_, err := s.events.BulkWrite(ctx, ws, options.BulkWrite().SetOrdered(false))
	if err != nil {
		if !isDuplicateOnlyWriteError(err) {
			return fmt.Errorf("bulk write events: %w", err)
		}
		// All duplicates, continue to save GTID
	}

	// Save GTID offset after events (best effort on non-transactional)
	_, err = s.offsets.UpdateByID(ctx, source, bson.M{
		"$set": bson.M{
			"source":    source,
			"gtid":      gtid,
			"file":      file,
			"pos":       pos,
			"updatedAt": time.Now().UTC(),
		},
	}, options.Update().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("save GTID (non-transactional): %w", err)
	}

	return nil
}

// RecoverPendingBatches re-applies any batches that were written to the staging
// collection but not yet committed (e.g. the process crashed between staging and
// the main write). Events are re-inserted idempotently (duplicate _id is ignored)
// and the GTID checkpoint is updated before the staging doc is archived.
func (s *MongoSink) RecoverPendingBatches(ctx context.Context) error {
	type stagingDoc struct {
		ID     string     `bson:"_id"`
		Events []EventDoc `bson:"events"`
		Source string     `bson:"source"`
		GTID   string     `bson:"gtid"`
		File   string     `bson:"file"`
		Pos    uint32     `bson:"pos"`
	}

	cursor, err := s.staging.Find(ctx, bson.M{"status": "pending"})
	if err != nil {
		return fmt.Errorf("find pending batches: %w", err)
	}
	defer cursor.Close(ctx)

	var docs []stagingDoc
	if err := cursor.All(ctx, &docs); err != nil {
		return fmt.Errorf("decode pending batches: %w", err)
	}

	recovered := 0
	for _, doc := range docs {
		// Re-apply the events — duplicate _id entries are silently ignored by
		// writeBatch so this is safe to call even if the events were partially written.
		if writeErr := s.writeBatch(ctx, doc.Events); writeErr != nil {
			log.Printf("Warning: recovery could not re-write events for batch %s: %v", doc.ID, writeErr)
			// Continue anyway — we still want to advance the GTID if events are there.
		}

		// Advance the persisted GTID checkpoint to this batch's position.
		if doc.GTID != "" {
			if saveErr := s.saveGTID(ctx, doc.Source, doc.GTID, doc.File, doc.Pos); saveErr != nil {
				log.Printf("Warning: recovery could not save GTID for batch %s: %v", doc.ID, saveErr)
			}
		}

		_, _ = s.staging.UpdateByID(ctx, doc.ID, bson.M{
			"$set": bson.M{
				"status":     "archived",
				"archivedAt": time.Now().UTC(),
			},
		})
		recovered++
	}

	if recovered > 0 {
		log.Printf("Recovered %d pending batch(es) from staging", recovered)
	}
	return nil
}

type Handler struct {
	canal.DummyEventHandler

	sink   *MongoSink
	source string
	batch  []EventDoc

	lastFile string
	lastPos  uint64
	lastGTID string
	loc      *time.Location

	// Position tracking for current batch
	batchFile string
	batchPos  uint32
	batchGTID string

	// Schema tracking for data integrity
	tableSchemas map[string][]string // table -> column names
}

func (h *Handler) String() string { return "audit-handler" }

// Flush writes any remaining events in batch to MongoDB atomically with GTID
func (h *Handler) Flush(ctx context.Context) error {
	if len(h.batch) == 0 {
		return nil
	}
	if err := h.sink.writeBatchWithGTID(ctx, h.batch, h.source, h.batchGTID, h.batchFile, h.batchPos); err != nil {
		return fmt.Errorf("flush batch: %w", err)
	}
	h.batch = h.batch[:0]
	return nil
}

func hasPrimaryKey(e *canal.RowsEvent) bool {
	return len(e.Table.PKColumns) > 0
}

func shortOP(action string) string {
	switch action {
	case canal.InsertAction:
		return "i"
	case canal.DeleteAction:
		return "d"
	default:
		return "u"
	}
}

func makeID(db, tbl string, pk any, ts time.Time, op string, file string, pos uint64, gtid string) string {
	s := fmt.Sprintf("%s|%s|%v|%d|%s|%s|%d", db, tbl, pk, ts.UnixNano(), op, file, gtid, pos)
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func toPK(e *canal.RowsEvent, row []any) any {
	// single or composite PK
	if n := len(e.Table.PKColumns); n == 1 {
		return row[e.Table.PKColumns[0]]
	} else if n > 1 {
		parts := make([]string, n)
		for i, idx := range e.Table.PKColumns {
			parts[i] = fmt.Sprint(row[idx])
		}
		return strings.Join(parts, "|")
	}
	// fallback: first column (shouldn’t happen; we skip no-PK tables)
	if len(row) > 0 {
		return row[0]
	}
	return nil
}

func (h *Handler) OnRow(e *canal.RowsEvent) error {
	if len(e.Table.PKColumns) == 0 {
		return nil
	} // skip tables without PK

	ts := time.Unix(int64(e.Header.Timestamp), 0).UTC()
	db, tbl := e.Table.Schema, e.Table.Name

	// Build column names only for columns that are in the binlog row data
	// (excludes virtual/generated columns)
	colNames := make([]string, 0, len(e.Table.Columns))
	for _, c := range e.Table.Columns {
		// Only add column names up to the actual row data length
		// We'll validate against actual row length in the loop
		colNames = append(colNames, c.Name)
	}

	pkVal := func(row []any) any {
		if n := len(e.Table.PKColumns); n == 1 {
			idx := e.Table.PKColumns[0]
			if idx >= len(row) {
				return nil // PK column index out of range
			}
			return row[idx]
		} else if n > 1 {
			parts := make([]string, 0, n)
			for _, idx := range e.Table.PKColumns {
				if idx >= len(row) {
					continue // Skip columns not in row
				}
				parts = append(parts, toS(row[idx]))
			}
			if len(parts) == 0 {
				return nil
			}
			return strings.Join(parts, "|")
		}
		if len(row) > 0 {
			return row[0]
		}
		return nil
	}

	addDoc := func(pk any, chg map[string]Delta, op string) error {
		doc := EventDoc{
			ID:    makeID(db, tbl, pk, ts, op, h.lastFile, h.lastPos, h.lastGTID),
			TS:    ts,
			OP:    op,
			Meta:  Meta{DB: db, Tbl: tbl, PK: pk},
			Chg:   chg,
			Src:   map[string]any{"binlog": map[string]any{"file": h.lastFile, "pos": h.lastPos}, "gtid": h.lastGTID},
			TSIST: ts.In(h.loc).Format("2006-01-02 15:04:05"),
		}
		h.batch = append(h.batch, doc)

		// Update batch position tracking
		h.batchFile = h.lastFile
		h.batchPos = uint32(h.lastPos)
		h.batchGTID = h.lastGTID

		if len(h.batch) >= 100 {
			if err := h.sink.writeBatchWithGTID(context.Background(), h.batch, h.source, h.batchGTID, h.batchFile, h.batchPos); err != nil {
				return fmt.Errorf("write batch with GTID: %w", err)
			}
			h.batch = h.batch[:0]
		}
		return nil
	}

	switch e.Action {
	case canal.InsertAction:
		for _, row := range e.Rows {
			chg := map[string]Delta{}
			maxIdx := len(colNames)
			if len(row) < maxIdx {
				maxIdx = len(row)
			}
			for i := 0; i < maxIdx; i++ {
				chg[colNames[i]] = Delta{F: nil, T: row[i]}
			}
			if err := addDoc(pkVal(row), chg, "i"); err != nil {
				return fmt.Errorf("insert action: %w", err)
			}
		}
	case canal.DeleteAction:
		for _, row := range e.Rows {
			chg := map[string]Delta{}
			maxIdx := len(colNames)
			if len(row) < maxIdx {
				maxIdx = len(row)
			}
			for i := 0; i < maxIdx; i++ {
				chg[colNames[i]] = Delta{F: row[i], T: nil}
			}
			if err := addDoc(pkVal(row), chg, "d"); err != nil {
				return fmt.Errorf("delete action: %w", err)
			}
		}
	case canal.UpdateAction:
		for i := 0; i < len(e.Rows); i += 2 {
			before, after := e.Rows[i], e.Rows[i+1]
			chg := map[string]Delta{}
			maxIdx := len(colNames)
			if len(before) < maxIdx {
				maxIdx = len(before)
			}
			if len(after) < maxIdx {
				maxIdx = len(after)
			}
			for c := 0; c < maxIdx; c++ {
				if !reflect.DeepEqual(before[c], after[c]) {
					chg[colNames[c]] = Delta{F: before[c], T: after[c]}
				}
			}
			if err := addDoc(pkVal(after), chg, "u"); err != nil {
				return fmt.Errorf("update action: %w", err)
			}
		}
	}
	return nil
}

func (h *Handler) OnPosSynced(
	header *replication.EventHeader,
	pos mysql.Position,
	set mysql.GTIDSet,
	force bool,
) error {
	h.lastFile = pos.Name
	h.lastPos = uint64(pos.Pos)

	// GTIDSet can be nil early on. Use lastGTID (from OnGTID) or empty string.
	if set != nil {
		h.lastGTID = set.String()
	}
	// Note: GTID is now saved atomically with batch write in writeBatchWithGTID
	return nil
}

func (h *Handler) OnRotate(header *replication.EventHeader, ev *replication.RotateEvent) error {
	h.lastFile = string(ev.NextLogName)
	h.lastPos = ev.Position
	return nil
}

func (h *Handler) OnTableChanged(header *replication.EventHeader, schema, table string) error {
	key := fmt.Sprintf("%s.%s", schema, table)
	delete(h.tableSchemas, key)
	// Flush current batch to ensure consistency
	if err := h.Flush(context.Background()); err != nil {
		log.Printf("Error flushing on schema change: %v", err)
		// Don't stop replication, just warn
	}
	return nil
}

func (h *Handler) OnXID(header *replication.EventHeader, nextPos mysql.Position) error { return nil }

func (h *Handler) OnGTID(header *replication.EventHeader, ev mysql.BinlogGTIDEvent) error {
	// Intentionally left as a no-op.
	// OnPosSynced is called after every event and receives the full mysql.GTIDSet
	// with a proper String() representation. Using fmt.Sprintf("%+v", ev) here
	// would store a Go-struct debug string that cannot be parsed back as a GTID set.
	return nil
}

// func (h *Handler) OnRowGTID(mysql.GTIDSet) error              { return nil }

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func normalizeMySQLFlavor(raw string) string {
	// Allow accidental inline comments in env value, e.g. "mysql # note".
	v := strings.TrimSpace(raw)
	if i := strings.Index(v, "#"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	v = strings.ToLower(v)
	if v == "mysql" || v == "mariadb" {
		return v
	}
	log.Printf("Warning: invalid MYSQL_FLAVOR=%q, defaulting to mysql", raw)
	return "mysql"
}

// runCanalWithRetry runs Canal with automatic reconnection on protocol errors.
// A fresh Canal instance is created for every attempt because the go-mysql Canal
// object is single-use: after Close() the syncer is not reset and cannot be
// restarted (would immediately fail with "Sync is running, must Close first").
func runCanalWithRetry(cfg *canal.Config, handler canal.EventHandler, sink *MongoSink, source string, maxRetries int) error {
	var lastErr error
	baseDelay := 2 * time.Second

	isRecoverableErr := func(err error) bool {
		errStr := err.Error()
		return strings.Contains(errStr, "invalid sequence") ||
			strings.Contains(errStr, "connection refused") ||
			strings.Contains(errStr, "connection reset") ||
			strings.Contains(errStr, "broken pipe") ||
			strings.Contains(errStr, "EOF") ||
			strings.Contains(errStr, "i/o timeout")
	}

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * baseDelay
			if delay > 60*time.Second {
				delay = 60 * time.Second
			}
			log.Printf("Canal retry attempt %d/%d, waiting %s", attempt+1, maxRetries, delay)
			time.Sleep(delay)
		}

		// Create a fresh Canal instance — the library does not support restart
		// after Close(), so we always start with a clean object.
		c, err := canal.NewCanal(cfg)
		if err != nil {
			lastErr = fmt.Errorf("create canal: %w", err)
			continue
		}
		c.SetEventHandler(handler)

		// Load the last committed GTID checkpoint from MongoDB.
		gtidStr, ok, err := sink.loadGTID(context.Background(), source)
		if err != nil {
			log.Printf("Error: Could not load GTID from MongoDB: %v", err)
		}

		// StartFromGTID blocks until the canal stops or errors.
		var runErr error
		if ok && gtidStr != "" {
			gtidSet, parseErr := mysql.ParseGTIDSet(mysql.MySQLFlavor, gtidStr)
			if parseErr != nil {
				log.Printf("Error: Could not parse saved GTID '%s': %v — falling back to master position", gtidStr, parseErr)
				ok = false
			} else {
				log.Printf("Resuming from saved GTID: %s", gtidStr)
				runErr = c.StartFromGTID(gtidSet)
				if runErr != nil {
					runErr = fmt.Errorf("start from GTID: %w", runErr)
				}
			}
		}

		if runErr == nil && (!ok || gtidStr == "") {
			gset, masterErr := c.GetMasterGTIDSet()
			if masterErr != nil {
				c.Close()
				lastErr = fmt.Errorf("get master GTID: %w", masterErr)
				continue
			}
			log.Printf("No saved GTID — starting from master position")
			runErr = c.StartFromGTID(gset)
			if runErr != nil {
				runErr = fmt.Errorf("start from master GTID: %w", runErr)
			}
		}

		c.Close() // always release resources for this attempt

		if runErr == nil {
			return nil // clean shutdown (e.g. SIGTERM)
		}

		lastErr = runErr

		if !isRecoverableErr(runErr) {
			return runErr
		}
	}

	return fmt.Errorf("canal retry exhausted after %d attempts: %w", maxRetries, lastErr)
}

func main() {
	// Load .env (absolute path is safest under systemd)
	_ = godotenv.Load(".env")

	// Timezone (server should already be IST; this just ensures conversion)
	loc, _ := time.LoadLocation(getenv("TZ", "Asia/Kolkata"))

	// Mongo
	mongoURI := getenv("MONGO_URI", "mongodb://127.0.0.1:27017/?appName=audit")
	mongoDB := getenv("MONGO_DB", "audit")
	mongoColl := getenv("MONGO_COLL", "row_changes")
	offsetsColl := getenv("MONGO_OFFSETS_COLL", "binlog_offsets")
	sink, err := newMongoSink(mongoURI, mongoDB, mongoColl, offsetsColl, loc)
	if err != nil {
		log.Fatal(err)
	}

	// Canal config
	cfg := canal.NewDefaultConfig()
	cfg.Addr = getenv("MYSQL_ADDR", "127.0.0.1:3306")
	cfg.User = getenv("MYSQL_USER", "repl")
	cfg.Password = os.Getenv("MYSQL_PASS")
	cfg.Flavor = normalizeMySQLFlavor(getenv("MYSQL_FLAVOR", "mysql"))

	// unique server id
	if sid := getenv("MYSQL_SERVER_ID", "2222"); sid != "" {
		if n, err := strconv.Atoi(sid); err == nil {
			cfg.ServerID = uint32(n)
		}
	}

	// include everything; exclude system schemas
	inc := getenv("INCLUDE_REGEX", ".*\\..*")
	exc := getenv("EXCLUDE_REGEX", "^(mysql|performance_schema|information_schema|sys)\\..*")
	cfg.IncludeTableRegex = []string{inc}
	cfg.ExcludeTableRegex = []string{exc}

	// No initial dump (start streaming). You can enable dump if you want a snapshot.
	cfg.Dump.ExecutionPath = "" // no mysqldump

	h := &Handler{
		sink:         sink,
		source:       "mysql://" + cfg.Addr,
		loc:          loc,
		tableSchemas: make(map[string][]string),
	}

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	// Run canal in goroutine so we can listen for signals
	errChan := make(chan error, 1)
	go func() {
		// Recover any pending batches from previous crash
		if err := sink.RecoverPendingBatches(context.Background()); err != nil {
			log.Printf("Error: Could not recover pending batches: %v", err)
			// Don't fail startup, continue with replication
		}

		// Run Canal with automatic retry on protocol errors.
		// Each attempt creates a fresh Canal instance from cfg.
		// Max 10 retries with exponential backoff (2s, 4s, 8s, 16s, 32s, 60s...)
		if err := runCanalWithRetry(cfg, h, sink, h.source, 10); err != nil {
			errChan <- err
			return
		}
	}()

	// Wait for signal or error
	select {
	case sig := <-sigChan:
		_ = sig

		// Flush remaining batch and close MongoDB — Canal is managed inside
		// runCanalWithRetry and is already closed when it returns.

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := h.Flush(ctx); err != nil {
			log.Printf("Error flushing batch during shutdown: %v", err)
		}
		cancel()

		// Close MongoDB client
		if err := sink.client.Disconnect(context.Background()); err != nil {
			log.Printf("Error closing MongoDB: %v", err)
		}
		os.Exit(0)

	case err := <-errChan:
		log.Fatalf("Canal error: %v", err)
	}
}
