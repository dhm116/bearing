#!/bin/sh
# Starts and stops the Acme demo (docker compose) and the graph UI in
# web/graph (spike/graph-svelte). Usage: demo/demo.sh start|stop|reset|status|logs
set -eu
cd "$(dirname "$0")/.."

web=web/graph
pidfile=$web/.dev.pid
log=$web/.dev.log
port=${DEMO_UI_PORT:-5180}

die() {
	echo "demo: $*" >&2
	exit 1
}

ui_running() { [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; }

start() {
	command -v docker >/dev/null || die "docker not found"
	command -v bun >/dev/null || die "bun not found (https://bun.sh)"
	docker info >/dev/null 2>&1 || die "the Docker daemon is not running"
	[ -f .env ] || cp .env.example .env # demo values, not secrets

	echo "demo: starting Bearing and the Acme fakes"
	docker compose up -d --build --wait
	docker compose cp fakes:/certs/ca.pem "$web/demo-ca.pem" >/dev/null 2>&1

	if ui_running; then
		echo "demo: graph UI already running (pid $(cat "$pidfile"))"
	else
		echo "demo: starting the graph UI"
		(cd "$web" && bun install --frozen-lockfile >/dev/null 2>&1) || die "bun install failed in $web"
		token=$(grep '^BEARING_API_TOKEN=' .env | cut -d= -f2-)
		(
			cd "$web"
			BEARING_TOKEN=$token BEARING_CA=$PWD/demo-ca.pem \
				nohup bun --bun vite dev --port "$port" --strictPort >.dev.log 2>&1 &
			echo $! >.dev.pid
		)
		i=0
		until curl -fs -o /dev/null "http://localhost:$port/"; do
			i=$((i + 1))
			[ "$i" -lt 60 ] && ui_running || {
				tail -20 "$log" >&2
				die "the graph UI did not start; log: $log"
			}
			sleep 0.5
		done
	fi
	echo
	echo "  graph UI      http://localhost:$port"
	echo "  Bearing API   https://localhost:8443"
	echo "  demo story    http://127.0.0.1:9090/demo/steps"
	echo
	echo "Press Sync now in the UI to read the sources. 'make demo-logs' follows the"
	echo "logs; 'make demo-stop' stops it all."
}

stop() {
	if ui_running; then
		kill "$(cat "$pidfile")"
		echo "demo: graph UI stopped"
	fi
	rm -f "$pidfile"
	docker compose down "$@"
}

status() {
	if ui_running; then
		echo "graph UI: running on http://localhost:$port (pid $(cat "$pidfile"), log $log)"
	else
		echo "graph UI: stopped"
	fi
	docker compose ps --format '{{.Service}}: {{.Status}}'
}

# logs follows the graph UI's log (its server, and errors the browser
# reports) and Bearing's, until Ctrl-C.
logs() {
	[ -f "$log" ] || die "no UI log yet; run make demo-start"
	tail -n 40 -F "$log" | awk '{ print "ui      | " $0; fflush() }' &
	trap 'kill $! 2>/dev/null' EXIT INT TERM
	docker compose logs -f --tail 40 bearing
}

case ${1:-} in
start) start ;;
stop) stop ;;
reset) stop -v && echo "demo: data removed; the story starts over on the next start" ;;
status) status ;;
logs) logs ;;
*) die "usage: demo/demo.sh start|stop|reset|status|logs" ;;
esac
