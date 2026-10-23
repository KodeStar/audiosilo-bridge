# AudioSilo Bridge

> **Use of AI:** AudioSilo is developed by me (a human), with assistance from AI, primarily Claude Code, to help me write, clean up, document, and review the code. That doesn't mean the app is generated on autopilot or "vibe coded". Nothing goes out until I've read it, tested it, and decided it belongs. AI is what lets one person keep up this pace, and I think it's important to disclose that.

> **Private source, public images, and an internal name.** This repository is
> private and "Bridge" is a working name (BRIDGE-PLAN.md decision 14). The images
> are public from v0.3.0: pull them from `ghcr.io/kodestar/audiosilo-bridge` with
> no login.

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
  jobs it ran (with Retry for a job that needs attention), the agent container's
  state, the monthly budget, and its log. It answers only the local network and
  always asks for its password.
- **AI agents never run in the Bridge.** They run in a separate `agent` container
  that sees only a shared staging folder and its own config: no library, none of the
  Bridge's data, and a network of its own with internet access only.

Status: **Phase 1a** (pending merge): on top of Phase 0's pairing, heartbeat, job
queue with leases, mapping checks and the proof job `bridge.verify`, the Bridge now
**generates characters and recaps** (`sidecars.generate`). An admin presses
Generate in the server's console or the player; the Bridge transcribes the book (or
reads its .epub), runs the AI agents in a separate container, and saves the results
with the book in its `.audiosilo/` folder, where the server picks them up. The
pipeline came from audiosilo-sidecars with its history. Next is Phase 1b (a native
transcription worker on another machine). The plan lives in the workspace's
`BRIDGE-PLAN.md`.

Code is licensed **AGPL-3.0** (see [LICENSE](LICENSE)).

## Run it beside a server (Docker)

[compose.example.yaml](compose.example.yaml) runs the server, the Bridge and the
agent container together. The server mounts the library folders read-only, the
Bridge mounts the same folders read-write. Mount them at the same path in both
containers and the Bridge needs no mapping. The Bridge and the agent container share
one `staging` volume, and nothing else.

1. Start both containers. The Bridge prints the password for its page once, in its
   log, on the first start.
2. In the server's admin console, open **Settings > Bridge** and press **Connect a
   bridge**.
3. Open the Bridge's page (`http://<host>:8091`), sign in, and paste the server's
   address and the code. Or set `AUDIOSILO_BRIDGE_SERVER_URL` and
   `AUDIOSILO_BRIDGE_PAIR_CODE` for the first start.
4. Back in the console, the Bridge shows as online with each library's mapping.
   **Verify** queues `bridge.verify` for a library to prove the loop.
5. Give the agent container a credential, using an account of the Bridge's own:
   put `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token` on any machine) in a
   `.env` file beside the compose file, or sign codex in once with
   `docker compose exec agent audiosilo-bridge agent-login codex` and set
   `AUDIOSILO_BRIDGE_AGENT_BACKEND: codex` on the Bridge. An API key
   (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`) works too, billed per token. The Bridge's
   page shows the agent as ready within about 15 seconds.
6. Generate characters and recaps from a book's page in the console (or the
   player, as an admin). The console shows an estimate first, then the job's stage,
   ETA and spend.

Images (`ghcr.io/kodestar/audiosilo-bridge`, public from v0.3.0, no login needed):

| Tag | What | Arch |
|---|---|---|
| `latest` | the full image: Go, ffmpeg and a CPU whisper.cpp for transcription | amd64, arm64 |
| `latest-slim` | Go and ffmpeg only, for a Bridge that never transcribes | amd64, arm64 |
| `latest-cuda` | transcription on an NVIDIA GPU (`--gpus all`) | amd64 |
| `latest-agent` | the agent container: the claude and codex CLIs, run by `audiosilo-bridge agent-runner` | amd64, arm64 |

Only `latest-agent` carries an AI agent CLI (decision 15). Run it as the compose
example does: user 10002, all capabilities dropped, a read-only root, its own
network.

## Configuration

`<data>/config.yaml`, with `AUDIOSILO_BRIDGE_*` environment overrides (a setting the
environment sets is locked on the page; an empty variable counts as unset). The data
folder is `/bridge-data` in the images (`AUDIOSILO_BRIDGE_DATA`), else
`~/.audiosilo-bridge`.

| Setting | Environment | Default | |
|---|---|---|---|
| `listen` | `AUDIOSILO_BRIDGE_LISTEN` | `:8091` | the page's address |
| `server.url` | `AUDIOSILO_BRIDGE_SERVER_URL` | | the server as the Bridge reaches it: where a pairing code from the environment goes, and the pairing form's starting value |
| `public_url` | `AUDIOSILO_BRIDGE_PUBLIC_URL` | | the page as your LAN reaches it; the console links to it |
| `libraries` | `AUDIOSILO_BRIDGE_LIBRARIES` (`<id>=<path>;...`) | the server's own root path | where the Bridge sees each library |
| `mapping.check_minutes` | `AUDIOSILO_BRIDGE_MAPPING_CHECK_MINUTES` | `10` | how often the mappings are checked |
| | `AUDIOSILO_BRIDGE_PAIR_CODE` | | pair on start (first start only) |
| | `AUDIOSILO_BRIDGE_SECRET_KEY` | `<data>/secret.key` | the key that seals the stored secrets (32 bytes, hex or base64) |
| | `PUID` / `PGID` | `1000` | the user the Bridge runs as (Unraid: 99/100); a start as another user (`--user`) needs a data folder that user owns |
| `agent.backend` | `AUDIOSILO_BRIDGE_AGENT_BACKEND` | `claude` | the CLI the agent container runs: `claude` or `codex` |
| `agent.staging_dir` | `AUDIOSILO_BRIDGE_AGENT_STAGING_DIR` | `<data>/staging` | the folder shared with the agent container (an absolute path; compose uses `/staging`) |
| `agent.claim_timeout_seconds` | `AUDIOSILO_BRIDGE_AGENT_CLAIM_TIMEOUT_SECONDS` | `120` | how long a job waits for the agent container to pick it up before the book waits and tries again (10 to 3600) |
| `agent.monthly_budget_usd` | `AUDIOSILO_BRIDGE_AGENT_MONTHLY_BUDGET_USD` | `100` | the agent spend cap for a calendar month (UTC), across every book; `0` is no cap. Over it, books wait until the 1st |
| `agent.book_budget_usd` | `AUDIOSILO_BRIDGE_AGENT_BOOK_BUDGET_USD` | `75` | the agent spend cap for one book |
| `podcasts.allow_networks` | `AUDIOSILO_BRIDGE_PODCASTS_ALLOW_NETWORKS` (comma-separated CIDRs) | | private networks podcast feeds and artwork may come from; only for trusted feed hosts on your LAN (a list warns at start and on Status). Applies at the next start |
| `podcasts.max_feed_mb` | `AUDIOSILO_BRIDGE_PODCASTS_MAX_FEED_MB` | `32` | the largest podcast feed fetched, once decoded (1 to 64). Applies at the next start |
| `transcripts.retention_days` | `AUDIOSILO_BRIDGE_TRANSCRIPTS_RETENTION_DAYS` | `30` | how long a finished book's transcripts are kept (`0`: deleted when its job ends); the facts and results stay |
| | `AUDIOSILO_BRIDGE_GITHUB_TOKEN` | | a GitHub token that can open issues on the community repos, for jobs that contribute their results (`contribute: issue`); read live, never stored, and used instead of the Accounts tab's GitHub sign-in while set |

The engine's other settings (`tools`, `asr`, `supervisor`, `contribution`,
`metadata`, the agent's models and timeout) are carried over from
audiosilo-sidecars and apply at the next start. The supervisor's automatic
recoveries are on by default; its model-assisted lane and backend failover are
always off (they would run an agent in the Bridge). The agent concurrency is the
agent container's `--slots` (default 1).

## Develop

Go 1.26 and Node 24. See [CLAUDE.md](CLAUDE.md) for the gate, the layout and the
conventions, and [PACKAGES.md](PACKAGES.md) for each package's rules.

```sh
go build ./... && go vet ./... && go test -race ./... && golangci-lint run
cd web && npm run typecheck && npm run lint && npm run format && npm test
scripts/build-web.sh                 # the page, embedded into bin/audiosilo-bridge
./bin/audiosilo-bridge --data ./data
docker build --target slim -t audiosilo-bridge:slim .   # slim | cuda | agent | full
```
