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
| `EUFY_VERIFY_CODE` | `[eufy]` verify_code | |
| `EUFY_CAPTCHA_ID` | `[eufy]` captcha_id | |
| `EUFY_CAPTCHA_ANSWER` | `[eufy]` captcha_answer | |
| `SERVER_HOST` | `[server]` host | `0.0.0.0` |
| `SERVER_PORT` | `[server]` port | `8080` |
| `SERVER_DEBUG` | `[server]` debug | `false` |
| `P2P_LOCAL_PORT` | `[p2p]` local_port | `0` (random) |
| `P2P_CONNECTION_TYPE` | `[p2p]` connection_type | `2` |
| `AUTH_TYPE` | `[auth]` type | |
| `AUTH_USERNAME` | `[auth]` username | |
| `AUTH_PASSWORD` | `[auth]` password | |

Env vars override `config.toml` values when set.

## Run

**Server:**

```bash
./eufy-server              # uses config.toml
./eufy-server myconf.toml  # custom config
```

Opens browser to `http://localhost:8080/login`. Web UI handles login + captcha (if required).

You can protect the server with digest auth if you want (see `[auth]` section of the toml).  If you want TLS, you should front the server via nginx or similar.


You can view your cameras and stream directly from the server ux.  That said, for forwarding your cameras to frigate or similar, you'll need the CLI piece.
Eufy will frequently prompt for CAPTCHA's on login.  Therefore once the server is running and maintaining a session, you can use the CLI tool to stream camera feeds from your server.
The server handles the P2P streaming and maintaining the session.  The cli tool borrows the session from the server and can steam mpeg-ts directly to stdout so that you can pipe it wherever you want
You can of course pipe this to ffmpeg and do anything you want with it.


**CLI:**

```bash
# List cameras
./eufy-cli list

# Stream camera to stdout (MPEG-TS)
./eufy-cli stream <deviceSN> [channel]

# Pipe to ffmpeg
./eufy-cli stream T8134... | ffplay -i - 
# If digest auth is enabled on the server
./eufy-cli -user admin -pass secret -server http://localhost:8080 list
```

## Streaming

**One stream per device (hub limitation):** Eufy stations/homebases only support one active P2P stream per device at a time — this is a firmware/hardware limitation, not a server restriction. If a stream is already running for a device, new requests reuse the existing session rather than starting a new one. You cannot stream different channels of the same camera simultaneously.

**Multiple clients, same camera:** Multiple clients can connect to and view the same camera stream concurrently. The server maintains a shared frame buffer — all connected clients receive the same frames without creating additional P2P connections. The stream stays alive as long as at least one client is connected (with a 30-second grace period after the last disconnect).

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

## Credit

This project is HEAVILY inspired by the amazing work done here:

- https://github.com/bropat/eufy-security-ws/

- https://github.com/bropat/eufy-security-client


## Is this AI Slop?

Yes!  Most of this was completed via LLM's.  I have done a couple passes over it to clean it up and try to make it reasonably secure and clean.  That said, use at your own risk (or don't if you don't want to us LLM generated code).

## Do you accept fixes / PRs?

Yes!  However, I will not review your trash LLM slop if you haven't done minimal passes to clean it up, run go linting, and fix obvious LLM slop.
