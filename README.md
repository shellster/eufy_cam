# eufy_cam

Stream Eufy security cameras.

## Build

```bash
just build
```

OR

```bash
go build -o eufy-server ./cmd/server
go build -o eufy-cli ./cmd/cli
```

## Configure

Copy `config.toml.example` to `config.toml` and fill in the username and password as well as the country your account is registered under.


```

All config values overridable via env vars:

| Variable | Maps to | Default |
|----------|---------|---------|
| `EUFY_USERNAME` | `[eufy]` username | |
| `EUFY_PASSWORD` | `[eufy]` password | |
| `EUFY_COUNTRY` | `[eufy]` country | `US` |
| `EUFY_LANGUAGE` | `[eufy]` language | `en` |
| `EUFY_TRUSTED_DEVICE_NAME` | `[eufy]` trusted_device_name | |
| `SERVER_HOST` | `[server]` host | `0.0.0.0` |
| `SERVER_PORT` | `[server]` port | `8080` |
| `SERVER_DEBUG` | `[server]` debug | `false` |
| `P2P_LOCAL_PORT` | `[p2p]` local_port | `0` (random) |
| `P2P_CONNECTION_TYPE` | `[p2p]` connection_type | `2` |
| `P2P_CONNECTION_TIMEOUT` | `[p2p]` connection_timeout | `60` |
| `P2P_ENCRYPTION_TIMEOUT` | `[p2p]` encryption_timeout | `30` |
| `P2P_DISCOVERY_ATTEMPTS` | `[p2p]` discovery_attempts | `30` |
| `P2P_ACK_TIMEOUT` | `[p2p]` ack_timeout | `15` |
| `AUTH_TYPE` | `[auth]` type | |
| `AUTH_USERNAME` | `[auth]` username | |
| `AUTH_PASSWORD` | `[auth]` password | |
| `STREAM_ENABLED` | `[stream]` enabled | `false` |
| `STREAM_PORT` | `[stream]` port | |
| `STREAM_BIND` | `[stream]` bind | `127.0.0.1` |

Env vars override `config.toml` values when set.

## Run

**Server:**

```bash
./eufy-server              # uses config.toml
./eufy-server myconf.toml  # custom config
```

Opens browser to `http://localhost:8080/login`. Web UI handles login + captcha (if required).

You can protect the server with digest auth if you want (see `[auth]` section of the toml).  If you want TLS, you should front the server via nginx or similar.


You can view your cameras and stream directly from the server's web UI at `http://localhost:8080` — every camera plays at a bare path like `/stream/<deviceSN>`, no parameters.

For consuming streams with ffmpeg, ffplay, frigate, or similar, every
camera is available as a static MPEG-TS URL — see
[Streaming with ffmpeg](#streaming-with-ffmpeg). Without digest auth (the
default) the main server serves ffmpeg directly. With digest auth enabled,
scripted consumers should use the optional
[Stream Port](#stream-port) (same URL, separate
unauthenticated port) or the [CLI](#cli-convenience-tool), which carries
the credentials.

## Streaming with ffmpeg

Every camera is a static MPEG-TS URL — `/api/stream/` plus the camera's
device SN, no parameters. Without digest auth (the default) the main
server serves these directly:

```bash
# Play a camera
ffplay http://localhost:8080/api/stream/T8134P2024342790

# Record 60 seconds without re-encoding
ffmpeg -i http://localhost:8080/api/stream/T8134P2024342790 -c copy -t 60 out.mp4

# Use in frigate / other NVRs
ffmpeg -i http://localhost:8080/api/stream/<deviceSN> ...
```

**With digest auth enabled** the main port challenges every request —
fine for the web UI (browsers prompt), hostile for ffmpeg. In that setup
either enable the [Stream Port](#stream-port) (identical
URLs, no auth, bind it to loopback or your NVR's interface) or use the
[CLI](#cli-convenience-tool), which handles the credentials.

The stream auto-starts on connect (waking battery cameras as needed) and
stops ~30 seconds after the last consumer of that camera disconnects —
across every endpoint (web UI, ffmpeg, CLI, stream port). Both H.264 and
H.265 cameras work: the output is standard MPEG-TS and ffmpeg/ffplay/VLC
decode HEVC natively (the browser HEVC limitation does not apply here).
Multiple simultaneous consumers of the same camera share one P2P session.

**First connect can be slow — two things to know:**

1. **Login/captcha must complete first.** Until the server has logged into
   the Eufy cloud, stream URLs will fail. Eufy frequently demands a captcha
   on fresh login: open `http://localhost:8080/login` in a browser once
   after startup and solve it. The server maintains the session on its own
   after that.
2. **Battery/solar cameras sleep.** eufyCam, SoloCam, and doorbell models
   take 10-40 seconds after the first connect to wake and deliver the first
   frame — ffmpeg/ffplay will sit on an empty stream during wake-up. That is
   normal; wait it out. Do NOT disconnect/reconnect rapidly to "fix" the
   delay: rapid start/stop cycles can put these cameras in a temporary
   refuse state for about a minute.

### Stream Port

An OPTIONAL second port serving the **same URLs** (`/api/stream/<deviceSN>`)
with **no authentication**. This exists for NVR-style deployments: run
digest auth on the main port for the web UI, and give the recorder only the
unauthenticated stream port. It binds to loopback by default — set `bind`
to `0.0.0.0` or a specific interface IP to expose it to your NVR.

**WARNING:** This port will **ALWAYS** be unauthenticated, so do not expose it publicly unless you know what you are doing.  This is true even when you have digest auth enabled for the main port.  The whole point of this feature is to expose it privately to an NVR when you want to otherwise require Digest Auth.

```toml
[stream]
enabled = true
port = 8090
# bind = "127.0.0.1"   # default
```

```bash
ffplay http://localhost:8090/api/stream/T8134P2024342790
```

The port exposes ONLY the `/api/stream/<deviceSN>` path — no API, no
login, no static files, nothing else answers. Consumers are counted
together with the main port: the camera keeps streaming until the last
consumer anywhere is gone for 30 seconds. 


## CLI (convenience tool)

The `eufy-cli` tool lists cameras and pipes a camera's MPEG-TS stream to
stdout so you can feed it straight into ffmpeg. It talks to the running
server (which owns the Eufy session and P2P connections), resolves the
camera's channel automatically, keeps the stream alive with a heartbeat
while running, and stops the camera cleanly on exit.

```bash
# List cameras (SN, name, model, channel)
./eufy-cli list

# Stream a camera to stdout as MPEG-TS
./eufy-cli stream T8134P2024342790 | ffplay -i -

# Record 60 seconds
./eufy-cli stream T8134P2024342790 | ffmpeg -i - -c copy -t 60 out.mp4
```

**When you don't need it:** if digest auth is disabled (the default) and/or the
stream port is enabled, point ffmpeg/ffplay directly at the stream path (either the service port if no auth or the stream port if auth)
port URL — see [Stream Port](#stream-port).

**When you do need it:** if you enable digest auth (the `[auth]` section of
the toml), everything under the main port requires credentials. Browsers
handle the digest prompt natively for the web UI, but scripted consumers
like ffmpeg get clumsy there. The CLI handles the credentials for you —
including the authenticated keep-alive heartbeat — on both `list` and
`stream`:

```bash
./eufy-cli --user admin --pass secret list
./eufy-cli --user admin --pass secret stream T8134P2024342790 | ffplay -
```

## Streaming behavior

**Multiple cameras simultaneously:** Each camera stream establishes its own dedicated P2P connection to the hub, so multiple cameras can stream concurrently without interfering with each other.

**Multiple clients, same camera:** Multiple clients can connect to and view the same camera stream concurrently — across the web UI, ffmpeg, the CLI, and the stream port. The server maintains a shared frame buffer — all connected clients receive the same frames without creating additional P2P connections. The stream stays alive as long as at least one consumer anywhere is connected (with a 30-second grace period after the last disconnect).

## Architecture

```
cmd/server/  — web server + UI
cmd/cli/     — CLI client
pkg/api/     — Eufy cloud API (auth, captcha)
pkg/devices/ — station management, P2P connections
pkg/p2p/     — P2P protocol (packets, encryption, streams)
pkg/stream/  — MPEG-TS stream sessions
pkg/crypto/  — RSA, AES, ECDH
pkg/web/     — HTTP handlers, auth middleware
```

## P2P Protocol

Implements Eufy's proprietary P2P protocol over UDP. Supports LEVEL_1 and LEVEL_2 encryption via AES-128-ECB. See `CLAUDE.md` for wire format details.

## Requirements

- Go 1.21+
- Eufy account credentials


## Why?

Because existing tools had subtle bugs and were written in NodeJS which made them difficult to port, memory hogs, and something I didn't want to debug.

## Version

- 1.0.0 May 5th 2026:  Initial Release

- 1.0.1 May 6th 2026:  Support multiple client streams from the same camera and stale stream automatic restart

- 1.0.2 May 7th 2026:  Support multiple camera streams at the same time, clean-up the UX, cli can serve stream via port or stdout

- 1.0.3 May 8th 2026:  Add server stream feature, clean-up some dumb LLM garbage.

- 1.0.4 May 14th 2026:  Fix video stream smearing/stalling (PTS mangling, keyframe index bug, videoStates data race). Add configurable P2P timeouts for internet/cloud relay connections.

- 1.0.5 Nov 3rd 2026:  Support H.265 streaming cameras, which should mean most if not all Eufy cameras / doorbells / pan-and-tilt, etc are now working.  Major improvements to the streaming resiliency.  Cameras should stream more consistently and the streams should glitch and fail less often.  A bunch of improvements to the UX as well to clean it up and properly display camera models.

## Credit

This project is HEAVILY inspired by the amazing work done here:

- https://github.com/bropat/eufy-security-ws/

- https://github.com/bropat/eufy-security-client


## Is this AI Slop?

Yes!  Most of this was completed via LLM's.  I have done a couple passes over it to clean it up and try to make it reasonably secure and clean.  That said, use at your own risk (or don't if you don't want to us LLM generated code).

## Do you accept fixes / PRs?

Yes!  However, I will not review your trash LLM slop if you haven't done minimal passes to clean it up, run go linting, and fix obvious LLM slop.
