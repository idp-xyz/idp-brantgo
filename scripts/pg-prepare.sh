#!/usr/bin/env bash
# Prepares one Brant database on a running PostgreSQL 16: creates the database if it is missing,
# runs deploy/postgres/00_roles.sql in it as the superuser, migrates it as brant_migrator with
# cmd/brant-migrate twice (the second run must apply nothing: migrations are idempotent), and runs
# deploy/postgres/10_app_grants.sql as brant_migrator. Every database the tests use is prepared by
# this script: CI's jobs (their service container), make db (scripts/dev-postgres.sh) and
# make integration/race SHARDS=N (scripts/test-shards.sh, one database per shard). psql runs inside
# the server's container, so the host needs docker and go, not a PostgreSQL client.
#
#   BRANT_PG_CONTAINER           the server's container (name or ID); required
#   BRANT_PG_PORT                the host port it publishes on 127.0.0.1 (default 55471)
#   BRANT_PG_DB                  the database (default brant)
#   BRANT_PG_SUPERUSER_PASSWORD  default dev-superuser-only  } the development containers' passwords;
#   BRANT_PG_MIGRATOR_PASSWORD   default dev-migrator-only   } CI passes its own
#   BRANT_PG_APP_PASSWORD        default dev-app-only        }
#
# The last two lines of the output are the DSN exports the tests read.
set -euo pipefail

NAME="${BRANT_PG_CONTAINER:?BRANT_PG_CONTAINER names the container the server runs in}"
PORT="${BRANT_PG_PORT:-55471}"
DB="${BRANT_PG_DB:-brant}"
SUPERUSER_PASSWORD="${BRANT_PG_SUPERUSER_PASSWORD:-dev-superuser-only}"
MIGRATOR_PASSWORD="${BRANT_PG_MIGRATOR_PASSWORD:-dev-migrator-only}"
APP_PASSWORD="${BRANT_PG_APP_PASSWORD:-dev-app-only}"
cd "$(dirname "$0")/.."

case "$DB" in
  '' | [!a-z]* | *[!a-z0-9_]*)
    echo "pg-prepare: BRANT_PG_DB must be lower-case letters, digits and _, got '${DB}'" >&2
    exit 2 ;;
esac

# psql_as <role> <password> <database> [psql arguments...] runs psql in the container on stdin.
psql_as() {
  local role=$1 password=$2 db=$3
  shift 3
  docker exec -i -e PGPASSWORD="$password" "$NAME" \
    psql -h 127.0.0.1 -U "$role" -d "$db" -q -v ON_ERROR_STOP=1 "$@"
}

psql_as postgres "$SUPERUSER_PASSWORD" postgres -v db="$DB" <<'SQL'
SELECT format('CREATE DATABASE %I', :'db')
WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = :'db') \gexec
SQL
psql_as postgres "$SUPERUSER_PASSWORD" "$DB" -v db="$DB" \
  -v migrator_password="$MIGRATOR_PASSWORD" -v app_password="$APP_PASSWORD" < deploy/postgres/00_roles.sql

bin="$(mktemp -d)"
trap 'rm -rf "$bin"' EXIT
go build -o "$bin/brant-migrate" ./cmd/brant-migrate
export IDP_BRANT_MIGRATOR_DSN="postgres://brant_migrator:${MIGRATOR_PASSWORD}@127.0.0.1:${PORT}/${DB}?sslmode=disable"
"$bin/brant-migrate" -apply
second="$("$bin/brant-migrate" -apply)"
printf '%s\n' "$second"
if ! grep -q '^DONE: 0 applied' <<<"$second"; then
  echo "pg-prepare: the second migration run of ${DB} applied migrations; they must be idempotent" >&2
  exit 1
fi

psql_as brant_migrator "$MIGRATOR_PASSWORD" "$DB" < deploy/postgres/10_app_grants.sql

cat <<EOF
export IDP_BRANT_MIGRATOR_DSN='${IDP_BRANT_MIGRATOR_DSN}'
export IDP_BRANT_POSTGRES_DSN='postgres://brant_app:${APP_PASSWORD}@127.0.0.1:${PORT}/${DB}?sslmode=disable'
EOF
