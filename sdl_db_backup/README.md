# SDL DB Backup

Creates full MySQL backups per database, twice daily (07:30 and 14:00), using a separate user-level `systemd` service/timer.

Behavior:
- Retries each database dump up to 3 times before failing the run.
- Deletes backup folders older than 5 days only after the current run finishes successfully.

## Files

- `main.go`: Go backup runner (recommended for testing)
- `mysql_full_backup.sh`: bash backup script
- `.env.example`: environment template
- `sdl-db-backup.service`: user service unit
- `sdl-db-backup.timer`: user timer unit

## Setup (No Root)

1. Create environment file:
   - `cp sdl_db_backup/.env.example sdl_db_backup/.env`
   - Set `DB_USER`, `DB_PASS`, `DB_HOST`, `DB_PORT`, `BACKUP_DIR`

2. For Go testing (logs progress in terminal):
   - `go run ./sdl_db_backup`

3. Ensure `go` is available to the service user:
   - `which go`

4. Install user units:
   - `mkdir -p ~/.config/systemd/user`
   - `cp sdl_db_backup/sdl-db-backup.service ~/.config/systemd/user/`
   - `cp sdl_db_backup/sdl-db-backup.timer ~/.config/systemd/user/`

5. Reload and enable timer:
   - `systemctl --user daemon-reload`
   - `systemctl --user enable --now sdl-db-backup.timer`

6. Verify:
   - `systemctl --user list-timers | grep sdl-db-backup`
   - `systemctl --user status sdl-db-backup.timer`
   - `journalctl --user -u sdl-db-backup.service -n 100 --no-pager`

## Output Layout

Each run creates a timestamped folder and stores one file per database:

- `<BACKUP_DIR>/<YYYY-MM-DD_HH-MM-SS>/<db_name>.sql.gz`

## Manual Test

- Go: `go run ./sdl_db_backup`
- Bash: `bash sdl_db_backup/mysql_full_backup.sh`
