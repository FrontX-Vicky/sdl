package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/joho/godotenv"
	"github.com/rivo/tview"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Delta struct {
	F any `bson:"f,omitempty" json:"f,omitempty"`
	T any `bson:"t,omitempty" json:"t,omitempty"`
}

type Meta struct {
	DB  string `bson:"db" json:"db"`
	Tbl string `bson:"tbl" json:"tbl"`
	PK  any    `bson:"pk" json:"pk"`
}

type EventDoc struct {
	ID    string           `bson:"_id" json:"_id"`
	TS    time.Time        `bson:"ts" json:"ts"`
	OP    string           `bson:"op" json:"op"`
	Meta  Meta             `bson:"meta" json:"meta"`
	Seq   int64            `bson:"seq,omitempty" json:"seq,omitempty"`
	Chg   map[string]Delta `bson:"chg,omitempty" json:"chg,omitempty"`
	Src   map[string]any   `bson:"src,omitempty" json:"src,omitempty"`
	TSIST string           `bson:"ts_ist,omitempty" json:"ts_ist,omitempty"`
}

type QueryParams struct {
	Database  string
	Table     string
	PK        any
	Operation string // "i", "u", "d" or empty for all
	StartTime time.Time
	EndTime   time.Time
	Limit     int64
}

type Stats struct {
	Total         int
	PerOp         map[string]int
	Series        map[string][]int
	BucketStart   time.Time
	BucketMinutes int
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func connectMongo() (*mongo.Collection, error) {
	if err := godotenv.Load(".env"); err != nil {
		// Silent: .env file optional
	}

	mongoURI := getenv("MONGO_URI", "mongodb://127.0.0.1:27017/?appName=audit")
	mongoDB := getenv("MONGO_DB", "audit")
	mongoColl := getenv("MONGO_COLL", "row_changes")

	log.Printf("Connecting to MongoDB %s (db=%s coll=%s)...", mongoURI, mongoDB, mongoColl)

	// Keep startup snappy so the UI doesn't appear to hang when MongoDB is down.
	connectTimeout := 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().
		ApplyURI(mongoURI).
		SetServerSelectionTimeout(connectTimeout).
		SetConnectTimeout(connectTimeout))
	if err != nil {
		return nil, fmt.Errorf("MongoDB connection failed: %v", err)
	}

	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("MongoDB ping failed: %v", err)
	}

	return client.Database(mongoDB).Collection(mongoColl), nil
}

func fetchEvents(coll *mongo.Collection, params QueryParams) ([]EventDoc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// Build filter
	filter := bson.M{}

	if params.Database != "" {
		filter["meta.db"] = params.Database
	}

	if params.Table != "" {
		filter["meta.tbl"] = params.Table
	}

	if params.PK != nil {
		filter["meta.pk"] = params.PK
	}

	if params.Operation != "" {
		filter["op"] = params.Operation
	}

	// Time range filter
	if !params.StartTime.IsZero() || !params.EndTime.IsZero() {
		timeFilter := bson.M{}
		if !params.StartTime.IsZero() {
			timeFilter["$gte"] = params.StartTime
		}
		if !params.EndTime.IsZero() {
			timeFilter["$lte"] = params.EndTime
		}
		filter["ts"] = timeFilter
	}

	// Query options
	opts := options.Find().SetSort(bson.D{bson.E{Key: "ts", Value: -1}}) // Sort by timestamp descending

	if params.Limit > 0 {
		opts.SetLimit(params.Limit)
	}

	cursor, err := coll.Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var events []EventDoc
	if err := cursor.All(ctx, &events); err != nil {
		return nil, err
	}

	return events, nil
}

func exportToJSON(events []EventDoc, filename string) error {
	data, err := json.MarshalIndent(events, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filename, data, 0644)
}

func exportToCSV(events []EventDoc, filename string) error {
	if len(events) == 0 {
		return fmt.Errorf("no events to export")
	}

	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	// Collect all unique column names from all events
	columnSet := make(map[string]bool)
	for _, event := range events {
		for col := range event.Chg {
			columnSet[col] = true
		}
	}

	// Convert to sorted slice for consistent column order
	columns := make([]string, 0, len(columnSet))
	for col := range columnSet {
		columns = append(columns, col)
	}
	sort.Strings(columns)

	// Build CSV header
	header := []string{
		"Event_ID",
		"Timestamp_UTC",
		"Timestamp_IST",
		"Operation",
		"Database",
		"Table",
		"Primary_Key",
		"Binlog_File",
		"Binlog_Position",
	}

	// Add columns for "from" and "to" values
	for _, col := range columns {
		header = append(header, col+"_FROM", col+"_TO")
	}

	if err := writer.Write(header); err != nil {
		return err
	}

	// Write data rows
	for _, event := range events {
		row := make([]string, len(header))

		// Basic fields
		row[0] = event.ID
		row[1] = event.TS.Format(time.RFC3339)
		row[2] = event.TSIST

		opName := map[string]string{"i": "INSERT", "u": "UPDATE", "d": "DELETE"}
		if name, ok := opName[event.OP]; ok {
			row[3] = name
		} else {
			row[3] = event.OP
		}

		row[4] = event.Meta.DB
		row[5] = event.Meta.Tbl
		row[6] = fmt.Sprintf("%v", event.Meta.PK)

		// Binlog info
		if event.Src != nil {
			if binlog, ok := event.Src["binlog"].(map[string]interface{}); ok {
				if file, ok := binlog["file"]; ok {
					row[7] = fmt.Sprintf("%v", file)
				}
				if pos, ok := binlog["pos"]; ok {
					row[8] = fmt.Sprintf("%v", pos)
				}
			}
		}

		// Change data
		idx := 9
		for _, col := range columns {
			if delta, exists := event.Chg[col]; exists {
				row[idx] = formatValue(delta.F)
				row[idx+1] = formatValue(delta.T)
			} else {
				row[idx] = ""
				row[idx+1] = ""
			}
			idx += 2
		}

		if err := writer.Write(row); err != nil {
			return err
		}
	}

	return nil
}

func formatValue(v any) string {
	if v == nil {
		return "NULL"
	}

	// Handle byte arrays (common in MySQL)
	if b, ok := v.([]byte); ok {
		return string(b)
	}

	// Handle time values
	if t, ok := v.(time.Time); ok {
		return t.Format("2006-01-02 15:04:05")
	}

	// Convert to string, handle quotes for CSV
	s := fmt.Sprintf("%v", v)

	// Escape quotes and clean up
	s = strings.ReplaceAll(s, "\"", "\"\"")

	return s
}

// Global app state
type AppState struct {
	app           *tview.Application
	coll          *mongo.Collection
	events        []EventDoc
	selectedEvent *EventDoc
	status        string
	refreshUI     func()
	stats         Stats
	lastUpdated   time.Time
	filters       struct {
		database  string
		table     string
		pk        any
		startTime time.Time
		endTime   time.Time
		limit     int64
	}
	autoRefresh bool
	stopRefresh chan bool
}

func newAppState(coll *mongo.Collection) *AppState {
	state := &AppState{
		coll:        coll,
		events:      []EventDoc{},
		autoRefresh: false,
		stopRefresh: make(chan bool, 1),
		status:      "Idle",
	}
	state.filters.limit = 100
	return state
}

func (s *AppState) loadEvents() error {
	if s.coll == nil {
		return fmt.Errorf("not connected to MongoDB")
	}

	params := QueryParams{
		Database:  s.filters.database,
		Table:     s.filters.table,
		PK:        s.filters.pk,
		StartTime: s.filters.startTime,
		EndTime:   s.filters.endTime,
		Limit:     s.filters.limit,
	}

	events, err := fetchEvents(s.coll, params)
	if err != nil {
		return err
	}
	s.events = events
	s.lastUpdated = time.Now()
	return nil
}

func computeStats(events []EventDoc) Stats {
	bucketCount := 30
	stats := Stats{
		PerOp:         map[string]int{"i": 0, "u": 0, "d": 0},
		Series:        map[string][]int{"i": make([]int, bucketCount), "u": make([]int, bucketCount), "d": make([]int, bucketCount)},
		BucketMinutes: bucketCount,
	}

	// Anchor buckets to "now" so the chart always moves forward.
	end := time.Now().UTC().Truncate(time.Minute)
	start := end.Add(-time.Duration(bucketCount-1) * time.Minute)
	stats.BucketStart = start

	for _, e := range events {
		stats.Total++
		stats.PerOp[e.OP]++

		minute := e.TS.UTC().Truncate(time.Minute)
		if minute.Before(start) {
			continue
		}
		idx := int(minute.Sub(start) / time.Minute)
		if idx >= 0 && idx < bucketCount {
			if _, ok := stats.Series[e.OP]; !ok {
				stats.Series[e.OP] = make([]int, bucketCount)
			}
			stats.Series[e.OP][idx]++
		}
	}

	return stats
}

func renderGraph(stats Stats, height, width int) string {
	if stats.BucketMinutes == 0 {
		return "No per-minute data."
	}

	if height < 4 {
		height = 4
	}
	if width < 20 {
		width = 20
	}

	maxVal := 0
	for _, series := range stats.Series {
		for _, v := range series {
			if v > maxVal {
				maxVal = v
			}
		}
	}

	type cell struct {
		r   rune
		col string
	}

	grid := make([][]cell, height)
	for i := range grid {
		grid[i] = make([]cell, width)
	}

	ops := []struct {
		key   string
		label string
		color string
	}{
		{"i", "INS", "green"},
		{"u", "UPD", "yellow"},
		{"d", "DEL", "red"},
	}

	scale := func(v int) int {
		if maxVal == 0 {
			return 0
		}
		return (v*(height-1) + maxVal - 1) / maxVal
	}

	for _, op := range ops {
		series := stats.Series[op.key]
		if series == nil {
			series = make([]int, stats.BucketMinutes)
		}
		prevY := -1
		for x, v := range series {
			if x >= width {
				break
			}
			y := height - 1 - scale(v)
			if y < 0 {
				y = 0
			}
			if y >= height {
				y = height - 1
			}

			// draw point
			cur := grid[y][x]
			if cur.r == 0 {
				grid[y][x] = cell{r: '•', col: op.color}
			} else {
				grid[y][x] = cell{r: '×', col: op.color}
			}

			// draw simple connector to previous point
			if prevY != -1 && prevY != y {
				step := 1
				if y < prevY {
					step = -1
				}
				for yy := prevY; yy != y; yy += step {
					cur2 := grid[yy][x]
					if cur2.r == 0 {
						grid[yy][x] = cell{r: '│', col: op.color}
					}
				}
			}
			prevY = y
		}
	}

	var sb strings.Builder
	// Legend
	sb.WriteString("[green]INS[-] [yellow]UPD[-] [red]DEL[-]\n")

	// Calculate Y-axis labels
	yAxisLabels := make([]string, height)
	for y := 0; y < height; y++ {
		val := maxVal - (maxVal*y)/(height-1)
		if y == height-1 {
			val = 0
		}
		yAxisLabels[y] = fmt.Sprintf("%3d", val)
	}

	// Find max label width
	maxLabelWidth := 3
	for _, label := range yAxisLabels {
		if len(label) > maxLabelWidth {
			maxLabelWidth = len(label)
		}
	}

	// Render grid with Y-axis labels
	for y := 0; y < height; y++ {
		// Y-axis label
		label := yAxisLabels[y]
		for len(label) < maxLabelWidth {
			label = " " + label
		}
		sb.WriteString(label + " ")

		var lastColor string
		for x := 0; x < width; x++ {
			c := grid[y][x]
			if c.r == 0 {
				if lastColor != "" {
					sb.WriteString("[-]")
					lastColor = ""
				}
				sb.WriteRune(' ')
				continue
			}
			if c.col != lastColor {
				if lastColor != "" {
					sb.WriteString("[-]")
				}
				sb.WriteString("[" + c.col + "]")
				lastColor = c.col
			}
			sb.WriteRune(c.r)
		}
		if lastColor != "" {
			sb.WriteString("[-]")
		}
		sb.WriteByte('\n')
	}

	start := stats.BucketStart.Local()
	end := start.Add(time.Duration(stats.BucketMinutes-1) * time.Minute)
	sb.WriteString(fmt.Sprintf("%s ... %s (per minute)", start.Format("15:04"), end.Format("15:04")))

	return sb.String()
}

func createMainUI(state *AppState) *tview.Pages {
	pages := tview.NewPages()

	// Create main layout
	mainFlex := tview.NewFlex().SetDirection(tview.FlexRow)

	// Header
	header := tview.NewTextView().
		SetTextAlign(tview.AlignCenter).
		SetText("[yellow]MySQL Binlog Audit Trail Viewer[-] - [green]Press ? for help[-]").
		SetDynamicColors(true)

	// Filter panel
	filterText := tview.NewTextView().
		SetDynamicColors(true).
		SetText("[cyan]Filters:[-] None | [yellow]F1[-] Set Filters | [yellow]F5[-] Refresh | [yellow]F9[-] Export | [yellow]F10[-] Auto-refresh | [yellow]?[-] Help")

	// Graph + stats panels (btop-style top section)
	graphText := tview.NewTextView()
	graphText.SetDynamicColors(true)
	graphText.SetWrap(false)
	graphText.SetBorder(true)
	graphText.SetTitle(" Activity (per minute) ")

	statsPanel := tview.NewTextView()
	statsPanel.SetDynamicColors(true)
	statsPanel.SetBorder(true)
	statsPanel.SetTitle(" Totals / Status ")
	statsPanel.SetText("[white]Total:[-] 0\n[green]INS:[-] 0  [yellow]UPD:[-] 0  [red]DEL:[-] 0\nStatus: starting...\nLast refresh: -")

	// Status bar (footer)
	statsText := tview.NewTextView().
		SetDynamicColors(true).
		SetText("[green]Events: 0[-] | [yellow]Auto-refresh: OFF[-] | Status: starting...")

	// Events table
	table := tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false).
		SetFixed(1, 0)

	// Set table headers
	headers := []string{"Timestamp (IST)", "OP", "Database", "Table", "PK", "Binlog Pos", "Changes"}
	for i, header := range headers {
		table.SetCell(0, i, tview.NewTableCell(header).
			SetTextColor(tcell.ColorYellow).
			SetAlign(tview.AlignLeft).
			SetSelectable(false))
	}

	// Detail panel (initially hidden)
	detailView := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(true)

	detailPanel := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tview.NewTextView().
			SetText("[yellow]Event Details (Press ESC to close)[-]").
			SetDynamicColors(true).
			SetTextAlign(tview.AlignCenter), 1, 0, false).
		AddItem(detailView, 0, 1, true)

	// Update table function
	updateTable := func() {
		state.stats = computeStats(state.events)

		// Totals + status panel
		ins := state.stats.PerOp["i"]
		upd := state.stats.PerOp["u"]
		del := state.stats.PerOp["d"]
		lastRef := "never"
		if !state.lastUpdated.IsZero() {
			lastRef = state.lastUpdated.Format("15:04:05")
		}
		statsPanel.SetText(fmt.Sprintf("[white]Total:[-] %d\n[green]INS:[-] %d  [yellow]UPD:[-] %d  [red]DEL:[-] %d\nStatus: %s\nLast refresh: %s",
			state.stats.Total, ins, upd, del, state.status, lastRef))

		// Trend graph - get dynamic dimensions
		_, _, graphWidth, graphHeight := graphText.GetRect()
		if graphWidth > 0 && graphHeight > 0 {
			graphText.SetText(renderGraph(state.stats, graphHeight-2, graphWidth-2))
		} else {
			graphText.SetText(renderGraph(state.stats, 20, 60))
		}

		// Clear existing rows (keep header)
		for row := table.GetRowCount() - 1; row > 0; row-- {
			table.RemoveRow(row)
		}

		// Add events
		for i, event := range state.events {
			opName := map[string]string{"i": "INS", "u": "UPD", "d": "DEL"}[event.OP]
			opColor := map[string]tcell.Color{
				"i": tcell.ColorGreen,
				"u": tcell.ColorYellow,
				"d": tcell.ColorRed,
			}[event.OP]
			if opColor == 0 {
				opColor = tcell.ColorWhite
			}

			tsIST := event.TSIST
			if tsIST == "" {
				tsIST = event.TS.Format("2006-01-02 15:04:05")
			}

			pk := fmt.Sprintf("%v", event.Meta.PK)
			if len(pk) > 20 {
				pk = pk[:17] + "..."
			}

			binlogPos := ""
			if event.Src != nil {
				if binlog, ok := event.Src["binlog"].(map[string]interface{}); ok {
					if file, ok := binlog["file"]; ok {
						if pos, ok := binlog["pos"]; ok {
							binlogPos = fmt.Sprintf("%v:%v", file, pos)
						}
					}
				}
			}

			changes := ""
			if len(event.Chg) > 0 {
				changedCols := make([]string, 0, len(event.Chg))
				for col := range event.Chg {
					changedCols = append(changedCols, col)
				}
				sort.Strings(changedCols)
				changes = strings.Join(changedCols, ", ")
				if len(changes) > 40 {
					changes = changes[:37] + "..."
				}
			}

			row := i + 1
			table.SetCell(row, 0, tview.NewTableCell(tsIST))
			table.SetCell(row, 1, tview.NewTableCell(opName).SetTextColor(opColor))
			table.SetCell(row, 2, tview.NewTableCell(event.Meta.DB))
			table.SetCell(row, 3, tview.NewTableCell(event.Meta.Tbl))
			table.SetCell(row, 4, tview.NewTableCell(pk))
			table.SetCell(row, 5, tview.NewTableCell(binlogPos).SetTextColor(tcell.ColorBlue))
			table.SetCell(row, 6, tview.NewTableCell(changes).SetTextColor(tcell.ColorYellow))
		}

		// Scroll table to top to show newest entries
		if len(state.events) > 0 {
			table.Select(1, 0)
		}

		// Update stats
		autoRefreshStatus := "OFF"
		if state.autoRefresh {
			autoRefreshStatus = "[green]ON[-]"
		}
		statsText.SetText(fmt.Sprintf("[green]Events: %d[-] | Auto-refresh: %s",
			len(state.events), autoRefreshStatus))
		if state.status != "" {
			statsText.SetText(fmt.Sprintf("%s | Status: %s", statsText.GetText(true), state.status))
		}

		// Update filter display
		filterDisplay := "[cyan]Filters:[-] "
		if state.filters.database != "" {
			filterDisplay += fmt.Sprintf("DB=%s ", state.filters.database)
		}
		if state.filters.table != "" {
			filterDisplay += fmt.Sprintf("Table=%s ", state.filters.table)
		}
		if state.filters.pk != nil {
			filterDisplay += fmt.Sprintf("PK=%v ", state.filters.pk)
		}
		if !state.filters.startTime.IsZero() {
			filterDisplay += fmt.Sprintf("From=%s ", state.filters.startTime.Format("2006-01-02"))
		}
		if !state.filters.endTime.IsZero() {
			filterDisplay += fmt.Sprintf("To=%s ", state.filters.endTime.Format("2006-01-02"))
		}
		if state.filters.database == "" && state.filters.table == "" && state.filters.pk == nil {
			filterDisplay += "None "
		}
		filterDisplay += "| [yellow]F1[-] Set | [yellow]F5[-] Refresh | [yellow]F9[-] Export"

		filterText.SetText(filterDisplay)
	}

	// Table selection handler
	table.SetSelectedFunc(func(row, col int) {
		if row > 0 && row <= len(state.events) {
			event := state.events[row-1]
			state.selectedEvent = &event

			// Show detail view
			detailText := formatEventDetail(event)
			detailView.SetText(detailText)
			pages.ShowPage("detail")
		}
	})

	// Assemble main view
	mainFlex.
		AddItem(header, 1, 0, false).
		AddItem(filterText, 1, 0, false).
		AddItem(tview.NewFlex().
			SetDirection(tview.FlexColumn).
			AddItem(graphText, 0, 7, false).
			AddItem(statsPanel, 0, 2, false), 0, 5, false). // larger top section (~55-65%)
		AddItem(table, 0, 5, true).
		AddItem(statsText, 1, 0, false)

	pages.AddPage("main", mainFlex, true, true)
	pages.AddPage("detail", detailPanel, true, false)

	// Keyboard shortcuts
	state.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		currentPage, _ := pages.GetFrontPage()

		if currentPage == "detail" && event.Key() == tcell.KeyESC {
			pages.HidePage("detail")
			return nil
		}

		if currentPage == "main" {
			switch event.Key() {
			case tcell.KeyF1:
				showFilterDialog(state, pages, updateTable)
				return nil
			case tcell.KeyF5:
				go func() {
					state.loadEvents()
					state.app.QueueUpdateDraw(updateTable)
				}()
				return nil
			case tcell.KeyF9:
				showExportDialog(state, pages)
				return nil
			case tcell.KeyF10:
				// Toggle auto-refresh
				if state.autoRefresh {
					state.autoRefresh = false
					state.stopRefresh <- true
				} else {
					state.autoRefresh = true
					go autoRefreshLoop(state, updateTable)
				}
				updateTable()
				return nil
			case tcell.KeyRune:
				if event.Rune() == '?' {
					showHelpDialog(pages)
					return nil
				}
				if event.Rune() == 'q' || event.Rune() == 'Q' {
					state.app.Stop()
					return nil
				}
			}
		}

		return event
	})

	// Initial table display
	updateTable()
	state.refreshUI = updateTable

	return pages
}

func formatEventDetail(event EventDoc) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("[yellow]Event ID:[-] %s\n\n", event.ID))

	tsIST := event.TSIST
	if tsIST == "" {
		tsIST = event.TS.Format("2006-01-02 15:04:05")
	}
	sb.WriteString(fmt.Sprintf("[cyan]Timestamp:[-] %s (UTC: %s)\n", tsIST, event.TS.Format(time.RFC3339)))

	opName := map[string]string{"i": "INSERT", "u": "UPDATE", "d": "DELETE"}[event.OP]
	sb.WriteString(fmt.Sprintf("[cyan]Operation:[-] [green]%s[-]\n", opName))
	sb.WriteString(fmt.Sprintf("[cyan]Database:[-] %s\n", event.Meta.DB))
	sb.WriteString(fmt.Sprintf("[cyan]Table:[-] %s\n", event.Meta.Tbl))
	sb.WriteString(fmt.Sprintf("[cyan]Primary Key:[-] %v\n\n", event.Meta.PK))

	if event.Src != nil {
		if binlog, ok := event.Src["binlog"].(map[string]interface{}); ok {
			sb.WriteString(fmt.Sprintf("[cyan]Binlog:[-] %v:%v\n", binlog["file"], binlog["pos"]))
		}
		if gtid, ok := event.Src["gtid"].(string); ok {
			sb.WriteString(fmt.Sprintf("[cyan]GTID:[-] %s\n", gtid))
		}
	}

	if len(event.Chg) > 0 {
		sb.WriteString("\n[yellow]Changes:[-]\n")
		cols := make([]string, 0, len(event.Chg))
		for col := range event.Chg {
			cols = append(cols, col)
		}
		sort.Strings(cols)

		for _, col := range cols {
			delta := event.Chg[col]
			fromVal := formatValue(delta.F)
			toVal := formatValue(delta.T)

			if len(fromVal) > 100 {
				fromVal = fromVal[:97] + "..."
			}
			if len(toVal) > 100 {
				toVal = toVal[:97] + "..."
			}

			sb.WriteString(fmt.Sprintf("  [green]%s:[-]\n", col))
			sb.WriteString(fmt.Sprintf("    From: %s\n", fromVal))
			sb.WriteString(fmt.Sprintf("    To:   %s\n\n", toVal))
		}
	}

	return sb.String()
}

func showFilterDialog(state *AppState, pages *tview.Pages, updateCallback func()) {
	form := tview.NewForm()
	form.SetBorder(true).SetTitle(" Set Filters (Ctrl+V to paste) ").SetTitleAlign(tview.AlignCenter)

	// Pre-fill with current values
	dbValue := state.filters.database
	tableValue := state.filters.table
	pkValue := ""
	if state.filters.pk != nil {
		pkValue = fmt.Sprintf("%v", state.filters.pk)
	}
	startDateValue := ""
	if !state.filters.startTime.IsZero() {
		startDateValue = state.filters.startTime.Format("2006-01-02")
	}
	endDateValue := ""
	if !state.filters.endTime.IsZero() {
		endDateValue = state.filters.endTime.Format("2006-01-02")
	}
	limitValue := fmt.Sprintf("%d", state.filters.limit)
	if state.filters.limit == 0 {
		limitValue = "100"
	}

	// Create input fields with paste-friendly settings
	dbField := tview.NewInputField().SetLabel("Database: ").SetText(dbValue).SetFieldWidth(30)
	tableField := tview.NewInputField().SetLabel("Table: ").SetText(tableValue).SetFieldWidth(30)
	pkField := tview.NewInputField().SetLabel("Primary Key: ").SetText(pkValue).SetFieldWidth(30)
	startDateField := tview.NewInputField().SetLabel("Start Date (YYYY-MM-DD): ").SetText(startDateValue).SetFieldWidth(30)
	endDateField := tview.NewInputField().SetLabel("End Date (YYYY-MM-DD): ").SetText(endDateValue).SetFieldWidth(30)
	limitField := tview.NewInputField().SetLabel("Limit: ").SetText(limitValue).SetFieldWidth(10)

	// Add all fields to form
	form.AddFormItem(dbField)
	form.AddFormItem(tableField)
	form.AddFormItem(pkField)
	form.AddFormItem(startDateField)
	form.AddFormItem(endDateField)
	form.AddFormItem(limitField)

	form.AddButton("Apply", func() {
		// Get values from fields
		state.filters.database = dbField.GetText()
		state.filters.table = tableField.GetText()

		pkStr := pkField.GetText()
		if pkStr != "" {
			var pkInt int64
			if _, err := fmt.Sscanf(pkStr, "%d", &pkInt); err == nil {
				state.filters.pk = pkInt
			} else {
				state.filters.pk = pkStr
			}
		} else {
			state.filters.pk = nil
		}

		startDateStr := startDateField.GetText()
		if startDateStr != "" {
			if t, err := time.Parse("2006-01-02", startDateStr); err == nil {
				state.filters.startTime = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
			}
		} else {
			state.filters.startTime = time.Time{}
		}

		endDateStr := endDateField.GetText()
		if endDateStr != "" {
			if t, err := time.Parse("2006-01-02", endDateStr); err == nil {
				state.filters.endTime = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.UTC)
			}
		} else {
			state.filters.endTime = time.Time{}
		}

		limitStr := limitField.GetText()
		limit, _ := strconv.ParseInt(limitStr, 10, 64)
		if limit == 0 {
			limit = 100
		}
		state.filters.limit = limit

		pages.HidePage("filter")
		go func() {
			state.loadEvents()
			state.app.QueueUpdateDraw(updateCallback)
		}()
	})

	form.AddButton("Cancel", func() {
		pages.HidePage("filter")
	})

	pages.AddPage("filter", tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(form, 18, 1, true).
			AddItem(nil, 0, 1, false), 60, 1, true).
		AddItem(nil, 0, 1, false), true, true)
}

func showExportDialog(state *AppState, pages *tview.Pages) {
	if len(state.events) == 0 {
		showMessageDialog(pages, "No events to export")
		return
	}

	form := tview.NewForm()
	form.SetBorder(true).SetTitle(" Export Options ").SetTitleAlign(tview.AlignCenter)

	timestamp := time.Now().Format("20060102_150405")
	csvFilename := fmt.Sprintf("audit_export_%s.csv", timestamp)
	jsonFilename := fmt.Sprintf("audit_export_%s.json", timestamp)

	form.AddButton("Export to CSV", func() {
		pages.HidePage("export")
		go func() {
			if err := exportToCSV(state.events, csvFilename); err != nil {
				showMessageDialog(pages, fmt.Sprintf("Error: %v", err))
			} else {
				showMessageDialog(pages, fmt.Sprintf("Exported %d events to %s", len(state.events), csvFilename))
			}
		}()
	})

	form.AddButton("Export to JSON", func() {
		pages.HidePage("export")
		go func() {
			if err := exportToJSON(state.events, jsonFilename); err != nil {
				showMessageDialog(pages, fmt.Sprintf("Error: %v", err))
			} else {
				showMessageDialog(pages, fmt.Sprintf("Exported %d events to %s", len(state.events), jsonFilename))
			}
		}()
	})

	form.AddButton("Cancel", func() {
		pages.HidePage("export")
	})

	pages.AddPage("export", tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(form, 10, 1, true).
			AddItem(nil, 0, 1, false), 40, 1, true).
		AddItem(nil, 0, 1, false), true, true)
}

func showHelpDialog(pages *tview.Pages) {
	helpText := `[yellow]Keyboard Shortcuts:[-]

[green]F1[-]     - Set filters (Database, Table, PK, Date range)
[green]F5[-]     - Refresh events
[green]F9[-]     - Export to CSV/JSON
[green]F10[-]    - Toggle auto-refresh (every 1 second)
[green]?[-]      - Show this help
[green]q/Q[-]    - Quit application
[green]Enter[-]  - View event details
[green]ESC[-]    - Close detail/dialog view

[yellow]Input Fields:[-]
[green]Ctrl+V[-]  - Paste from clipboard (in filter dialog)
[green]Ctrl+C[-]  - Copy selected text
- Use Shift+Insert as alternative paste

[yellow]Mouse:[-]
- Click to select event
- Double-click to view details
- Scroll to navigate

[yellow]Navigation:[-]
- Arrow keys to navigate table
- Page Up/Down for fast scrolling
- Home/End to jump to top/bottom`

	textView := tview.NewTextView().
		SetDynamicColors(true).
		SetText(helpText).
		SetTextAlign(tview.AlignLeft)

	textView.SetBorder(true).SetTitle(" Help ").SetTitleAlign(tview.AlignCenter)

	textView.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyESC || event.Rune() == 'q' {
			pages.HidePage("help")
		}
		return event
	})

	pages.AddPage("help", tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(textView, 22, 1, true).
			AddItem(nil, 0, 1, false), 70, 1, true).
		AddItem(nil, 0, 1, false), true, true)
}

func showMessageDialog(pages *tview.Pages, message string) {
	modal := tview.NewModal().
		SetText(message).
		AddButtons([]string{"OK"}).
		SetDoneFunc(func(buttonIndex int, buttonLabel string) {
			pages.HidePage("message")
		})

	pages.AddPage("message", modal, true, true)
}

func autoRefreshLoop(state *AppState, updateCallback func()) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if !state.autoRefresh {
				return
			}
			if err := state.loadEvents(); err != nil {
				state.status = fmt.Sprintf("[red]Auto-refresh failed: %v[-]", err)
			}
			state.app.QueueUpdateDraw(updateCallback)
		case <-state.stopRefresh:
			return
		}
	}
}

func main() {
	state := newAppState(nil)
	app := tview.NewApplication().EnableMouse(true)
	state.app = app

	pages := createMainUI(state)

	// Connect to MongoDB in the background so the UI always appears.
	state.status = "Connecting to MongoDB..."
	go func() {
		coll, err := connectMongo()
		if err != nil {
			state.app.QueueUpdateDraw(func() {
				state.status = fmt.Sprintf("[red]MongoDB connect failed: %v[-]", err)
				if state.refreshUI != nil {
					state.refreshUI()
				}
				showMessageDialog(pages, fmt.Sprintf("MongoDB connection failed:\n%v\n\nCheck MONGO_URI/MONGO_DB/MONGO_COLL and ensure MongoDB is running.", err))
			})
			return
		}

		state.coll = coll
		state.status = "Connected. Loading events..."
		state.app.QueueUpdateDraw(func() {
			if state.refreshUI != nil {
				state.refreshUI()
			}
		})

		if err := state.loadEvents(); err != nil {
			state.app.QueueUpdateDraw(func() {
				state.status = fmt.Sprintf("[red]Load error: %v[-]", err)
				if state.refreshUI != nil {
					state.refreshUI()
				}
				showMessageDialog(pages, fmt.Sprintf("Failed to load events:\n%v", err))
			})
			return
		}

		state.app.QueueUpdateDraw(func() {
			state.status = fmt.Sprintf("Connected (%d events)", len(state.events))
			if state.refreshUI != nil {
				state.refreshUI()
			}
		})
	}()

	if err := app.SetRoot(pages, true).SetFocus(pages).Run(); err != nil {
		log.Fatalf("Error running application: %v", err)
	}
}
