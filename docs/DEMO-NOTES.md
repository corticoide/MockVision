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
| `profiles/milesight-demo.yaml` is validated and listed as Draft | `POST /packages`, level `draft` |
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
leaves the camera list. No console errors besides the expected 401 before
login, and no CSP violations.

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

- **Signatures are not verified.** Every package is treated as unsigned;
  a `manifest.sig` only adds a warning to the report.
- **`extends` is rejected**: profiles must be published already resolved.
- **No fixtures and no self-test**, so a profile never reaches the
  *captured* or *verified* levels; hand-written profiles stay *draft*.
- A loose `profile.yaml` may carry `profile.id` and `profile.version`
  itself, since it has no manifest.
- **The official catalog is not bundled** (D81): profiles are imported by
  hand, as the acceptance criteria ask.
- **Values are not translated.** A bound parameter holds the canonical value
  as it is (`h264`, `1920x1080`). A vendor that names them otherwise, such
  as Dahua's `H.264` or its width and height apart, keeps those parameters
  declarative: they answer and store the vendor's value but do not reach
  the stream.
- **Engine errors are fixed.** A `state.get` of an unknown parameter or a
  `state.set` the parameter refuses answers `Error: <reason>` with a 400,
  whatever the vendor answers.

### Isolation

- **No external plugins.** The engine contract (`sdk/engine`, with its gRPC
  mirror in `sdk/proto`) is respected, but only the built-in engines exist
  and the gRPC transport is not implemented. Cameras drop every privilege
  as they start, so launching sandboxed plugin processes from a camera will
  need a helper request of its own.
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

### API and data

- No `Idempotency-Key`.
- No faults and no *degraded* state: the state exists in the model but
  nothing sets it.
- WebSocket topics: `node`, `cameras`, `camera:<id>`, `events` and
  `profiles`.
- Settings are stored as one JSON document under a single key of the
  settings table.
- **Metrics live in memory only**: a sample every 2 seconds, kept for 10
  minutes. D92 asks for 1-second samples, plus 10-second and 1-minute
  series that are not kept.
- Gaps (requests the profile does not cover) are logged and published on the
  camera's topic, but not stored; request counters are not persisted.

### Panel

- English and Spanish: the panel starts in the browser's language and
  remembers the one picked.
- No shadcn/ui or Radix: native `<dialog>` and a small router, so the panel
  runs under a CSP without `unsafe-inline`. The design tokens (colors,
  radius, 32 px rows, Inter and JetBrains Mono embedded) are the design's.
- Camera detail tabs for faults and logs come with their features of the
  v1 plan; the detail page has General, Network, Protocols, Media, Rules,
  Triggers, Users, Configuration and Events.
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
