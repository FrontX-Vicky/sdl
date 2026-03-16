# SDL Documentation

| File | Contents |
|------|----------|
| [getting-started.md](getting-started.md) | Prerequisites, `.env` setup, build, quick start |
| [commands.md](commands.md) | CLI reference for `sql_import` and `sdl_restore` |
| [fetch.md](fetch.md) | `sdl_fetch` TUI usage, shortcuts, examples |
| [architecture.md](architecture.md) | System design, event structure, failure handling |
| [operations.md](operations.md) | Production deployment, MySQL/MongoDB config, monitoring |
| [performance.md](performance.md) | MongoDB indexes, query tuning, TUI fetch tool |

## System Components

| Binary | Source | Role |
|--------|--------|------|
| `sdl_binary` | `main.go` | Binlog listener — captures MySQL changes → MongoDB |
| `sdl_view` | `view.go` | Live-tail audit events in the terminal |
| `sdl_fetch` | `sdl_fetch/fetch.go` | TUI query tool for audit logs |
| `sdl_restore` | `sdl_restore/restore.go` | Replay MongoDB audit events back to MySQL |
| `sql_import` | `sdl_import/sql_import.go` | Import a phpMyAdmin `.sql` dump into MySQL via upserts |
