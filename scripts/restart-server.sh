#!/bin/bash
# Restart the eufy-server safely: kill every running instance BY PID (never a
# blind name-pattern kill racing a start), wait for each to actually exit,
# then start one fully detached instance and wait for it to answer.
set -u
cd "$(dirname "$0")/.."

PIDFILE=/tmp/eufy-server.pid

kill_and_wait() {
    local pid=$1
    kill -0 "$pid" 2>/dev/null || return 0
    kill "$pid" 2>/dev/null
    for _ in $(seq 1 50); do
        kill -0 "$pid" 2>/dev/null || return 0
        sleep 0.2
    done
    kill -9 "$pid" 2>/dev/null
}

# Stop the pidfile instance first, then any other exact-name instances
# (e.g. orphans from earlier manual starts).
[ -f "$PIDFILE" ] && kill_and_wait "$(cat "$PIDFILE")"
rm -f "$PIDFILE"
for pid in $(pgrep -x eufy-server); do
    kill_and_wait "$pid"
done

setsid nohup ./eufy-server >> /tmp/eufy-server.log 2>&1 < /dev/null &
echo $! > "$PIDFILE"

# Wait for the web server to answer.
for _ in $(seq 1 60); do
    curl -s -o /dev/null "http://localhost:8080/api/login/status" && exit 0
    sleep 1
done
echo "server did not become ready" >&2
exit 1
