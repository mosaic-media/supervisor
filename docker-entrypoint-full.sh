#!/bin/sh
# Entrypoint for the `full` image (ADR 0121): PostgreSQL, then the Supervisor.
#
# **Its whole job is the ordering.** The database comes up before the Supervisor
# and goes down after it, so the Platform is already stopped when the database
# stops. Left to Docker the two would be signalled independently, and a Platform
# killed mid-transaction is the unclean stop that costs a recovery on the next
# boot — the exact thing the per-child stop grace exists to prevent.
#
# It is a shell script and not a process supervisor, deliberately. The process
# supervisor is the binary it starts; adding a second one to manage the first
# would be two things with the same job and a question about which owns a
# restart. If PostgreSQL dies here, this exits and the container restarts —
# which is the honest outcome, because everything above it is unusable.
set -eu

PGDATA="${PGDATA:-/var/lib/postgresql/data}"
PGBIN="$(ls -d /usr/lib/postgresql/*/bin | head -n 1)"
DB_NAME="${MOSAIC_DB_NAME:-mosaic}"
DB_USER="${MOSAIC_DB_USER:-mosaic}"

# A password is generated once and kept beside the data, never defaulted to a
# known value. The database listens on loopback only, so this is defence in
# depth rather than the boundary — but a shipped image with a published password
# is the kind of default that ends up reachable when somebody publishes the port.
PW_FILE="${PGDATA}/../mosaic-db-password"

log() { echo "mosaic-entrypoint: $*" >&2; }

if [ ! -s "${PGDATA}/PG_VERSION" ]; then
    log "initialising the database (first boot)"
    install -d -o postgres -g postgres -m 0700 "${PGDATA}"
    su postgres -c "${PGBIN}/initdb --auth-local=trust --auth-host=scram-sha-256 -D '${PGDATA}'" >/dev/null

    # Loopback only. Nothing outside this container has any business reaching
    # the database directly, and the Supervisor is the only public door
    # (ADR 0005).
    echo "listen_addresses = '127.0.0.1'" >> "${PGDATA}/postgresql.conf"
fi

if [ ! -s "${PW_FILE}" ]; then
    ( umask 077; head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > "${PW_FILE}" )
    chown postgres:postgres "${PW_FILE}"
fi
DB_PASSWORD="$(cat "${PW_FILE}")"

log "starting postgresql"
su postgres -c "${PGBIN}/pg_ctl -D '${PGDATA}' -o '-c listen_addresses=127.0.0.1' -w -t 60 start" >/dev/null

# Idempotent: an existing role and database are the ordinary case on every boot
# after the first, and both statements are written to say nothing when they are
# already true.
su postgres -c "psql -v ON_ERROR_STOP=1 --no-psqlrc -q" <<SQL
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${DB_USER}') THEN
    CREATE ROLE ${DB_USER} LOGIN PASSWORD '${DB_PASSWORD}';
  ELSE
    ALTER ROLE ${DB_USER} LOGIN PASSWORD '${DB_PASSWORD}';
  END IF;
END \$\$;
SQL
su postgres -c "psql -v ON_ERROR_STOP=1 --no-psqlrc -q -tAc \"SELECT 1 FROM pg_database WHERE datname='${DB_NAME}'\"" \
    | grep -q 1 \
    || su postgres -c "createdb -O '${DB_USER}' '${DB_NAME}'"

# The Platform reads this; the Supervisor passes its own environment to its
# children (ADR 0004), so setting it here is what reaches it.
export MOSAIC_POSTGRES_DSN="${MOSAIC_POSTGRES_DSN:-postgres://${DB_USER}:${DB_PASSWORD}@127.0.0.1:5432/${DB_NAME}?sslmode=disable}"

install -d -o mosaic -g mosaic -m 0755 /var/lib/mosaic /run/mosaic

# **The Supervisor runs in the background and this waits for it**, rather than
# exec'ing it — because exec would replace this process and there would be
# nothing left to stop the database afterwards. The cost is one extra process;
# what it buys is the ordering this file exists for.
log "starting the supervisor"
setpriv --reuid=mosaic --regid=mosaic --clear-groups \
    /usr/local/bin/mosaic-supervisor &
SUPERVISOR_PID=$!

stop() {
    log "stopping"
    # The Supervisor's own shutdown stops the Platform and the Shell in order
    # and waits for each. Only when it has finished is the database safe to
    # stop, which is the whole point of the sequence.
    kill -TERM "${SUPERVISOR_PID}" 2>/dev/null || true
    wait "${SUPERVISOR_PID}" 2>/dev/null || true
    log "stopping postgresql"
    su postgres -c "${PGBIN}/pg_ctl -D '${PGDATA}' -m fast -w -t 60 stop" >/dev/null 2>&1 || true
    exit 0
}
trap stop TERM INT

wait "${SUPERVISOR_PID}"
# Reached when the Supervisor exits on its own, which is a failure rather than a
# shutdown. The database is stopped cleanly on the way out regardless: whatever
# went wrong above it, a database that was not shut down costs a recovery.
log "the supervisor exited"
su postgres -c "${PGBIN}/pg_ctl -D '${PGDATA}' -m fast -w -t 60 stop" >/dev/null 2>&1 || true
exit 1
