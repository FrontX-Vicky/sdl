package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Delta struct {
	F any `bson:"f,omitempty"`
	T any `bson:"t,omitempty"`
}
type Meta struct {
	DB  string `bson:"db"`
	Tbl string `bson:"tbl"`
	PK  any    `bson:"pk"`
}
type EventDoc struct {
	ID    string           `bson:"_id"`
	TS    time.Time        `bson:"ts"`
	OP    string           `bson:"op"`
	Meta  Meta             `bson:"meta"`
	Chg   map[string]Delta `bson:"chg,omitempty"`
	Src   map[string]any   `bson:"src,omitempty"`
	TSIST string           `bson:"ts_ist,omitempty"`
}

func main() {
	var (
		uri    = flag.String("uri", "mongodb://127.0.0.1:27017", "MongoDB URI")
		db     = flag.String("db", "audit", "Database name")
		coll   = flag.String("coll", "row_changes", "Collection name")
		limit  = flag.Int("history", 20, "Print this many recent docs before live tail (0 to skip)")
		desc   = flag.Bool("desc", true, "Show history newest first")
		since  = flag.String("since", "", "Only show docs with ts >= RFC3339 (history and live)")
		op     = flag.String("op", "", "Filter by op: i|u|d")
		table  = flag.String("table", "", "Filter by table as db.table")
		wide   = flag.Bool("wide", false, "Wider CHANGES column")
		poll   = flag.Duration("poll", 0, "Polling fallback interval (e.g. 2s). Set if change streams not available")
	)
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(*uri))
	if err != nil { log.Fatalf("mongo connect: %v", err) }
	defer func() { _ = client.Disconnect(context.Background()) }()

	c := client.Database(*db).Collection(*coll)

	// Optional history
	filter := buildFilter(*op, *table, *since)
	if *limit > 0 {
		opts := options.Find().SetLimit(int64(*limit))
		order := -1
		if !*desc { order = 1 }
		opts.SetSort(bson.D{{Key: "ts", Value: order}})
		cur, err := c.Find(ctx, filter, opts)
		if err != nil { log.Fatalf("find history: %v", err) }
		var rows []EventDoc
		if err := cur.All(ctx, &rows); err != nil { log.Fatalf("read history: %v", err) }
		printHeader(*wide)
		for _, r := range rows {
			printRow(r, *wide)
		}
		if len(rows) > 0 {
			fmt.Printf("\n-- history above (%d rows) --\n\n", len(rows))
		}
	}

	// Live: try change stream first
	if *poll > 0 {
		log.Printf("Change stream fallback disabled; polling every %s…", *poll)
		pollLoop(ctx, c, filter, *poll, *wide)
		return
	}

	csFilter := changeStreamPipeline(*op, *table, *since)
	opts := options.ChangeStream().
		SetFullDocument(options.UpdateLookup)
	stream, err := c.Watch(ctx, csFilter, opts)
	if err != nil {
		log.Printf("change stream unavailable (%v). Falling back to polling every 2s.", err)
		pollLoop(ctx, c, filter, 2*time.Second, *wide)
		return
	}
	defer stream.Close(ctx)

	printHeader(*wide)
	for stream.Next(ctx) {
		var ev struct {
			OperationType string    `bson:"operationType"`
			ClusterTime   any       `bson:"clusterTime"`
			FullDocument  EventDoc  `bson:"fullDocument"`
			DocumentKey   bson.M    `bson:"documentKey"`
		}
		if err := stream.Decode(&ev); err != nil {
			log.Printf("decode stream: %v", err)
			continue
		}
		// Only inserts are expected from your writer; guard anyway.
		if ev.OperationType != "insert" {
			continue
		}
		if !matchFilter(ev.FullDocument, *op, *table, *since) {
			continue
		}
		printRow(ev.FullDocument, *wide)
	}
	if err := stream.Err(); err != nil && ctx.Err() == nil {
		log.Printf("stream error: %v", err)
	}
	log.Println("bye")
}

func buildFilter(op, table, since string) bson.M {
	f := bson.M{}
	if op == "i" || op == "u" || op == "d" {
		f["op"] = op
	}
	if table != "" {
		db, tb := splitTable(table)
		if db != "" { f["meta.db"] = db }
		if tb != "" { f["meta.tbl"] = tb }
	}
	if since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			f["ts"] = bson.M{"$gte": t}
		}
	}
	return f
}

func changeStreamPipeline(op, table, since string) mongo.Pipeline {
	// Match only inserts into this collection, then optional field matches.
	match := bson.D{{Key: "operationType", Value: "insert"}}
	and := bson.A{bson.D(match)}

	// Field-level matches
	if op == "i" || op == "u" || op == "d" {
		and = append(and, bson.D{{Key: "fullDocument.op", Value: op}})
	}
	if table != "" {
		db, tb := splitTable(table)
		if db != "" { and = append(and, bson.D{{Key: "fullDocument.meta.db", Value: db}}) }
		if tb != "" { and = append(and, bson.D{{Key: "fullDocument.meta.tbl", Value: tb}}) }
	}
	if since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			and = append(and, bson.D{{Key: "fullDocument.ts", Value: bson.M{"$gte": t}}})
		}
	}

	return mongo.Pipeline{
		{{Key: "$match", Value: bson.D{{Key: "$and", Value: and}}}},
	}
}

func matchFilter(e EventDoc, op, table, since string) bool {
	if op == "i" || op == "u" || op == "d" {
		if e.OP != op { return false }
	}
	if table != "" {
		db, tb := splitTable(table)
		if db != "" && e.Meta.DB != db { return false }
		if tb != "" && e.Meta.Tbl != tb { return false }
	}
	if since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			if e.TS.Before(t) { return false }
		}
	}
	return true
}

func pollLoop(ctx context.Context, c *mongo.Collection, baseFilter bson.M, every time.Duration, wide bool) {
	// Naive tail by ts with polling
	var last time.Time
	if v, ok := baseFilter["ts"].(bson.M); ok {
		if gte, ok2 := v["$gte"].(time.Time); ok2 {
			last = gte
		}
	}
	printHeader(wide)

	t := time.NewTicker(every)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f := bson.M{}
			for k, v := range baseFilter { f[k] = v }
			if !last.IsZero() {
				f["ts"] = bson.M{"$gt": last}
			}
			opts := options.Find().SetSort(bson.D{{Key: "ts", Value: 1}})
			cur, err := c.Find(ctx, f, opts)
			if err != nil {
				log.Printf("poll find: %v", err)
				continue
			}
			var rows []EventDoc
			if err := cur.All(ctx, &rows); err != nil {
				log.Printf("poll read: %v", err)
				continue
			}
			for _, r := range rows {
				printRow(r, wide)
				if r.TS.After(last) { last = r.TS }
			}
		}
	}
}

// -------- Presentation helpers --------

func splitTable(s string) (db, tbl string) {
	parts := strings.SplitN(s, ".", 2)
	if len(parts) == 2 { return parts[0], parts[1] }
	return s, ""
}

func printHeader(wide bool) {
	maxCH := 60
	if wide { maxCH = 120 }
	h := []string{"TS(IST)", "OP", "DB", "TABLE", "PK", "CHANGES", "GTID", "FILE:POS"}
	w := []int{19, 2, 16, 18, 18, maxCH, 28, 22}
	fmt.Println()
	line := ""
	for i, hd := range h {
		if i > 0 { line += "  " }
		line += fmt.Sprintf("%-*s", w[i], hd)
	}
	fmt.Println(line)
	total := 0
	for _, x := range w { total += x }
	fmt.Println(strings.Repeat("-", total+14))
}

func printRow(e EventDoc, wide bool) {
	maxCH := 60
	if wide { maxCH = 120 }

	tsStr := e.TSIST
	if tsStr == "" {
		// IST (UTC+5:30)
		tsStr = e.TS.In(time.FixedZone("IST", 5*3600+1800)).Format("2006-01-02 15:04:05")
	}
	gtid, filepos := gtidAndFilePos(e.Src)

	cols := []string{
		clip(tsStr, 19),
		clip(strings.ToLower(e.OP), 2),
		clip(e.Meta.DB, 16),
		clip(e.Meta.Tbl, 18),
		clip(toS(e.Meta.PK), 18),
		clip(changesSummary(e.Chg), maxCH),
		clip(gtid, 28),
		clip(filepos, 22),
	}
	w := []int{19, 2, 16, 18, 18, maxCH, 28, 22}

	line := ""
	for i, v := range cols {
		if i > 0 { line += "  " }
		line += fmt.Sprintf("%-*s", w[i], v)
	}
	fmt.Println(line)
}

func toS(v any) string {
	switch x := v.(type) {
	case nil:
		return "∅"
	case string:
		return x
	case []byte:
		if len(x) > 32 { return fmt.Sprintf("%x…(%dB)", x[:16], len(x)) }
		return fmt.Sprintf("%x", x)
	case time.Time:
		return x.Format(time.RFC3339)
	default:
		return fmt.Sprint(x)
	}
}

func clip(s string, n int) string {
	if n <= 0 { return s }
	r := []rune(s)
	if len(r) <= n { return s }
	if n <= 1 { return string(r[:n]) }
	return string(r[:n-1]) + "…"
}

func changesSummary(ch map[string]Delta) string {
	if len(ch) == 0 { return "" }
	keys := make([]string, 0, len(ch))
	for k := range ch { keys = append(keys, k) }
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		d := ch[k]
		f := summarizeVal(d.F)
		t := summarizeVal(d.T)
		parts = append(parts, fmt.Sprintf("%s:%s→%s", k, f, t))
	}
	return strings.Join(parts, " | ")
}

func summarizeVal(v any) string {
	switch x := v.(type) {
	case nil:
		return "∅"
	case string:
		if len(x) > 40 { return fmt.Sprintf("%q", x[:37]+"…") }
		return fmt.Sprintf("%q", x)
	case []byte:
		if len(x) > 16 { return fmt.Sprintf("0x%x…(%dB)", x[:8], len(x)) }
		return fmt.Sprintf("0x%x", x)
	case time.Time:
		return x.UTC().Format("2006-01-02 15:04:05Z")
	default:
		s := fmt.Sprint(x)
		if len(s) > 40 { return s[:37] + "…" }
		return s
	}
}

func gtidAndFilePos(src map[string]any) (gtid string, filepos string) {
	if src == nil { return "", "" }
	if g, ok := src["gtid"]; ok && g != nil {
		gtid = fmt.Sprint(g)
	}
	if bl, ok := src["binlog"].(map[string]any); ok {
		file := fmt.Sprint(bl["file"])
		pos := fmt.Sprint(bl["pos"])
		if file != "" || pos != "" {
			filepos = fmt.Sprintf("%s:%s", file, pos)
		}
	}
	return
}
