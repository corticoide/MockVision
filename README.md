# MockVision

Simulated IP cameras on a real network. Each camera joins your LAN with its
own IP and MAC, serves RTSP and the HTTP API its vendor profile describes,
and sends events to your VMS, NVR or backend. A web panel served by the node
manages them.

It is for testing software that consumes cameras without buying them and
without touching production devices. It simulates what a camera does towards
the outside; it does not replace one.

> **Status: v1 in development.** A demo profile and the draft of a real
> model (a Dahua dome) so far; [docs/DEMO-NOTES.md](docs/DEMO-NOTES.md)
> lists what is still simplified.
> Leer en español: [README.es.md](README.es.md).

## What the demo does

- A camera created in the panel appears on the LAN with its own IP and MAC
  (macvlan), or with the node's MAC on Wi-Fi (ipvlan). It takes a static IP
  or leases one by DHCP, falling back to its factory address as a real one
  does. Before using its IP and MAC it probes the LAN for them, and it
  announces itself with gratuitous ARP.
- RTSP streams looped from a picture: main, sub and third, as the profile
  defines them, in H.264, H.265 or MJPEG. Each picture is encoded once per
  stream setting and the loop costs almost no CPU.
- An HTTP API from a YAML profile, with Digest authentication: snapshot,
  device information and reading and writing a parameter.
- Video analytics: lines and regions drawn on the camera's picture, and
  events on them (line crossing, region entrance and exit, loitering,
  intrusion) fired by hand or at random moments, and the people counts,
  occupancy and heat map the camera keeps from them. The profile declares
  every analytic.
- Events delivered as the device does: an HTTP notification (Basic or
  Digest), a message to an MQTT broker, the snapshot uploaded to an FTP or
  SFTP server, a mail with the snapshot attached; with the device's
  retries, which each target can override. Every delivery is logged with
  its status and latency.
- Faults, as a device fails: a protocol down or late, a status for every
  request (401, 500…), a moved clock, the network down, an IP conflict,
  the SD card out, failing, read only or full, and a simulated reboot.
  Every fault ends on its own or by hand, and the camera shows as
  *degraded* while one is on.
- Recordings, as the device keeps them: each event the profile says
  records its snapshot and a clip on a simulated SD card, with a quota and
  cyclic overwrite, or on a NAS share over NFS or SMB. Clients search and
  download them through the camera's API and play them back over RTSP by
  time range.
- Metrics for each camera (CPU, RAM, clients). A camera is refused, with the
  reason, when it would go over the camera limit or the node's resources.
- A panel that updates live over a WebSocket.

## Requirements

- Linux on amd64 or arm64: Debian 12 or 13, Ubuntu 24.04 or later, or
  Raspberry Pi OS 64-bit. The kernel needs macvlan.
- **A wired network**, for cameras with their own MAC (macvlan): Wi-Fi
  does not accept extra MACs, and switches with port security can block
  them too. In a VM, the hypervisor must allow promiscuous mode or MAC
  changes. On Wi-Fi, cameras use the node's MAC (ipvlan, kernel module
  `ipvlan`) and a static IP.
- Docker with Compose v2, or a native install with FFmpeg and systemd.
  Docker Desktop on Windows or macOS does not work, because it runs behind
  NAT.

## Quick start with Docker

```sh
git clone https://github.com/corticoide/MockVision.git
cd MockVision
docker compose up -d   # builds the image the first time
```

Open `http://<node>:8080`. On the first visit the panel asks you to create the
administrator; there are no default credentials. Creating it needs the
node's one-time **setup code**, so whoever reaches the port first cannot
claim the node; it is printed in the log and kept until it is used:

```sh
docker compose logs mockvision | grep setup_code   # or:
docker compose exec -u mockvision mockvision cat /data/setup-code
```

Then:

1. **Profiles → Import profile:** choose `profiles/milesight-demo.yaml`. It
   is validated and listed as *Draft*.
2. **Targets → New target:** enter the URL that should receive the events,
   for example `http://192.168.1.10:8000/events`, or pick another type: an
   MQTT broker, an FTP or SFTP server, a mail server (see
   [Event targets](#event-targets)).
3. **Cameras → New camera:** enter a free IP address of your LAN and pick the
   target. The camera starts and turns *Running*.

From **another device** on the same LAN (IP `192.168.1.50` and password
`secret` are examples):

```sh
ping 192.168.1.50
ip neigh show 192.168.1.50   # the camera's own MAC, not the node's
ffprobe rtsp://admin:secret@192.168.1.50:554/main   # also /sub and /third
curl --digest -u admin:secret -o snapshot.jpg http://192.168.1.50/snapshot.cgi
curl --digest -u admin:secret "http://192.168.1.50/cgi-bin/operator/operator.cgi?action=get.system.information"
curl --digest -u admin:secret "http://192.168.1.50/cgi-bin/operator/param.cgi?action=set&Image.Brightness=70"
curl --digest -u admin:secret "http://192.168.1.50/cgi-bin/operator/param.cgi?action=get&name=Image.Brightness"
```

Camera accounts have a role: `admin` and `operator` accounts may change
parameters, `viewer` accounts only read (a profile can set the roles of each
route). The camera user is the one entered when the camera was created. If the
password was left empty, the camera uses the profile's factory account
(`admin` / `ms1234`). Press **Trigger event** on the camera: the target
receives the POST, and **Events** shows the delivery, the HTTP status and the
latency.

> **Test from another device.** Linux does not let a host reach its own
> macvlan interfaces. The node therefore cannot open its cameras' streams,
> and its cameras cannot deliver events to a receiver running on the node.
> Put the client and the targets on other machines, or turn on **Settings →
> Reach the cameras from this node** (see [Network](#network)). The panel's
> snapshot preview works anyway, because it does not use the network.

### Automation with API tokens

Create a token in **Settings → API tokens**; it is shown once. Scripts and
CI send it as a Bearer header, with no cookie or custom header:

```sh
TOKEN=mvt_…   # from Settings
curl -H "Authorization: Bearer $TOKEN" "http://<node>:8080/api/v1/cameras?state=running"
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"action":"stop","ids":["<camera id>"]}' http://<node>:8080/api/v1/cameras/actions/bulk
```

A read token can only query the node (and open the WebSocket); a write
token can change it, except tokens, which are managed from the panel only.
**Audit** shows every change with where it came from: the panel, the API
with the token's name, a client of a camera's emulated API, or the node.

### Background jobs

Encoding a stream and importing a package run as jobs (**Jobs** in the
panel, `/api/v1/jobs` in the API), with their progress and history. At most
two run at once (**Settings → Limits**); the rest wait in the queue.
Closing the browser stops nothing, and a job cut short by a restart stays
*Interrupted* until you resume it from its last checkpoint. **Prepare
renditions** encodes up front every stream the cameras need, so starting
many of them waits for nothing; when one fails it asks whether to retry,
skip it or stop, and skips it if nobody answers within ten minutes. A job
waiting for an answer does not hold back the others.

### Streams and codecs

A camera encodes the same picture once per use, as real ones do: the
**main** stream for recording, a light **sub** stream for grids and phones,
and a **third** one, often MJPEG, for simple clients. Each has its RTSP
address (`rtsp://<ip>/main`, `/sub`, `/third` with the demo profile), shown
in the camera's **Media** tab with its snapshot. Codec, resolution, frame
rate, bitrate and GOP change there, or from the camera's own API, when the
profile binds a parameter to them; the stream is encoded again and the
camera switches to it without restarting.

- **H.264** plays in every client. **H.265** needs about half the bitrate for
  the same picture, but not every client plays it. **MJPEG** sends every
  frame as a JPEG; over RTSP it carries at most 2040×2040 in multiples of 8.
- A client that connects gets a keyframe at once, as from a real encoder.

### Rules and triggers

Rules say where events happen and triggers say when; the camera does not
look at its picture. In the camera's **Rules** tab you draw them over its
snapshot:

- A **line** reports crossings. Its sides are A (on the left, walking from
  its first point to its second) and B; it reports A → B, B → A or both.
- A **region** reports what you tick: entrance, exit, loitering or
  intrusion.
- Each rule can detect some object classes only (car, person…); none means
  all of the profile's.

An event names its rule (ID, name and type), the direction of a crossing and
an object with a class, a color, a confidence and a box placed on the rule.
The camera makes up what nobody gives it.

- **By hand:** **Trigger event** in the header, the ⚡ buttons of each rule,
  or `POST /api/v1/cameras/{id}/events` with `type` and, optionally,
  `rule_id`, `direction`, `object`, `plate` and `speed`.
- **At random:** the **Triggers** tab keeps triggers that emit an event of a
  type, on a rule or on any enabled one, at a random moment between two
  waits, while the camera runs. They can give their events license plates,
  from a list or generated from formats such as `AA999AA` (9 a digit, A a
  letter, X a hex digit), and speeds in a range. **Fire once** tries one.

```sh
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -X PUT \
  -d '{"triggers":[{"name":"Traffic","event_type":"line_crossing","min_seconds":5,"max_seconds":30}]}' \
  http://<node>:8080/api/v1/cameras/<camera id>/triggers
```

Rules and triggers apply at once, without restarting the camera. A camera
of the demo starts with the rules its profile ships (**Line 1** and
**Region 1**); clones copy rules and triggers, and restoring a camera brings
its factory rules back and keeps its triggers, which then fire on any rule.
An event that comes from a line or a region needs one: without an enabled
rule that reports it, the camera does not send it, as a real one would not.

#### What the profile decides

Nothing about analytics is built into the node; the profile declares it:

- which kinds of rule the camera has (`vca.rules`), the objects it detects
  and the rules it ships with (`vca.factory_rules`);
- the events it can send, each with its payload for every transport. Common
  ones have canonical names (`line_crossing`, `region_entrance`, `lpr`…);
  anything else is `custom:<name>`;
- the kind of rule each event comes from (`rule: line`, `region` or
  `none`). Canonical events have a default; a vendor event such as
  `custom:object_left` can come from regions and be drawn like any other;
- how often it may report each event, and which parameter turns it off
  (`bind: events.<type>.enabled`, the demo's `Event.LineCrossing.Enable`).

A profile without line crossings has no lines to draw, no crossings to fire
and no ⚡ for them in the camera list.

#### Counts, heat map and reports

Like a real camera, each camera counts from the events it emits: crossings
of each line by direction and object class (people counting), entries,
exits and occupancy of each region, events by type, and where objects were
on a grid over the picture (a heat map). The **Rules** tab shows the counts
of each rule and, with **Heat map**, shades where objects were; **Reset
counts** starts again from zero. `GET /api/v1/cameras/{id}/analytics?cols=32&rows=18`
returns them.

Profiles serve them in the vendor's format through their templates:
`analytics` (every count), `lineCount "Gate" "A->B"`, `occupancy "Lot"` and
`heatmap 32 18` (rows of cells); `storage` gives the SD card or NAS share. The demo answers
`/cgi-bin/operator/operator.cgi?action=get.vca.counting` and
`action=get.vca.heatmap`, and pushes a report: an event marked `report: true`
(`custom:people_counting`) carries the counts instead of an object; a
trigger with the same shortest and longest wait sends one at a fixed
interval.

### Event targets

A target is a receiver several cameras share; each camera sends it the
events of the types it was linked for, through the transports its profile
defines for each event.

| Type | URL | What the camera does |
| --- | --- | --- |
| HTTP | `http://host:port/path` | A request per event with the profile's payload. **Authentication** Basic, or Digest: the camera answers the target's challenge (MD5 or SHA-256, `qop=auth`). |
| MQTT | `mqtt://host:1883`, `mqtts://host:8883` | Connects as soon as it runs and keeps the session up, pinging when idle and reconnecting when it drops; publishes each event with the profile's topic, QoS (0, 1 or 2) and retain flag. The profile's birth and will messages announce it online and offline. A target may replace the topic and the client ID with templates; each camera needs its own client ID, its serial by default. |
| FTP | `ftp://host:21/dir` | Uploads the event's snapshot, or a document the profile renders, in passive mode (EPSV, else PASV), under a directory and name of the profile's templates. The path is relative to the login directory; `%2F` starts it at the root. |
| SFTP | `sftp://host:22/dir` | The same over SSH with the target's password. The path is absolute; `/~/` starts it at the home directory. **Host key** pins the server's SHA256 fingerprint; empty accepts any key, as most cameras do. |
| E-mail | `smtp://host:25` | A mail per event with the profile's subject and text and the snapshot attached, to up to five recipients; plain, STARTTLS or TLS (port 465), with AUTH PLAIN or LOGIN. The profile's interval sends at most one mail per target in it: the rest show as *Skipped*. |

**Delivery.** The profile sets the device's timeout, retries and pause
between attempts (D42); a target may override any of them. **Test** checks a
target from a running camera that uses it, across the same network as the
deliveries, or else from the node: a request to an HTTP target, a session
with a broker, a login and the target's directory on FTP and SFTP, the
sender and recipients on a mail server. Nothing is uploaded or mailed.

The demo profile publishes its analytics events to
`milesight/<serial>/event/<event>` and the counting report, retained, to
`milesight/<serial>/counting`; uploads the snapshot to
`<serial>/<date>/<time>_<event>.jpg`; and mails at most once every 10 s.

### Faults

The **Faults** tab of a camera injects the failures a client has to
survive (D43). A fault applies at once, without restarting the camera, and
the camera shows as *degraded*, naming its faults, until the last one ends;
**Needs attention** on the dashboard lists them too. Every fault ends when
its duration is over (30 s to 24 h) or by hand (RN-14); a stopped camera
gets the faults still on as it starts.

| Fault | What a client sees |
| --- | --- |
| Service down | The protocol (RTSP, the HTTP API…) resets its connections and refuses new ones; the others keep answering. |
| Latency | Everything the protocol reads waits the delay: every answer comes late. |
| Error status | Every request to the HTTP API or RTSP gets 401 (with a challenge, as refused credentials), 403, 404, 500 or 503. |
| Clock skew | The camera's clock moves: its events, its answers and its templates carry the moved time. |
| Network down | The camera answers nobody, not even ARP, and reaches nobody. It raises `network_lost`, which goes out once it is back if its retries last. |
| IP conflict | The camera raises `ip_conflict` and keeps answering. |
| SD card missing, error, read only, full | Only for a camera with a card. Its state is forced and it raises `storage_missing`, `storage_failure` or `storage_full`; it records nothing meanwhile, and a missing or failing card cannot be searched. |

**Reboot** takes the camera off the network for its boot time (the
profile's `identity.boot_time`, 30 s when it says none, or the seconds
given), as the real one does, and it comes back with its faults. The API
has the same: `POST /api/v1/cameras/{id}/faults`, `DELETE
/api/v1/cameras/{id}/faults/{fault}`, `GET /api/v1/faults` and `POST
/api/v1/cameras/{id}/actions/reboot`.

### Storage

The **Storage** tab gives a camera a simulated SD card or a NAS share
(D68, D69), as its model allows: the profile's `storage` declares the
largest card it takes and the NAS protocols, and each event's `record` what
it keeps: the snapshot and a clip of a stream, up to 5 minutes. Clips are
MPEG-TS of the stream's loop, H.264 or H.265.

- **SD card.** A directory of the node with a quota, from 64 MB up to the
  model's largest card. The service writes it and the camera only reads it.
  When full it overwrites the oldest recordings, as cameras do; with
  overwrite off it stops recording and raises `storage_full`. A new card,
  or one that grows, must fit on the node's disk with what the other cards
  promised and have not used yet, or it is refused (D91). **Format** wipes
  it; taking it out wipes it too, and a smaller one keeps the newest
  recordings that fit.
- **NAS share.** `nfs://host[:port]/export[?uid=N&gid=N]` or
  `smb://host[:port]/share[/folder]` with a username and password
  (`DOMAIN\user` for a domain account). The camera connects and writes from
  its own address, under a folder named after its serial; its firewall
  opens the share's host. An NFS export must allow the camera's address and
  ports above 1023 (`insecure`), since cameras run without privileges. The
  tab shows why the camera cannot reach its share.

The camera's API searches and downloads the recordings with the handlers
`sd.search` (by time, kind and event, in the device's words and time
format) and `sd.download`; the template function `storage` gives the card's
state and space; and the `rtsp` engine's `playback` path plays the clips of
a time range back. The demo answers
`/cgi-bin/operator/operator.cgi?action=get.record.search&starttime=…&endtime=…&type=video`,
`/cgi-bin/operator/download.cgi?file=…`, `action=get.storage.info` and
`rtsp://…/playback?starttime=20261007T143000Z&endtime=20261007T150000Z`. The
panel lists the newest recordings and downloads them; the API has `GET` and
`PUT /api/v1/cameras/{id}/storage`, `POST …/storage/actions/format`, `GET
…/recordings` and `GET …/recordings/{recording}/download`.

### Profiles

A profile describes what one camera model does on the network, as its
clients see it: its streams and RTSP paths, its HTTP API, the events it
sends and how it sends them. The device's own web panel is out of scope,
and so is any setting that only that panel reads or changes.

- `profiles/milesight-demo.yaml` is illustrative: its routes and payloads
  were not captured from a device.
- `profiles/dahua-ipc-hdbw1230e-s4.yaml` drafts a real model, the Dahua
  IPC-HDBW1230E-S4 (2 MP dome), from its manual, its datasheet and Dahua's
  public HTTP API. It serves RTSP at `/cam/realmonitor?channel=1&subtype=0`
  (main) and `subtype=1` (sub), challenges with `Login to <serial>` as the
  unit does, and answers `magicBox.cgi`, `snapshot.cgi`,
  `global.cgi?action=getCurrentTime` and `configManager.cgi`:
  `getConfig&name=Encode` reads a whole table and
  `setConfig&Encode[0].MainFormat[0].Video.FPS=15` changes the stream. With
  curl, `-g` sends the brackets as they are:
  `curl -g --digest -u admin:admin1234 'http://<ip>/cgi-bin/configManager.cgi?action=getConfig&name=Encode'`.
  Its events have no transport yet: a Dahua sends them over
  `eventManager.cgi?action=attach`, ONVIF or its private protocol, all on
  the roadmap, like codec and resolution changes reaching the stream. The
  file's header lists what else is missing.

Engines repeat what the vendor shows on the wire: `auth.realm` may name the
camera (`"Login to {{ .Camera.Serial }}"`) in the HTTP API and in RTSP, and
the RTSP engine's `server` sets the `Server` header of its answers.

### Network

The camera's **Network** tab, and the new-camera dialog, choose how it joins
the LAN. Changes apply when the camera restarts; the tab shows the address
it holds now and where it came from.

| Mode | For | Addressing | Keep in mind |
|---|---|---|---|
| *macvlan* (default) | wired networks | static IP or DHCP | its own MAC, like a real device: Wi-Fi and switch ports with port security drop it |
| *ipvlan* | Wi-Fi, switch ports that allow one MAC | static IP | the node's MAC, so DHCP servers cannot tell it apart |

A network card carries macvlan or ipvlan cameras, not both, as the kernel
wants; **Reach the cameras from this node** counts as macvlan. The panel
names the cameras in the way.

- **DHCP.** The camera asks the LAN's server, like a new camera out of the
  box, and renews its lease. When no server answers within about 15 seconds
  it takes its profile's factory address (`192.168.5.190` for the demo
  profile) and keeps asking. A renewal with another router or DNS servers
  applies at once; another address, or a lost lease, restarts the camera.
- **DNS.** The camera's own servers, else the lease's, else the node's.
- **Conflicts.** An IP or a MAC another device answers for stops the start
  and names the device. **Start even if another device answers** skips the
  check, to test how clients handle a conflict.
- **Outbound.** A camera connects only to the event targets, its DNS
  servers and DHCP; clients reach it from anywhere, as a real one. An FTP
  server is open on every port, for its passive data connections.
- **Reaching the cameras from the node.** **Settings → Reach the cameras
  from this node** adds a bridge interface, `mv-bridge`, and a route to each
  macvlan camera, so players, recorders and targets on the node itself work.
  The parent interface needs an IPv4 address. It is off by default because
  it changes the node's network; ipvlan cameras stay out of reach either
  way.

A camera that retrying cannot start, on a busy network card or a kernel
without ipvlan, stops trying and says why; the panel shows each reason in
its language.

### Configuration

`compose.yaml` passes the two settings most installs need. Others go in its
`environment` section.

| Variable | Default | Meaning |
|---|---|---|
| `MOCKVISION_LISTEN` | `:8080` | Address of the panel and the API; use `<ip>:<port>` to listen only on a management IP |
| `MOCKVISION_PARENT_IF` | default route's interface | Interface the cameras attach to; the panel's Settings can change it |
| `MOCKVISION_DATA` | `/data` | Database, node key, pictures and encoded streams |
| `MOCKVISION_FFMPEG` | `ffmpeg` | FFmpeg binary |
| `MOCKVISION_SECURE_COOKIES` | off | `1` behind an HTTPS reverse proxy |
| `MOCKVISION_ALLOWED_ORIGINS` | none | Extra origins (`host:port`) allowed to call the API |
| `MOCKVISION_ALLOWED_HOSTS` | any | Host names the panel answers to (comma separated); set it to stop DNS rebinding. IP addresses are always accepted |
| `MOCKVISION_TRUSTED_PROXIES` | none | Reverse proxies (IPs or CIDRs) whose `X-Forwarded-For` names the client, for sign-in limits and the audit log |
| `MOCKVISION_LOG_LEVEL`, `MOCKVISION_LOG_FORMAT` | `info`, text | `debug`…`error`; `json` |
| `MOCKVISION_SERVICE_USER`, `MOCKVISION_CAMERA_USER` | `mockvision`, `mockvision-cam` | Users of the main service and of the cameras: names that must exist, or numeric uids; they must differ |

The limits (maximum cameras, RAM and CPU thresholds, event retention) are set
in **Settings**.

### Without Docker

Install FFmpeg, build (`make build`, needs Go 1.27 and Node 22) and install
the binary with the systemd unit in
[deploy/mockvision.service](deploy/mockvision.service); its header lists the
steps. The service and camera users must exist, and the binary must be
executable by the camera user (mode 0755): cameras start as that user.

## How it works

One binary runs as three kinds of process:

```
mockvision run      root, 9 capabilities   network helper: namespaces, macvlan/ipvlan, probes, firewall, launching cameras
 └─ mockvision serve   uid mockvision, none   panel, REST API, WebSocket, SQLite, reconciler
     └─ FFmpeg, package validator   confined: seccomp and Landlock
 └─ mockvision camera  uid mockvision-cam, none   one per camera, in its network and PID namespaces
```

- The **network helper** is the only privileged process. It accepts a closed
  set of validated requests from the service over a private socket. It never
  runs a shell, and once the service is started it empties its capability
  bounding set: nothing it launches can hold a capability.
- The **main service** keeps the desired state in SQLite and reconciles it
  with the kernel at boot and every 10 seconds. Cameras with autostart come
  back after a restart.
- Each **camera** gets its sockets already open in its namespace and starts
  directly as the camera user, never as root, in a PID namespace of its own.
  Before reading any input it sets `no_new_privs` and a seccomp filter (no
  namespaces, mounts, tracing, modules or keyrings), and Landlock limits its
  files to its streams, its SD card (read only) and what DNS and TLS need.
  It then serves the profile's engines (`rtsp`, `http-api`, `http-push`
  and the rest), records to its NAS share, runs its random
  triggers and talks to the service in JSON lines; the service checks what
  it reports against the profile, and keeps an event's rule and trigger
  only when they are the camera's own. A DHCP camera leases its address
  itself, over a socket the helper opened for it: it parses what servers
  send without privileges, the service checks the lease and the helper
  sets it.
- **Probes.** Before a camera takes an IP, the helper sends an ARP probe
  for it (RFC 5227); before it takes a MAC, the helper looks for it in the
  node's tables and interfaces, asks for it over IPv6 and listens for a
  moment. The kernel refuses a MAC another interface of the node has, even
  when the probe is skipped.
- **Firewall.** Each camera's namespace has an nftables table that lets out
  one set of address, protocol and port: its targets, and its DNS servers
  on port 53; a second set opens every port of FTP servers, for their
  passive data connections, and of NAS shares. Target names are resolved again every minute with the
  camera's DNS servers, and an address seen in the last ten minutes stays
  allowed, for names that rotate. DHCP, answers to its clients, RTP from
  its own ports and loopback pass too.
- **FFmpeg**, which decodes uploaded pictures, and the **package validator**
  run confined too: FFmpeg reaches only the picture it reads and the
  rendition it writes, the validator no file at all. Neither can read the
  database or the node key.

Named namespaces make `ip netns exec sim-<camera> …` work for debugging (with
Docker: `docker compose exec mockvision ip netns`). Where Docker's AppArmor
profile forbids mounts, cameras use anonymous namespaces instead.

The architecture, profile format, API and decisions come from the project's
design document; `backend/internal/api/openapi.yaml` describes the REST API.

## Development

```sh
make dev                     # local mode: no privileges, cameras on 127.0.0.1 with their own ports
cd frontend && npm run dev   # panel with hot reload on :5173, proxied to the node on :8080
```

Local mode is for working on the panel, the API and the engines. It has no IP
or MAC on the LAN.

| Command | What it runs |
|---|---|
| `make test` | `go vet`, unit tests and the panel's type check |
| `make test-integration` | network namespaces, macvlan, ipvlan, MAC probe, firewall, DHCP socket and bridge on a virtual link (root) |
| `make e2e` | the demo's acceptance criteria on an isolated virtual LAN (root, iproute2, ffmpeg, curl, ping, python3) |
| `make e2e-compose` | the same criteria against the Docker image started with `compose.yaml` |
| `make generate` | sqlc queries and the panel's API types from `openapi.yaml` |

The binary must be built with `CGO_ENABLED=0`: dropping privileges and the
sandbox change every thread at once, and only a pure Go binary can do that.

```
backend/    cmd/mockvision, internal/ (domain, app, store, netctl, sandbox, camera, engines, media, pkg, api, telemetry)
frontend/   React + TypeScript + Vite panel, embedded in the binary
database/   migrations and queries (sqlc)
profiles/   profile schema, the demo profile and the Dahua draft
sdk/        engine contract (Go and gRPC)
deploy/     Dockerfile and systemd unit
docs/       notes and the code audit (AUDITORIA.md)
```

## Security

- The first run creates the administrator with a one-time setup code from
  the node's log. Passwords use Argon2id, computed at most two at a time;
  each address gets ten sign-in attempts a minute, and repeated failures on
  an account lock it for that address.
- Sessions are `HttpOnly` and `SameSite=Strict` cookies. Every request that
  changes state needs a custom header and passes an `Origin` check. The
  panel runs under a strict CSP and loads nothing from other origins.
- Camera and target passwords are encrypted with XChaCha20-Poly1305. The key
  is kept outside the database, and the API never returns them.
- API tokens are stored as SHA-256 hashes and shown once. They carry a scope
  (read or write), may expire, and are revoked at once from the panel,
  WebSockets opened with them included. Panel sessions last 12 hours idle
  and 7 days at most. The audit log keeps every change for 90 days with its
  origin and IP.
- Clients of a camera's emulated API are bounded too: encoder changes are
  coalesced per camera, the job queue holds at most 500 jobs, and unused
  renditions are removed after a day.
- [docs/AUDITORIA.md](docs/AUDITORIA.md) is the code audit these measures
  come from, with how each finding was fixed.
- The panel is plain HTTP. Put it behind an HTTPS reverse proxy
  (`MOCKVISION_SECURE_COOKIES=1`) before exposing it beyond a lab network.

## License

[Apache 2.0](LICENSE)
