#!/usr/bin/env bash
# Starts Brant's development PostgreSQL 16: container brant-pg16 on 127.0.0.1:55471, and prepares
# its database brant with scripts/pg-prepare.sh (roles brant_migrator and brant_app, migrations,
# application grants). The passwords are for this local container only. Prints the DSNs the tests
# and processes read.
#
# The image is PostgreSQL 16.12 by the digest CI pins (postgres:16.12-alpine on Docker Hub); the
# default pulls the same digest from the ECR Public mirror. Set BRANT_PG_IMAGE to use another source.
#
# BRANT_PG_WAL_ARCHIVE=1 creates the container with continuous WAL archiving for the recovery drill
# (scripts/backup-restore.sh, WBS S5-11): every completed WAL segment is copied to the volume
# ${NAME}-wal, mounted at /wal-archive, and the server switches segments at the latest every
# BRANT_PG_ARCHIVE_TIMEOUT seconds (default 60, at most 900: the RPO candidate is 15 minutes). The
# settings apply when the container is created; an existing container keeps its own.
set -euo pipefail

IMAGE="${BRANT_PG_IMAGE:-public.ecr.aws/docker/library/postgres@sha256:97ff59a4e30e08d1c11bdcd9455e7832368c0572b576c9092cde2df4ae5552a3}"
NAME="${BRANT_PG_CONTAINER:-brant-pg16}"
PORT="${BRANT_PG_PORT:-55471}"
WAL_ARCHIVE="${BRANT_PG_WAL_ARCHIVE:-0}"
ARCHIVE_TIMEOUT="${BRANT_PG_ARCHIVE_TIMEOUT:-60}"
DB=brant
cd "$(dirname "$0")/.."

if ! docker inspect "$NAME" >/dev/null 2>&1; then
  volumes=() server=()
  if [ "$WAL_ARCHIVE" = 1 ]; then
    if ! [ "$ARCHIVE_TIMEOUT" -ge 1 ] 2>/dev/null || [ "$ARCHIVE_TIMEOUT" -gt 900 ]; then
      echo "BRANT_PG_ARCHIVE_TIMEOUT must be 1..900 seconds, got ${ARCHIVE_TIMEOUT}" >&2
      exit 2
    fi
    volumes=(-v "${NAME}-wal:/wal-archive")
    server=(-c wal_level=replica -c archive_mode=on -c "archive_timeout=${ARCHIVE_TIMEOUT}"
      -c 'archive_command=test ! -f /wal-archive/%f && cp %p /wal-archive/%f')
  fi
  docker run -d --name "$NAME" -p "127.0.0.1:${PORT}:5432" \
    -e POSTGRES_PASSWORD=dev-superuser-only -e POSTGRES_DB="$DB" \
    -v "${NAME}-data:/var/lib/postgresql/data" ${volumes[@]+"${volumes[@]}"} "$IMAGE" ${server[@]+"${server[@]}"} >/dev/null
  if [ "$WAL_ARCHIVE" = 1 ]; then
    # A new volume is root's; the archiver runs as postgres and retries until it may write.
    docker exec -u root "$NAME" chown postgres:postgres /wal-archive
  fi
elif [ "$(docker inspect -f '{{.State.Running}}' "$NAME")" != true ]; then
  docker start "$NAME" >/dev/null
fi

# The image's init server listens on the socket only, so a TCP answer means the final server is up.
for _ in $(seq 1 60); do
  docker exec "$NAME" pg_isready -h 127.0.0.1 -U postgres -d "$DB" -q && break
  sleep 1
done
docker exec "$NAME" pg_isready -h 127.0.0.1 -U postgres -d "$DB"

BRANT_PG_CONTAINER="$NAME" BRANT_PG_PORT="$PORT" BRANT_PG_DB="$DB" scripts/pg-prepare.sh
