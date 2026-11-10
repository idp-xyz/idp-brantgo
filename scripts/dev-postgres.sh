#!/usr/bin/env bash
# Starts Brant's development PostgreSQL 16: container brant-pg16 on 127.0.0.1:55471, roles
# brant_migrator and brant_app, migrations and application grants. The passwords are for this local
# container only. Prints the DSNs the tests and processes read.
#
# The image is PostgreSQL 16.12 by the digest CI pins (postgres:16.12-alpine on Docker Hub); the
# default pulls the same digest from the ECR Public mirror. Set BRANT_PG_IMAGE to use another source.
set -euo pipefail

IMAGE="${BRANT_PG_IMAGE:-public.ecr.aws/docker/library/postgres@sha256:97ff59a4e30e08d1c11bdcd9455e7832368c0572b576c9092cde2df4ae5552a3}"
NAME="${BRANT_PG_CONTAINER:-brant-pg16}"
PORT="${BRANT_PG_PORT:-55471}"
DB=brant
cd "$(dirname "$0")/.."

if ! docker inspect "$NAME" >/dev/null 2>&1; then
  docker run -d --name "$NAME" -p "127.0.0.1:${PORT}:5432" \
    -e POSTGRES_PASSWORD=dev-superuser-only -e POSTGRES_DB="$DB" \
    -v "${NAME}-data:/var/lib/postgresql/data" "$IMAGE" >/dev/null
elif [ "$(docker inspect -f '{{.State.Running}}' "$NAME")" != true ]; then
  docker start "$NAME" >/dev/null
fi

# The image's init server listens on the socket only, so a TCP answer means the final server is up.
for _ in $(seq 1 60); do
  docker exec "$NAME" pg_isready -h 127.0.0.1 -U postgres -d "$DB" -q && break
  sleep 1
done
docker exec "$NAME" pg_isready -h 127.0.0.1 -U postgres -d "$DB"

docker exec -i "$NAME" psql -h 127.0.0.1 -U postgres -d "$DB" -q -v ON_ERROR_STOP=1 -v db="$DB" \
  -v migrator_password=dev-migrator-only -v app_password=dev-app-only < deploy/postgres/00_roles.sql

export IDP_BRANT_MIGRATOR_DSN="postgres://brant_migrator:dev-migrator-only@127.0.0.1:${PORT}/${DB}?sslmode=disable"
go run ./cmd/brant-migrate -apply
docker exec -i -e PGPASSWORD=dev-migrator-only "$NAME" \
  psql -h 127.0.0.1 -U brant_migrator -d "$DB" -q -v ON_ERROR_STOP=1 < deploy/postgres/10_app_grants.sql

cat <<EOF
export IDP_BRANT_MIGRATOR_DSN='${IDP_BRANT_MIGRATOR_DSN}'
export IDP_BRANT_POSTGRES_DSN='postgres://brant_app:dev-app-only@127.0.0.1:${PORT}/${DB}?sslmode=disable'
EOF
