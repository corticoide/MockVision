# Technical demo: notes and simplifications

The demo follows the "Demo técnica" tab of the design document. Its goal is
one path, end to end: a camera created from the panel appears on the LAN with
its own IP and MAC, serves RTSP and HTTP as its profile describes, and sends a
line-crossing event by HTTP POST. Anything built simpler than the design says
is listed here, so it can be picked up later.

The repository already has the final structure: the domain, the SQLite
schema, the REST API, the engine contract and the process model are the ones
of the design. Most items below are therefore missing features, not
different designs.

## Acceptance criteria

`make e2e` checks them on an isolated virtual LAN (a veth pair, with a
"client" device in its own namespace). `make e2e-compose` runs the same checks
against the Docker image started with `compose.yaml`.

| Criterion | How it is checked |
|---|---|
| `docker compose up` and the first login creates the administrator | e2e in compose mode; `/auth/me` reports `setup_required` on a fresh node |
| `profiles/milesight-demo.yaml` is validated and listed as Draft | it comes in the official catalog, installed when the node starts: `GET /profiles` lists it as `draft`, with `milesight/base` and the Dahua draft as `documented` |
| The camera answers ping and `ip neigh` shows a MAC other than the node's | ping and `ip neigh` from the client namespace |
| `ffprobe rtsp://admin:<pw>@<ip>:554/main` returns H.264 at the configured resolution | ffprobe over TCP and UDP, `h264,640,360` |
| `curl --digest …/snapshot.cgi` returns a JPEG; device info carries the serial | curl from the client; a wrong password gets 401 |
| A manual event POSTs to the target; the panel shows status and latency | receiver on the client; `delivery_status: ok` and latency; browser run (see below) |
| Stopping removes the namespace and the interface | `ip netns` and ping after the stop |
| Restarting the node or the container brings back cameras with autostart | stop, start, ping |
| Going over the maximum or the resources rejects the creation with a reason | `max_cameras` set to 1; problem with code `max_cameras` and the reason |
| Reopening the browser shows the right state | a second browser page with the same session sees the camera running |
| No camera process keeps capabilities | `/proc/<pid>/status` of the camera and of the main service: every capability set empty, uid not 0, `no_new_privs` |
| A camera connects only to the event targets (v1) | a TCP connection from the camera's namespace reaches the target's port and not another port of the client, until a target uses it |
| A MAC in use stops the start (v1) | a camera with the MAC of the client's interface, which lives on the node, ends in error with the conflict: the kernel refuses it. The probe on the LAN is covered by the netctl integration test `TestMACProbe` |
| DHCP, with the factory address as fallback (v1) | without a server the camera answers on `192.168.5.190`, and a second one waits until the first stops and then takes it; with the e2e's DHCP server it declines Gate 1's address, leases the next and releases it when deleted |
| ipvlan, and one mode per network card (v1) | where the kernel has ipvlan, a camera on a second card answers with the card's MAC, and a macvlan camera on that card is refused with `parent_busy` |
| The node reaches its cameras through the bridge (v1) | ping and snapshot from the node with the bridge on, not with it off |
| Rules and triggers (v1) | a manual loitering on a region carries the rule's name and its only object class; a crossing the line does not report gets 422; the emulated API turns crossings off (409) and on; a random trigger sends a crossing every second or two, stops when disabled and fires once on request, without restarting the camera; its rule and trigger survive a node restart |
| Event transports (v1) | linked to an MQTT broker on the client, the running camera connects at once, with its serial as client ID and its will, and says online; a line crossing reaches the broker (QoS 1), the FTP server (the snapshot under `<serial>/<date>/`, in passive mode through the camera's firewall) and the mail server (with the snapshot attached); a second crossing within 10 s skips the mail; each target passes its test from the camera; unlinked, the camera disconnects from the broker |
| Faults (v1) | RTSP down: ffprobe fails while the HTTP API answers and the camera is degraded with the fault named; ended by hand, the stream is back. A 401 fault refuses the right password and expires on its own after 3 s. Network down: no ping, no ARP; `network_lost` fails to leave and reaches the target once back. None of them restarts the camera; a reboot takes it off the network for its boot time and back |
| Storage (v1) | a 256 GB card is refused with code `disk` on a smaller disk; on a 64 MB card a line crossing records its snapshot and a 10 s clip; the client finds the clip by time through the camera's API, downloads it (ffprobe: H.264, 10 s, the same bytes as the panel's download) and plays the range back over RTSP; `sd_missing` sends `storage_missing` to the target and the search answers 503; with Samba on the client, the camera writes its recordings to an SMB share in a folder of its serial and the panel reads them through it |
| Dahua profile (v1) | from the catalog, a Dahua camera is named after its serial; a client attached to `eventManager.cgi` with `codes=[All]&heartbeat=2` reads `Code=VideoMotion;action=Start;index=0`, then `Stop`, and heartbeats; `setConfig` of `Compression=H.265`, `Width=1280` and `Height=720` turns the main stream into H.265 at 1280×720 (ffprobe), and a height the sub stream lacks gets `Error` / `Bad Request!`; a wrong password raises `LoginFailure` |
| Packages and catalog (v1) | a key made with `pkg keygen` and trusted through the API signs a package built with `pkg build -k`: it installs as signed by Acme, and its two recordings (a request and a line crossing pushed over HTTP) match an ephemeral camera on an isolated network, so it is captured and both are verified; the same package with its manifest changed after signing is refused (422); a model that extends `milesight/base@^0.1` resolves with 0.1.0 pinned; the export is the package imported, and `pkg verify --key` trusts it; Gate 1 moves from 0.7.0 to 0.8.0 after the plan says it restarts, and its stream plays |
| Analytics from the profile (v1) | a new camera has the profile's factory line and region, and a crossing without a rule happens on the factory line; after two crossings and an entry, the node, the emulated API's counting route and a 16×9 heat map agree (2 crossings, 1 car inside, 3 objects); a report trigger with a fixed interval pushes the counts |

The panel was also driven through the whole path in Chromium with
Playwright, on a node in local mode: first-run wizard, profile import with
its report, target and test request, camera creation, snapshot preview,
manual events, event log, assets and settings. Since feature 7, also the
rule editor (drawing a line and a region with the mouse, dragging a corner,
moving it with the keyboard), firing a rule's event, a random trigger and
the header's trigger dialog, in English and Spanish; and the analytics the
profile declares: factory rules on a new camera, counts on each rule, the
heat map and resetting the counts, a report fired by hand and as a trigger,
and a camera without rules, whose crossings cannot be fired and whose ⚡
leaves the camera list. Since feature 10, also the Storage tab: an SD card
given, an event's snapshot and clip listed live and downloaded, the card
taken out by a fault (its format refused) and back, formatted, and a NAS
share the camera cannot reach, with its reason, in English and Spanish.
Since feature 12, also packages: the catalog listed with its badges, a key
trusted in Settings, a signed package with recordings imported (captured,
two recordings matching), its page with level, signature, versions, the
comparison with 0.7.0, the import report and the self-test, its export, a
catalog profile duplicated under an ID of one's own, and a running camera
moved to the new version from its page after its plan, in English and
Spanish. No
console errors besides the expected 401 before login and the 409 of a
refused action, and no CSP violations.

Not verified: VLC playback (ffprobe is), a Raspberry Pi, a physical switch,
and the systemd unit on a real host (it passes `systemd-analyze verify`).

## Measurements

On an x86_64 VM with a 6.18 kernel, a 640×360 stream at 15 fps:

- Namespace, macvlan and ARP probe (3 probes 150 ms apart, then 300 ms of
  listening): about 0.9 s. The design's target for a start is 2 s.
- A camera process: about 19–20 MiB RSS; 0.3 % CPU idle and up to about
  0.9 % during the end-to-end run. The targets are 25 MB and 1 %.
- The main service: about 23 MB RSS.
- The first camera that uses a picture at a resolution waits once for FFmpeg
  to encode it; the rendition is reused afterwards.

## Simplifications

### Network

- **The MAC probe is a heuristic.** IPv4 has no way to ask who has a MAC:
  the helper looks in the node's neighbor tables and interfaces, sends an
  IPv6 neighbor solicitation and an all-nodes echo to that MAC, and listens
  for its frames for a moment. A silent device, or one without IPv6, goes
  unnoticed until it talks.
- **ipvlan is checked only where the kernel has it**: the development VM
  has no ipvlan module, so its integration test and its e2e step skip
  there.
- **One mode per network card.** The kernel does not mix macvlan and ipvlan
  on one card, and the node bridge is macvlan; the panel refuses the mix
  and names the cameras in the way.
- IPv6 is off in the cameras' namespaces: cameras are IPv4 devices.
- Under Docker's AppArmor profile the helper cannot mount, so cameras use
  anonymous namespaces and `ip netns` does not list them. Everything else
  works the same.

### Profiles and packages

- **The official catalog is unsigned in development builds.** A release
  signs it with `mockvision pkg catalog -k` and stamps the key's public half
  into the binary (`-ldflags -X .../pkg.officialKeys=`); until the project
  has its catalog key, the catalog installs unsigned and nothing reaches
  *verified*. The community catalog repository (D82) does not exist yet.
- **The self-test sees HTTP only.** It replays requests to the http-api
  engines and checks events pushed over `http_push`; RTSP (SDP), MQTT, FTP,
  mail and `attach` expectations are reported as not checked, so a profile
  with them is not *captured*. JSON paths are simple (`$.a.b[0]`); XML is
  compared as text, with regular expressions.
- **Upgrades** keep the values, protocols, streams, rules and triggers the
  new version accepts and say what they drop; a camera's NAS or SD settings
  are kept as they are.
- **Duplicates** are new unsigned packages; there is no editor in the panel
  (D22): export, edit and import the next version.
- A loose `profile.yaml` may carry `profile.id` and `profile.version`
  itself, since it has no manifest.
- **Vendor values** translate one by one (`map`) or as a width and a
  height; a value with no translation, or a value that depends on several
  parameters besides the resolution, is not supported. Defaults come from
  six identity fields only.
- **Vendor errors** are one answer per action: the reason (`.Result`) is
  MockVision's words, and a request that sets several parameters fails as a
  whole, never in part.
- **events.attach** writes the `attach` transport's parts as they happen:
  no backlog for a client that connects later, no JSON `data` of Dahua's
  smart events, and a client that reads too slowly loses parts. Events with
  only that transport have no delivery to log.
- **The base profiles are drafts.** `milesight/base` and the Dahua
  IPC-HDBW1230E-S4 come from manuals and public API documents, not from a
  capture; the values marked *to confirm* in them await one.

### Isolation

- **Plugins run as their camera's user**, not a user of their own, and
  without a cgroup of their own: the camera's limits cover both. They are
  confined by seccomp, no_new_privs and Landlock (their package, read only;
  no TCP connection without `net.connect` where the kernel has Landlock
  ABI 4), and die with their camera. A plugin package provides one engine.
- **Installing a plugin runs its program** once as the service user,
  confined the same way and unable to connect, to compare what it says it
  is with its manifest; a token that may import packages may trigger that
  run, but only a panel session enables a plugin.
- **Faults do not reach a plugin's sockets.** The sockets are handed to
  the plugin's process, past the camera's fault gate: *down* and *slow*
  apply to built-in engines only; a plugin asks the status fault itself.
- **Plugins are written in Go** with `sdk/plugin`; another language needs
  the gRPC services of `sdk/proto` and the descriptor numbers (fd 3 the
  engine's, fd 4 the camera's, 5 on the sockets) by hand.
- **One user for all cameras** (`mockvision-cam`), not one per camera.
  Cameras cannot reach the service's data, but they share a uid among
  themselves. Each runs in a PID namespace of its own, so one cannot signal
  the others, and Landlock limits its files to the renditions directory.
- **FFmpeg and the package validator run as the service user**, confined by
  seccomp and Landlock (`mockvision sandbox-exec`): FFmpeg reaches only the
  picture it reads and the directory it writes, the validator no file. A
  separate user for them would need a helper request of its own. On a
  kernel without Landlock, or in a container that refuses it, they and the
  cameras run without it; cameras log a warning.
- **Docker capabilities.** The design lists NET_ADMIN, NET_RAW and SYS_ADMIN
  on top of Docker's defaults (D61). `compose.yaml` drops all capabilities
  and adds back only what the helper uses: those three plus
  NET_BIND_SERVICE, SETUID, SETGID, SETPCAP, CHOWN and KILL, all of them
  part of Docker's defaults. The result is a strict subset of D61. The root
  filesystem is read-only and `no-new-privileges` is set.

### Analytics

- **No detection on the picture.** Rules say where events happen and
  triggers say when; an event's object (class, color, confidence, box) is
  made up and placed on its rule. The design keeps real detection out of v1.
- **Triggers are manual and random**; schedules, scripts and external
  triggers (MQTT or incoming webhooks) come in v1.1 (D40).
- **The profile declares every analytic**: kinds of rule, factory rules,
  events and the kind of rule each comes from, reports. An event that comes
  from rules needs an enabled one that reports it. Cameras created before
  profiles had factory rules get them once, at boot, if they have no rules.
- **Counts live in the camera process**: they start with it (or at a reset)
  and are lost when it stops, where a real camera keeps its counting
  history on flash. They accumulate from the camera start; reports carry
  the totals, not the counts of each interval.
- The heat map counts where each object's box was centered, on a 128×72
  grid; maps of other sizes are sums of it.
- Rule and trigger IDs are ULIDs, as every ID of the node; a profile that
  must send small numbers can name rules "1", "2"…
- A trigger bound to a rule keeps it: the rule cannot go away while the
  trigger uses it. Restoring a camera brings back its factory rules, as a
  real reset does, and keeps its triggers, which then fire on any rule.
- The loitering time and the intrusion delay of real cameras are not
  modeled: a region reports the event when its trigger says.

### Event transports

- **MQTT 3.1.1** only, with a client of MockVision's own; MQTT 5 is not
  spoken. The camera never subscribes: it only publishes.
- **The camera connects to a broker when it learns of it**, as it starts or
  when the target is linked, and keeps the session: a broker restart sees
  it reconnect within seconds, with a growing pause after failures. An
  event while the broker is down fails its attempts like any other target,
  with the profile's retries; nothing is queued for later (D70 is v1.1).
- **FTP is passive only** (EPSV, else PASV with the control connection's
  host): the camera cannot listen for active mode's data connection. FTPS
  is not spoken; SFTP is, with a password (keyboard-interactive too) and an
  optional pinned host key. A camera's firewall opens every port of an FTP
  server, for its data connections.
- **Mail** speaks PLAIN and LOGIN, the mechanisms cameras use, over plain,
  STARTTLS or implicit TLS; a certificate that does not verify is refused
  unless the target accepts it. The interval of the profile counts per
  target, whatever the event; the mails it holds back are logged as
  *skipped*, not sent later.
- **Connection tests send nothing**: an MQTT session, an FTP or SFTP login
  and the target's directory (created if missing), the sender and
  recipients of a mail (then RSET).
- **Delivery overrides** are per target, for every camera that uses it;
  the per-link overrides of the schema (`camera_targets.overrides_json`)
  stay unused.

### Faults

- **The faults of D43 and the network's**: a protocol down, latency, a
  status, a moved clock, the network down, an IP conflict, the SD card's
  states, and a reboot.
- **A protocol down resets** each new connection once accepted, as the
  kernel of a crashed service would answer; a client sees its connection
  reset rather than refused.
- **Latency delays every read** of the protocol's connections, so every
  request waits it; a stream already playing goes on, since its client
  hardly writes.
- **The network down** is two nftables tables (ip and arp) in the camera's
  namespace that drop everything but loopback: addresses and routes stay,
  and nothing has to be restored. In local mode the camera process alone
  refuses its clients and fails its deliveries. The event it raises is not
  queued: its delivery retries as the profile says, and goes out if they
  outlast the outage.
- **Degraded** names the faults on; the health of an engine (a broker out
  of reach) does not degrade the camera.
- **A reboot** stops the process and starts a new one after the boot time,
  with a new namespace; starting or stopping the camera meanwhile ends the
  wait. Its state and counts start again, as a real device's after a
  reboot.
- Ended faults are kept 30 days.

### Storage

- **Recordings are written as the event arrives**: the clip covers the 10
  seconds after the event (its `end` is in the future for that long) and
  is the stream's loop, so no pre-event buffer is simulated. Clips are
  MPEG-TS; an MJPEG stream records snapshots only.
- **The service writes the SD card** and the camera reads it: a camera
  process, which faces the LAN, cannot fill the node's disk. The NAS share
  is written by the camera itself, from its address and through its
  firewall, as the device does.
- **The NFS client is MockVision's own** (version 3 over TCP, AUTH_SYS,
  the portmapper or a fixed port); SMB uses go-smb2 (SMB 2 and 3, NTLM).
  Kernel NFS servers need `insecure` on the export, since cameras bind no
  privileged port.
- **The index of recordings is the service's**: the API's search, the
  panel and RTSP playback read it; a recording removed from the share by
  hand stays listed and fails to download. Changing the share forgets the
  old one's recordings, which stay on it.
- **Playback** plays the stream of the clips for as long as they last in
  the range, stamped with their recording time, and then ends the session;
  it does not seek or change speed. No ONVIF Profile G (D68, v2).
- **The disk check** counts what the cards promised and have not used, at
  each new or larger card; recordings that fill the node's disk for other
  reasons are not watched.
- **A full card** with overwrite off stops recording below 2 MB of free
  space or when a recording does not fit; a format, a larger card or
  overwrite on lifts it.

### API and data

- No `Idempotency-Key`.
- WebSocket topics: `node`, `cameras`, `camera:<id>`, `events` and
  `profiles`.
- Settings are stored as one JSON document under a single key of the
  settings table.
- **Metrics**: cameras report every 2 seconds and the service keeps a
  sample a second at most, for 10 minutes, in memory; the 10-second and
  1-minute series are the means of those samples, stored for a day and a
  week (D92).
- **Request statistics are aggregates**: per minute, client and route,
  reported every 10 seconds. Reports of the same minute add up: the median
  is weighted by their requests and the 95th percentile is the highest of
  them, so both are approximations over a minute. Latencies are measured
  in the camera, from the request read to the answer written. There is no
  raw request log (D92's circular buffer and HAR export).
- **Sessions are TCP connections**: RTSP sessions are timed by their
  connection, and a client that keeps reconnecting shows as many
  connections. Whether a client is connected now comes from the camera's
  notices.
- **Gaps** are stored per request shape and client for 30 days; query
  values are left out of their summary. A camera's log keeps its last 2000
  lines for 7 days: what it logged at warning or above and its state
  changes.
- **Load scenarios** (D93) are not part of this version.

### Panel

- English and Spanish: the panel starts in the browser's language and
  remembers the one picked.
- **shadcn/ui on Radix** (D49), with the design's tokens (colors, radius,
  32 px rows, Inter and JetBrains Mono embedded) rather than shadcn's
  theme. The CSP stays without `unsafe-inline`: the panel's page carries a
  nonce of its own on every load, which the style element of a modal's
  scroll lock takes; nothing else is inline. Selects stay native (`<select>`)
  and confirmations use the browser's `confirm()`; toasts are the panel's
  own. A small router replaces React Router.
- The detail page has General, Network, Protocols, Media, Rules, Triggers,
  Users, Configuration, Storage, Faults, Events and Diagnostics.
- The rule editor draws on the latest snapshot of the main stream, refreshed
  every 5 seconds; the camera page loads the first time a camera is opened.
- The event log shows the latest 100 events; the API pages with a cursor.

### Distribution

- No published image or release binaries yet (the design plans multi-arch
  images on GitHub Container Registry). `docker compose up` builds the image
  locally.
- The image is based on Ubuntu 24.04 and weighs about 780 MB, most of it
  FFmpeg's dependencies. A trimmed FFmpeg with only what MockVision uses
  would make it much smaller.

### Scraper

- **Read only, and only what is authorized (RN-17, RN-18).** The scraper
  sends the safe read methods alone — HTTP `GET`/`HEAD`/`OPTIONS`, RTSP
  `OPTIONS`/`DESCRIBE`, a TLS peek, a TCP connect — and a program that names
  any other method is rejected at parse time. A device is probed or captured
  only after the user marks it authorized; credentials are sealed with the
  node's key and a compiled profile is sanitized, so a serial, MAC or IP
  never leaves as itself.
- **The read-only half of the scraper only (D44).** This v1 registers,
  discovers, probes, captures and compiles a draft. Writing a setting back to
  a real device to confirm a route, and the credential brute force, are later
  work; nothing here changes a device.
- **Vendor identification is a guess** from the HTTP `Server` header and a
  device-info body, enough to offer the right program. It is not an
  inventory; a device with no banner reads as unknown and still captures with
  a generic program.
- **Discovery is a TCP sweep plus multicast.** It connects to the chosen
  ports across the subnet and sends one SSDP and one WS-Discovery query; it
  does not do full ONVIF `GetDeviceInformation`, mDNS/DNS-SD service
  resolution, or SNMP. The sweep is bounded (a `/22` at most, 1024 hosts) and
  rate-limited, and only private, loopback and link-local ranges are allowed.
- **Programs are versioned YAML, shipped in the catalog (D73).**
  `milesight/demo-capture` is the one built in; more install as
  `kind: program` packages through the same signed pipeline. A step reads one
  route, a stream's `DESCRIBE`, or waits for a pushed event; there is no
  branching or scripting.
- **The event receiver is a plain HTTP sink.** A device gets a stable,
  unguessable receiver URL (public and CSRF-exempt, since a camera, not a
  browser, reaches it); the capture job registers an in-memory receiver under
  that token while it runs and records the first event pushed to it. Secret
  headers (authorization, cookie, forwarding) are dropped before the fixture
  is stored. Only `http_push` events are captured this way — a camera that
  raises events only over MQTT or `attach` has none to record here.
- **Ports are mapped when a camera is local.** A program names a device's
  real ports (80, 554); a simulated camera on the same host listens on
  ephemeral ports, so a device may carry a port map (logical → actual). A
  real camera on the LAN needs none, and the node reaches it through the node
  bridge (D26), the same path the e2e uses.
- **Compile derives a draft, not a verified profile.** It builds the engines,
  routes and one recording from the fixtures and reads the codec from the
  SDP; resolution and the finer stream parameters are left at the profile's
  defaults for the user to confirm. The draft installs as a *captured*
  package (its provenance is the capture) and a camera can be created from
  it, but it reaches *verified* only once signed.
