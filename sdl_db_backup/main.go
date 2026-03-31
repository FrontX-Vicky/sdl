package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

const runTimestampLayout = "2006-01-02_15-04-05"

type config struct {
	DBUser           string
	DBPass           string
	DBHost           string
	DBPort           string
	BackupDir        string
	LogDir           string
	RunLogPath       string
	LockFile         string
	MySQLBin         string
	MySQLDumpBin     string
	RetryCount       int
	RetentionDays    int
	TimeoutPerDB     time.Duration
	DiscoveryTimeout time.Duration
	PreflightTimeout time.Duration
	RetryBaseDelay   time.Duration
	RetryMaxDelay    time.Duration
	CleanupFailFatal bool
}

type databaseResult struct {
	Name          string `json:"name"`
	Status        string `json:"status"`
	Attempts      int    `json:"attempts"`
	Duration      string `json:"duration"`
	OutputPath    string `json:"output_path,omitempty"`
	SizeBytes     int64  `json:"size_bytes,omitempty"`
	ErrorCategory string `json:"error_category,omitempty"`
	Error         string `json:"error,omitempty"`
}

type runRecord struct {
	Timestamp          time.Time        `json:"timestamp"`
	RunID              string           `json:"run_id"`
	Status             string           `json:"status"`
	BackupDir          string           `json:"backup_dir"`
	RunFolder          string           `json:"run_folder,omitempty"`
	LogFile            string           `json:"log_file,omitempty"`
	FailureReason      string           `json:"failure_reason,omitempty"`
	CleanupError       string           `json:"cleanup_error,omitempty"`
	Duration           string           `json:"duration"`
	DatabasesTotal     int              `json:"databases_total"`
	DatabasesSucceeded int              `json:"databases_succeeded"`
	DatabasesFailed    int              `json:"databases_failed"`
	Results            []databaseResult `json:"results,omitempty"`
}

type countingWriter struct {
	writer io.Writer
	bytes  int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.writer.Write(p)
	cw.bytes += int64(n)
	return n, err
}

var systemDBs = []string{
	"information_schema",
	"performance_schema",
	"mysql",
	"sys",
}

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		log.Printf("warning: invalid integer for %s=%q; using %d", key, raw, fallback)
		return fallback
	}
	return value
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		log.Printf("warning: invalid duration for %s=%q; using %s", key, raw, fallback)
		return fallback
	}
	return value
}

func getenvBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		log.Printf("warning: invalid boolean for %s=%q; using %t", key, raw, fallback)
		return fallback
	}
	return value
}

func loadEnvFile() string {
	if explicit := strings.TrimSpace(os.Getenv("BACKUP_ENV_FILE")); explicit != "" {
		if err := godotenv.Load(explicit); err != nil {
			log.Printf("warning: could not load BACKUP_ENV_FILE=%s: %v", explicit, err)
			return ""
		}
		log.Printf("loaded env from %s", explicit)
		return explicit
	}

	candidates := []string{
		".env",
		filepath.Join("sdl_db_backup", ".env"),
	}
	for _, path := range candidates {
		if err := godotenv.Load(path); err == nil {
			log.Printf("loaded env from %s", path)
			return path
		}
	}

	log.Printf("warning: no .env file found in known locations; using process env")
	return ""
}

func loadConfig() config {
	_ = loadEnvFile()

	dbPass := getenv("DB_PASS", "")
	if dbPass == "" {
		dbPass = getenv("DB_PASSWORD", "")
	}
	if dbPass == "" {
		dbPass = getenv("MYSQL_PASS", "")
	}

	backupDir := getenv("BACKUP_DIR", "/mnt/volume_1/backup/mysql_backup")
	logDir := getenv("BACKUP_LOG_DIR", filepath.Join(backupDir, "logs"))

	return config{
		DBUser:           getenv("DB_USER", ""),
		DBPass:           dbPass,
		DBHost:           getenv("DB_HOST", "127.0.0.1"),
		DBPort:           getenv("DB_PORT", "3306"),
		BackupDir:        backupDir,
		LogDir:           logDir,
		RunLogPath:       filepath.Join(logDir, "backup-runs.jsonl"),
		LockFile:         getenv("BACKUP_LOCK_FILE", filepath.Join(logDir, "backup.lock")),
		MySQLBin:         getenv("MYSQL_BIN", "mysql"),
		MySQLDumpBin:     getenv("MYSQLDUMP_BIN", "mysqldump"),
		RetryCount:       getenvInt("BACKUP_RETRY_COUNT", 3),
		RetentionDays:    getenvInt("BACKUP_RETENTION_DAYS", 5),
		TimeoutPerDB:     getenvDuration("BACKUP_TIMEOUT_PER_DB", 30*time.Minute),
		DiscoveryTimeout: getenvDuration("BACKUP_DISCOVERY_TIMEOUT", 30*time.Second),
		PreflightTimeout: getenvDuration("BACKUP_PREFLIGHT_TIMEOUT", 15*time.Second),
		RetryBaseDelay:   getenvDuration("BACKUP_RETRY_BASE_DELAY", 2*time.Second),
		RetryMaxDelay:    getenvDuration("BACKUP_RETRY_MAX_DELAY", 20*time.Second),
		CleanupFailFatal: getenvBool("BACKUP_CLEANUP_FAIL_FATAL", false),
	}
}

func initRunLogger(logDir, runID string) (*os.File, string, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("create log dir %s: %w", logDir, err)
	}
	logFilePath := filepath.Join(logDir, runID+".log")
	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, "", fmt.Errorf("open run log %s: %w", logFilePath, err)
	}
	log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	return logFile, logFilePath, nil
}

func acquireLock(lockFile string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(lockFile), 0o755); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}

	file, err := os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create lock file %s: %w", lockFile, err)
		}
		// Lock file exists — check whether the owning process is still alive.
		state, _ := os.ReadFile(lockFile)
		if stale, pid := isLockStale(string(state)); stale {
			log.Printf("removing stale lock file (pid=%d is no longer running)", pid)
			if rmErr := os.Remove(lockFile); rmErr != nil && !os.IsNotExist(rmErr) {
				return nil, fmt.Errorf("remove stale lock: %w", rmErr)
			}
			file, err = os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
			if err != nil {
				return nil, fmt.Errorf("create lock file after stale removal: %w", err)
			}
		} else {
			info := strings.TrimSpace(string(state))
			if info != "" {
				return nil, fmt.Errorf("another backup run is already active: %s", info)
			}
			return nil, errors.New("another backup run is already active")
		}
	}

	_, _ = fmt.Fprintf(file, "pid=%d started=%s", os.Getpid(), time.Now().Format(time.RFC3339))
	_ = file.Close()

	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = os.Remove(lockFile)
	}, nil
}

// isLockStale parses the PID from a lock file and returns true if that
// process is no longer running. Also returns the parsed PID for logging.
func isLockStale(content string) (bool, int) {
	for _, field := range strings.Fields(content) {
		after, ok := strings.CutPrefix(field, "pid=")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(after)
		if err != nil || pid <= 0 {
			return true, 0 // malformed — treat as stale
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			return true, pid
		}
		// Signal 0 tests process existence without delivering a real signal.
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return true, pid // process is dead
		}
		return false, pid // process is alive
	}
	return true, 0 // no PID found — treat as stale
}

func validateWritableDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("create dir %s: %w", path, err)
	}
	tempFile, err := os.CreateTemp(path, ".sdl-backup-check-*")
	if err != nil {
		return fmt.Errorf("write test in %s: %w", path, err)
	}
	name := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close write test file %s: %w", name, err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("remove write test file %s: %w", name, err)
	}
	return nil
}

func validatePrerequisites(cfg config) error {
	if cfg.DBUser == "" {
		return errors.New("DB_USER is required")
	}
	if err := validateWritableDir(cfg.BackupDir); err != nil {
		return err
	}
	if err := validateWritableDir(cfg.LogDir); err != nil {
		return err
	}
	if _, err := exec.LookPath(cfg.MySQLBin); err != nil {
		return fmt.Errorf("mysql binary %q not found in PATH: %w", cfg.MySQLBin, err)
	}
	if _, err := exec.LookPath(cfg.MySQLDumpBin); err != nil {
		return fmt.Errorf("mysqldump binary %q not found in PATH: %w", cfg.MySQLDumpBin, err)
	}
	if err := pingMySQL(cfg); err != nil {
		return err
	}
	return nil
}

func pingMySQL(cfg config) error {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.PreflightTimeout)
	defer cancel()

	cmd := mysqlCmdContext(ctx, cfg, cfg.MySQLBin, "-N", "-e", "SELECT 1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		category := classifyFailure(err, message)
		return fmt.Errorf("mysql connectivity check failed (%s): %s", category, chooseFailureMessage(err, message))
	}
	return nil
}

func cleanupOldBackups(backupDir, currentRun string, retentionDays int) error {
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return err
	}

	currentRun = filepath.Clean(currentRun)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		runTime, err := time.Parse(runTimestampLayout, entry.Name())
		if err != nil {
			// Not a timestamped backup folder (e.g. logs/); skip.
			continue
		}

		path := filepath.Clean(filepath.Join(backupDir, entry.Name()))
		if currentRun != "" && path == currentRun {
			continue
		}
		if runTime.Before(cutoff) {
			log.Printf("deleting old backup folder: %s", path)
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("delete old backup %s: %w", path, err)
			}
		}
	}
	return nil
}

func mysqlCmdContext(ctx context.Context, cfg config, bin string, args ...string) *exec.Cmd {
	base := []string{"-h", cfg.DBHost, "-P", cfg.DBPort, "-u", cfg.DBUser}
	all := append(base, args...)
	cmd := exec.CommandContext(ctx, bin, all...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+cfg.DBPass)
	return cmd
}

func listDatabases(cfg config) ([]string, error) {
	log.Printf("discovering databases with %s", cfg.MySQLBin)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.DiscoveryTimeout)
	defer cancel()

	cmd := mysqlCmdContext(ctx, cfg, cfg.MySQLBin, "-N", "-e", "SHOW DATABASES")
	out, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		return nil, fmt.Errorf("mysql database discovery failed (%s): %s", classifyFailure(err, message), chooseFailureMessage(err, message))
	}

	var databases []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		dbName := strings.TrimSpace(scanner.Text())
		if dbName == "" || slices.Contains(systemDBs, dbName) {
			continue
		}
		databases = append(databases, dbName)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return databases, nil
}

func removePartialOutput(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: could not remove partial backup %s: %v", path, err)
	}
}

func dumpDatabase(cfg config, dbName, outFile string) (int64, error) {
	log.Printf("starting dump for database=%s", dbName)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.TimeoutPerDB)
	defer cancel()

	file, err := os.OpenFile(outFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return 0, err
	}

	closeFile := func() {
		if closeErr := file.Close(); closeErr != nil {
			log.Printf("warning: could not close output file %s: %v", outFile, closeErr)
		}
	}

	counter := &countingWriter{writer: file}
	gz := gzip.NewWriter(counter)

	args := []string{
		"--single-transaction",
		"--quick",
		"--routines",
		"--triggers",
		"--events",
		"--set-gtid-purged=OFF",
		"--databases", dbName,
	}
	cmd := mysqlCmdContext(ctx, cfg, cfg.MySQLDumpBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		closeFile()
		removePartialOutput(outFile)
		return 0, err
	}

	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		closeFile()
		removePartialOutput(outFile)
		return 0, err
	}

	if _, err := io.Copy(gz, stdout); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = gz.Close()
		closeFile()
		removePartialOutput(outFile)
		return 0, fmt.Errorf("stream mysqldump output: %w", err)
	}

	if err := gz.Close(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		closeFile()
		removePartialOutput(outFile)
		return 0, fmt.Errorf("finalize gzip: %w", err)
	}

	if err := file.Close(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		removePartialOutput(outFile)
		return 0, fmt.Errorf("close gzip file: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		removePartialOutput(outFile)
		message := strings.TrimSpace(stderr.String())
		return 0, fmt.Errorf("mysqldump failed (%s): %s", classifyFailure(err, message), chooseFailureMessage(err, message))
	}

	log.Printf("completed dump for database=%s output=%s size_bytes=%d", dbName, outFile, counter.bytes)
	return counter.bytes, nil
}

func dumpWithRetry(cfg config, dbName, outFile string) databaseResult {
	started := time.Now()
	result := databaseResult{Name: dbName, Status: "failed"}

	for attempt := 1; attempt <= cfg.RetryCount; attempt++ {
		result.Attempts = attempt
		sizeBytes, err := dumpDatabase(cfg, dbName, outFile)
		if err == nil {
			result.Status = "success"
			result.OutputPath = outFile
			result.SizeBytes = sizeBytes
			result.Duration = time.Since(started).Round(time.Millisecond).String()
			return result
		}

		result.Error = err.Error()
		result.ErrorCategory = classifyFailure(err, err.Error())
		log.Printf("attempt %d/%d failed for database=%s category=%s err=%v", attempt, cfg.RetryCount, dbName, result.ErrorCategory, err)

		if attempt == cfg.RetryCount || !shouldRetry(result.ErrorCategory) {
			break
		}

		delay := retryDelay(cfg, attempt)
		log.Printf("retrying database=%s in %s (attempt %d/%d)", dbName, delay, attempt+1, cfg.RetryCount)
		time.Sleep(delay)
	}

	result.Duration = time.Since(started).Round(time.Millisecond).String()
	return result
}

func retryDelay(cfg config, attempt int) time.Duration {
	delay := cfg.RetryBaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= cfg.RetryMaxDelay {
			return cfg.RetryMaxDelay
		}
	}
	if delay > cfg.RetryMaxDelay {
		return cfg.RetryMaxDelay
	}
	return delay
}

func shouldRetry(category string) bool {
	switch category {
	case "auth", "permission", "disk", "view", "config":
		return false
	default:
		return true
	}
}

func classifyFailure(err error, detail string) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	text := strings.ToLower(strings.TrimSpace(detail + " " + err.Error()))
	switch {
	case strings.Contains(text, "access denied"):
		return "auth"
	case strings.Contains(text, "permission denied"):
		return "permission"
	case strings.Contains(text, "no space left on device"), strings.Contains(text, "disk full"):
		return "disk"
	case strings.Contains(text, "references invalid table"), strings.Contains(text, "1356"):
		return "view"
	case strings.Contains(text, "can't connect"), strings.Contains(text, "connection refused"),
		strings.Contains(text, "server has gone away"), strings.Contains(text, "connection reset"),
		strings.Contains(text, "no route to host"):
		return "connect"
	case strings.Contains(text, "deadline exceeded"), strings.Contains(text, "timed out"),
		strings.Contains(text, "error 3024"), strings.Contains(text, "max_execution_time"):
		return "timeout"
	case strings.Contains(text, "unknown variable"), strings.Contains(text, "unknown option"):
		return "config"
	case strings.Contains(text, "executable file not found"):
		return "binary"
	default:
		return "command"
	}
}

func chooseFailureMessage(err error, detail string) string {
	trimmed := strings.TrimSpace(detail)
	if trimmed != "" {
		return trimmed
	}
	return err.Error()
}

func appendRunRecord(path string, record runRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer file.Close()

	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return nil
}

func writeManifest(runFolder string, record runRecord) error {
	manifestPath := filepath.Join(runFolder, "manifest.json")
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, append(encoded, '\n'), 0o640)
}

func finalizeRun(cfg config, record *runRecord, startedAt time.Time) int {
	if record.Duration == "" {
		record.Duration = time.Since(startedAt).Round(time.Millisecond).String()
	}

	if record.RunFolder != "" {
		if err := writeManifest(record.RunFolder, *record); err != nil {
			log.Printf("warning: failed to write manifest for run=%s: %v", record.RunID, err)
		}
	}
	if err := appendRunRecord(cfg.RunLogPath, *record); err != nil {
		log.Printf("warning: failed to append run record %s: %v", cfg.RunLogPath, err)
	}

	log.Printf(
		"backup summary status=%s total=%d success=%d failed=%d duration=%s",
		record.Status,
		record.DatabasesTotal,
		record.DatabasesSucceeded,
		record.DatabasesFailed,
		record.Duration,
	)
	if record.FailureReason != "" {
		log.Printf("backup failure reason: %s", record.FailureReason)
	}
	if record.CleanupError != "" {
		log.Printf("backup cleanup issue: %s", record.CleanupError)
	}

	if record.Status == "success" {
		return 0
	}
	return 1
}

func run() int {
	startedAt := time.Now()
	runID := startedAt.Format(runTimestampLayout)
	cfg := loadConfig()
	record := runRecord{
		Timestamp: time.Now().UTC(),
		RunID:     runID,
		Status:    "failed",
		BackupDir: cfg.BackupDir,
	}

	logFile, logFilePath, err := initRunLogger(cfg.LogDir, runID)
	if err != nil {
		log.Printf("warning: persistent run logging is unavailable: %v", err)
	} else {
		defer logFile.Close()
		record.LogFile = logFilePath
	}

	log.Println("mysql full backup started")
	log.Printf(
		"using mysql target user=%s host=%s port=%s backup_dir=%s log_dir=%s",
		cfg.DBUser,
		cfg.DBHost,
		cfg.DBPort,
		cfg.BackupDir,
		cfg.LogDir,
	)

	releaseLock, err := acquireLock(cfg.LockFile)
	if err != nil {
		record.FailureReason = err.Error()
		return finalizeRun(cfg, &record, startedAt)
	}
	defer releaseLock()

	if err := validatePrerequisites(cfg); err != nil {
		record.FailureReason = err.Error()
		return finalizeRun(cfg, &record, startedAt)
	}

	databases, err := listDatabases(cfg)
	if err != nil {
		record.FailureReason = err.Error()
		return finalizeRun(cfg, &record, startedAt)
	}
	record.DatabasesTotal = len(databases)

	if len(databases) == 0 {
		cleanupErr := cleanupOldBackups(cfg.BackupDir, "", cfg.RetentionDays)
		if cleanupErr != nil {
			record.CleanupError = cleanupErr.Error()
			if cfg.CleanupFailFatal {
				record.FailureReason = cleanupErr.Error()
			} else {
				log.Printf("cleanup warning with no databases to back up: %v", cleanupErr)
			}
		}
		if record.FailureReason == "" {
			record.Status = "success"
		}
		log.Println("no user databases found; exiting")
		return finalizeRun(cfg, &record, startedAt)
	}

	log.Printf("found %d databases: %s", len(databases), strings.Join(databases, ", "))

	runFolder := filepath.Join(cfg.BackupDir, runID)
	if err := os.MkdirAll(runFolder, 0o755); err != nil {
		record.FailureReason = fmt.Sprintf("failed to create backup folder %s: %v", runFolder, err)
		return finalizeRun(cfg, &record, startedAt)
	}
	record.RunFolder = runFolder
	log.Printf("backup folder: %s", runFolder)

	for index, dbName := range databases {
		outputPath := filepath.Join(runFolder, dbName+".sql.gz")
		log.Printf("[%d/%d] processing %s", index+1, len(databases), dbName)
		result := dumpWithRetry(cfg, dbName, outputPath)
		record.Results = append(record.Results, result)
		if result.Status == "success" {
			record.DatabasesSucceeded++
			continue
		}
		record.DatabasesFailed++
	}

	cleanupErr := cleanupOldBackups(cfg.BackupDir, runFolder, cfg.RetentionDays)
	if cleanupErr != nil {
		record.CleanupError = cleanupErr.Error()
		if cfg.CleanupFailFatal {
			record.FailureReason = cleanupErr.Error()
		} else {
			log.Printf("cleanup warning: %v", cleanupErr)
		}
	}

	switch {
	case record.DatabasesFailed == 0 && record.FailureReason == "":
		record.Status = "success"
	case record.DatabasesSucceeded > 0:
		record.Status = "partial"
	default:
		record.Status = "failed"
	}
	if record.DatabasesFailed > 0 && record.FailureReason == "" {
		record.FailureReason = fmt.Sprintf("%d of %d database backups failed", record.DatabasesFailed, record.DatabasesTotal)
	}

	return finalizeRun(cfg, &record, startedAt)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	os.Exit(run())
}
