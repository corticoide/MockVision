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
# node bridge, ipvlan where the kernel has it, faults, the SD card and a NAS
# share (over SMB when the machine has Samba), the Dahua profile, packages
# (the catalog, signatures, the self-test, inheritance, export and a
# camera's upgrade), an external plugin, diagnostics, clean stop, and
# cameras coming back after a restart.
#
# Run as root on Linux with iproute2, ffmpeg, curl, ping, python3 and Go
# (to build the example plugin):
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
binary) tools="ip ffprobe ffmpeg curl ping python3 go" ;;
compose) tools="ip ffprobe curl ping python3 docker go" ;;
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
client python3 "$ROOT/backend/e2e/sinks.py" "$WORK/sinks" </dev/null >/dev/null 2>&1 &
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

step "the official catalog is installed when the node starts (D81)"
CATALOG=$(api GET /profiles | json '" ".join(sorted(p["profile_id"] + "@" + p["version"] + ":" + p["level"] + ":" + p["source"] for p in d["items"]))')
for want in milesight/base@0.1.0:documented:catalog milesight/demo@0.7.0:draft:catalog dahua/ipc-hdbw1230e-s4@0.2.0:documented:catalog; do
	case " $CATALOG " in *" $want "*) ;; *) fail "the catalog lacks $want: $CATALOG" ;; esac
done
ok "milesight/base, milesight/demo (draft) and the Dahua draft (documented) came with the node, read only"

step "event target on the client and a camera with a fixed IP"
TID=$(api POST /targets -H 'Content-Type: application/json' -d "{\"name\":\"client\",\"url\":\"http://$CLIENT_IP:9000/events\"}" | json 'd["id"]')
CID=$(api POST /cameras -H 'Content-Type: application/json' -d "{
	\"name\": \"Gate 1\", \"profile_id\": \"milesight/demo\", \"profile_version\": \"0.7.0\",
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

step "event transports: an MQTT broker, an FTP server and a mail server (D31, D42)"
SINKS=$WORK/sinks
target() { api POST /targets -H 'Content-Type: application/json' -d "$1" | json 'd["id"]'; }
MQTT_TID=$(target "{\"name\":\"broker\",\"type\":\"mqtt\",\"url\":\"mqtt://$CLIENT_IP:1883\",\"username\":\"cam\",\"password\":\"e2e-mqtt\"}") || fail "mqtt target"
FTP_TID=$(target "{\"name\":\"nas\",\"type\":\"ftp\",\"url\":\"ftp://$CLIENT_IP:2121/cams\",\"username\":\"cam\",\"password\":\"e2e-ftp\"}") || fail "ftp target"
MAIL_TID=$(target "{\"name\":\"mail\",\"type\":\"smtp\",\"url\":\"smtp://$CLIENT_IP:2525\",\"username\":\"cam\",\"password\":\"e2e-mail\",
	\"from\":\"gate1@e2e.lan\",\"to\":[\"ops@e2e.lan\"],\"delivery\":{\"retries\":0}}") || fail "mail target"
api PATCH "/cameras/$CID" -H 'Content-Type: application/json' -d "{\"target_ids\":[\"$TID\",\"$MQTT_TID\",\"$FTP_TID\",\"$MAIL_TID\"]}" >/dev/null
# Like a real camera it connects to the broker as soon as it has it, with
# its serial as client ID, and announces itself online.
for _ in $(seq 1 40); do grep -q "PUBLISH milesight/$SERIAL/status qos=1 retain=1 online" "$SINKS/mqtt.log" 2>/dev/null && break; sleep 0.25; done
grep -q "CONNECT client=$SERIAL user=cam ok=True will=milesight/$SERIAL/status:offline" "$SINKS/mqtt.log" ||
	fail "the camera did not connect to the broker: $(cat "$SINKS/mqtt.log" 2>/dev/null)"
grep -q "PUBLISH milesight/$SERIAL/status qos=1 retain=1 online" "$SINKS/mqtt.log" || fail "no birth message"
ok "the camera connected to the broker without restarting, with its will, and said online"
EID=$(api POST "/cameras/$CID/events" -H 'Content-Type: application/json' -d '{"type":"line_crossing"}' | json 'd["id"]')
for _ in $(seq 1 40); do
	[ "$(api GET "/events/$EID" | json 'len(d["deliveries"])')" -ge 4 ] && break
	sleep 0.25
done
ev=$(api GET "/events/$EID")
[ "$(echo "$ev" | json 'd["delivery_status"]')" = ok ] || fail "deliveries: $(echo "$ev" | json 'd["deliveries"]')"
grep -q "PUBLISH milesight/$SERIAL/event/LineCrossing qos=1 retain=0 .*$EID" "$SINKS/mqtt.log" || fail "the broker did not get $EID"
ok "published to milesight/$SERIAL/event/LineCrossing with QoS 1"
day=$(echo "$ev" | json 'd["at"][:10]')
up=$(ls "$SINKS/ftp/cams/$SERIAL/$day/" 2>/dev/null | grep '_LineCrossing.jpg$' | head -n 1)
[ -n "$up" ] || fail "no upload under cams/$SERIAL/$day: $(cat "$SINKS/ftp.log" 2>/dev/null)"
head -c 2 "$SINKS/ftp/cams/$SERIAL/$day/$up" | od -An -tx1 | grep -q "ff d8" || fail "the upload is not a JPEG"
grep -q "^EPSV" "$SINKS/ftp.log" || fail "the upload did not use passive mode"
ok "the snapshot went to cams/$SERIAL/$day/$up in passive mode, through the camera's firewall"
mail=$(grep -o 'mail-[0-9]*.eml from=gate1@e2e.lan to=ops@e2e.lan' "$SINKS/smtp.log" | head -n 1 | cut -d' ' -f1)
[ -n "$mail" ] || fail "no mail: $(cat "$SINKS/smtp.log" 2>/dev/null)"
grep -q "^Subject: LineCrossing on Network Camera" "$SINKS/$mail" || fail "mail subject: $(grep Subject "$SINKS/$mail")"
grep -q "^Content-Type: image/jpeg" "$SINKS/$mail" || fail "the mail has no snapshot"
ok "mailed to ops@e2e.lan with the snapshot attached"
# Within the profile's 10 s mail interval the next mail is skipped.
sleep 1.1
EID2=$(api POST "/cameras/$CID/events" -H 'Content-Type: application/json' -d '{"type":"line_crossing"}' | json 'd["id"]')
for _ in $(seq 1 40); do
	[ "$(api GET "/events/$EID2" | json 'len(d["deliveries"])')" -ge 4 ] && break
	sleep 0.25
done
[ "$(api GET "/events/$EID2" | json '[x["status"] for x in d["deliveries"] if x["target_name"]=="mail"][0]')" = skipped ] ||
	fail "the second mail was not skipped: $(api GET "/events/$EID2" | json 'd["deliveries"]')"
ok "a second crossing within 10 s skips the mail, as the device's interval"
for id in "$MQTT_TID" "$FTP_TID" "$MAIL_TID"; do
	res=$(api POST "/targets/$id/actions/test")
	[ "$(echo "$res" | json 'd["ok"] and d["from"]')" = camera ] || fail "target test: $res"
done
ok "each target tested from the camera: a broker session, an FTP login, the mail recipients"
api PATCH "/cameras/$CID" -H 'Content-Type: application/json' -d "{\"target_ids\":[\"$TID\"]}" >/dev/null
for _ in $(seq 1 40); do grep -q "^DISCONNECT" "$SINKS/mqtt.log" && break; sleep 0.25; done
grep -q "^DISCONNECT" "$SINKS/mqtt.log" || fail "unlinking the broker did not end the session"
# Deleted: the FTP server opened every port of the client in the
# firewalls, which the firewall step below checks closed.
for id in "$MQTT_TID" "$FTP_TID" "$MAIL_TID"; do api DELETE "/targets/$id" -o /dev/null; done
ok "unlinked, the camera says goodbye to the broker"

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
resp=$(api POST /cameras -H 'Content-Type: application/json' -d "{\"name\":\"Gate 2\",\"profile_id\":\"milesight/demo\",\"profile_version\":\"0.7.0\",\"network\":{\"ip\":\"$CAM2_IP\"}}")
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

step "background jobs: renditions and a preparation (D72, D74)"
[ "$(api GET '/jobs?type=rendition&status=completed' | json 'len(d["items"])')" -ge 1 ] || fail "no rendition was encoded as a job"
JID=$(api POST /jobs -H 'Content-Type: application/json' -d '{"type":"renditions.prepare"}' | json 'd["id"]')
for _ in $(seq 1 120); do
	st=$(api GET "/jobs/$JID" | json 'd["status"]')
	case "$st" in completed | failed | canceled) break ;; esac
	sleep 0.5
done
[ "$st" = completed ] || fail "renditions.prepare ended $st: $(api GET "/jobs/$JID")"
ok "renditions ran as jobs; preparation: $(api GET "/jobs/$JID" | json 'd["result"]')"

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
	\"name\": \"Clash\", \"profile_id\": \"milesight/demo\", \"profile_version\": \"0.7.0\",
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
	"name": "Lobby", "profile_id": "milesight/demo", "profile_version": "0.7.0",
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
	"name": "Lobby 2", "profile_id": "milesight/demo", "profile_version": "0.7.0",
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
		\"name\": \"Wi-Fi 1\", \"profile_id\": \"milesight/demo\", \"profile_version\": \"0.7.0\",
		\"network\": {\"mode\": \"ipvlan\", \"parent\": \"$WLAN\", \"ip\": \"10.78.0.10\", \"netmask\": \"255.255.255.0\"},
		\"stream\": {\"resolution\": \"640x360\"}, \"start\": true}" | json 'd["id"]')
	wait_state "$IID" running 60
	client ping -c 2 -W 2 10.78.0.10 >/dev/null || fail "the client cannot reach the ipvlan camera"
	[ "$(client ip neigh show 10.78.0.10 | awk '{print $5}')" = "$WMAC" ] || fail "the ipvlan camera does not answer with its card's MAC $WMAC"
	[ "$(api GET "/cameras/$IID" | json 'd["status"].get("mac")')" = "$WMAC" ] || fail "the status does not show the card's MAC"
	ok "10.78.0.10 answers with the MAC of $WLAN, $WMAC"
	code=$(api POST /cameras -H 'Content-Type: application/json' -o "$WORK/mix.json" -w '%{http_code}' -d "{
		\"name\": \"Wired\", \"profile_id\": \"milesight/demo\", \"profile_version\": \"0.7.0\",
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

step "faults: a protocol down, a status, the network down and a reboot (D43, RN-14)"
fault() { api POST "/cameras/$CID/faults" -H 'Content-Type: application/json' -d "$1" | json 'd["id"]'; }
probe_main() { client timeout 10 ffprobe -v error -rtsp_transport tcp -show_entries stream=codec_name -of csv=p=0 "rtsp://admin:e2e-new-pw@$CAM_IP:554/main" 2>/dev/null; }
PID=$(api GET "/cameras/$CID" | json 'd["status"]["pid"]')
FID=$(fault '{"kind":"service_down","instance":"rtsp","duration_s":120}') || fail "inject service_down"
wait_status "$CID" 'd["status"]["state"]' degraded 10
[ "$(api GET "/cameras/$CID" | json 'd["status"]["reason"]')" = "faults on: rtsp down" ] || fail "reason: $(api GET "/cameras/$CID" | json 'd["status"]')"
probe_main && fail "RTSP answered while down"
code=$(client curl -s -o /dev/null -w '%{http_code}' --digest -u admin:e2e-new-pw "http://$CAM_IP/snapshot.cgi")
[ "$code" = 200 ] || fail "the HTTP API stopped with RTSP ($code)"
api DELETE "/cameras/$CID/faults/$FID" -o /dev/null
wait_status "$CID" 'd["status"]["state"]' running 10
[ "$(probe_main)" = h264 ] || fail "RTSP did not come back"
ok "RTSP down: ffprobe fails, the HTTP API still answers, the camera is degraded; ended by hand, the stream is back"
fault '{"kind":"error_status","instance":"http","status":401,"duration_s":3}' >/dev/null
sleep 0.5
code=$(client curl -s -o /dev/null -w '%{http_code}' --digest -u admin:e2e-new-pw "http://$CAM_IP/snapshot.cgi")
[ "$code" = 401 ] || fail "the 401 fault answered $code"
wait_status "$CID" 'd["status"]["state"]' running 10
code=$(client curl -s -o /dev/null -w '%{http_code}' --digest -u admin:e2e-new-pw "http://$CAM_IP/snapshot.cgi")
[ "$code" = 200 ] || fail "after the 401 fault expired: $code"
ok "for 3 s the HTTP API refused the right password with 401, then the fault expired on its own"
fault '{"kind":"network_down","duration_s":6}' >/dev/null
wait_status "$CID" 'd["status"]["reason"]' "faults on: network down" 10
client ip neigh flush dev eth0 >/dev/null 2>&1 || true
client ping -c 1 -W 1 "$CAM_IP" >/dev/null 2>&1 && fail "the camera answered with its network down"
EV=""
for _ in $(seq 1 40); do
	EV=$(api GET "/events?camera_id=$CID&type=network_lost" | json 'd["items"][0]["id"] if d["items"] else ""')
	[ -n "$EV" ] && break
	sleep 0.25
done
[ -n "$EV" ] || fail "no network_lost event"
wait_status "$CID" 'd["status"]["state"]' running 15
client ping -c 2 -W 2 "$CAM_IP" >/dev/null || fail "the camera did not come back on the network"
wait_received "$EV"
for _ in $(seq 1 40); do [ "$(api GET "/events/$EV" | json 'd["delivery_status"]')" = ok ] && break; sleep 0.5; done
[ "$(api GET "/events/$EV" | json '[x["status"] for x in d["deliveries"]][0]')" = retry ] || fail "network_lost went out while the network was down"
ok "network down for 6 s: no ping, no ARP; network_lost failed to leave and reached the target once back"
[ "$(api GET "/cameras/$CID" | json 'd["status"]["pid"]')" = "$PID" ] || fail "a fault restarted the camera"
api POST "/cameras/$CID/actions/reboot" -H 'Content-Type: application/json' -d '{"seconds":4}' | json 'd["status"]["state"]' | grep -qx restarting || fail "reboot"
client ping -c 1 -W 1 "$CAM_IP" >/dev/null 2>&1 && fail "the camera answered while rebooting"
sleep 2
[ "$(api GET "/cameras/$CID" | json 'd["status"]["state"]')" = restarting ] || fail "the reboot was cut short"
wait_state "$CID" running 60
client ping -c 2 -W 2 "$CAM_IP" >/dev/null || fail "no answer after the reboot"
[ "$(api GET "/cameras/$CID" | json 'd["status"]["pid"]')" != "$PID" ] || fail "the reboot kept the process"
ok "rebooted: off the network for its 4 s of boot, then back on $CAM_IP"

step "storage: an SD card and a NAS share, searched, downloaded and played back (D68, D69, D91)"
# A card the node's disk cannot hold, with what the others promised, is
# refused (when the disk is smaller than the model's largest card).
free_mb=$(node_exec df -Pm "$( [ "$MODE" = compose ] && echo /data || echo "$WORK/data")" | awk 'NR==2 {print $4}')
if [ "$free_mb" -lt 262144 ]; then
	code=$(api PUT "/cameras/$CID/storage" -H 'Content-Type: application/json' -d '{"kind":"sd","size_mb":262144}' -o "$WORK/rej.json" -w '%{http_code}')
	[ "$code" = 409 ] && [ "$(json 'd["code"]' <"$WORK/rej.json")" = disk ] || fail "a 256 GB card on $free_mb MB free: $code $(cat "$WORK/rej.json")"
	ok "a 256 GB card does not fit in $free_mb MB of disk: refused with code disk"
fi
[ "$(api PUT "/cameras/$CID/storage" -H 'Content-Type: application/json' -d '{"kind":"sd","size_mb":64}' | json 'd["status"]["state"]')" = present ] ||
	fail "a 64 MB card"
EV=$(api POST "/cameras/$CID/events" -H 'Content-Type: application/json' -d '{"type":"line_crossing"}' | json 'd["id"]')
for _ in $(seq 1 40); do
	[ "$(api GET "/cameras/$CID/recordings" | json 'len(d["items"])')" = 2 ] && break
	sleep 0.25
done
RECS=$(api GET "/cameras/$CID/recordings")
CLIP=$(echo "$RECS" | json '[r["name"] for r in d["items"] if r["kind"]=="clip"][0]')
CLIP_ID=$(echo "$RECS" | json '[r["id"] for r in d["items"] if r["kind"]=="clip"][0]')
[ "$(echo "$RECS" | json '[r["event_id"] for r in d["items"]] == ["'"$EV"'"] * 2')" = True ] || fail "recordings: $RECS"
ok "line_crossing recorded its snapshot and a 10 s clip on the card"
# The client searches the card through the camera's API, downloads the clip
# and plays the range back over RTSP.
START=$(echo "$RECS" | json '[r["start"] for r in d["items"] if r["kind"]=="clip"][0]')
from=$(date -u -d "$START - 1 minute" '+%Y-%m-%d %H:%M:%S')
to=$(date -u -d "$START + 1 minute" '+%Y-%m-%d %H:%M:%S')
found=$(client curl -s -G --digest -u admin:e2e-new-pw "http://$CAM_IP/cgi-bin/operator/operator.cgi" --data-urlencode action=get.record.search \
	--data-urlencode "starttime=$from" --data-urlencode "endtime=$to" --data-urlencode type=video)
[ "$(echo "$found" | json 'd["records"][0]["file"]')" = "$CLIP" ] || fail "search: $found"
code=$(client curl -s -o "$WORK/clip.ts" -w '%{http_code} %{content_type}' --digest -u admin:e2e-new-pw "http://$CAM_IP/cgi-bin/operator/download.cgi?file=$CLIP")
[ "$code" = "200 video/mp2t" ] || fail "download: $code $(head -c 200 "$WORK/clip.ts")"
out=$(ffprobe -v error -show_entries format=duration:stream=codec_name -of csv=p=0 "$WORK/clip.ts" | tr '\n' ' ') || fail "ffprobe of the clip"
case "$out" in h264*" 10."* | h264*" 9.9"*) ;; *) fail "the downloaded clip: $out" ;; esac
api GET "/cameras/$CID/recordings/$CLIP_ID/download" -o "$WORK/panel.ts"
cmp -s "$WORK/clip.ts" "$WORK/panel.ts" || fail "the panel's download differs from the camera's"
pb_from=$(date -u -d "$START - 1 minute" '+%Y%m%dT%H%M%SZ')
pb_to=$(date -u -d "$START + 1 minute" '+%Y%m%dT%H%M%SZ')
out=$(client timeout 20 ffprobe -v error -rtsp_transport tcp -show_entries stream=codec_name -of csv=p=0 \
	"rtsp://admin:e2e-new-pw@$CAM_IP:554/playback?starttime=$pb_from&endtime=$pb_to") || fail "RTSP playback"
[ "$out" = h264 ] || fail "playback: $out"
ok "the client found the clip by time, downloaded it ($(stat -c %s "$WORK/clip.ts") bytes of H.264, 10 s) and played the range back over RTSP"
# The card goes missing: the device says so to its target and its search fails.
FID=$(fault '{"kind":"sd_missing","duration_s":60}')
for _ in $(seq 1 40); do grep -q '"eventType": *"SDCardMissing"' "$WORK/received.log" 2>/dev/null && break; sleep 0.25; done
grep -q '"eventType": *"SDCardMissing"' "$WORK/received.log" || fail "storage_missing did not reach the target"
code=$(client curl -s -o /dev/null -w '%{http_code}' -G --digest -u admin:e2e-new-pw "http://$CAM_IP/cgi-bin/operator/operator.cgi" --data-urlencode action=get.record.search)
[ "$code" = 503 ] || fail "search without a card: $code"
api DELETE "/cameras/$CID/faults/$FID" -o /dev/null
wait_status "$CID" 'd["status"]["state"]' running 10
ok "sd_missing: storage_missing reached the target, the search answered 503; ended, the card is back"
# A NAS share on the client, over SMB, when the machine has Samba.
if command -v smbd >/dev/null; then
	SMB=$WORK/smb
	mkdir -p "$SMB"/{share,private,lock,state,cache,ncalrpc,binddns}
	cat >"$SMB/smb.conf" <<-CONF
	[global]
	interfaces = $CLIENT_IP/24
	bind interfaces only = yes
	private dir = $SMB/private
	lock directory = $SMB/lock
	state directory = $SMB/state
	cache directory = $SMB/cache
	pid directory = $SMB
	ncalrpc dir = $SMB/ncalrpc
	binddns dir = $SMB/binddns
	log file = $SMB/log.smbd
	passdb backend = tdbsam:$SMB/private/passdb.tdb
	disable netbios = yes
	server role = standalone server
	[cams]
	path = $SMB/share
	read only = no
	valid users = root
	CONF
	printf 'e2e-smb\ne2e-smb\n' | smbpasswd -c "$SMB/smb.conf" -a -s root >/dev/null
	client smbd --foreground --no-process-group -s "$SMB/smb.conf" </dev/null >/dev/null 2>&1 &
	for _ in $(seq 1 40); do client bash -c "echo >/dev/tcp/$CLIENT_IP/445" 2>/dev/null && break; sleep 0.25; done
	[ "$(api PUT "/cameras/$CID/storage" -H 'Content-Type: application/json' \
		-d "{\"kind\":\"nas\",\"nas_url\":\"smb://$CLIENT_IP/cams/recordings\",\"nas_username\":\"root\",\"nas_password\":\"e2e-smb\"}" | json 'd["kind"]')" = nas ] ||
		fail "a NAS share"
	sleep 1.1 # line_crossing's minimum interval
	EV=$(api POST "/cameras/$CID/events" -H 'Content-Type: application/json' -d '{"type":"line_crossing"}' | json 'd["id"]')
	for _ in $(seq 1 60); do
		[ "$(api GET "/cameras/$CID/recordings" | json 'len([r for r in d["items"] if r["location"]=="nas"])')" = 2 ] && break
		sleep 0.25
	done
	RECS=$(api GET "/cameras/$CID/recordings")
	CLIP=$(echo "$RECS" | json '[r["name"] for r in d["items"] if r["kind"]=="clip"][0]')
	[ -s "$SMB/share/recordings/$SERIAL/$CLIP" ] || fail "the clip is not on the share: $RECS $(api GET "/cameras/$CID/storage")"
	CLIP_ID=$(echo "$RECS" | json '[r["id"] for r in d["items"] if r["kind"]=="clip"][0]')
	api GET "/cameras/$CID/recordings/$CLIP_ID/download" -o "$WORK/nas.ts"
	cmp -s "$WORK/nas.ts" "$SMB/share/recordings/$SERIAL/$CLIP" || fail "the panel read another file through the camera"
	ok "the camera wrote its recordings to smb://$CLIENT_IP/cams as root, in a folder of its serial; the panel reads them through it"
	api PUT "/cameras/$CID/storage" -H 'Content-Type: application/json' -d '{"kind":"none"}' -o /dev/null
else
	ok "no smbd on this machine: the NAS over SMB is left to the unit tests"
fi

step "Dahua profile: eventManager attach, Dahua's values and errors (D29)"
DH_IP=10.77.0.13
DH=$(api POST /cameras -H 'Content-Type: application/json' -d "{\"name\": \"Hall\", \"profile_id\": \"dahua/ipc-hdbw1230e-s4\",
	\"profile_version\": \"0.2.0\", \"network\": {\"ip\": \"$DH_IP\"}, \"target_ids\": [\"$TID\"], \"start\": true}" | json 'd["id"]')
wait_state "$DH" running 120
DH_SERIAL=$(api GET "/cameras/$DH" | json 'd["serial"]')
dh() { client curl -s --digest -u admin:admin1234 "http://$DH_IP$1"; }
[ "$(dh '/cgi-bin/magicBox.cgi?action=getMachineName' | tr -d '\r')" = "name=$DH_SERIAL" ] || fail "the unit is not named after its serial"
ok "the unit, from the catalog's Dahua profile, answers as Dahua's, named after its serial $DH_SERIAL"
# A client keeps attach open; a motion reaches it as Start, then Stop.
client curl -s -N --max-time 12 --digest -u admin:admin1234 \
	"http://$DH_IP/cgi-bin/eventManager.cgi?action=attach&codes=%5BAll%5D&heartbeat=2" >"$WORK/attach.log" 2>/dev/null &
ATTACH=$!
sleep 1
api POST "/cameras/$DH/events" -H 'Content-Type: application/json' -d '{"type":"motion"}' >/dev/null
wait "$ATTACH" || true
grep -q "Code=VideoMotion;action=Start;index=0" "$WORK/attach.log" || fail "no VideoMotion Start: $(head -c 400 "$WORK/attach.log")"
grep -q "Code=VideoMotion;action=Stop;index=0" "$WORK/attach.log" || fail "no VideoMotion Stop"
grep -q "Heartbeat" "$WORK/attach.log" || fail "no heartbeat"
grep -q "^--myboundary" "$WORK/attach.log" || fail "not a multipart answer"
ok "eventManager.cgi attach streamed VideoMotion Start and Stop and its heartbeats"
# Dahua's values move the stream; a pair the stream lacks gets Dahua's error.
out=$(dh '/cgi-bin/configManager.cgi?action=setConfig&Encode%5B0%5D.ExtraFormat%5B0%5D.Video.Height=240' | tr -d '\r' | tr '\n' ' ')
[ "$out" = "Error Bad Request! " ] || fail "704x240: $out"
out=$(dh '/cgi-bin/configManager.cgi?action=setConfig&Encode%5B0%5D.MainFormat%5B0%5D.Video.Compression=H.265&Encode%5B0%5D.MainFormat%5B0%5D.Video.Width=1280&Encode%5B0%5D.MainFormat%5B0%5D.Video.Height=720' | tr -d '\r')
[ "$out" = OK ] || fail "setConfig: $out"
probe_dh() { client timeout 10 ffprobe -v error -rtsp_transport tcp -select_streams v:0 -show_entries stream=codec_name,width,height -of csv=p=0 \
	"rtsp://admin:admin1234@$DH_IP:554/cam/realmonitor?channel=1&subtype=0" 2>/dev/null; }
for _ in $(seq 1 60); do [ "$(probe_dh)" = "hevc,1280,720" ] && break; sleep 1; done
[ "$(probe_dh)" = "hevc,1280,720" ] || fail "the main stream after Dahua's H.265 at 1280x720: $(probe_dh)"
ok "setConfig of Compression=H.265, Width=1280 and Height=720 turned the main stream into H.265 at 1280x720; 704x240 got Error / Bad Request!"
code=$(client curl -s -o /dev/null -w '%{http_code}' --digest -u admin:wrong "http://$DH_IP/cgi-bin/magicBox.cgi?action=getDeviceType")
[ "$code" = 401 ] || fail "a wrong password answered $code"
for _ in $(seq 1 20); do
	[ "$(api GET "/events?camera_id=$DH&type=custom:login_failure" | json 'len(d["items"])')" -ge 1 ] && break
	sleep 0.25
done
[ "$(api GET "/events?camera_id=$DH&type=custom:login_failure" | json 'len(d["items"])')" -ge 1 ] || fail "no LoginFailure"
ok "a wrong password raised LoginFailure"
api DELETE "/cameras/$DH" -o /dev/null

step "packages: a trusted signature, the self-test, inheritance, export and a camera's upgrade (D05, D17, D83, D88)"
# The package tools run where the binary is: on the host, or in the image.
mvcli() {
	if [ "$MODE" = compose ]; then docker run --rm -u 0 -e MOCKVISION_KEY_PASSWORD -v "$WORK:$WORK" mockvision:latest "$@"; else "$BIN" "$@"; fi
}
PK=$WORK/pkg
mkdir -p "$PK/demo/fixtures"
mvcli pkg keygen -o "$PK/acme" -W >/dev/null || fail "pkg keygen"
python3 -c 'import json,sys; print(json.dumps({"name": "Acme", "public_key": open(sys.argv[1]).read()}))' "$PK/acme.pub" >"$PK/key.json"
api POST /trusted-keys -H 'Content-Type: application/json' -d "@$PK/key.json" | json 'd["key_id"]' >/dev/null || fail "trusting the key"
sed 's/^  version: 0.7.0$/  version: 0.8.0/' "$ROOT/profiles/milesight-demo.yaml" >"$PK/demo/profile.yaml"
printf 'format: 1\nkind: profile\nid: milesight/demo\nversion: 0.8.0\nprovenance: { source: captured }\n' >"$PK/demo/manifest.yaml"
cat >"$PK/demo/fixtures/recorded.yaml" <<'FIXTURES'
fixtures:
  - id: device-info
    request: { method: GET, path: /cgi-bin/operator/operator.cgi, query: { action: get.system.information } }
    response:
      status: 200
      headers: { Content-Type: application/json }
      body: |
        {"deviceName": "Network Camera", "vendor": "Milesight", "model": "MS-DEMO", "serialNumber": "6C0123456789",
         "macAddress": "1C:C3:16:00:00:01", "ipAddress": "192.168.5.190", "firmwareVersion": "demo-1.0.0",
         "systemTime": "2026-09-20T10:00:00Z"}
    vary:
      - { in: body, path: $.serialNumber, as: serial }
      - { in: body, path: $.macAddress, as: any }
      - { in: body, path: $.ipAddress, as: any }
      - { in: body, path: $.systemTime, as: timestamp }
  - id: line-crossing
    trigger: { type: line_crossing, direction: "A->B" }
    expect:
      http_push:
        body: |
          {"eventType": "LineCrossing", "eventId": "x", "time": "2026-09-20T10:00:00.000Z",
           "device": {"name": "Network Camera", "serialNumber": "6C0123456789", "macAddress": "x", "ipAddress": "x"},
           "rule": {"id": "x", "name": "Line 1", "type": "line"}, "direction": "A->B", "object": null}
        vary:
          - { in: body, path: $.eventId, as: any }
          - { in: body, path: $.time, as: timestamp }
          - { in: body, path: $.device.serialNumber, as: serial }
          - { in: body, path: $.device.macAddress, as: any }
          - { in: body, path: $.device.ipAddress, as: any }
          - { in: body, path: $.rule.id, as: any }
          - { in: body, path: $.object, as: any }
FIXTURES
chmod -R a+rwX "$PK"
mvcli pkg build "$PK/demo" -o "$PK/demo.mvpkg" -k "$PK/acme.key" >/dev/null || fail "pkg build"
IMP=$(api POST /packages -F "file=@$PK/demo.mvpkg")
[ "$(echo "$IMP" | json 'd["profile"]["signature_status"] + " " + d["profile"]["signer"] + " " + d["profile"]["level"]')" = "trusted Acme captured" ] ||
	fail "the signed package with its recordings: $(echo "$IMP" | json 'd["report"]')"
[ "$(echo "$IMP" | json 'd["report"]["self_test"]["passed"]')" = 2 ] || fail "self-test: $(echo "$IMP" | json 'd["report"]["self_test"]')"
[ "$(echo "$IMP" | json 'd["report"]["verified"]["route:http/device-info"] + " " + d["report"]["verified"]["event:line_crossing"]')" = "verified verified" ] ||
	fail "coverage: $(echo "$IMP" | json 'd["report"]["verified"]')"
[ "$(api GET '/jobs?type=import' | json 'd["items"][0]["status"]')" = completed ] || fail "the import did not run as a completed job"
node_exec ip netns list | grep -q sim-selftest && fail "the self-test camera's namespace was left behind"
ok "signed by a trusted key and its 2 recordings match an ephemeral camera on a network of its own: captured"
# The same package with its manifest changed after signing.
python3 - "$PK/demo.mvpkg" "$PK/tampered.mvpkg" <<'PY'
import sys, zipfile
src, dst = zipfile.ZipFile(sys.argv[1]), zipfile.ZipFile(sys.argv[2], "w", zipfile.ZIP_DEFLATED)
for i in src.infolist():
    data = src.read(i.filename)
    if i.filename == "manifest.yaml":
        data += b"license: MIT\n"
    dst.writestr(i.filename, data)
dst.close()
PY
code=$(api POST /packages -F "file=@$PK/tampered.mvpkg" -o "$PK/tampered.json" -w '%{http_code}')
[ "$code" = 422 ] && grep -q 'changed after it was signed' "$PK/tampered.json" || fail "a package changed after signing answered $code: $(cat "$PK/tampered.json")"
ok "a manifest changed after signing is rejected (422)"
# A model that extends the catalog's Milesight base.
printf 'schema: 1\nprofile:\n  id: milesight/c2965\n  version: 1.0.0\n  name: Milesight C2965\n  vendor: Milesight\n  model: MS-C2965-PB\n  extends: milesight/base@^0.1\n' >"$PK/c2965.yaml"
[ "$(api POST /packages -F "file=@$PK/c2965.yaml" | json 'd["profile"]["extends"]')" = milesight/base@0.1.0 ] || fail "the child of milesight/base"
ok "milesight/c2965 extends milesight/base@^0.1, resolved with 0.1.0 pinned"
# Exported, it is the signed package: pkg verify trusts it with the key.
api GET /profiles/milesight/demo/versions/0.8.0/export -o "$PK/exported.mvpkg"
cmp -s "$PK/exported.mvpkg" "$PK/demo.mvpkg" || fail "the export is not the package imported"
chmod a+r "$PK/exported.mvpkg"
mvcli pkg verify "$PK/exported.mvpkg" --key "$PK/acme.pub" | grep -q 'signature:trusted by acme.pub' || fail "pkg verify of the export"
ok "the export is the signed package; pkg verify --key trusts it offline"
# Gate 1 moves from 0.7.0 to 0.8.0: the plan first, then a restart.
[ "$(api POST "/cameras/$CID/actions/upgrade-profile" -H 'Content-Type: application/json' -d '{"version":"0.8.0","dry_run":true}' | json 'd["plan"]["restart"]')" = True ] ||
	fail "the plan of the upgrade"
[ "$(api POST "/cameras/$CID/actions/upgrade-profile" -H 'Content-Type: application/json' -d '{"version":"0.8.0"}' | json 'd["camera"]["profile"]["version"]')" = 0.8.0 ] ||
	fail "the upgrade"
wait_state "$CID" running 90
GATE_IP=$(api GET "/cameras/$CID" | json 'd["network"]["ip"]')
client ffprobe -v error -rtsp_transport tcp -show_entries stream=codec_name -of csv=p=0 \
	"rtsp://admin:e2e-new-pw@$GATE_IP/main" | grep -q . || fail "after the upgrade, RTSP does not play"
ok "Gate 1 runs milesight/demo@0.8.0 after the plan said it would restart; its stream plays"

step "plugins: a signed example plugin, approved, runs confined in a camera (D84, D85, D86)"
# The example plugin, built for this machine and signed with the key above.
mkdir -p "$PK/hello/bin/linux-$(cd "$ROOT" && go env GOARCH)"
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$PK/hello/bin/linux-$(go env GOARCH)/hello" ./examples/plugins/hello) || fail "building the example plugin"
printf 'format: 1\nkind: plugin\nid: examples/hello\nversion: 1.0.0\nrequires: { contract: 1 }\nplugin: { engine: hello, executable: hello, permissions: [net.listen, state.read, events.emit] }\n' \
	>"$PK/hello/manifest.yaml"
chmod -R a+rwX "$PK"
mvcli pkg build "$PK/hello" -o "$PK/hello.mvpkg" -k "$PK/acme.key" >/dev/null || fail "pkg build of the plugin"
IMP=$(api POST /packages -F "file=@$PK/hello.mvpkg")
PLID=$(echo "$IMP" | json 'd["plugin"]["id"]') || fail "the plugin package: $IMP"
[ "$(echo "$IMP" | json 'd["plugin"]["signature_status"] + " " + str(d["plugin"]["enabled"]) + " " + ",".join(d["plugin"]["permissions"])')" = \
	"trusted False net.listen,state.read,events.emit" ] || fail "the plugin as installed: $IMP"
ok "examples/hello 1.0.0 installed disabled, signed by Acme; its program described itself confined"
# Until it is enabled no profile may use its engine.
sed -e 's/^  id: milesight\/demo$/  id: examples\/hello-cam/' -e 's/^  version: 0.7.0$/  version: 0.1.0/' \
	-e 's/^engines:$/engines:\n  hello:\n    engine: hello@^1\n    port: 7000\n    greeting: HELLO/' \
	-e 's/^events:$/events:\n  motion: { vendor_name: Motion }/' "$ROOT/profiles/milesight-demo.yaml" >"$PK/hello-cam.yaml"
code=$(api POST /packages -F "file=@$PK/hello-cam.yaml" -o "$PK/hello-cam.json" -w '%{http_code}')
[ "$code" = 422 ] && grep -q 'unknown engine' "$PK/hello-cam.json" || fail "a profile with a disabled plugin answered $code"
[ "$(api PATCH "/plugins/$PLID" -H 'Content-Type: application/json' -d '{"enabled":true}' | json 'd["enabled"]')" = True ] || fail "enabling the plugin"
[ "$(api POST /packages -F "file=@$PK/hello-cam.yaml" | json 'd["profile"]["profile_id"]')" = examples/hello-cam ] || fail "the profile with the plugin"
ok "enabled, which approves its permissions: a profile now uses engine hello@^1"
PL_IP=10.77.0.14
PL=$(api POST /cameras -H 'Content-Type: application/json' -d "{\"name\": \"Plugged\", \"profile_id\": \"examples/hello-cam\",
	\"profile_version\": \"0.1.0\", \"network\": {\"ip\": \"$PL_IP\"}, \"start\": true}" | json 'd["id"]')
wait_state "$PL" running 120
PL_SERIAL=$(api GET "/cameras/$PL" | json 'd["serial"]')
hello() { client python3 - "$PL_IP" "$@" <<'PY'
import socket, sys
c = socket.create_connection((sys.argv[1], 7000), timeout=5)
f = c.makefile("rw")
print(f.readline().strip())
for cmd in sys.argv[2:]:
    f.write(cmd + "\n"); f.flush()
    print(f.readline().strip())
PY
}
out=$(hello event "set Lobby")
echo "$out" | sed 's/^/   /'
[ "$(echo "$out" | sed -n 1p)" = "HELLO $PL_SERIAL Network Camera" ] || fail "the plugin's greeting: $out"
echo "$out" | sed -n 2p | grep -q '^EMITTED ' || fail "the plugin did not raise its event: $out"
echo "$out" | sed -n 3p | grep -q '^DENIED .*state.write' || fail "a write without state.write: $out"
for _ in $(seq 1 20); do
	[ "$(api GET "/events?camera_id=$PL&type=motion" | json 'len(d["items"])')" -ge 1 ] && break
	sleep 0.25
done
[ "$(api GET "/events?camera_id=$PL&type=motion" | json 'len(d["items"])')" -ge 1 ] || fail "the plugin's event is not in the log"
ok "from the LAN: its greeting reads the camera's serial and state, its event is logged, a write without state.write is denied"
HPID=$(node_exec sh -c 'for d in /proc/[0-9]*; do
	tr "\0" " " 2>/dev/null <"$d/cmdline" | grep -q "/bin/linux-[a-z0-9]*/hello" && basename "$d"
done; true' | head -n 1)
[ -n "$HPID" ] || fail "the plugin's process was not found"
hst=$(node_exec cat "/proc/$HPID/status")
huid=$(echo "$hst" | awk '/^Uid:/{print $2}')
cuid=$(node_exec cat "/proc/$(echo "$hst" | awk '/^PPid:/{print $2}')/status" | awk '/^Uid:/{print $2}')
[ "$huid" = "$cuid" ] && [ "$huid" != 0 ] || fail "the plugin runs as $huid, its camera as $cuid"
echo "$hst" | grep -q 'NoNewPrivs:\s*1' && echo "$hst" | grep -q 'Seccomp:\s*2' || fail "the plugin is not confined: $(echo "$hst" | grep -E 'NoNewPrivs|Seccomp')"
echo "$hst" | grep -E '^CapEff' | awk '{print $2}' | grep -q '^0*$' || fail "the plugin holds capabilities"
ok "the plugin (pid $HPID) runs as its camera's user $huid, with no capabilities, no_new_privs and a seccomp filter"
api DELETE "/cameras/$PL" -o /dev/null

step "diagnostics: what the camera served, its clients, unknown requests, log and metrics (D79, D92)"
GATE_IP=$(api GET "/cameras/$CID" | json 'd["network"]["ip"]')
for _ in 1 2 3; do
	client curl -s -o /dev/null --digest -u admin:e2e-new-pw "http://$GATE_IP/cgi-bin/operator/operator.cgi?action=get.system.information"
done
client curl -s -o /dev/null --digest -u admin:not-the-password "http://$GATE_IP/cgi-bin/operator/operator.cgi?action=get.system.information"
client curl -s -o /dev/null --digest -u admin:e2e-new-pw "http://$GATE_IP/cgi-bin/e2e-unknown.cgi?probe=1"
client ffprobe -v error -rtsp_transport tcp -show_entries stream=codec_name -of csv=p=0 "rtsp://admin:e2e-new-pw@$GATE_IP/main" >/dev/null 2>&1 || true
diag_ok() {
	api GET "/cameras/$CID/clients?window=1h" >"$WORK/clients.json"
	python3 - "$WORK/clients.json" "$CLIENT_IP" <<'PY'
import json, sys
c = [x for x in json.load(open(sys.argv[1]))["items"] if x["ip"] == sys.argv[2]]
routes = {r["route"]: r for r in c[0]["routes"]} if c else {}
ok = (c and routes.get("device-info", {}).get("count", 0) >= 3 and "rtsp:DESCRIBE" in routes and c[0]["auth_failures"] >= 1
      and c[0]["gaps"] >= 1 and set(c[0]["protocols"]) >= {"http", "rtsp"})
if ok:
    i = routes["device-info"]
    print(f'{c[0]["connections"]} connections over {",".join(c[0]["protocols"])}, device-info {i["count"]} times, '
          f'p95 {i["p95_ms"]:.1f} ms, {c[0]["auth_failures"]} refused, {c[0]["gaps"]} unknown')
sys.exit(0 if ok else 1)
PY
}
# The camera reports what it served every 10 s.
for _ in $(seq 1 40); do diag_ok >/dev/null && break; sleep 0.5; done
summary=$(diag_ok) || fail "the client's diagnosis: $(cat "$WORK/clients.json")"
ok "client $CLIENT_IP: $summary"
api GET "/cameras/$CID/gaps" | json '"\n".join(g["summary"] for g in d["items"])' | grep -q '^GET /cgi-bin/e2e-unknown.cgi?probe$' ||
	fail "the unknown request is not a gap: $(api GET "/cameras/$CID/gaps")"
[ "$(api GET "/cameras/$CID/requests?window=1h&format=csv" | head -n 1)" = "minute,client_ip,route,count,errors,auth_failures,p50_ms,p95_ms,max_ms" ] ||
	fail "the CSV export"
api GET "/cameras/$CID/logs" | json '"\n".join(l["msg"] for l in d["items"])' | grep -q '^running$' || fail "the camera's log"
[ "$(api GET "/cameras/$CID/metrics?range=1h" | json 'len(d["samples"])')" -ge 1 ] || fail "no stored metrics"
api GET /metrics | grep -q "^mockvision_camera_up{camera_id=\"$CID\",name=\"Gate 1\",state=\"running\"} 1$" || fail "Prometheus metrics"
ok "the unknown request is a gap, the CSV export, the camera's log, metrics every 10 s and Prometheus' text format"

step "scraper: register, probe read-only, capture and compile a draft profile (D44, D73, D75, RN-17, RN-18)"
GATE_IP=$(api GET "/cameras/$CID" | json 'd["network"]["ip"]')
# The scraper runs on the node, so it reaches a macvlan camera through the
# node bridge (D26); a real camera on the LAN is reached directly.
ip addr add 10.77.0.1/24 dev "$LAN"
api PATCH /settings -H 'Content-Type: application/json' -d '{"node_bridge":true}' >/dev/null
ping -c 2 -W 2 "$GATE_IP" >/dev/null || fail "the node cannot reach the camera through the bridge"
# Registered without authorizing, a probe is refused (RN-17).
DEV=$(api POST /devices -H 'Content-Type: application/json' -d "{
	\"name\": \"Gate (captured)\", \"host\": \"$GATE_IP\", \"ports\": [80, 554],
	\"username\": \"admin\", \"password\": \"e2e-new-pw\"}")
DEV_ID=$(echo "$DEV" | json 'd["id"]')
[ "$(echo "$DEV" | json 'd["has_password"]')" = True ] || fail "the device kept no password"
code=$(api POST "/devices/$DEV_ID/actions/probe" -o "$WORK/probe-denied.json" -w '%{http_code}')
[ "$code" = 409 ] && grep -q 'confirm you own' "$WORK/probe-denied.json" ||
	fail "an unauthorized probe was not refused: $code $(cat "$WORK/probe-denied.json")"
ok "a probe is refused until the device is authorized (RN-17)"
# Authorized, the read-only probe identifies the Milesight HTTP service. The
# update replaces the device, so every field it keeps is sent again.
api PATCH "/devices/$DEV_ID" -H 'Content-Type: application/json' -d "{
	\"name\": \"Gate (captured)\", \"host\": \"$GATE_IP\", \"ports\": [80, 554],
	\"username\": \"admin\", \"authorized\": true}" >/dev/null
[ "$(api GET "/devices/$DEV_ID" | json 'd["authorized"]')" = True ] || fail "the device was not authorized"
DET=$(api POST "/devices/$DEV_ID/actions/probe")
echo "$DET" | json 'd["detected"]["reachable"]' | grep -qx True || fail "the probe did not reach the device: $DET"
[ "$(echo "$DET" | json 'd["detected"]["vendor"]')" = Milesight ] ||
	fail "the probe did not identify Milesight: $(echo "$DET" | json 'd["detected"]')"
echo "$DET" | json '",".join(str(x["port"])+":"+x["proto"]+":"+x.get("auth","") for x in d["detected"]["services"])' | grep -q '80:http:digest' ||
	fail "the HTTP service with digest was not found: $(echo "$DET" | json 'd["detected"]["services"]')"
api GET "/devices/$DEV_ID" | json 'd.get("password","")' | grep -q . && fail "the API exposed a password (RN-18)"
ok "authorized, the read-only probe finds Milesight on 80 (digest); the password never comes back (RN-18)"
# Only a read-only program is offered for this device.
api GET "/programs?device=$DEV_ID" | json '",".join(p["program_id"] for p in d["items"])' | grep -q 'milesight/demo-capture' ||
	fail "the demo capture program is not offered: $(api GET "/programs?device=$DEV_ID")"
ok "the catalog offers milesight/demo-capture for a Milesight device"
# Capture it. A line crossing is pushed to the device's receiver URL while
# the capture listens; in the field a camera's alarm HTTP push targets it.
CAP=$(api POST "/devices/$DEV_ID/captures" -H 'Content-Type: application/json' -d '{"program":"milesight/demo-capture"}')
CAP_ID=$(echo "$CAP" | json 'd["id"]')
RECV="http://127.0.0.1:$PORT$(api GET "/devices/$DEV_ID" | json 'd["receiver_url"]')"
for _ in $(seq 1 16); do
	curl -sS -o /dev/null -X POST -H 'Content-Type: application/json' -H 'Authorization: Digest secret-not-recorded' \
		-d '{"eventType":"LineCrossing","serialNumber":"6C00DEADBEEF","ipAddress":"10.77.0.12","time":"2026-10-09T10:00:00Z"}' "$RECV" || true
	[ "$(api GET "/captures/$CAP_ID" | json 'd["status"]')" != running ] && break
	sleep 0.5
done
for _ in $(seq 1 40); do
	[ "$(api GET "/captures/$CAP_ID" | json 'd["status"]')" != running ] && break
	sleep 0.25
done
api GET "/captures/$CAP_ID" -o "$WORK/capture.json" >/dev/null
[ "$(json 'd["status"]' <"$WORK/capture.json")" = done ] || fail "the capture did not finish: $(cat "$WORK/capture.json")"
python3 - "$WORK/capture.json" <<'PY' || fail "the recorded fixtures are wrong: $(cat "$WORK/capture.json")"
import json, sys
d = json.load(open(sys.argv[1]))
f = {x["step_id"]: x for x in d["result"]["fixtures"]}
assert f["device-info"]["status"] == 200 and "Milesight" in f["device-info"]["body"], f["device-info"]
assert f["snapshot"]["binary"] and f["snapshot"]["bytes"] > 0, f["snapshot"]
assert "m=video" in f["rtsp-main"]["body"], f["rtsp-main"]
ev = f["line-crossing-event"]
assert ev["kind"] == "event" and ev["status"] == 200 and "LineCrossing" in ev["body"], ev
assert "Authorization" not in (ev.get("headers") or {}), ev["headers"]
print("   ok: %d read-only steps recorded (%d ok); the pushed event is kept without its secret header" % (d["result"]["steps"], d["result"]["ok"]))
PY
# Compile the capture into a draft profile, redacting the device's identity.
CMP=$(api POST "/captures/$CAP_ID/actions/compile" -H 'Content-Type: application/json' \
	-d '{"profile_id":"acme/scraped-cam","vendor":"Milesight","model":"Scraped X"}')
[ "$(echo "$CMP" | json '(d["profile"] or {}).get("profile_id","")')" = acme/scraped-cam ] ||
	fail "the compile produced no draft: $CMP"
[ "$(echo "$CMP" | json 'd["profile"]["version"]')" = 0.1.0 ] || fail "draft version: $(echo "$CMP" | json 'd["profile"]')"
[ "$(echo "$CMP" | json 'd["profile"]["source"]')" = capture ] || fail "draft source: $(echo "$CMP" | json 'd["profile"]["source"]')"
[ -n "$(api GET "/captures/$CAP_ID" | json 'd.get("draft_profile_id","")')" ] || fail "the capture is not linked to its draft"
ok "compiled into acme/scraped-cam@0.1.0 (source capture), linked to the capture"
# A camera can be created from the compiled draft: the round trip closes (D75).
NEW_CID=$(api POST /cameras -H 'Content-Type: application/json' -d "{
	\"name\": \"From scraped draft\", \"profile_id\": \"acme/scraped-cam\", \"profile_version\": \"0.1.0\",
	\"network\": {\"ip\": \"10.77.0.30\", \"netmask\": \"255.255.255.0\"}}" | json 'd["id"]')
[ -n "$NEW_CID" ] || fail "no camera was created from the draft"
api DELETE "/cameras/$NEW_CID" -o /dev/null
api DELETE "/devices/$DEV_ID" -o /dev/null
[ "$(api GET /devices | json 'len(d["items"])')" = 0 ] || fail "the device was not deleted"
ok "a camera is created from the scraped draft, then the draft camera and device are removed"
# Restore the node's network: the bridge is only for the scraper's reach.
api PATCH /settings -H 'Content-Type: application/json' -d '{"node_bridge":false}' >/dev/null
ip link show mv-bridge >/dev/null 2>&1 && fail "the node bridge is still there"
ip addr del 10.77.0.1/24 dev "$LAN"
ok "the node bridge is turned off again"

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
