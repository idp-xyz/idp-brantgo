#!/usr/bin/env bash
# Runs the unit and PostgreSQL 16 integration tests with internal/bootstrap, which holds almost all
# of the suite's time, split into shards by tools/testshard. CI and make use the same commands.
#
#   scripts/test-shards.sh shard <integration|race> <k> <n>
#       Shard k of n of internal/bootstrap with go test -v, against the database that
#       IDP_BRANT_POSTGRES_DSN and IDP_BRANT_MIGRATOR_DSN name; the output goes to stdout and to
#       bin/shards/<mode>-<k>-of-<n>.log. It fails unless go test passes and the log shows every test
#       of the shard passing exactly once. CI's bootstrap jobs run this.
#   scripts/test-shards.sh others <integration|race>
#       Every package of the module except internal/bootstrap (integration with -v). CI's job for the
#       other packages runs this.
#   scripts/test-shards.sh all <integration|race> <n>
#       make integration/race SHARDS=n: prepares databases brant_shard_1 .. brant_shard_n with
#       scripts/pg-prepare.sh in the container that publishes BRANT_PG_PORT (default 55471) on the
#       host, runs the n shards, each against its own database, and the other packages (against
#       brant_shard_1) in parallel, then proves from the logs that every test of internal/bootstrap
#       passed exactly once, in its own shard. Logs go to bin/shards/<mode>/; one summary at the end.
#       The run fails if a shard or the other packages fail or a test is missing.
#
# integration is go test -tags integration -count=1 -timeout 45m, race adds -race and -timeout 60m, as
# make integration and make race without SHARDS. Set CI=true to make a missing database a failure.
set -euo pipefail
cd "$(dirname "$0")/.."

PKG=./internal/bootstrap
GO="${GO:-go}"
OUT=bin/shards
MIGRATOR_PASSWORD="${BRANT_PG_MIGRATOR_PASSWORD:-dev-migrator-only}"
APP_PASSWORD="${BRANT_PG_APP_PASSWORD:-dev-app-only}"

die() {
  echo "test-shards: $*" >&2
  exit 2
}

[ $# -ge 2 ] || die "usage: $0 shard <integration|race> <k> <n> | others <integration|race> | all <integration|race> <n>"
cmd=$1 mode=$2
case "$mode" in
  integration) flags=(-tags integration -count=1 -timeout 45m) list_flags=(-tags integration) ;;
  race) flags=(-tags integration -race -count=1 -timeout 60m) list_flags=(-tags integration -race) ;;
  *) die "mode must be integration or race, got '$mode'" ;;
esac
positive() { [[ "$1" =~ ^[1-9][0-9]*$ ]]; }

mkdir -p "$OUT"
tool="$OUT/testshard"
build_tool() { "$GO" build -o "$tool" ./tools/testshard; }

# others_packages prints every package of the module except internal/bootstrap.
others_packages() {
  local skip
  skip="$("$GO" list "$PKG")"
  "$GO" list -tags integration ./... | grep -vxF "$skip"
}

# kill_tree <pid> stops a process this script started and everything it started.
kill_tree() {
  local child
  for child in $(ps -o pid= --ppid "$1" 2>/dev/null); do
    kill_tree "$child"
  done
  kill -TERM "$1" 2>/dev/null || true
}

minutes() { printf '%dm%02ds' $(($1 / 60)) $(($1 % 60)); }

case "$cmd" in
shard)
  if ! { [ $# -eq 4 ] && positive "$3" && positive "$4" && [ "$3" -le "$4" ]; }; then
    die "usage: $0 shard <integration|race> <k> <n>, 1 <= k <= n"
  fi
  k=$3 n=$4
  list="$OUT/$mode-tests.txt" log="$OUT/$mode-$k-of-$n.log"
  build_tool
  "$tool" -names "${list_flags[@]}" "$PKG" >"$list"
  "$tool" -from "$list" -shards "$n" -check "$PKG"
  run="$("$tool" -from "$list" -shards "$n" -shard "$k" "$PKG")"
  status=0
  "$GO" test "${flags[@]}" -v -run "$run" "$PKG" 2>&1 | tee "$log" || status=$?
  verified=0
  "$tool" -from "$list" -shards "$n" -shard "$k" -verify "$PKG" "$log" || verified=$?
  if [ "$status" -ne 0 ] || [ "$verified" -ne 0 ]; then
    echo "test-shards: $mode shard $k of $n failed (go test exit $status, verification exit $verified)" >&2
    exit 1
  fi
  ;;

others)
  [ $# -eq 2 ] || die "usage: $0 others <integration|race>"
  verbose=()
  [ "$mode" = integration ] && verbose=(-v)
  mapfile -t pkgs < <(others_packages)
  "$GO" test "${flags[@]}" ${verbose[@]+"${verbose[@]}"} "${pkgs[@]}"
  ;;

all)
  if ! { [ $# -eq 3 ] && positive "$3"; }; then
    die "usage: $0 all <integration|race> <n>, n >= 1"
  fi
  n=$3
  port="${BRANT_PG_PORT:-55471}"
  container="${BRANT_PG_CONTAINER:-$(docker ps --filter "publish=$port" --format '{{.Names}}')}"
  if [ -z "$container" ] || [ "$(wc -w <<<"$container")" -ne 1 ]; then
    die "no single running container publishes port $port; start one with make db PG_PORT=$port"
  fi
  docker port "$container" 5432/tcp 2>/dev/null | grep -q ":$port\$" ||
    die "container $container does not publish port $port"
  dir="$OUT/$mode"
  rm -rf "$dir"
  mkdir -p "$dir"
  started=$SECONDS
  build_tool

  echo "test-shards: preparing brant_shard_1 .. brant_shard_$n in container $container (port $port)"
  for k in $(seq 1 "$n"); do
    BRANT_PG_CONTAINER="$container" BRANT_PG_PORT="$port" BRANT_PG_DB="brant_shard_$k" \
      scripts/pg-prepare.sh >"$dir/database-$k.log" 2>&1 ||
      { tail -20 "$dir/database-$k.log" >&2; die "preparing brant_shard_$k failed (see $dir/database-$k.log)"; }
  done

  list="$dir/tests.txt"
  "$tool" -names "${list_flags[@]}" "$PKG" >"$list" # builds the test binary the shards then share
  "$tool" -from "$list" -shards "$n" -check "$PKG"
  mapfile -t pkgs < <(others_packages)
  echo "test-shards: $mode with $PKG in $n shards and ${#pkgs[@]} other packages, in parallel; logs in $dir/"

  dsn() { # dsn <role> <password> <database>
    echo "postgres://$1:$2@127.0.0.1:$port/$3?sslmode=disable"
  }
  pids=()
  stop_all() {
    local p
    for p in "${pids[@]}"; do kill_tree "$p"; done
    exit 130
  }
  trap stop_all INT TERM
  for k in $(seq 1 "$n"); do
    run="$("$tool" -from "$list" -shards "$n" -shard "$k" "$PKG")"
    (
      begin=$SECONDS
      status=0
      IDP_BRANT_MIGRATOR_DSN="$(dsn brant_migrator "$MIGRATOR_PASSWORD" "brant_shard_$k")" \
        IDP_BRANT_POSTGRES_DSN="$(dsn brant_app "$APP_PASSWORD" "brant_shard_$k")" \
        "$GO" test "${flags[@]}" -v -run "$run" "$PKG" >"$dir/shard-$k.log" 2>&1 || status=$?
      echo "$status $((SECONDS - begin))" >"$dir/shard-$k.status"
    ) &
    pids+=($!)
  done
  verbose=()
  [ "$mode" = integration ] && verbose=(-v)
  (
    begin=$SECONDS
    status=0
    IDP_BRANT_MIGRATOR_DSN="$(dsn brant_migrator "$MIGRATOR_PASSWORD" brant_shard_1)" \
      IDP_BRANT_POSTGRES_DSN="$(dsn brant_app "$APP_PASSWORD" brant_shard_1)" \
      "$GO" test "${flags[@]}" ${verbose[@]+"${verbose[@]}"} "${pkgs[@]}" >"$dir/others.log" 2>&1 || status=$?
    echo "$status $((SECONDS - begin))" >"$dir/others.status"
  ) &
  pids+=($!)
  wait "${pids[@]}" || true
  trap - INT TERM

  failed=0
  echo
  echo "test-shards: $mode SHARDS=$n, summary"
  for k in $(seq 1 "$n"); do
    read -r status secs <"$dir/shard-$k.status" || { status=missing secs=0; }
    [ "$status" = 0 ] || failed=1
    tests="$("$tool" -from "$list" -shards "$n" -shard "$k" -names "$PKG" | wc -l)"
    printf '  shard %d/%d   %4d tests   brant_shard_%-3d exit %-3s %8s   %s\n' \
      "$k" "$n" "$tests" "$k" "$status" "$(minutes "$secs")" "$dir/shard-$k.log"
  done
  read -r status secs <"$dir/others.status" || { status=missing secs=0; }
  [ "$status" = 0 ] || failed=1
  printf '  others      %4s pkgs    brant_shard_1   exit %-3s %8s   %s\n' "${#pkgs[@]}" "$status" "$(minutes "$secs")" "$dir/others.log"
  if [ "$status" != 0 ]; then
    grep -E '^(--- FAIL|FAIL|panic:)' "$dir/others.log" | head -40 | sed 's/^/    /' || true
  fi
  logs=()
  for k in $(seq 1 "$n"); do logs+=("$dir/shard-$k.log"); done
  if ! "$tool" -from "$list" -shards "$n" -verify "$PKG" "${logs[@]}"; then
    failed=1
  fi
  echo "  wall $(minutes $((SECONDS - started))) (databases included)"
  if [ "$failed" -ne 0 ]; then
    echo "test-shards: $mode SHARDS=$n FAILED"
    exit 1
  fi
  echo "test-shards: $mode SHARDS=$n passed"
  ;;

*) die "unknown command '$cmd': shard, others or all" ;;
esac
