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
| The line-crossing button POSTs to the target; the panel shows status and latency | receiver on the client; `delivery_status: ok` and latency; browser run (see below) |
| Stopping removes the namespace and the interface | `ip netns` and ping after the stop |
| Restarting the node or the container brings back cameras with autostart | stop, start, ping |
| Going over the maximum or the resources rejects the creation with a reason | `max_cameras` set to 1; problem with code `max_cameras` and the reason |
| Reopening the browser shows the right state | a second browser page with the same session sees the camera running |
| No camera process keeps capabilities | `/proc/<pid>/status` of the camera and of the main service: every capability set empty, uid not 0, `no_new_privs` |

The panel was also driven through the whole path in Chromium with
Playwright, on a node in local mode: first-run wizard, profile import with
its report, target and test request, camera creation, snapshot preview,
line crossing, event log, assets and settings. No console errors besides the
expected 401 before login, and no CSP violations.

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

- **macvlan only.** No ipvlan L2 mode (D27) and no DHCP (D24): cameras take
  a static IP.
- **No firewall per namespace.** The helper does not install nftables rules
  in the cameras' namespaces.
- **ARP probe for the IP only.** RFC 5227 probe before taking the address; a
  MAC already used on the LAN is not detected.
- **DNS is inherited from the node** (its resolv.conf) and is not editable
  per camera; the gateway is.
- **No bridge for access from the node** (D26). The node cannot reach its
  own macvlan cameras, so clients and targets have to be on other devices.
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
- **H.264 only**, one stream per camera (`main`), in the demo itself. Since
  v1 feature 5 cameras serve main, sub and third streams in H.264, H.265 or
  MJPEG.
- The RTSP Digest realm is `ipcam`, fixed by the RTSP library; the profile's
  realm applies to the HTTP API.

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

### API and data

- No API tokens and no `Idempotency-Key`; the API uses the panel's session.
- Audit rows are written, but there is no `GET /audit`.
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
- **No jobs system.** Renditions are encoded in the service, once per asset
  and encoding parameters (codec, resolution, frame rate, GOP, bitrate),
  with concurrent requests deduplicated.

### Panel

- English only; no i18n.
- No shadcn/ui or Radix: native `<dialog>` and a small router, so the panel
  runs under a CSP without `unsafe-inline`. The design tokens (colors,
  radius, 32 px rows, Inter and JetBrains Mono embedded) are the design's.
- Camera detail tabs for rules, triggers, faults and logs come with their
  features of the v1 plan; the detail page has General, Network, Protocols,
  Media, Users, Configuration and Events.
- The event log shows the latest 100 events; the API pages with a cursor.

### Distribution

- No published image or release binaries yet (the design plans multi-arch
  images on GitHub Container Registry). `docker compose up` builds the image
  locally.
- The image is based on Ubuntu 24.04 and weighs about 780 MB, most of it
  FFmpeg's dependencies. A trimmed FFmpeg with only what MockVision uses
  would make it much smaller.
