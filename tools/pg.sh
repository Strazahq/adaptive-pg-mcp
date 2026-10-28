#!/bin/sh
# Postgres 14 to 18 test harness in local Docker, for the tests built with -tags pg.
#
# sh tools/pg.sh up starts one Alpine Postgres container per major version,
# published on 127.0.0.1 only, with its data on tmpfs. It waits until each
# server accepts connections and writes their connection strings to
# .tools/pg.env with mode 600. sh tools/pg.sh down removes the containers that
# carry the harness label and that file. The script touches no other container
# and never prunes anything.
set -u
cd "$(dirname "$0")/.." || exit 1

label="adaptive-pg-mcp.harness=1"
prefix="adaptive-pg-mcp-pg"
majors="14 15 16 17 18"
env_file=".tools/pg.env"
wait_seconds=90
# A pulled image takes about 450 MB of Docker's disk, and the containers need a little more.
image_kb=460800
spare_kb=102400

say() { printf 'pg.sh: %s\n' "$*" >&2; }
fail() { say "$*"; exit 1; }

# image prints the pinned image for a major version.
image() {
  case "$1" in
    14) echo "postgres:14.24-alpine3.24" ;;
    15) echo "postgres:15.19-alpine3.24" ;;
    16) echo "postgres:16.15-alpine3.24" ;;
    17) echo "postgres:17.11-alpine3.24" ;;
    18) echo "postgres:18.6-alpine3.24" ;;
  esac
}

# need_docker refuses when the docker command or its daemon is missing.
need_docker() {
  command -v docker >/dev/null 2>&1 || fail "docker is not installed or not on PATH, so sh tools/pg.sh $1 cannot run. Install Docker and run it again."
  docker info >/dev/null 2>&1 || fail "the Docker daemon does not answer, so sh tools/pg.sh $1 cannot run. Start Docker, check that this user may reach it, and run it again."
}

# no_space says the disk is full and exits.
no_space() {
  fail "$1 failed because Docker's disk is full. Free space, for example with docker builder prune or by removing images you no longer use, and run sh tools/pg.sh up again."
}

# down removes the containers that carry the harness label, and the connection strings.
down() {
  need_docker down
  ids=$(docker ps -aq --filter "label=$label") || fail "docker could not list the harness containers. Run docker ps -a --filter label=$label to see why."
  if [ -n "$ids" ]; then
    docker rm -f -v $ids >/dev/null || fail "docker could not remove the harness containers. Run docker ps -a --filter label=$label and remove them by hand."
  fi
  rm -f "$env_file"
}

# check_disk refuses when Docker's disk cannot hold the images still to pull.
check_disk() {
  missing=0
  for m in $majors; do
    docker image inspect "$(image "$m")" >/dev/null 2>&1 || missing=$((missing + 1))
  done
  dir=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null)
  avail=$(df -Pk "$dir" 2>/dev/null | awk 'NR == 2 { print $4 }')
  # A Docker that keeps its disk inside a virtual machine has no local path to measure; the pull itself still reports a full disk.
  [ -n "$avail" ] || return 0
  need=$((missing * image_kb + spare_kb))
  if [ "$avail" -lt "$need" ]; then
    fail "Docker's disk at $dir has $((avail / 1024)) MB free, and the harness needs about $((need / 1024)) MB to pull $missing images and start. Free space, for example with docker builder prune or by removing images you no longer use, and run sh tools/pg.sh up again."
  fi
}

# pull fetches the pinned image of a major version when it is not present.
pull() {
  img=$(image "$1")
  docker image inspect "$img" >/dev/null 2>&1 && return 0
  say "pulling $img"
  if ! out=$(docker pull -q "$img" 2>&1); then
    printf '%s\n' "$out" | grep -q -i 'no space left' && no_space "pulling $img"
    fail "docker could not pull $img: $(printf '%s\n' "$out" | tail -n 1). Check the network and the tag, and run sh tools/pg.sh up again."
  fi
}

# tmpfs_args prints a --tmpfs option for every volume the image declares, and
# for its PGDATA when no volume holds it, so no data reaches Docker's disk.
# The Postgres 18 image keeps PGDATA in /var/lib/postgresql/18/docker under
# the volume /var/lib/postgresql, while 14 to 17 keep it in the volume
# /var/lib/postgresql/data, so the mounts come from the image and not from a
# fixed path.
tmpfs_args() {
  img=$(image "$1")
  vols=$(docker image inspect -f '{{range $k, $v := .Config.Volumes}}{{$k}} {{end}}' "$img") || fail "docker could not inspect $img. Run docker image inspect $img to see why."
  pgdata=$(docker image inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$img" | sed -n 's/^PGDATA=//p')
  [ -n "$pgdata" ] || fail "$img sets no PGDATA, so the script cannot tell where its data lives. Pin an official postgres image in tools/pg.sh."
  covered=no
  for v in $vols; do
    case "$v" in /*) ;; *) fail "$img declares the volume $v, which is not an absolute path. Pin an official postgres image in tools/pg.sh." ;; esac
    case "$pgdata/" in "${v%/}"/*) covered=yes ;; esac
  done
  [ "$covered" = yes ] || vols="$vols $pgdata"
  for v in $vols; do printf ' --tmpfs %s:rw,size=512m' "$v"; done
}

# start runs the container for a major version. The settings keep a server
# small, because its data and WAL live in RAM: a run that creates many
# databases would otherwise hold up to 1 GB of WAL per server until a checkpoint.
start() {
  name="$prefix$1"
  img=$(image "$1")
  mounts=$(tmpfs_args "$1") || exit 1
  if ! out=$(POSTGRES_PASSWORD="$password" docker run -d --name "$name" --label "$label" \
      -e POSTGRES_PASSWORD -p 127.0.0.1::5432 $mounts "$img" \
      -c shared_buffers=32MB -c min_wal_size=32MB -c max_wal_size=64MB 2>&1); then
    printf '%s\n' "$out" | grep -q -i 'no space left' && no_space "starting $name"
    printf '%s\n' "$out" | grep -q -i 'conflict' && fail "a container named $name exists without the label $label, so the script leaves it alone. Rename or remove it yourself and run sh tools/pg.sh up again."
    fail "docker could not start $name: $(printf '%s\n' "$out" | tail -n 1 | sed "s/$password/[redacted]/g"). Run sh tools/pg.sh down, then sh tools/pg.sh up again."
  fi
}

# not_up reports a server that did not come up, with its last log lines and the password redacted.
not_up() {
  name="$prefix$1"
  logs=$(docker logs --tail 10 "$name" 2>&1 | sed "s/$password/[redacted]/g")
  say "$2"
  printf '%s\n' "$logs" | sed 's/^/  /' >&2
  printf '%s\n' "$logs" | grep -q -i 'no space left' && fail "Postgres $1 ran out of space. Its data sits on a 512 MB tmpfs in RAM and its container on Docker's disk, so check free -m and df -h, free disk with docker builder prune if it is full, and run sh tools/pg.sh up again."
  fail "Run docker logs $name for the whole log, then sh tools/pg.sh down to remove the harness."
}

# wait_ready waits until a server accepts connections on TCP inside its container, which the
# image's first-start server never does, so a ready answer comes from the final server.
wait_ready() {
  name="$prefix$1"
  while :; do
    running=$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null)
    [ "$running" = true ] || not_up "$1" "Postgres $1 did not start: its container $name stopped. Its last log lines follow."
    docker exec "$name" pg_isready -q -h 127.0.0.1 -p 5432 -U postgres >/dev/null 2>&1 && return 0
    [ "$waited" -ge "$wait_seconds" ] && not_up "$1" "Postgres $1 did not accept connections within $wait_seconds seconds. Its last log lines follow."
    sleep 1
    waited=$((waited + 1))
  done
}

# up starts the harness from scratch and writes the connection strings once every server is ready.
up() {
  need_docker up
  down
  check_disk
  for m in $majors; do pull "$m"; done

  password=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
  [ "${#password}" -eq 48 ] || fail "/dev/urandom gave no random bytes, so there is no password for the harness. Check that /dev/urandom is readable and run sh tools/pg.sh up again."

  for m in $majors; do start "$m"; done
  waited=0
  for m in $majors; do wait_ready "$m"; done

  mkdir -p .tools || fail "the folder .tools cannot be created, so the connection strings have nowhere to go. Check the permissions of the repository folder."
  tmp=$(umask 077 && mktemp "$env_file.XXXXXX") || fail "a file in .tools cannot be created, so the connection strings have nowhere to go. Check the permissions and free space of the repository folder."
  {
    echo "# Written by sh tools/pg.sh up and removed by sh tools/pg.sh down. Never commit it."
    for m in $majors; do
      addr=$(docker port "$prefix$m" 5432/tcp | head -n 1)
      port=${addr##*:}
      case "$port" in '' | *[!0-9]*) rm -f "$tmp"; fail "Docker reports no published port for $prefix$m. Run sh tools/pg.sh down, then sh tools/pg.sh up again." ;; esac
      printf 'PGTEST_PG%s=postgres://postgres:%s@127.0.0.1:%s/postgres?sslmode=disable\n' "$m" "$password" "$port"
      say "Postgres $m accepts connections on 127.0.0.1:$port"
    done
  } >"$tmp" || { rm -f "$tmp"; exit 1; }
  chmod 600 "$tmp" && mv -f "$tmp" "$env_file" || fail "$env_file cannot be written. Check the permissions and free space of the repository folder."
  say "the connection strings are in $env_file. Run go test -tags pg ./..., then sh tools/pg.sh down."
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  *) fail "the command is up or down. sh tools/pg.sh up starts the Postgres 14 to 18 harness, and sh tools/pg.sh down removes it." ;;
esac
