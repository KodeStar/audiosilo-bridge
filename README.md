# AudioSilo Bridge

> **Use of AI:** AudioSilo is developed by me (a human), with assistance from AI, primarily Claude Code, to help me write, clean up, document, and review the code. That doesn't mean the app is generated on autopilot or "vibe coded". Nothing goes out until I've read it, tested it, and decided it belongs. AI is what lets one person keep up this pace, and I think it's important to disclose that.

> **Private, and an internal name.** This repository is private and "Bridge" is a
> working name (BRIDGE-PLAN.md decision 14). Nothing here is released yet.

The Bridge is the optional, always-on companion to an
[AudioSilo](https://audiosilo.app) server. It **writes** to the library folders and
runs the heavy jobs (transcription, generated character sheets and recaps, and later
podcast fetching, read-along alignment, Audible backups and file moves), so the
server itself can stay read-only and safe to expose to the internet.

- **The Bridge calls the server; the server never calls the Bridge.** It pairs once
  with a code from the server's admin console, then sends a heartbeat every 30
  seconds and asks the server for jobs. Remove the Bridge and nothing breaks: its
  buttons just disappear from the server.
- **Every library mapping is checked, not assumed.** The Bridge compares a known
  book's file sizes through its own mount with what the server sees, and probes
  whether it can write there, before any job may write into that library.
- **Its own page** (port 8091) handles pairing, the library mappings, settings, the
  jobs it ran and its log. It answers only the local network and always asks for its
  password.

Status: **Phase 0** (foundations): pairing, the heartbeat and capability, the job
queue with leases, the mapping checks and the proof job `bridge.verify`. The
pipeline that generates characters and recaps (imported from audiosilo-sidecars with
its history) is wired in Phase 1a. The plan lives in the workspace's
`BRIDGE-PLAN.md`.

Code is licensed **AGPL-3.0** (see [LICENSE](LICENSE)).

## Run it beside a server (Docker)

[compose.example.yaml](compose.example.yaml) runs the server and the Bridge
together. The server mounts the library folders read-only, the Bridge mounts the
same folders read-write. Mount them at the same path in both containers and the
Bridge needs no mapping.

1. Start both containers. The Bridge prints the password for its page once, in its
   log, on the first start.
2. In the server's admin console, open **Settings > Bridge** and press **Connect a
   bridge**.
3. Open the Bridge's page (`http://<host>:8091`), sign in, and paste the server's
   address and the code. Or set `AUDIOSILO_BRIDGE_SERVER_URL` and
   `AUDIOSILO_BRIDGE_PAIR_CODE` for the first start.
4. Back in the console, the Bridge shows as online with each library's mapping.
   **Verify** queues `bridge.verify` for a library to prove the loop.

Images (`ghcr.io/kodestar/audiosilo-bridge`, private while the repo is):

| Tag | What | Arch |
|---|---|---|
| `latest` | the full image: Go, ffmpeg and a CPU whisper.cpp for transcription | amd64, arm64 |
| `latest-slim` | Go and ffmpeg only, for a Bridge that never transcribes | amd64, arm64 |
| `latest-cuda` | transcription on an NVIDIA GPU (`--gpus all`) | amd64 |

No image carries an AI agent CLI: agents run in a separate container that sees only
a staging folder (decision 15), which arrives with Phase 1a.

## Configuration

`<data>/config.yaml`, with `AUDIOSILO_BRIDGE_*` environment overrides (a setting the
environment sets is locked on the page). The data folder is `/bridge-data` in the
images (`AUDIOSILO_BRIDGE_DATA`), else `~/.audiosilo-bridge`.

| Setting | Environment | Default | |
|---|---|---|---|
| `listen` | `AUDIOSILO_BRIDGE_LISTEN` | `:8091` | the page's address |
| `server.url` | `AUDIOSILO_BRIDGE_SERVER_URL` | | the server as the Bridge reaches it: where a pairing code from the environment goes, and the pairing form's starting value |
| `public_url` | `AUDIOSILO_BRIDGE_PUBLIC_URL` | | the page as your LAN reaches it; the console links to it |
| `libraries` | `AUDIOSILO_BRIDGE_LIBRARIES` (`<id>=<path>;...`) | the server's own root path | where the Bridge sees each library |
| `mapping.check_minutes` | `AUDIOSILO_BRIDGE_MAPPING_CHECK_MINUTES` | `10` | how often the mappings are checked |
| | `AUDIOSILO_BRIDGE_PAIR_CODE` | | pair on start (first start only) |
| | `AUDIOSILO_BRIDGE_SECRET_KEY` | `<data>/secret.key` | the key that seals the stored secrets (32 bytes, hex or base64) |
| | `PUID` / `PGID` | `1000` | the user the Bridge runs as (Unraid: 99/100) |

The engine's settings (`tools`, `asr`, `agent`, `supervisor`, `contribution`,
`metadata`) are carried over from audiosilo-sidecars and take effect with Phase 1a.

## Develop

Go 1.26 and Node 24. See [CLAUDE.md](CLAUDE.md) for the gate, the layout and the
conventions, and [PACKAGES.md](PACKAGES.md) for each package's rules.

```sh
go build ./... && go vet ./... && go test -race ./... && golangci-lint run
cd web && npm run typecheck && npm run lint && npm run format && npm test
scripts/build-web.sh                 # the page, embedded into bin/audiosilo-bridge
./bin/audiosilo-bridge --data ./data
docker build --target slim -t audiosilo-bridge:slim .
```
