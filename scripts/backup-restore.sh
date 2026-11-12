#!/usr/bin/env bash
# Backup, restore and upgrade drills on Brant's development PostgreSQL 16 (WBS S5-11; baseline
# 11.2 恢复 and 升级支持: "恢复指标需要实际演练，不能只检查备份文件存在"). A drill on one development
# host, not a production procedure: the backups stay in Docker volumes and files of this host.
#
#   scripts/backup-restore.sh logical   pg_dump of the quiet source, pg_restore into a fresh restore target,
#                                       consistency checks and a service check on the restored database
#   scripts/backup-restore.sh pitr      base backup (pg_basebackup, verified) and the source's WAL archive:
#                                       a named restore point taken while the source is quiet, then writes,
#                                       then the source is killed; restore to the named point (exact) and to
#                                       the end of the archive (RPO measured against the acknowledged writes)
#   scripts/backup-restore.sh upgrade   database at the S5 start (BRANT_UPGRADE_FROM's migrations and its
#                                       integration tests as data), backed up, restored, migrated to head,
#                                       head's integration smoke run against it
#   scripts/backup-restore.sh drill     logical, pitr and upgrade in that order; facts in $BRANT_DRILL_DIR
#   scripts/backup-restore.sh check CONTAINER PREFIX
#                                       the consistency facts of one database into PREFIX.*
#
# The consistency facts of a database: per table of brant_* and bento its row count and a digest of
# its rows (PREFIX.digest); every CHECK constraint that computes a SHA-256 (business hashes,
# document and file bytes, evidence entries) re-evaluated over every row (PREFIX.hashes); the
# migration ledger brant_migrations.applied (PREFIX.ledger); the Outbox by status (PREFIX.outbox);
# the stored file and document bytes against their SHA-256 (PREFIX.files).
#
# Environment: BRANT_PG_CONTAINER and BRANT_PG_PORT name the source (scripts/dev-postgres.sh; pitr
# needs it created with BRANT_PG_WAL_ARCHIVE=1); BRANT_RESTORE_CONTAINER and BRANT_RESTORE_PORT the
# restore target, which every drill removes and recreates with its volume; BRANT_DRILL_DIR (default
# bin/drill/<UTC time>); BRANT_DRILL_WRITERS (4) users writing for BRANT_DRILL_WRITE_SECONDS (120)
# with the source killed after BRANT_DRILL_KILL_AFTER (90) seconds; BRANT_UPGRADE_FROM (3b76201,
# the S5 start), BRANT_UPGRADE_SEED (go test -run pattern of its suite, default all) and
# BRANT_UPGRADE_SMOKE (head's -run pattern); BRANT_PG_IMAGE as scripts/dev-postgres.sh. Needs
# docker, git, go and the binaries of `make build` in bin/. The passwords are the development ones.
set -euo pipefail

IMAGE="${BRANT_PG_IMAGE:-public.ecr.aws/docker/library/postgres@sha256:97ff59a4e30e08d1c11bdcd9455e7832368c0572b576c9092cde2df4ae5552a3}"
SRC="${BRANT_PG_CONTAINER:-brant-pg16}"
SRC_PORT="${BRANT_PG_PORT:-55471}"
TGT="${BRANT_RESTORE_CONTAINER:-${SRC}-restore}"
TGT_PORT="${BRANT_RESTORE_PORT:-$((SRC_PORT + 1))}"
WRITERS="${BRANT_DRILL_WRITERS:-4}"
WRITE_SECONDS="${BRANT_DRILL_WRITE_SECONDS:-120}"
KILL_AFTER="${BRANT_DRILL_KILL_AFTER:-90}"
UPGRADE_FROM="${BRANT_UPGRADE_FROM:-3b76201}"
UPGRADE_SEED="${BRANT_UPGRADE_SEED:-.}"
UPGRADE_SMOKE="${BRANT_UPGRADE_SMOKE:-TestBRC001FullChainOverHTTP|TestLoadGeneratorSmoke}"
DB=brant
SU_PW=dev-superuser-only MIG_PW=dev-migrator-only APP_PW=dev-app-only
cd "$(dirname "$0")/.."
ROOT="$PWD"
DIR="${BRANT_DRILL_DIR:-bin/drill/$(date -u +%Y%m%dT%H%M%SZ)}"
mkdir -p "$DIR"
DIR="$(cd "$DIR" && pwd)"
BASE_VOLUME="${SRC}-basebackup"

if [ "$TGT" = "$SRC" ] || [ "$TGT_PORT" = "$SRC_PORT" ]; then
  echo "the restore target must differ from the source (container and port)" >&2
  exit 2
fi

app_dsn() { echo "postgres://brant_app:${APP_PW}@127.0.0.1:$1/${DB}?sslmode=disable"; }
mig_dsn() { echo "postgres://brant_migrator:${MIG_PW}@127.0.0.1:$1/${DB}?sslmode=disable"; }

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*" | tee -a "$DIR/drill.log" >&2; }
now() { date -u +%s.%N; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.2f", b - a }'; }
# fact KEY VALUE: one measured fact of the drill (facts.tsv).
fact() { printf '%s\t%s\n' "$1" "$2" >>"$DIR/facts.tsv"; log "$1 = $2"; }
host() { fact "$1.host" "$(uptime | sed 's/^ *//'); nproc $(nproc); MemAvailable $(awk '/MemAvailable/ { printf "%d MiB", $2 / 1024 }' /proc/meminfo)"; }
fail() { log "FAILED: $*"; exit 1; }

sql() { # sql CONTAINER [psql args]: psql as the superuser inside the container, unaligned, tab-separated
  local c=$1
  shift
  docker exec -i "$c" psql -h 127.0.0.1 -U postgres -d "$DB" -X -q -v ON_ERROR_STOP=1 -At -F $'\t' "$@"
}

wait_ready() { # wait_ready CONTAINER: the final server answers on TCP (the image's init server listens on the socket only)
  local c=$1 i
  for i in $(seq 1 600); do
    docker exec "$c" pg_isready -h 127.0.0.1 -U postgres -d "$DB" -q 2>/dev/null && return 0
    [ "$(docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null)" = true ] || { docker logs --tail 30 "$c" >&2; return 1; }
    sleep 0.2
  done
  return 1
}

wait_promoted() { # wait_promoted CONTAINER: recovery has ended and the server accepts writes
  local c=$1 i
  for i in $(seq 1 3000); do
    if docker exec "$c" pg_isready -h 127.0.0.1 -U postgres -d "$DB" -q 2>/dev/null &&
      [ "$(sql "$c" -c 'SELECT pg_is_in_recovery()' 2>/dev/null)" = f ]; then
      return 0
    fi
    [ "$(docker inspect -f '{{.State.Running}}' "$c" 2>/dev/null)" = true ] || { docker logs --tail 30 "$c" >&2; return 1; }
    sleep 0.2
  done
  return 1
}

remove_target() {
  docker rm -f "$TGT" >/dev/null 2>&1 || true
  docker volume rm "${TGT}-data" >/dev/null 2>&1 || true
}

fresh_target() { # an empty PostgreSQL 16 with Brant's roles, as scripts/dev-postgres.sh creates one
  remove_target
  docker run -d --name "$TGT" -p "127.0.0.1:${TGT_PORT}:5432" -e POSTGRES_PASSWORD="$SU_PW" -e POSTGRES_DB="$DB" \
    -v "${TGT}-data:/var/lib/postgresql/data" "$IMAGE" >/dev/null
  wait_ready "$TGT" || fail "restore target did not start"
  docker exec -i "$TGT" psql -h 127.0.0.1 -U postgres -d "$DB" -q -v ON_ERROR_STOP=1 -v db="$DB" \
    -v migrator_password="$MIG_PW" -v app_password="$APP_PW" <deploy/postgres/00_roles.sql
}

load_bin() { # the load generator (tests/load) built once per drill
  [ -x "$DIR/brant-load" ] || go build -o "$DIR/brant-load" ./tests/load
  for b in brant-api brant-worker brant-admin brant-migrate; do
    [ -x "bin/$b" ] || fail "bin/$b is missing: run make build"
  done
}

# check CONTAINER PREFIX: the consistency facts of one database (see the header).
check() {
  local c=$1 out=$2
  sql "$c" >"$out.digest" <<'SQL'
SELECT format('SELECT %L, count(*), md5(coalesce(string_agg(h, '''' ORDER BY h), '''')) FROM (SELECT md5(t::text) AS h FROM %I.%I t) s',
              n.nspname || '.' || c.relname, n.nspname, c.relname)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND (n.nspname LIKE 'brant\_%' OR n.nspname = 'bento')
ORDER BY n.nspname, c.relname
\gexec
SQL
  sql "$c" >"$out.hashes" <<'SQL'
SELECT format('SELECT %L, count(*) FILTER (WHERE NOT (%s)), count(*) FROM %s', conrelid::regclass::text || ':' || conname,
              pg_get_expr(conbin, conrelid), conrelid::regclass)
FROM pg_constraint
WHERE contype = 'c' AND pg_get_expr(conbin, conrelid) LIKE '%sha256(%' AND connamespace::regnamespace::text LIKE 'brant\_%'
ORDER BY 1
\gexec
SQL
  sql "$c" -c "SELECT count(*), md5(string_agg(id || ':' || sha256, ',' ORDER BY id)) FROM brant_migrations.applied" >"$out.ledger"
  sql "$c" -c "SELECT status, count(*), coalesce(max(enqueued_seq), 0) FROM bento.outbox GROUP BY status ORDER BY status" >"$out.outbox"
  sql "$c" >"$out.files" <<'SQL'
SELECT format('SELECT %L, count(*), coalesce(sum(octet_length(content)), 0), count(*) FILTER (WHERE encode(sha256(content), ''hex'') <> %I), '
              'md5(coalesce(string_agg(%I, '','' ORDER BY %I), '''')) FROM %s', t, col, col, col, t)
FROM (VALUES ('brant_evd.business_file', 'sha256'), ('brant_trd.quote_document', 'content_sha256'),
             ('brant_trd.sales_order_document', 'content_sha256'), ('brant_ful.shipment_document_rendition', 'content_sha256')) v(t, col)
WHERE to_regclass(t) IS NOT NULL
\gexec
SQL
}

# summarize PREFIX NAME: the facts of a check as one line each.
summarize() {
  local out=$1 name=$2
  fact "$name.tables" "$(wc -l <"$out.digest") tables, $(awk -F'\t' '{ s += $2 } END { print s + 0 }' "$out.digest") rows"
  fact "$name.hash_constraints" "$(wc -l <"$out.hashes") constraints over $(awk -F'\t' '{ s += $3 } END { print s + 0 }' "$out.hashes") rows, $(awk -F'\t' '{ s += $2 } END { print s + 0 }' "$out.hashes") violations"
  fact "$name.ledger" "$(tr '\t' ' ' <"$out.ledger")"
  fact "$name.outbox" "$(awk -F'\t' '{ printf "%s%s=%s", sep, $1, $2; sep = ", " }' "$out.outbox")"
  fact "$name.files" "$(awk -F'\t' '{ printf "%s%s %s files %s bytes %s mismatches", sep, $1, $2, $3, $4; sep = "; " }' "$out.files")"
  [ "$(awk -F'\t' '{ s += $2 } END { print s + 0 }' "$out.hashes")" = 0 ] || fail "$name: a SHA-256 constraint does not hold"
  [ "$(awk -F'\t' '{ s += $4 } END { print s + 0 }' "$out.files")" = 0 ] || fail "$name: stored bytes differ from their SHA-256"
}

# same A B NAME: two databases' facts are identical (rows, ledger, Outbox, files).
same() {
  local a=$1 b=$2 name=$3 f
  for f in digest ledger outbox files; do
    diff "$a.$f" "$b.$f" >"$DIR/$name.$f.diff" || fail "$name: $f differs (see $DIR/$name.$f.diff)"
  done
  fact "$name.identical" "row digests of $(wc -l <"$a.digest") tables, migration ledger, Outbox by status and file bytes equal"
}

# ledger_noop PORT NAME: brant-migrate of head on the restored database applies nothing and finds
# no checksum changed.
ledger_noop() {
  local out
  out="$(IDP_BRANT_MIGRATOR_DSN="$(mig_dsn "$1")" bin/brant-migrate -apply 2>&1 | tail -1)" || fail "$2: brant-migrate: $out"
  fact "$2.migrate_again" "$out"
  case "$out" in "DONE: 0 applied"*) ;; *) fail "$2: brant-migrate applied migrations to a restored database" ;; esac
}

# service PORT NAME: the restored database serves BRC-001 order cycles over the HTTP API, with the
# relay running (brant-api, brant-worker -dispatch, a fresh tenant), and the Outbox moves.
service() {
  local port=$1 name=$2 start pending_before pending_after
  pending_before="$(sql "$TGT" -c "SELECT count(*) FROM bento.outbox WHERE status IN ('PENDING', 'LEASED')")"
  start="$(now)"
  "$DIR/brant-load" -mode writes -bin bin -dsn "$(app_dsn "$port")" -vus 2 -duration 15s -tenant "$name.$(date -u +%H%M%S)" \
    -ack-log "$DIR/$name.service.acks.jsonl" -out "$DIR/$name.service" >"$DIR/$name.service.log" 2>&1 || fail "$name: service check (see $DIR/$name.service.log)"
  fact "$name.service_s" "$(since "$start")"
  pending_after="$(sql "$TGT" -c "SELECT count(*) FROM bento.outbox WHERE status IN ('PENDING', 'LEASED')")"
  fact "$name.service" "$(grep '^Writes:' "$DIR/$name.service.log"); Outbox pending+leased $pending_before before, $pending_after after"
  grep -q 'cycles [1-9][0-9]* completed, 0 failed' "$DIR/$name.service.log" || fail "$name: no order cycle completed on the restored database"
}

logical() {
  log "== logical: pg_dump / pg_restore ($SRC:$SRC_PORT -> $TGT:$TGT_PORT)"
  load_bin
  host logical
  wait_ready "$SRC" || fail "source is not running"
  local t
  t="$(now)"
  check "$SRC" "$DIR/logical.source"
  fact logical.source_check_s "$(since "$t")"
  summarize "$DIR/logical.source" logical.source
  fact logical.source_bytes "$(sql "$SRC" -c 'SELECT pg_database_size(current_database())')"
  t="$(now)"
  docker exec "$SRC" pg_dump -h 127.0.0.1 -U postgres -d "$DB" -Fc -Z 6 >"$DIR/brant.dump"
  fact logical.dump_s "$(since "$t")"
  fact logical.dump_bytes "$(stat -c %s "$DIR/brant.dump")"

  local rto
  rto="$(now)"
  t="$(now)"
  fresh_target
  fact logical.target_s "$(since "$t")"
  t="$(now)"
  docker cp "$DIR/brant.dump" "$TGT:/tmp/brant.dump" >/dev/null
  docker exec "$TGT" pg_restore -h 127.0.0.1 -U postgres -d "$DB" --exit-on-error -j 4 /tmp/brant.dump
  docker exec "$TGT" rm -f /tmp/brant.dump
  fact logical.restore_s "$(since "$t")"
  t="$(now)"
  docker exec "$TGT" vacuumdb -h 127.0.0.1 -U postgres -d "$DB" --analyze-only -j 4 -q
  fact logical.analyze_s "$(since "$t")"
  t="$(now)"
  check "$TGT" "$DIR/logical.restored"
  summarize "$DIR/logical.restored" logical.restored
  same "$DIR/logical.source" "$DIR/logical.restored" logical
  ledger_noop "$TGT_PORT" logical
  fact logical.verify_s "$(since "$t")"
  service "$TGT_PORT" logical
  fact logical.rto_s "$(since "$rto") (fresh target, roles, pg_restore -j 4, analyze, checks, service check; detection and decision not included)"
  host logical.end
}

pitr() {
  log "== pitr: base backup + WAL archive ($SRC:$SRC_PORT -> $TGT:$TGT_PORT)"
  load_bin
  host pitr
  wait_ready "$SRC" || fail "source is not running"
  [ "$(sql "$SRC" -c 'SHOW archive_mode')" = on ] || fail "$SRC does not archive WAL: create it with BRANT_PG_WAL_ARCHIVE=1 scripts/dev-postgres.sh"
  fact pitr.archive "archive_timeout $(sql "$SRC" -c 'SHOW archive_timeout'), $(sql "$SRC" -c "SELECT archived_count || ' archived, ' || failed_count || ' failed, last ' || coalesce(last_archived_wal, '-') FROM pg_stat_archiver")"
  local t
  t="$(now)"
  docker volume rm "$BASE_VOLUME" >/dev/null 2>&1 || true
  docker run --rm --network "container:$SRC" -v "$BASE_VOLUME:/backup" "$IMAGE" \
    pg_basebackup -h 127.0.0.1 -U postgres -D /backup/base -Fp -X stream -c fast
  fact pitr.base_backup_s "$(since "$t")"
  t="$(now)"
  docker run --rm -v "$BASE_VOLUME:/backup:ro" "$IMAGE" pg_verifybackup -n -q /backup/base || fail "pg_verifybackup refused the base backup"
  fact pitr.verify_backup_s "$(since "$t")"
  fact pitr.base_backup_bytes "$(docker run --rm -v "$BASE_VOLUME:/backup:ro" "$IMAGE" du -sb /backup/base | cut -f1)"

  # A named restore point while the source is quiet, and its facts.
  local point="drill.$(date -u +%Y%m%dT%H%M%S)"
  sql "$SRC" -c "SELECT pg_create_restore_point('$point')" >/dev/null
  t="$(now)"
  check "$SRC" "$DIR/pitr.point"
  fact pitr.point_check_s "$(since "$t")"
  sql "$SRC" -c "SELECT pg_switch_wal()" >/dev/null
  fact pitr.point "$point at $(date -u +%Y-%m-%dT%H:%M:%S.%6NZ)"

  # Writes until and past the kill; every acknowledged command is logged.
  local tenant="drill.$(date -u +%H%M%S)" writer kill_at
  "$DIR/brant-load" -mode writes -bin bin -dsn "$(app_dsn "$SRC_PORT")" -vus "$WRITERS" -duration "${WRITE_SECONDS}s" -worker=false \
    -tenant "$tenant" -ack-log "$DIR/pitr.acks.jsonl" -out "$DIR/pitr.writes" >"$DIR/pitr.writes.log" 2>&1 &
  writer=$!
  sleep "$KILL_AFTER"
  fact pitr.before_kill "$(sql "$SRC" -c "SELECT 'last archived ' || coalesce(last_archived_wal, '-') || ' at ' || coalesce(last_archived_time::text, '-') || ', current ' || pg_walfile_name(pg_current_wal_lsn()) FROM pg_stat_archiver")"
  kill_at="$(date -u +%Y-%m-%dT%H:%M:%S.%6NZ)"
  docker kill "$SRC" >/dev/null
  fact pitr.killed_at "$kill_at ($SRC SIGKILL; its data volume counts as lost, the WAL archive and the base backup survive)"
  wait "$writer" || true
  fact pitr.writes "$(grep '^Writes:' "$DIR/pitr.writes.log" || echo 'no summary')"
  fact pitr.archive_after_kill "$(docker run --rm -v "${SRC}-wal:/wal-archive:ro" "$IMAGE" sh -c 'echo "$(ls /wal-archive | wc -l) files, $(du -sb /wal-archive | cut -f1) bytes, last $(ls /wal-archive | tail -1)"')"

  # Restore 1: to the named point; the database equals the source at that point.
  restore_pitr "name:$point" point
  check "$TGT" "$DIR/pitr.point.restored"
  summarize "$DIR/pitr.point.restored" pitr.point.restored
  same "$DIR/pitr.point" "$DIR/pitr.point.restored" pitr.point

  # Restore 2: to the end of the archive; what it lacks of the acknowledged writes is the loss.
  local rto
  rto="$(now)"
  restore_pitr latest latest
  local last
  last="$(sql "$TGT" -c "SELECT to_char(pg_last_xact_replay_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"')")"
  fact pitr.latest.last_replayed_commit "$last"
  fact pitr.latest.rpo_s "$(awk -v a="$(date -u -d "$last" +%s.%N)" -v b="$(date -u -d "$kill_at" +%s.%N)" 'BEGIN { printf "%.2f", b - a }') (kill time - last replayed commit)"
  "$DIR/brant-load" -mode acks -dsn "$(app_dsn "$TGT_PORT")" -tenant "$tenant" -ack-log "$DIR/pitr.acks.jsonl" >"$DIR/pitr.latest.acks.json"
  fact pitr.latest.acks "$(tr -d '\n ' <"$DIR/pitr.latest.acks.json")"
  t="$(now)"
  check "$TGT" "$DIR/pitr.latest.restored"
  summarize "$DIR/pitr.latest.restored" pitr.latest.restored
  fact pitr.latest.leased_at_crash "$(sql "$TGT" -c "SELECT count(*) FROM bento.outbox WHERE status = 'LEASED'") LEASED rows (re-claimed after their lease expires)"
  ledger_noop "$TGT_PORT" pitr.latest
  fact pitr.latest.verify_s "$(since "$t")"
  service "$TGT_PORT" pitr.latest
  fact pitr.latest.rto_s "$(since "$rto") (fresh volume, base backup copied, WAL replayed, promoted, checks, service check; detection and decision not included)"

  # The source recovers from its own WAL: every acknowledged write is there.
  t="$(now)"
  docker start "$SRC" >/dev/null
  wait_ready "$SRC" || fail "source did not restart"
  fact pitr.source_crash_recovery_s "$(since "$t")"
  "$DIR/brant-load" -mode acks -dsn "$(app_dsn "$SRC_PORT")" -tenant "$tenant" -ack-log "$DIR/pitr.acks.jsonl" >"$DIR/pitr.source.acks.json"
  fact pitr.source.acks "$(tr -d '\n ' <"$DIR/pitr.source.acks.json")"
  grep -q '"lost": 0' "$DIR/pitr.source.acks.json" || fail "the source lost acknowledged commands in crash recovery"
  host pitr.end
}

# restore_pitr TARGET NAME: a fresh target from the base backup, recovered from the WAL archive to
# TARGET ("name:<restore point>" or "latest") and promoted.
restore_pitr() {
  local target=$1 name=$2 t conf
  conf="restore_command = 'cp /wal-archive/%f %p'"
  if [ "$target" != latest ]; then
    conf="$conf"$'\n'"recovery_target_name = '${target#name:}'"$'\n'"recovery_target_action = 'promote'"
  fi
  t="$(now)"
  remove_target
  docker volume create "${TGT}-data" >/dev/null
  docker run --rm -e CONF="$conf" -v "$BASE_VOLUME:/backup:ro" -v "${TGT}-data:/var/lib/postgresql/data" "$IMAGE" sh -ec '
    cd /var/lib/postgresql/data
    cp -a /backup/base/. .
    rm -f backup_manifest postmaster.pid
    printf "%s\n" "$CONF" >>postgresql.auto.conf
    touch recovery.signal
    chown -R postgres:postgres . && chmod 700 .'
  fact "pitr.$name.provision_s" "$(since "$t")"
  t="$(now)"
  docker run -d --name "$TGT" -p "127.0.0.1:${TGT_PORT}:5432" -v "${TGT}-data:/var/lib/postgresql/data" \
    -v "${SRC}-wal:/wal-archive:ro" "$IMAGE" >/dev/null
  wait_promoted "$TGT" || fail "pitr $name: the restored server did not finish recovery"
  fact "pitr.$name.recovery_s" "$(since "$t")"
  fact "pitr.$name.recovery_log" "$(docker logs "$TGT" 2>&1 | grep -E 'recovery stopping|last completed transaction|redo done|selected new timeline|archive recovery complete' | sed 's/^.*LOG: *//' | tr '\n' ';')"
  # How much WAL the recovery replayed: from the base backup's start to where redo ended.
  local start_lsn redo_lsn
  start_lsn="$(docker run --rm -v "$BASE_VOLUME:/backup:ro" "$IMAGE" sed -n 's/^START WAL LOCATION: \([0-9A-F]*\/[0-9A-F]*\).*/\1/p' /backup/base/backup_label)"
  redo_lsn="$(docker logs "$TGT" 2>&1 | sed -n 's/.*redo done at \([0-9A-F]*\/[0-9A-F]*\).*/\1/p' | tail -1)"
  if [ -n "$start_lsn" ] && [ -n "$redo_lsn" ]; then
    fact "pitr.$name.wal_replayed_bytes" "$(sql "$TGT" -c "SELECT pg_wal_lsn_diff('$redo_lsn', '$start_lsn')::bigint") ($start_lsn to $redo_lsn)"
  fi
}

upgrade() {
  log "== upgrade: S5 start ($UPGRADE_FROM) -> head on $TGT:$TGT_PORT"
  load_bin
  host upgrade
  local old="$DIR/upgrade-from" t
  rm -rf "$old" && mkdir -p "$old"
  git archive "$UPGRADE_FROM" | tar -x -C "$old"
  fact upgrade.from "$(git log -1 --format='%h %s' "$UPGRADE_FROM")"
  fact upgrade.to "$(git log -1 --format='%h %s' HEAD)$(git diff --quiet HEAD -- migrations deploy cmd/brant-migrate || echo ' (with uncommitted changes)')"
  fresh_target
  t="$(now)"
  (cd "$old" && GOFLAGS=-buildvcs=false IDP_BRANT_MIGRATOR_DSN="$(mig_dsn "$TGT_PORT")" go run ./cmd/brant-migrate -apply) \
    >"$DIR/upgrade.old-migrate.log" 2>&1 || fail "brant-migrate of $UPGRADE_FROM (see $DIR/upgrade.old-migrate.log)"
  fact upgrade.old_migrate "$(tail -1 "$DIR/upgrade.old-migrate.log")"
  docker exec -i -e PGPASSWORD="$MIG_PW" "$TGT" psql -h 127.0.0.1 -U brant_migrator -d "$DB" -q -v ON_ERROR_STOP=1 <"$old/deploy/postgres/10_app_grants.sql"
  fact upgrade.old_migrate_s "$(since "$t")"
  t="$(now)"
  local seed=0
  (cd "$old" && CI=true GOFLAGS=-buildvcs=false IDP_BRANT_MIGRATOR_DSN="$(mig_dsn "$TGT_PORT")" IDP_BRANT_POSTGRES_DSN="$(app_dsn "$TGT_PORT")" \
    go test -tags integration -count=1 -timeout 30m -v -run "$UPGRADE_SEED" ./internal/bootstrap/) >"$DIR/upgrade.seed.log" 2>&1 || seed=$?
  fact upgrade.seed "go test -run '$UPGRADE_SEED' ./internal/bootstrap/ of $UPGRADE_FROM: exit $seed in $(since "$t") s; $(grep -c '^--- PASS' "$DIR/upgrade.seed.log" || true) passed lines, $(grep -c '^--- FAIL' "$DIR/upgrade.seed.log" || true) failed lines"
  t="$(now)"
  docker exec "$TGT" pg_dump -h 127.0.0.1 -U postgres -d "$DB" -Fc -Z 6 >"$DIR/s5-start.dump"
  fact upgrade.dump "$(stat -c %s "$DIR/s5-start.dump") bytes in $(since "$t") s"
  check "$TGT" "$DIR/upgrade.before"
  summarize "$DIR/upgrade.before" upgrade.before

  t="$(now)"
  fresh_target
  docker cp "$DIR/s5-start.dump" "$TGT:/tmp/brant.dump" >/dev/null
  docker exec "$TGT" pg_restore -h 127.0.0.1 -U postgres -d "$DB" --exit-on-error -j 4 /tmp/brant.dump
  docker exec "$TGT" rm -f /tmp/brant.dump
  fact upgrade.restore_s "$(since "$t")"
  check "$TGT" "$DIR/upgrade.restored"
  same "$DIR/upgrade.before" "$DIR/upgrade.restored" upgrade.restored

  t="$(now)"
  IDP_BRANT_MIGRATOR_DSN="$(mig_dsn "$TGT_PORT")" bin/brant-migrate -apply >"$DIR/upgrade.migrate.log" 2>&1 || fail "head brant-migrate (see $DIR/upgrade.migrate.log)"
  docker exec -i -e PGPASSWORD="$MIG_PW" "$TGT" psql -h 127.0.0.1 -U brant_migrator -d "$DB" -q -v ON_ERROR_STOP=1 <deploy/postgres/10_app_grants.sql
  fact upgrade.migrate_s "$(since "$t")"
  fact upgrade.migrate "$(tail -1 "$DIR/upgrade.migrate.log"); applied: $(grep '^APPLIED' "$DIR/upgrade.migrate.log" | sed 's/^APPLIED brant://' | tr '\n' ' ')"
  ledger_noop "$TGT_PORT" upgrade
  check "$TGT" "$DIR/upgrade.after"
  summarize "$DIR/upgrade.after" upgrade.after
  # Every table of the S5 start keeps its rows (a migration may add columns, so digests may
  # change); only the migration ledger grows.
  local changed
  changed="$(join -t $'\t' <(cut -f1,2 "$DIR/upgrade.before.digest" | sort) <(cut -f1,2 "$DIR/upgrade.after.digest" | sort) |
    awk -F'\t' '$2 != $3 && $1 != "brant_migrations.applied" { print $1 }' | tr '\n' ' ')"
  [ -z "$changed" ] || fail "upgrade: tables of the S5 start changed their row count: $changed"
  fact upgrade.rows_kept "$(wc -l <"$DIR/upgrade.before.digest") tables of the S5 start: same row counts after the upgrade (the ledger grew $(cut -f1 "$DIR/upgrade.before.ledger") -> $(cut -f1 "$DIR/upgrade.after.ledger")); $(comm -12 <(sort "$DIR/upgrade.before.digest") <(sort "$DIR/upgrade.after.digest") | wc -l) with identical row digests"
  t="$(now)"
  local smoke=0
  CI=true IDP_BRANT_MIGRATOR_DSN="$(mig_dsn "$TGT_PORT")" IDP_BRANT_POSTGRES_DSN="$(app_dsn "$TGT_PORT")" \
    go test -tags integration -count=1 -timeout 30m -v -run "$UPGRADE_SMOKE" ./internal/bootstrap/ ./tests/load/ >"$DIR/upgrade.smoke.log" 2>&1 || smoke=$?
  fact upgrade.smoke "go test -run '$UPGRADE_SMOKE' ./internal/bootstrap/ ./tests/load/ on the upgraded database: exit $smoke in $(since "$t") s; $(grep -E '^(ok|FAIL|--- )' "$DIR/upgrade.smoke.log" | tr '\t' ' ' | tr '\n' ';')"
  [ "$smoke" = 0 ] || fail "upgrade: the integration smoke failed on the upgraded database (see $DIR/upgrade.smoke.log)"
  rm -rf "$old"
  host upgrade.end
}

case "${1:-}" in
logical) logical ;;
pitr) pitr ;;
upgrade) upgrade ;;
drill)
  logical
  pitr
  upgrade
  ;;
check)
  [ $# = 3 ] || { echo "usage: $0 check CONTAINER PREFIX" >&2; exit 2; }
  check "$2" "$3"
  ;;
*)
  echo "usage: $0 logical|pitr|upgrade|drill|check CONTAINER PREFIX (see the header)" >&2
  exit 2
  ;;
esac
log "facts: $DIR/facts.tsv"
