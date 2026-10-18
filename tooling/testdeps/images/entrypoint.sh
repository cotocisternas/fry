#!/bin/sh
set -eu
if [ "${1:-}" != postgres ]; then exec "$@"; fi
: "${POSTGRES_USER:=postgres}" "${POSTGRES_DB:=fry_test}"
: "${POSTGRES_PASSWORD:?POSTGRES_PASSWORD must be set}"
if [ ! -s "$PGDATA/PG_VERSION" ]; then
  password_file=$(mktemp)
  trap 'rm -f "$password_file"' EXIT
  printf '%s\n' "$POSTGRES_PASSWORD" > "$password_file"
  initdb -D "$PGDATA" --username="$POSTGRES_USER" --pwfile="$password_file" \
    --auth-local=trust --auth-host=scram-sha-256 --encoding=UTF8 --locale=C.UTF-8
  printf '%s\n' 'host all all 0.0.0.0/0 scram-sha-256' 'host all all ::/0 scram-sha-256' >> "$PGDATA/pg_hba.conf"
  rm -f "$password_file"
  trap - EXIT
  pg_ctl -D "$PGDATA" -o "-c listen_addresses='' -c unix_socket_directories=/tmp" -w start
  createdb -h /tmp -U "$POSTGRES_USER" "$POSTGRES_DB"
  pg_ctl -D "$PGDATA" -m fast -w stop
fi
shift
exec postgres -D "$PGDATA" -c listen_addresses='*' -c unix_socket_directories=/tmp "$@"
