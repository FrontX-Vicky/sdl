#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${BACKUP_ENV_FILE:-$SCRIPT_DIR/.env}"

if [[ -f "$ENV_FILE" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "$ENV_FILE"
  set +a
fi

DB_USER="${DB_USER:-}"
DB_PASS="${DB_PASS:-}"
DB_HOST="${DB_HOST:-127.0.0.1}"
DB_PORT="${DB_PORT:-3306}"
BACKUP_DIR="${BACKUP_DIR:-/mnt/volume_1/backup/mysql_backup}"
MYSQL_BIN="${MYSQL_BIN:-mysql}"
MYSQLDUMP_BIN="${MYSQLDUMP_BIN:-mysqldump}"

if [[ -z "$DB_USER" ]]; then
  echo "ERROR: DB_USER is required (set it in $ENV_FILE)" >&2
  exit 1
fi

RUN_TS="$(date +%F_%H-%M-%S)"
RUN_DIR="${BACKUP_DIR}/${RUN_TS}"
mkdir -p "$RUN_DIR"

TMP_CNF="$(mktemp)"
cleanup() {
  rm -f "$TMP_CNF"
}
trap cleanup EXIT

chmod 600 "$TMP_CNF"
cat > "$TMP_CNF" <<EOF
[client]
user=$DB_USER
password=$DB_PASS
host=$DB_HOST
port=$DB_PORT
EOF

mapfile -t DATABASES < <(
  "$MYSQL_BIN" --defaults-extra-file="$TMP_CNF" -N -e "SHOW DATABASES" \
    | grep -Ev '^(information_schema|performance_schema|mysql|sys)$' || true
)

if [[ "${#DATABASES[@]}" -eq 0 ]]; then
  echo "No user databases found at $(date '+%F %T')."
  exit 0
fi

for DB in "${DATABASES[@]}"; do
  OUT_FILE="${RUN_DIR}/${DB}.sql.gz"
  "$MYSQLDUMP_BIN" --defaults-extra-file="$TMP_CNF" \
    --single-transaction \
    --quick \
    --routines \
    --triggers \
    --events \
    --set-gtid-purged=OFF \
    --databases "$DB" \
    | gzip -c > "$OUT_FILE"
  echo "Backed up ${DB} -> ${OUT_FILE}"
done

echo "Backup completed at $(date '+%F %T'). Files: ${RUN_DIR}"
