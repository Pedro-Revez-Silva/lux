#!/usr/bin/env bash
# Branch previews, locally: a lux with previews on *.lux.localhost, a git
# server with a hello-world app, and the reference orchestrator
# (examples/preview-orchestrator) that wakes the preview on request and
# stops it when idle. See docs/development.md, "Try branch previews locally".
#
#   scripts/demo-wake.sh up        # bring it all up (detached), print the URL
#   scripts/demo-wake.sh open      # a fresh sign-in link for the preview (60s, single use)
#   scripts/demo-wake.sh push "hello from B"   # a new commit on main: synced into a running preview, else shown on the next wake
#   scripts/demo-wake.sh status    # the server and its Run
#   scripts/demo-wake.sh logs      # the orchestrator's log
#   scripts/demo-wake.sh down      # take it all down
#
# DEMO_IDLE (default 1m): how long without a request before the
# orchestrator stops the Run. DEMO_PREVIEW_PORT (default 8090): the
# preview listener's port on 127.0.0.1. LUX_TEST_PG_PORT and
# LUX_TEST_S3_PORT (the shared Postgres and S3, tests/env.py) are kept at
# up and used again by down, which drops the demo's database and bucket
# there.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PORT=${DEMO_PREVIEW_PORT:-8090}
IDLE=${DEMO_IDLE:-1m}
LAST=${LUX_TEST_LOG_ROOT:-/tmp}/lux-dev-env.json
HOSTNAME_=web.pr1.lux.localhost

env_file() {
	[[ -f $LAST ]] || { echo "no demo is up (scripts/demo-wake.sh up)" >&2; exit 1; }
	cat "$LAST"
}
field() { python3 -c "import json,sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])" "$(env_file)" "$1"; }
lux() { LUX_URL=$(field luxd_url) LUX_API_KEY=$(field api_key) "$ROOT/bin/lux" "$@"; }
git_container() { echo "lux-e2e-$(field run_id)-git"; }
ports_file() { echo "$(field log_dir)/demo-ports"; }

case ${1:-} in
up)
	if [[ ! -f $LAST ]]; then
		(cd "$ROOT/tests" && uv run python run_tests.py --serve --detach --preview-local "$PORT")
	fi
	run_id=$(field run_id)
	network=$(field network)
	log_dir=$(field log_dir)
	[[ -f $(ports_file) ]] ||
		printf 'export LUX_TEST_PG_PORT=%q LUX_TEST_S3_PORT=%q\n' "${LUX_TEST_PG_PORT:-}" "${LUX_TEST_S3_PORT:-}" >"$(ports_file)"
	# The git server: the e2e suite's (smart HTTP, a token as password), on
	# the environment's network, removed with it.
	docker build -q -t localhost/lux-gitserver:test -f "$ROOT/tests/images/gitserver/Containerfile" "$ROOT/tests/images/gitserver" >/dev/null
	token=ghp_demo$(date +%s)
	docker run -d --name "$(git_container)" --label "lux-e2e-run=$run_id" --network "$network" \
		-e "GIT_TOKEN=$token" localhost/lux-gitserver:test >/dev/null
	git_ip=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$network\").IPAddress}}" "$(git_container)")
	docker exec "$(git_container)" sh -c 'set -e; mkdir -p /tmp/w && cd /tmp/w && git init -q -b main &&
		git config user.email demo@lux && git config user.name demo &&
		echo "hello from commit A" > message.txt && git add -A && git commit -qm A &&
		git clone -q --bare /tmp/w /repos/app.git && git -C /repos/app.git config http.receivepack true'
	echo "$token" >"$log_dir/demo-git-token"
	# The orchestrator.
	(cd "$ROOT" && go build -o "$log_dir/preview-orchestrator" ./examples/preview-orchestrator)
	LUX_URL=$(field luxd_url) LUX_API_KEY=$(field api_key) GIT_TOKEN=$token \
		nohup "$log_dir/preview-orchestrator" -hostname "$HOSTNAME_" -name web -port 8080 \
		-command "lux-fake app 8080 /workspace/app /workspace/data/visits" \
		-image localhost/lux-fake:test -repo "http://$git_ip:8080/app.git" -branch main \
		-idle-after "$IDLE" -label pr=1 >"$log_dir/orchestrator.log" 2>&1 &
	echo $! >"$log_dir/orchestrator.pid"
	for _ in $(seq 50); do grep -qs "server srv_" "$log_dir/orchestrator.log" && break; sleep 0.2; done
	cat <<EOF

Branch preview demo is up.

  preview:      http://$HOSTNAME_:$PORT/   (asleep: no Run serves it yet)
  sign in:      scripts/demo-wake.sh open   (prints a link: open it within 60s)
  new commit:   scripts/demo-wake.sh push "hello from commit B"
  watch:        scripts/demo-wake.sh status; scripts/demo-wake.sh logs
                LUX_URL=$(field luxd_url) LUX_API_KEY=$(field api_key) bin/lux events --all
  idle after:   $IDLE without a request, the orchestrator stops the Run
  down:         scripts/demo-wake.sh down
EOF
	;;
open)
	read -r sid url < <(lux server show "$HOSTNAME_" -o json | python3 -c 'import json,sys; s=json.load(sys.stdin); print(s["id"], s["url"])')
	ticket=$(curl -fsS -X POST -H "Authorization: Bearer $(field api_key)" "$(field luxd_url)/v1/servers/$sid/tickets" |
		python3 -c 'import json,sys; print(json.load(sys.stdin)["ticket"])')
	echo "$url/.lux/auth?ticket=$ticket&to=/"
	;;
push)
	msg=${2:-hello from a new commit}
	docker exec "$(git_container)" sh -c "set -e; cd /tmp/w && echo '$msg' > message.txt && git commit -qam '$msg' &&
		git push -q /repos/app.git HEAD:main && git rev-parse --short HEAD"
	# What a forge webhook would make the owner do: a Run serving the
	# preview now moves its checkout to the new commit (lux sync); the
	# app shows it on the next reload. Asleep, nothing to do: the next
	# wake resumes on main's latest commit.
	run=$(lux server show "$HOSTNAME_" -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["runId"] or "")')
	if [[ -n $run ]] && lux sync "$run" app=main --wait >/dev/null 2>&1; then
		echo "synced the running preview ($run): reload the page" >&2
	else
		echo "the preview is not running: the next visit wakes it on this commit" >&2
	fi
	;;
status)
	lux server show "$HOSTNAME_"
	run=$(lux server show "$HOSTNAME_" -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["runId"] or "")')
	[[ -n $run ]] && lux get "$run" | head -5
	;;
logs)
	tail -n 50 "$(field log_dir)/orchestrator.log"
	;;
down)
	log_dir=$(field log_dir)
	[[ -f $log_dir/orchestrator.pid ]] && kill "$(cat "$log_dir/orchestrator.pid")" 2>/dev/null || true
	# The ports up ran with: teardown reaches the same Postgres and S3.
	if [[ -f $(ports_file) ]]; then
		# shellcheck disable=SC1090
		. "$(ports_file)"
		[[ -n $LUX_TEST_PG_PORT ]] || unset LUX_TEST_PG_PORT
		[[ -n $LUX_TEST_S3_PORT ]] || unset LUX_TEST_S3_PORT
	fi
	(cd "$ROOT/tests" && uv run python run_tests.py --down)
	;;
*)
	sed -n '2,16p' "$0"
	exit 2
	;;
esac
