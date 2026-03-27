package main

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type config struct {
	DBUser        string
	DBPass        string
	DBHost        string
	DBPort        string
	BackupDir     string
	MySQLBin      string
	MySQLDumpBin  string
	RetryCount    int
	RetentionDays int
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

func loadEnvFile() string {
	// Highest priority: explicit path.
	if explicit := strings.TrimSpace(os.Getenv("BACKUP_ENV_FILE")); explicit != "" {
		if err := godotenv.Load(explicit); err != nil {
			log.Printf("warning: could not load BACKUP_ENV_FILE=%s: %v", explicit, err)
			return ""
		}
		log.Printf("loaded env from %s", explicit)
		return explicit
	}

	// Useful defaults for this repo/project layout.
	candidates := []string{
		".env",
		filepath.Join("sdl_db_backup", ".env"),
	}
	for _, p := range candidates {
		if err := godotenv.Load(p); err == nil {
			log.Printf("loaded env from %s", p)
			return p
		}
	}
	log.Printf("warning: no .env file found in known locations; using process env")
	return ""
}

func loadConfig() (config, error) {
	_ = loadEnvFile()

	dbPass := getenv("DB_PASS", "")
	if dbPass == "" {
		dbPass = getenv("DB_PASSWORD", "")
	}
	if dbPass == "" {
		dbPass = getenv("MYSQL_PASS", "")
	}

	cfg := config{
		DBUser:        getenv("DB_USER", ""),
		DBPass:        dbPass,
		DBHost:        getenv("DB_HOST", "127.0.0.1"),
		DBPort:        getenv("DB_PORT", "3306"),
		BackupDir:     getenv("BACKUP_DIR", "/mnt/volume_1/backup/mysql_backup"),
		MySQLBin:      getenv("MYSQL_BIN", "mysql"),
		MySQLDumpBin:  getenv("MYSQLDUMP_BIN", "mysqldump"),
		RetryCount:    3,
		RetentionDays: 5,
	}
	if cfg.DBUser == "" {
		return cfg, fmt.Errorf("DB_USER is required")
	}
	return cfg, nil
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
		path := filepath.Clean(filepath.Join(backupDir, entry.Name()))
		if path == currentRun {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			log.Printf("warning: could not read backup folder metadata: %s (%v)", path, err)
			continue
		}
		if info.ModTime().Before(cutoff) {
			log.Printf("deleting old backup folder: %s", path)
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("delete old backup %s: %w", path, err)
			}
		}
	}
	return nil
}

func mysqlCmd(cfg config, bin string, args ...string) *exec.Cmd {
	base := []string{"-h", cfg.DBHost, "-P", cfg.DBPort, "-u", cfg.DBUser}
	all := append(base, args...)
	cmd := exec.Command(bin, all...)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+cfg.DBPass)
	return cmd
}

func listDatabases(cfg config) ([]string, error) {
	log.Printf("discovering databases with %s", cfg.MySQLBin)
	cmd := mysqlCmd(cfg, cfg.MySQLBin, "-N", "-e", "SHOW DATABASES")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			msg := strings.TrimSpace(string(ee.Stderr))
			if msg == "" {
				msg = ee.Error()
			}
			return nil, fmt.Errorf("mysql failed: %s", msg)
		}
		return nil, err
	}

	var databases []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		db := strings.TrimSpace(sc.Text())
		if db == "" || slices.Contains(systemDBs, db) {
			continue
		}
		databases = append(databases, db)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return databases, nil
}

func dumpDatabase(cfg config, dbName, outFile string) error {
	log.Printf("starting dump for database=%s", dbName)

	file, err := os.Create(outFile)
	if err != nil {
		return err
	}
	defer file.Close()

	gz := gzip.NewWriter(file)
	defer gz.Close()

	args := []string{
		"--single-transaction",
		"--quick",
		"--routines",
		"--triggers",
		"--events",
		"--set-gtid-purged=OFF",
		"--databases", dbName,
	}
	cmd := mysqlCmd(cfg, cfg.MySQLDumpBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return err
	}

	if _, err := io.Copy(gz, stdout); err != nil {
		_ = cmd.Process.Kill()
		_ = os.Remove(outFile)
		return fmt.Errorf("stream mysqldump output: %w", err)
	}

	if err := gz.Close(); err != nil {
		_ = os.Remove(outFile)
		return fmt.Errorf("finalize gzip: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		_ = os.Remove(outFile)
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("mysqldump failed for %s: %s", dbName, msg)
		}
		return fmt.Errorf("mysqldump failed for %s: %w", dbName, err)
	}

	log.Printf("completed dump for database=%s output=%s", dbName, outFile)
	return nil
}

func dumpWithRetry(cfg config, dbName, outFile string) error {
	var lastErr error
	for attempt := 1; attempt <= cfg.RetryCount; attempt++ {
		if attempt > 1 {
			sleepFor := time.Duration(attempt*2) * time.Second
			log.Printf("retrying database=%s in %s (attempt %d/%d)", dbName, sleepFor, attempt, cfg.RetryCount)
			time.Sleep(sleepFor)
		}

		err := dumpDatabase(cfg, dbName, outFile)
		if err == nil {
			return nil
		}
		lastErr = err
		log.Printf("attempt %d/%d failed for database=%s: %v", attempt, cfg.RetryCount, dbName, err)
	}
	return fmt.Errorf("all %d attempts failed for database=%s: %w", cfg.RetryCount, dbName, lastErr)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("mysql full backup started")

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	log.Printf("using mysql target user=%s host=%s port=%s backup_dir=%s", cfg.DBUser, cfg.DBHost, cfg.DBPort, cfg.BackupDir)

	dbs, err := listDatabases(cfg)
	if err != nil {
		log.Fatalf("failed to list databases: %v", err)
	}
	if len(dbs) == 0 {
		log.Println("no user databases found; exiting")
		return
	}
	log.Printf("found %d databases: %s", len(dbs), strings.Join(dbs, ", "))

	runFolder := filepath.Join(cfg.BackupDir, time.Now().Format("2006-01-02_15-04-05"))
	if err := os.MkdirAll(runFolder, 0o755); err != nil {
		log.Fatalf("failed to create backup folder %s: %v", runFolder, err)
	}
	log.Printf("backup folder: %s", runFolder)

	start := time.Now()
	for i, db := range dbs {
		outFile := filepath.Join(runFolder, db+".sql.gz")
		log.Printf("[%d/%d] processing %s", i+1, len(dbs), db)
		if err := dumpWithRetry(cfg, db, outFile); err != nil {
			log.Fatalf("backup failed: %v", err)
		}
	}

	if err := cleanupOldBackups(cfg.BackupDir, runFolder, cfg.RetentionDays); err != nil {
		log.Fatalf("backup succeeded but cleanup failed: %v", err)
	}

	log.Printf("mysql full backup completed successfully in %s", time.Since(start).Round(time.Millisecond))
}
