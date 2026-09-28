#!/usr/bin/env bash
# End-to-end test of the technical demo's acceptance criteria.
#
# It builds MockVision, creates an isolated virtual LAN (a veth pair) with a
# "client" device in its own network namespace, starts the node with
# `mockvision run` and checks, from the client: ping and MAC (M1), RTSP with
# ffprobe over TCP and UDP (M2), the HTTP API with Digest (M3), a
# line-crossing event (M4), admission (M7), the privileges of the service
# and camera processes, rules and triggers (manual and random), the counts
# and heat map the camera derives from its events, editing a
# running camera (accounts, stream and address), sub and third streams in
# H.264, H.265 and MJPEG, API tokens
# with bulk actions and the audit log, background jobs, the outbound
# firewall, the MAC probe, DHCP with the factory address as fallback, the
# node bridge, ipvlan where the kernel has it, clean stop, and cameras
# coming back after a restart.
#
# Run as root on Linux with iproute2, ffmpeg, curl, ping and python3:
#   sudo backend/e2e/run.sh
#
# E2E_BIN=path uses an already built binary instead of building one.
#
# E2E_MODE=compose runs the same checks against the Docker image started
# with the compose.yaml at the root (it builds the image unless
# E2E_NO_BUILD=1 and the image exists):
#   sudo E2E_MODE=compose backend/e2e/run.sh
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
WORK=$(mktemp -d /tmp/mockvision-e2e.XXXXXX)
chmod 755 "$WORK"
BIN=$WORK/mockvision
LAN=mve2e0
CLIENT_NS=mve2e-client
CLIENT_IP=10.77.0.2
CAM_IP=10.77.0.10
CAM2_IP=10.77.0.11
CAM3_IP=10.77.0.12
PORT=${E2E_PORT:-18090}
API=http://127.0.0.1:$PORT/api/v1
RUN_PID=
MODE=${E2E_MODE:-binary}
COMPOSE=(docker compose -f "$ROOT/compose.yaml" -p mockvision-e2e)

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { printf '   ok: %s\n' "$*"; }
fail() {
	printf '\n\033[31mFAIL: %s\033[0m\n' "$*" >&2
	echo "--- node log (last 40 lines)" >&2
	node_log | tail -n 40 >&2 || true
	exit 1
}
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
api() {
	local method=$1 path=$2
	shift 2
	curl -sS -b "$WORK/cookies" -c "$WORK/cookies" -H 'X-MockVision-Request: 1' -X "$method" "$API$path" "$@"
}
client() { ip netns exec "$CLIENT_NS" "$@"; }

# The node's processes and namespaces live in the container in compose mode.
node_exec() {
	if [ "$MODE" = compose ]; then "${COMPOSE[@]}" exec -T mockvision "$@"; else "$@"; fi
}
node_log() {
	if [ "$MODE" = compose ]; then "${COMPOSE[@]}" logs --no-color 2>/dev/null; else cat "$WORK/node.log"; fi
}

cleanup() {
	set +e
	[ -n "$RUN_PID" ] && kill "$RUN_PID" 2>/dev/null && wait "$RUN_PID" 2>/dev/null
	[ "$MODE" = compose ] && "${COMPOSE[@]}" down --volumes --timeout 30 >/dev/null 2>&1
	# ip netns exec may fork: stop every process of the client namespace.
	ip netns pids "$CLIENT_NS" 2>/dev/null | xargs -r kill 2>/dev/null
	ip netns del "$CLIENT_NS" 2>/dev/null
	ip link del "$LAN" 2>/dev/null
	if [ -z "${E2E_KEEP:-}" ]; then rm -rf "$WORK"; else echo "kept $WORK"; fi
}
trap cleanup EXIT

[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 2; }
case "$MODE" in
binary) tools="ip ffprobe ffmpeg curl ping python3" ;;
compose) tools="ip ffprobe curl ping python3 docker" ;;
*) echo "E2E_MODE must be binary or compose" >&2; exit 2 ;;
esac
for tool in $tools; do
	command -v "$tool" >/dev/null || { echo "missing $tool" >&2; exit 2; }
done

wait_status() { # camera, Python expression over the camera d, value, seconds
	local i got
	for ((i = 0; i < $4 * 2; i++)); do
		got=$(api GET "/cameras/$1" | json "$2")
		[ "$got" = "$3" ] && return 0
		sleep 0.5
	done
	fail "camera $1: $2 is $got, want $3"
}

wait_received() { # event ID: the client's target got it
	for _ in $(seq 1 40); do
		grep -q "$1" "$WORK/received.log" 2>/dev/null && return 0
		sleep 0.25
	done
	fail "the target did not receive event $1"
}

wait_state() { # camera, state, seconds
	local i state
	for ((i = 0; i < $3 * 2; i++)); do
		state=$(api GET "/cameras/$1" | json 'd["status"]["state"]')
		[ "$state" = "$2" ] && return 0
		if [ "$state" = error ]; then
			fail "camera went to error: $(api GET "/cameras/$1" | json 'd["status"]["reason"]')"
		fi
		sleep 0.5
	done
	fail "camera $1 did not reach $2 (last: $state)"
}

start_node() {
	if [ "$MODE" = compose ]; then
		MOCKVISION_LISTEN=127.0.0.1:$PORT MOCKVISION_PARENT_IF=$LAN "${COMPOSE[@]}" up -d --no-build >/dev/null 2>&1 ||
			fail "docker compose up failed"
	else
		# The test host has no mockvision users: numeric ones, as the image's.
		MOCKVISION_DATA=$WORK/data MOCKVISION_LISTEN=127.0.0.1:$PORT MOCKVISION_PARENT_IF=$LAN \
			MOCKVISION_SERVICE_USER=10001 MOCKVISION_CAMERA_USER=10002 \
			"$BIN" run >>"$WORK/node.log" 2>&1 &
		RUN_PID=$!
	fi
	for _ in $(seq 1 120); do
		curl -s -o /dev/null "$API/auth/me" && return 0
		sleep 0.25
	done
	fail "the node did not start"
}

stop_node() {
	if [ "$MODE" = compose ]; then
		"${COMPOSE[@]}" stop --timeout 30 >/dev/null 2>&1 || fail "docker compose stop failed"
	else
		kill "$RUN_PID"
		wait "$RUN_PID" || true
		RUN_PID=
	fi
}

# Named namespaces are listed by ip netns; where mounting is not allowed
# (Docker's AppArmor profile) cameras use anonymous namespaces instead.
netns_named() { node_exec test -e "/run/netns/$1"; }

step "build"
if [ "$MODE" = compose ]; then
	if [ -z "${E2E_NO_BUILD:-}" ] || ! docker image inspect mockvision:latest >/dev/null 2>&1; then
		"${COMPOSE[@]}" build >"$WORK/build.log" 2>&1 || { tail -n 40 "$WORK/build.log" >&2; fail "docker compose build failed"; }
	fi
	ok "image mockvision:latest, $(docker run --rm mockvision:latest version)"
	# A run that was interrupted may have left its container and volume.
	"${COMPOSE[@]}" down --volumes >/dev/null 2>&1 || true
elif [ -n "${E2E_BIN:-}" ]; then
	cp "$E2E_BIN" "$BIN"
	ok "$("$BIN" version) ($E2E_BIN)"
else
	(cd "$ROOT" && CGO_ENABLED=0 go build -o "$BIN" ./backend/cmd/mockvision)
	ok "$("$BIN" version)"
fi

step "isolated virtual LAN with a client device"
# Every interface gets its MAC here: udev replaces a MAC the kernel made up
# (MACAddressPolicy=persistent) moments after the interface appears.
ip link add "$LAN" address 02:e2:e0:00:00:01 type veth peer name "${LAN}p" address 02:e2:e0:00:00:02
ip link set "$LAN" up
ip link set "${LAN}p" up
ip netns add "$CLIENT_NS"
ip link add mve2ecl link "$LAN" address 02:e2:e0:00:00:03 type macvlan mode bridge
ip link set mve2ecl netns "$CLIENT_NS"
client ip link set lo up
client ip link set mve2ecl name eth0
client ip addr add "$CLIENT_IP/24" dev eth0
client ip link set eth0 up
client python3 "$ROOT/backend/e2e/receiver.py" 9000 "$WORK/received.log" </dev/null >/dev/null 2>&1 &
ok "client $CLIENT_IP on $LAN"

step "start the node (network helper + unprivileged service)"
start_node
setup=$(curl -s "$API/auth/me" | json 'd.get("setup_required")')
[ "$setup" = True ] || fail "a fresh node must require setup"
ok "first run asks for the administrator"

step "first login creates the administrator with the setup code"
code=$(api POST /auth/setup -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-1"}' -o /dev/null -w '%{http_code}')
[ "$code" = 403 ] || fail "setup without the setup code answered $code"
if [ "$MODE" = compose ]; then
	# Root holds no capability in the container: read it as the service.
	SETUP_CODE=$("${COMPOSE[@]}" exec -T -u 10001 mockvision cat /data/setup-code | tr -d '[:space:]')
else
	SETUP_CODE=$(tr -d '[:space:]' <"$WORK/data/setup-code")
fi
node_log | grep -q "setup_code=$SETUP_CODE" || fail "the setup code is not in the node's log"
api POST /auth/setup -H 'Content-Type: application/json' -d "{\"username\":\"admin\",\"password\":\"e2e-password-1\",\"setup_code\":\"$SETUP_CODE\"}" |
	json 'd["user"]["username"]' | grep -qx admin || fail "setup"
if [ "$MODE" = compose ]; then node_exec test ! -e /data/setup-code; else [ ! -e "$WORK/data/setup-code" ]; fi ||
	fail "the setup code outlived the setup"
ok "setup needs the one-time code from the log; admin created and logged in"

step "import profiles/milesight-demo.yaml"
level=$(api POST /packages -F "file=@$ROOT/profiles/milesight-demo.yaml" | json 'd["profile"]["level"]')
[ "$level" = draft ] || fail "expected level draft, got $level"
ok "validated and listed as draft (Borrador)"

step "event target on the client and a camera with a fixed IP"
TID=$(api POST /targets -H 'Content-Type: application/json' -d "{\"name\":\"client\",\"url\":\"http://$CLIENT_IP:9000/events\"}" | json 'd["id"]')
CID=$(api POST /cameras -H 'Content-Type: application/json' -d "{
	\"name\": \"Gate 1\", \"profile_id\": \"milesight/demo\", \"profile_version\": \"0.4.0\",
	\"network\": {\"ip\": \"$CAM_IP\", \"netmask\": \"255.255.255.0\"},
	\"users\": [{\"username\": \"admin\", \"password\": \"e2e-cam-pw\", \"role\": \"admin\"}],
	\"stream\": {\"resolution\": \"640x360\"}, \"target_ids\": [\"$TID\"], \"start\": true}" | json 'd["id"]')
wait_state "$CID" running 90
CAM=$(api GET "/cameras/$CID")
MAC=$(echo "$CAM" | json 'd["network"]["mac"]')
NETNS=$(echo "$CAM" | json 'd["status"]["netns"]')
PID=$(echo "$CAM" | json 'd["status"]["pid"]')
SERIAL=$(echo "$CAM" | json 'd["serial"]')
ok "camera $CID running in $NETNS (pid $PID, MAC $MAC)"

step "M1: another device pings the camera and sees its own MAC"
client ping -c 2 -W 2 "$CAM_IP" >/dev/null || fail "ping $CAM_IP"
NEIGH=$(client ip neigh show "$CAM_IP" | awk '{print $5}')
NODE_MAC=$(cat "/sys/class/net/$LAN/address")
[ "$NEIGH" = "$MAC" ] || fail "ip neigh shows $NEIGH, want $MAC"
[ "$NEIGH" != "$NODE_MAC" ] || fail "the camera answers with the node's MAC"
ok "ping answers; ip neigh shows $NEIGH (node: $NODE_MAC)"
if netns_named "$NETNS"; then
	node_exec ip netns list | grep -q "^$NETNS" || fail "namespace $NETNS is not listed by ip netns"
	ok "ip netns exec $NETNS works for debugging"
elif [ "$MODE" = compose ]; then
	ok "anonymous namespace (the container may not mount; ip netns does not list it)"
else
	fail "namespace $NETNS is not listed by ip netns"
fi

step "M2: ffprobe reads the RTSP stream at the configured resolution"
for transport in tcp udp; do
	out=$(client ffprobe -v error -rtsp_transport "$transport" -select_streams v:0 \
		-show_entries stream=codec_name,width,height -of csv=p=0 "rtsp://admin:e2e-cam-pw@$CAM_IP:554/main") || fail "ffprobe over $transport"
	[ "$out" = "h264,640,360" ] || fail "ffprobe over $transport: $out"
	ok "$transport: $out"
done

step "M3: HTTP API with Digest"
code=$(client curl -s -o "$WORK/snap.jpg" -w '%{http_code} %{content_type}' --digest -u admin:e2e-cam-pw "http://$CAM_IP/snapshot.cgi")
[ "$code" = "200 image/jpeg" ] || fail "snapshot: $code"
head -c 2 "$WORK/snap.jpg" | od -An -tx1 | grep -q "ff d8" || fail "snapshot is not a JPEG"
ok "snapshot.cgi returns a JPEG"
code=$(client curl -s -o /dev/null -w '%{http_code}' --digest -u admin:wrong "http://$CAM_IP/snapshot.cgi")
[ "$code" = 401 ] || fail "wrong password answered $code"
ok "wrong credentials get 401"
info=$(client curl -s --digest -u admin:e2e-cam-pw "http://$CAM_IP/cgi-bin/operator/operator.cgi?action=get.system.information")
[ "$(echo "$info" | json 'd["serialNumber"]')" = "$SERIAL" ] || fail "device info: $info"
ok "device information carries serial $SERIAL"
client curl -s --digest -u admin:e2e-cam-pw "http://$CAM_IP/cgi-bin/operator/param.cgi?action=set&Image.Brightness=70" | grep -qx OK || fail "param set"
got=$(client curl -s --digest -u admin:e2e-cam-pw "http://$CAM_IP/cgi-bin/operator/param.cgi?action=get&name=Image.Brightness")
[ "$got" = "Image.Brightness=70" ] || fail "param get: $got"
sleep 0.5
origin=$(api GET "/cameras/$CID/config" | json '[p["origin"] for p in d["params"] if p["key"]=="Image.Brightness"][0]')
[ "$origin" = "client:$CLIENT_IP" ] || fail "change origin: $origin"
ok "parameter written by the client is persisted with origin $origin"

step "M4: line crossing reaches the target and the delivery is logged"
[ "$(api GET "/cameras/$CID" | json '",".join(r["name"] for r in d["rules"])')" = "Line 1,Region 1" ] ||
	fail "the camera lacks its profile's factory rules"
EID=$(api POST "/cameras/$CID/events" -H 'Content-Type: application/json' -d '{"type":"line_crossing"}' | json 'd["id"]')
for _ in $(seq 1 20); do
	grep -q "$EID" "$WORK/received.log" 2>/dev/null && break
	sleep 0.25
done
grep -q "$EID" "$WORK/received.log" || fail "the target did not receive $EID"
grep "$EID" "$WORK/received.log" | grep -q '"name":"Line 1","type":"line"' || fail "the crossing is not on the factory line"
sleep 0.5
ev=$(api GET "/events/$EID")
[ "$(echo "$ev" | json 'd["delivery_status"]')" = ok ] || fail "delivery: $ev"
ok "event $EID delivered, latency $(echo "$ev" | json 'd["latency_ms"]') ms"

step "rules and triggers: events name their rule, a random trigger keeps sending (D39, D40)"
rules=$(api PUT "/cameras/$CID/rules" -H 'Content-Type: application/json' -d '{"rules":[
	{"name":"Gate line","type":"line","points":[{"x":0.1,"y":0.6},{"x":0.9,"y":0.6}],"direction":"A->B"},
	{"name":"Lot","type":"region","points":[{"x":0.2,"y":0.2},{"x":0.6,"y":0.2},{"x":0.6,"y":0.5},{"x":0.2,"y":0.5}],
	 "events":["region_entrance","loitering"],"object_classes":["car"]}]}')
LINE_ID=$(echo "$rules" | json '[r["id"] for r in d["rules"] if r["name"]=="Gate line"][0]') || fail "rules not saved: $rules"
LOT_ID=$(echo "$rules" | json '[r["id"] for r in d["rules"] if r["name"]=="Lot"][0]')
EID=$(api POST "/cameras/$CID/events" -H 'Content-Type: application/json' -d '{"type":"loitering"}' | json 'd["id"]')
wait_received "$EID"
case "$(grep "$EID" "$WORK/received.log")" in
*'"name":"Lot","type":"region"'*'"class":"car"'*) ;;
*) fail "loitering payload: $(grep "$EID" "$WORK/received.log")" ;;
esac
[ "$(api GET "/events/$EID" | json 'd["rule_id"]')" = "$LOT_ID" ] || fail "the event does not name rule Lot"
ok "a manual loitering happens on region Lot, with a car, the only object the rule detects"
code=$(api POST "/cameras/$CID/events" -H 'Content-Type: application/json' -o /dev/null -w '%{http_code}' \
	-d "{\"type\":\"line_crossing\",\"rule_id\":\"$LINE_ID\",\"direction\":\"B->A\"}")
[ "$code" = 422 ] || fail "a crossing the line does not report answered $code"
client curl -s --digest -u admin:e2e-cam-pw "http://$CAM_IP/cgi-bin/operator/param.cgi?action=set&Event.LineCrossing.Enable=false" |
	grep -qx OK || fail "switching crossings off from the emulated API"
code=$(api POST "/cameras/$CID/events" -H 'Content-Type: application/json' -d '{"type":"line_crossing"}' -o /dev/null -w '%{http_code}')
[ "$code" = 409 ] || fail "a crossing with detection off answered $code"
client curl -s --digest -u admin:e2e-cam-pw "http://$CAM_IP/cgi-bin/operator/param.cgi?action=set&Event.LineCrossing.Enable=true" |
	grep -qx OK || fail "switching crossings on from the emulated API"
ok "a crossing the line does not report is refused; a client of the emulated API turns crossings off and on"
trigger='"name":"Traffic","event_type":"line_crossing","rule_id":"'$LINE_ID'","min_seconds":1,"max_seconds":1'
TRG_ID=$(api PUT "/cameras/$CID/triggers" -H 'Content-Type: application/json' -d "{\"triggers\":[{$trigger}]}" | json 'd["triggers"][0]["id"]')
crossings() { grep -c '"name":"Gate line"' "$WORK/received.log" || true; }
for _ in $(seq 1 60); do
	[ "$(crossings)" -ge 3 ] && break
	sleep 0.25
done
[ "$(crossings)" -ge 3 ] || fail "the random trigger sent $(crossings) crossings"
grep '"name":"Gate line"' "$WORK/received.log" | grep -qv '"direction":"A->B"' && fail "a crossing went the way the line does not report"
from=$(api GET "/events?camera_id=$CID&limit=50" | json "sum(1 for e in d['items'] if e.get('trigger_id') == '$TRG_ID')")
[ "$from" -ge 3 ] || fail "$from stored events name the trigger"
ok "a random trigger sends a crossing on Gate line every second or two; $from events name it"
api PUT "/cameras/$CID/triggers" -H 'Content-Type: application/json' -d "{\"triggers\":[{\"id\":\"$TRG_ID\",$trigger,\"enabled\":false}]}" >/dev/null
sleep 1.5
before=$(crossings)
sleep 3
[ "$(crossings)" = "$before" ] || fail "a disabled trigger kept sending"
FID=$(api POST "/cameras/$CID/triggers/$TRG_ID/actions/fire" | json 'd["id"]')
wait_received "$FID"
[ "$(api GET "/events/$FID" | json 'd["trigger_id"]')" = "$TRG_ID" ] || fail "the fired event does not name its trigger"
[ "$(api GET "/cameras/$CID" | json 'd["status"]["pid"]')" = "$PID" ] || fail "rules and triggers restarted the camera"
ok "disabled, the trigger stops; fired by hand, it sends one; the camera never restarted (pid $PID)"

step "analytics: counts, occupancy and a heat map from the camera's events, served and reported"
api POST "/cameras/$CID/analytics/actions/reset" >/dev/null
for body in "{\"type\":\"line_crossing\",\"rule_id\":\"$LINE_ID\"}" "{\"type\":\"region_entrance\",\"rule_id\":\"$LOT_ID\"}" \
	"{\"type\":\"line_crossing\",\"rule_id\":\"$LINE_ID\"}"; do
	# One crossing a second at most, as the profile says.
	sleep 1.1
	api POST "/cameras/$CID/events" -H 'Content-Type: application/json' -d "$body" >/dev/null
done
counts=$(api GET "/cameras/$CID/analytics?cols=8&rows=4")
[ "$(echo "$counts" | json '[(l["a_to_b"], l["b_to_a"]) for l in d["lines"] if l["name"] == "Gate line"][0]')" = "(2, 0)" ] ||
	fail "line counts: $counts"
[ "$(echo "$counts" | json '[(r["entries"], r["occupancy"]) for r in d["regions"] if r["name"] == "Lot"][0]')" = "(1, 1)" ] ||
	fail "region counts: $counts"
[ "$(echo "$counts" | json 'sum(d["heat"]["cells"])')" = 3 ] || fail "heat map: $counts"
ok "the node reads 2 crossings on Gate line, 1 car in Lot and 3 objects on the heat map"
served=$(client curl -s --digest -u admin:e2e-cam-pw "http://$CAM_IP/cgi-bin/operator/operator.cgi?action=get.vca.counting")
[ "$(echo "$served" | json '[l["in"] for l in d["lines"] if l["name"] == "Gate line"][0]')" = 2 ] || fail "counting over the emulated API: $served"
heat=$(client curl -s --digest -u admin:e2e-cam-pw "http://$CAM_IP/cgi-bin/operator/operator.cgi?action=get.vca.heatmap&cols=16&rows=9")
[ "$(echo "$heat" | json '(d["cols"], d["rows"], sum(map(sum, d["data"])))')" = "(16, 9, 3)" ] || fail "heat map over the emulated API: $heat"
ok "a client of the emulated API reads the same counts and a 16x9 heat map"
report='"name":"Counting","event_type":"custom:people_counting","min_seconds":2,"max_seconds":2'
api PUT "/cameras/$CID/triggers" -H 'Content-Type: application/json' \
	-d "{\"triggers\":[{\"id\":\"$TRG_ID\",$trigger,\"enabled\":false},{$report}]}" >/dev/null
for _ in $(seq 1 40); do
	grep -q '"eventType":"PeopleCounting"' "$WORK/received.log" && break
	sleep 0.25
done
grep '"eventType":"PeopleCounting"' "$WORK/received.log" | grep -q '"name":"Gate line","in":2' ||
	fail "people counting report: $(grep PeopleCounting "$WORK/received.log" | head -1)"
api PUT "/cameras/$CID/triggers" -H 'Content-Type: application/json' -d "{\"triggers\":[{\"id\":\"$TRG_ID\",$trigger,\"enabled\":false}]}" >/dev/null
ok "a report trigger with a fixed interval pushes the counts to the target"

step "v1 editing: accounts and stream apply without a restart"
probe() { # password -> codec,width,height
	client ffprobe -v error -rtsp_transport tcp -select_streams v:0 \
		-show_entries stream=codec_name,width,height -of csv=p=0 "rtsp://admin:$1@$CAM_IP:554/main" 2>/dev/null
}
api PUT "/cameras/$CID/users" -H 'Content-Type: application/json' \
	-d '{"users":[{"username":"admin","password":"e2e-new-pw","role":"admin"},{"username":"viewer","password":"e2e-view","role":"viewer"}]}' |
	json 'len(d["users"])' | grep -qx 2 || fail "accounts not saved"
[ "$(probe e2e-new-pw)" = "h264,640,360" ] || fail "RTSP refused the new password"
probe e2e-cam-pw >/dev/null && fail "RTSP still accepts the old password"
code=$(client curl -s -o /dev/null -w '%{http_code}' --digest -u viewer:e2e-view "http://$CAM_IP/snapshot.cgi")
[ "$code" = 200 ] || fail "the new account got $code"
code=$(client curl -s -o /dev/null -w '%{http_code}' --digest -u viewer:e2e-view "http://$CAM_IP/cgi-bin/operator/param.cgi?action=set&Image.Brightness=10")
[ "$code" = 403 ] || fail "a viewer account changed a parameter ($code)"
ok "new password and new account work at once; the old password is refused; a viewer cannot change settings"
api PATCH "/cameras/$CID/streams/main" -H 'Content-Type: application/json' -d '{"resolution":"1280x720"}' >/dev/null
for _ in $(seq 1 120); do
	[ "$(probe e2e-new-pw)" = "h264,1280,720" ] && break
	sleep 0.5
done
[ "$(probe e2e-new-pw)" = "h264,1280,720" ] || fail "the stream did not switch to 1280x720"
[ "$(api GET "/cameras/$CID" | json 'd["status"]["pid"]')" = "$PID" ] || fail "the camera restarted"
ok "ffprobe reads 1280x720 from the same camera process (pid $PID)"

step "streams and codecs: sub and third streams, H.265 on the fly (D35)"
probe_path() { # path -> codec,width,height
	client ffprobe -v error -rtsp_transport tcp -select_streams v:0 \
		-show_entries stream=codec_name,width,height -of csv=p=0 "rtsp://admin:e2e-new-pw@$CAM_IP:554$1" 2>/dev/null
}
urls=$(api GET "/cameras/$CID" | json '" ".join(s["name"] + "=" + s.get("url", "") for s in d["streams"])')
[ "$urls" = "main=rtsp://$CAM_IP/main sub=rtsp://$CAM_IP/sub third=rtsp://$CAM_IP/third" ] || fail "stream URLs: $urls"
[ "$(probe_path /sub)" = "h264,640,360" ] || fail "sub stream: $(probe_path /sub)"
[ "$(probe_path /third)" = "mjpeg,640,360" ] || fail "third stream: $(probe_path /third)"
ok "the camera serves /main, /sub (H.264 640x360) and /third (MJPEG 640x360)"
client ffmpeg -v error -rtsp_transport tcp -i "rtsp://admin:e2e-new-pw@$CAM_IP:554/third" -frames:v 10 -f null - 2>"$WORK/third.err" ||
	fail "decoding the MJPEG stream: $(cat "$WORK/third.err")"
[ ! -s "$WORK/third.err" ] || fail "decoding the MJPEG stream: $(cat "$WORK/third.err")"
ok "10 MJPEG frames decode without errors"
api PATCH "/cameras/$CID/streams/sub" -H 'Content-Type: application/json' -d '{"codec":"h265","resolution":"320x180"}' >/dev/null
for _ in $(seq 1 120); do
	[ "$(probe_path /sub)" = "hevc,320,180" ] && break
	sleep 0.5
done
[ "$(probe_path /sub)" = "hevc,320,180" ] || fail "the sub stream did not switch to H.265: $(probe_path /sub)"
client ffmpeg -v error -rtsp_transport tcp -i "rtsp://admin:e2e-new-pw@$CAM_IP:554/sub" -frames:v 25 -f null - 2>"$WORK/sub.err" ||
	fail "decoding the H.265 stream: $(cat "$WORK/sub.err")"
[ ! -s "$WORK/sub.err" ] || fail "decoding the H.265 stream: $(cat "$WORK/sub.err")"
[ "$(api GET "/cameras/$CID" | json 'd["status"]["pid"]')" = "$PID" ] || fail "the camera restarted"
ok "the sub stream switched to H.265 320x180 and decodes past its GOP, same process"
code=$(api GET "/cameras/$CID/snapshot?stream=third" -o "$WORK/third.jpg" -w '%{http_code}')
[ "$code" = 200 ] && head -c 2 "$WORK/third.jpg" | od -An -tx1 | grep -q "ff d8" || fail "snapshot of the third stream: $code"
ok "the panel shows the snapshot of each stream"

step "the main service and the camera hold no capabilities"
check_privileges() { # pid, label
	local status caps uid
	status=$(node_exec cat "/proc/$1/status") || fail "cannot read the status of $2 (pid $1)"
	caps=$(echo "$status" | grep -E '^Cap(Inh|Prm|Eff|Bnd|Amb)')
	echo "$caps" | sed 's/^/   /'
	echo "$caps" | awk '{print $2}' | grep -qv '^0*$' && fail "$2 keeps capabilities"
	echo "$status" | grep -q 'NoNewPrivs:\s*1' || fail "$2: no_new_privs not set"
	uid=$(echo "$status" | awk '/^Uid:/{print $2}')
	[ "$uid" != 0 ] || fail "$2 runs as root"
	ok "$2 (pid $1): all capability sets are empty, uid $uid, no_new_privs"
}
SVC_PID=$(node_exec sh -c 'for d in /proc/[0-9]*; do
	tr "\0" " " 2>/dev/null <"$d/cmdline" | grep -q "^[^ ]*mockvision serve --helper-fd " && basename "$d"
done; true' | head -n 1) || true
[ -n "$SVC_PID" ] || fail "the main service process was not found"
check_privileges "$SVC_PID" "main service"
check_privileges "$PID" "camera"
cam_status=$(node_exec cat "/proc/$PID/status")
echo "$cam_status" | grep -q 'Seccomp:\s*2' || fail "the camera has no seccomp filter"
echo "$cam_status" | awk '/^NSpid:/{exit !(NF == 3 && $3 == 1)}' || fail "the camera does not run in its own PID namespace"
ok "camera: seccomp filter on, PID 1 of its own PID namespace"

step "M7: metrics and admission"
metrics=$(api GET /node/metrics)
echo "$metrics" | json "d['cameras']['$CID']['rss_bytes']" >/dev/null || fail "no metrics for the camera"
ok "camera RSS $(echo "$metrics" | json "round(d['cameras']['$CID']['rss_bytes']/1048576,1)") MiB, CPU $(echo "$metrics" | json "round(d['cameras']['$CID']['cpu_percent'],2)") %"
api PATCH /settings -H 'Content-Type: application/json' -d '{"max_cameras":1}' >/dev/null
resp=$(api POST /cameras -H 'Content-Type: application/json' -d "{\"name\":\"Gate 2\",\"profile_id\":\"milesight/demo\",\"profile_version\":\"0.4.0\",\"network\":{\"ip\":\"$CAM2_IP\"}}")
[ "$(echo "$resp" | json 'd.get("code")')" = max_cameras ] || fail "creation over the maximum was not rejected: $resp"
ok "rejected: $(echo "$resp" | json 'd["detail"]')"
api PATCH /settings -H 'Content-Type: application/json' -d '{"max_cameras":100}' >/dev/null

step "API tokens: bulk clone and delete from automation, with the audit"
SECRET=$(api POST /tokens -H 'Content-Type: application/json' -d '{"name":"e2e-ci","scopes":["write"]}' | json 'd["secret"]')
READ=$(api POST /tokens -H 'Content-Type: application/json' -d '{"name":"e2e-read","scopes":["read"]}' | json 'd["secret"]')
tok() { # secret, method, path, curl arguments
	local secret=$1 method=$2 path=$3
	shift 3
	curl -sS -H "Authorization: Bearer $secret" -X "$method" "$API$path" "$@"
}
[ "$(tok "$READ" GET '/cameras?state=running' | json 'len(d["items"])')" = 1 ] || fail "a read token cannot list the running cameras"
code=$(tok "$READ" POST "/cameras/$CID/actions/stop" -o /dev/null -w '%{http_code}')
[ "$code" = 403 ] || fail "a read token stopped a camera: $code"
ok "a read token lists cameras and cannot change them"
res=$(tok "$SECRET" POST /cameras/actions/bulk -H 'Content-Type: application/json' -d "{\"action\":\"clone\",\"ids\":[\"$CID\"],\"start\":true}")
[ "$(echo "$res" | json 'd["succeeded"]')" = 1 ] || fail "bulk clone: $res"
COPY=$(echo "$res" | json 'd["results"][0]["camera"]["id"]')
COPY_IP=$(echo "$res" | json 'd["results"][0]["camera"]["network"]["ip"]')
[ "$COPY_IP" = "$CAM2_IP" ] || fail "the copy took $COPY_IP, want the next free address $CAM2_IP"
wait_state "$COPY" running 60
client ping -c 2 -W 2 "$COPY_IP" >/dev/null || fail "the copy does not answer on $COPY_IP"
ok "a write token cloned Gate 1 as $(echo "$res" | json 'd["results"][0]["camera"]["name"]') on $COPY_IP; the client reaches it"
res=$(tok "$SECRET" POST /cameras/actions/bulk -H 'Content-Type: application/json' -d "{\"action\":\"delete\",\"ids\":[\"$COPY\",\"missing\"]}")
[ "$(echo "$res" | json '(d["succeeded"], d["failed"], d["results"][1]["error"]["status"])')" = "(1, 1, 404)" ] || fail "bulk delete: $res"
client ping -c 1 -W 1 "$COPY_IP" >/dev/null 2>&1 && fail "the deleted copy still answers"
ok "bulk delete removed the copy and reported the unknown camera on its own"
api GET '/audit?origin=api' | json '[e["action"] for e in d["items"] if e["token"]["name"] == "e2e-ci"]' | grep -q camera.clone ||
	fail "the token's clone is not audited as coming from the API"
by_client=$(api GET "/audit?origin=camera&entity_id=$CID" | json 'd["items"][0]["origin_ip"]')
[ "$by_client" = "$CLIENT_IP" ] || fail "the change through the emulated API is not audited from $CLIENT_IP: $by_client"
ok "the audit shows the token's changes as API and the client's as coming from $CLIENT_IP"
TOKEN_ID=$(api GET /tokens | json '[t["id"] for t in d["items"] if t["name"] == "e2e-ci"][0]')
api DELETE "/tokens/$TOKEN_ID" -o /dev/null
code=$(tok "$SECRET" GET /cameras -o /dev/null -w '%{http_code}')
[ "$code" = 401 ] || fail "a revoked token got $code"
ok "a revoked token is refused at once"

step "background jobs: import, renditions and a preparation (D72, D74)"
[ "$(api GET '/jobs?type=import' | json 'd["items"][-1]["status"]')" = completed ] || fail "the import did not run as a completed job"
[ "$(api GET '/jobs?type=rendition&status=completed' | json 'len(d["items"])')" -ge 1 ] || fail "no rendition was encoded as a job"
JID=$(api POST /jobs -H 'Content-Type: application/json' -d '{"type":"renditions.prepare"}' | json 'd["id"]')
for _ in $(seq 1 120); do
	st=$(api GET "/jobs/$JID" | json 'd["status"]')
	case "$st" in completed | failed | canceled) break ;; esac
	sleep 0.5
done
[ "$st" = completed ] || fail "renditions.prepare ended $st: $(api GET "/jobs/$JID")"
ok "import and renditions ran as jobs; preparation: $(api GET "/jobs/$JID" | json 'd["result"]')"

step "outbound firewall: a camera connects only to the event targets (D25)"
[ "$(api GET "/cameras/$CID" | json 'd["status"]["firewall"]')" = True ] || fail "the camera has no outbound firewall"
if netns_named "$NETNS"; then
	client python3 "$ROOT/backend/e2e/receiver.py" 9001 "$WORK/other.log" </dev/null >/dev/null 2>&1 &
	# A TCP connection from inside the camera's namespace.
	reaches() { node_exec ip netns exec "$NETNS" timeout 3 bash -c "exec 3<>/dev/tcp/$CLIENT_IP/$1" 2>/dev/null; }
	reaches_within() { # port, seconds: 0 when it becomes reachable
		for ((i = 0; i < $2; i++)); do reaches "$1" && return 0; done
		return 1
	}
	reaches 9000 || fail "the camera cannot connect to its event target"
	reaches 9001 && fail "the camera connected to a port of the client that no target uses"
	OTHER=$(api POST /targets -H 'Content-Type: application/json' -d "{\"name\":\"other\",\"url\":\"http://$CLIENT_IP:9001/x\"}" | json 'd["id"]')
	reaches_within 9001 10 || fail "a new target did not open the firewall of the running camera"
	api DELETE "/targets/$OTHER" -o /dev/null
	for _ in $(seq 1 10); do reaches 9001 || break; done
	reaches 9001 && fail "a deleted target is still open"
	ok "from its namespace the camera reaches $CLIENT_IP:9000 (a target) and not :9001, until a target uses it; no restart"
else
	ok "firewall in place (anonymous namespace: the connections are not tried from inside)"
fi

step "MAC probe: a camera does not start with a MAC another device has (RN-06)"
CLIENT_MAC=$(client cat /sys/class/net/eth0/address)
BAD=$(api POST /cameras -H 'Content-Type: application/json' -d "{
	\"name\": \"Clash\", \"profile_id\": \"milesight/demo\", \"profile_version\": \"0.4.0\",
	\"network\": {\"ip\": \"10.77.0.13\", \"mac\": \"$CLIENT_MAC\"}, \"start\": true}" | json 'd["id"]')
for _ in $(seq 1 60); do
	[ "$(api GET "/cameras/$BAD" | json 'd["status"]["state"]')" = error ] && break
	sleep 0.5
done
reason=$(api GET "/cameras/$BAD" | json 'd["status"]["reason"]')
echo "$reason" | grep -q "MAC $CLIENT_MAC is already in use" || fail "a camera started with the client's MAC: $reason"
client ping -c 1 -W 2 "$CAM_IP" >/dev/null || fail "the client lost its network"
api DELETE "/cameras/$BAD" -o /dev/null
ok "refused: $reason"

step "DHCP: a new camera takes its factory address without a server, then leases one (D23, D24)"
LID=$(api POST /cameras -H 'Content-Type: application/json' -d '{
	"name": "Lobby", "profile_id": "milesight/demo", "profile_version": "0.4.0",
	"network": {"ip_mode": "dhcp"}, "stream": {"resolution": "640x360"}, "start": true}' | json 'd["id"]')
wait_state "$LID" running 90
LMAC=$(api GET "/cameras/$LID" | json 'd["network"]["mac"]')
got=$(api GET "/cameras/$LID" | json '(d["status"]["ip"], d["status"]["ip_source"])')
[ "$got" = "('192.168.5.190', 'factory')" ] || fail "without a DHCP server: $got"
client ip addr add 192.168.5.2/24 dev eth0
client ping -c 2 -W 2 192.168.5.190 >/dev/null || fail "the client cannot reach the factory address"
[ "$(client ip neigh show 192.168.5.190 | awk '{print $5}')" = "$LMAC" ] || fail "the factory address answers with another MAC"
ok "no server answered: the camera took the profile's factory address 192.168.5.190, as a real one"
# A second one finds the address taken, and takes it once the first stops.
L2ID=$(api POST /cameras -H 'Content-Type: application/json' -d '{
	"name": "Lobby 2", "profile_id": "milesight/demo", "profile_version": "0.4.0",
	"network": {"ip_mode": "dhcp"}, "stream": {"resolution": "640x360"}, "start": true}' | json 'd["id"]')
wait_status "$L2ID" 'd["status"].get("reason_code")' dhcp_factory_in_use 90
ok "a second camera did not take 192.168.5.190 while Lobby holds it"
api POST "/cameras/$LID/actions/stop" >/dev/null
wait_state "$LID" stopped 20
wait_status "$L2ID" '(d["status"]["state"], d["status"].get("ip"))' "('running', '192.168.5.190')" 120
L2MAC=$(api GET "/cameras/$L2ID" | json 'd["network"]["mac"]')
client ip neigh flush dev eth0
client ping -c 2 -W 2 192.168.5.190 >/dev/null || fail "the client cannot reach the second camera"
[ "$(client ip neigh show 192.168.5.190 | awk '{print $5}')" = "$L2MAC" ] || fail "192.168.5.190 answers with another MAC than Lobby 2's"
client ip addr del 192.168.5.2/24 dev eth0
api DELETE "/cameras/$L2ID" -o /dev/null
ok "stopped, Lobby left it: Lobby 2 answers on 192.168.5.190 with its MAC $L2MAC"
# The pool starts with Gate 1's address: the service refuses it and the
# camera declines it, as RFC 2131 asks.
DHCP_IP=10.77.0.100
client python3 "$ROOT/backend/e2e/dhcpd.py" eth0 "$CLIENT_IP" "$CAM_IP,$DHCP_IP" "$WORK/dhcp.log" </dev/null >/dev/null 2>&1 &
sleep 0.5
api POST "/cameras/$LID/actions/start" >/dev/null
wait_state "$LID" running 90
got=$(api GET "/cameras/$LID" | json '(d["status"]["ip"], d["status"]["ip_source"])')
[ "$got" = "('$DHCP_IP', 'dhcp')" ] || fail "with a DHCP server: $got; server log: $(cat "$WORK/dhcp.log" 2>/dev/null)"
grep -q "^DECLINE $LMAC $CAM_IP$" "$WORK/dhcp.log" || fail "the address of Gate 1 was not declined: $(cat "$WORK/dhcp.log")"
grep -q "^ACK $LMAC $DHCP_IP$" "$WORK/dhcp.log" || fail "no lease of $DHCP_IP in the server's log"
client ping -c 2 -W 2 "$DHCP_IP" >/dev/null || fail "the client cannot reach the leased address"
[ "$(client ip neigh show "$DHCP_IP" | awk '{print $5}')" = "$LMAC" ] || fail "the leased address answers with another MAC"
client ping -c 1 -W 2 "$CAM_IP" >/dev/null || fail "Gate 1 lost its address to the lease"
ok "leased $DHCP_IP with MAC $LMAC after declining $CAM_IP, which Gate 1 holds"
api DELETE "/cameras/$LID" -o /dev/null
sleep 0.5
grep -q "^RELEASE $LMAC $DHCP_IP$" "$WORK/dhcp.log" || fail "the deleted camera did not release its lease"
ok "deleting the camera released its lease"

step "node bridge: the node itself reaches its macvlan cameras (D26)"
ip addr add 10.77.0.1/24 dev "$LAN"
ping -c 1 -W 1 "$CAM_IP" >/dev/null 2>&1 && fail "the node reached a macvlan camera without the bridge"
api PATCH /settings -H 'Content-Type: application/json' -d '{"node_bridge":true}' >/dev/null
bridge=$(api GET /node | json 'd["bridge"]')
[ "$(api GET /node | json 'd["bridge"].get("interface")')" = mv-bridge ] || fail "bridge: $bridge"
ping -c 2 -W 2 "$CAM_IP" >/dev/null || fail "the node cannot ping the camera through the bridge"
code=$(curl -s -o /dev/null -w '%{http_code}' --digest -u admin:e2e-new-pw "http://$CAM_IP/snapshot.cgi")
[ "$code" = 200 ] || fail "the node got $code from the camera through the bridge"
ok "with the bridge the node pings $CAM_IP and reads its snapshot"
api PATCH /settings -H 'Content-Type: application/json' -d '{"node_bridge":false}' >/dev/null
ip link show mv-bridge >/dev/null 2>&1 && fail "the bridge is still there"
ping -c 1 -W 1 "$CAM_IP" >/dev/null 2>&1 && fail "the node still reaches the camera without the bridge"
ip addr del 10.77.0.1/24 dev "$LAN"
ok "turned off, the bridge is gone"

step "ipvlan: a camera with its card's MAC, and one mode per card (D27)"
# A second card, whose other end is a device of the client.
WLAN=mve2e1
ip link add "$WLAN" address 02:e2:e1:00:00:01 type veth peer name "${WLAN}p" address 02:e2:e1:00:00:02
ip link set "${WLAN}p" netns "$CLIENT_NS"
client ip link set "${WLAN}p" name eth1
client ip addr add 10.78.0.2/24 dev eth1
client ip link set eth1 up
ip link set "$WLAN" up
if ip link add mve2eiv link "$WLAN" type ipvlan mode l2 2>/dev/null; then
	ip link del mve2eiv
	WMAC=$(cat "/sys/class/net/$WLAN/address")
	IID=$(api POST /cameras -H 'Content-Type: application/json' -d "{
		\"name\": \"Wi-Fi 1\", \"profile_id\": \"milesight/demo\", \"profile_version\": \"0.4.0\",
		\"network\": {\"mode\": \"ipvlan\", \"parent\": \"$WLAN\", \"ip\": \"10.78.0.10\", \"netmask\": \"255.255.255.0\"},
		\"stream\": {\"resolution\": \"640x360\"}, \"start\": true}" | json 'd["id"]')
	wait_state "$IID" running 60
	client ping -c 2 -W 2 10.78.0.10 >/dev/null || fail "the client cannot reach the ipvlan camera"
	[ "$(client ip neigh show 10.78.0.10 | awk '{print $5}')" = "$WMAC" ] || fail "the ipvlan camera does not answer with its card's MAC $WMAC"
	[ "$(api GET "/cameras/$IID" | json 'd["status"].get("mac")')" = "$WMAC" ] || fail "the status does not show the card's MAC"
	ok "10.78.0.10 answers with the MAC of $WLAN, $WMAC"
	code=$(api POST /cameras -H 'Content-Type: application/json' -o "$WORK/mix.json" -w '%{http_code}' -d "{
		\"name\": \"Wired\", \"profile_id\": \"milesight/demo\", \"profile_version\": \"0.4.0\",
		\"network\": {\"parent\": \"$WLAN\", \"ip\": \"10.78.0.11\", \"netmask\": \"255.255.255.0\"}}")
	[ "$code" = 422 ] && grep -q "Wi-Fi 1" "$WORK/mix.json" || fail "a macvlan camera beside an ipvlan one: $code $(cat "$WORK/mix.json")"
	ok "a macvlan camera on $WLAN is refused and the answer names Wi-Fi 1"
	# A macvlan interface MockVision did not make: the kernel refuses the
	# ipvlan one, and retrying cannot help.
	api POST "/cameras/$IID/actions/stop" >/dev/null
	wait_state "$IID" stopped 20
	for _ in $(seq 1 20); do
		ip link add mve2emv link "$WLAN" address 02:e2:e1:00:00:03 type macvlan mode bridge 2>/dev/null && break
		sleep 0.25 # the stopped camera's ipvlan port may linger a moment
	done
	ip link show mve2emv >/dev/null 2>&1 || fail "cannot add a macvlan to $WLAN"
	api POST "/cameras/$IID/actions/start" >/dev/null
	wait_status "$IID" 'd["status"].get("reason_code")' parent_busy 30
	sleep 3
	got=$(api GET "/cameras/$IID" | json '(d["status"]["state"], d["status"]["retries"])')
	[ "$got" = "('error', 0)" ] || fail "a camera on a busy card was retried: $got"
	ip link del mve2emv
	api POST "/cameras/$IID/actions/start" >/dev/null
	wait_state "$IID" running 60
	api DELETE "/cameras/$IID" -o /dev/null
	ok "with a foreign macvlan on $WLAN it stops in error (parent_busy) without retrying, and starts once it is gone"
else
	ok "skipped: the kernel has no ipvlan"
fi
ip link del "$WLAN"

step "v1 editing: a new address applies at the restart (RN-09)"
pending=$(api PATCH "/cameras/$CID" -H 'Content-Type: application/json' \
	-d "{\"network\":{\"ip\":\"$CAM3_IP\",\"netmask\":\"255.255.255.0\"}}" | json '",".join(d["status"]["pending_restart"])')
[ "$pending" = network ] || fail "pending restart: $pending"
client ping -c 1 -W 2 "$CAM_IP" >/dev/null || fail "the camera left its address before the restart"
ok "saved; the camera keeps $CAM_IP until it restarts"
api POST "/cameras/$CID/actions/restart" >/dev/null
wait_state "$CID" running 60
client ping -c 2 -W 2 "$CAM3_IP" >/dev/null || fail "no answer on the new address $CAM3_IP"
client ping -c 1 -W 1 "$CAM_IP" >/dev/null 2>&1 && fail "the old address still answers"
NEIGH=$(client ip neigh show "$CAM3_IP" | awk '{print $5}')
[ "$NEIGH" = "$MAC" ] || fail "new address answers with $NEIGH, want $MAC"
[ -z "$(api GET "/cameras/$CID" | json '",".join(d["status"]["pending_restart"])')" ] || fail "still pending after the restart"
ok "after the restart the camera answers on $CAM3_IP with its MAC $MAC"
CAM_IP=$CAM3_IP

step "stopping removes the namespace and its interface"
api POST "/cameras/$CID/actions/stop" >/dev/null
wait_state "$CID" stopped 20
node_exec ip netns list | grep -q "^$NETNS" && fail "namespace $NETNS still exists"
client ping -c 1 -W 1 "$CAM_IP" >/dev/null 2>&1 && fail "the stopped camera still answers"
ok "namespace $NETNS removed, the IP no longer answers"

step "restarting the node restores cameras with autostart"
api POST "/cameras/$CID/actions/start" >/dev/null
wait_state "$CID" running 60
stop_node
if [ "$MODE" = binary ]; then
	ip netns list | grep -q "^sim-" && fail "namespaces left after the node stopped"
fi
client ping -c 1 -W 1 "$CAM_IP" >/dev/null 2>&1 && fail "the camera still answers after the node stopped"
ok "node stopped cleanly"
start_node
api POST /auth/login -H 'Content-Type: application/json' -d '{"username":"admin","password":"e2e-password-1"}' >/dev/null
wait_state "$CID" running 60
client ping -c 1 -W 2 "$CAM_IP" >/dev/null || fail "camera not reachable after the restart"
ok "camera came back and answers"
FID=$(api POST "/cameras/$CID/triggers/$TRG_ID/actions/fire" | json 'd["id"]')
wait_received "$FID"
grep "$FID" "$WORK/received.log" | grep -q '"name":"Gate line"' || fail "the trigger lost its rule over the restart"
ok "its rules and triggers came back too: the stored trigger fires on Gate line"

printf '\n\033[32mALL CHECKS PASSED\033[0m\n'
