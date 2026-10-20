# CLAUDE.md - AudioSilo Bridge

Guidance for working in this repository. Keep this file updated as the codebase
evolves. This is a repo of the AudioSilo workspace (`~/dev/audiosilo`): read the
workspace [CLAUDE.md](../CLAUDE.md) first, then **`BRIDGE-PLAN.md`** at the
workspace root (the decisions, every phase's scope and the handoff log; it wins
over everything else while the Bridge is being built) and the Bridge section of
[CROSS-REPO.md](../CROSS-REPO.md).

## What this is

The **Bridge**: the optional, always-on companion to an AudioSilo server that
**writes** to the libraries and runs the heavy jobs, so the server stays read-only.

- **It pulls; the server never connects to it** (decision 8). Pairing exchanges a
  one-time code from the console for a `bridge` token (`POST /api/v1/bridge/pair`);
  then a heartbeat every 30 s (`PUT /bridge/status`), long-poll claims
  (`GET /bridge/jobs/next`) and progress (`PATCH /bridge/jobs/{id}`), which renews a
  2-minute lease. The server's `bridge_jobs` row is the intent and the lease; this
  repo's `server_jobs` row is the execution truth.
- **No standalone mode** (decision 2): it always pairs with a server.
- **Mappings are checked, never assumed** (decision 13): see `internal/mapping`.
- **Agents never run in this container** (decision 15): the `agent` image (the
  native claude and codex CLIs, run by `audiosilo-bridge agent-runner`) sees only the
  staging volume it shares with the Bridge and its own `/agent` volume. The Bridge
  drops typed requests there (`internal/agentproto`), never argv; slim, full and
  cuda carry no agent CLI, and config forces the supervisor's model-assisted lane
  and backend failover off.
- **Private, internal name** (decision 14): the repo stays private until the owner
  says otherwise; rename it with the product before any public release.

Module path: `github.com/kodestar/audiosilo-bridge`. Code is **AGPL-3.0**.

The engine (ASR, the agent pipeline, QA, spelling, contribution, the scheduler and
supervisor) came from **audiosilo-sidecars** with its history (`git log --follow`
and `git blame` reach the reliability fixes). sidecars stays usable until Phase 4,
then is archived with the manager. Phase 0 imported and tested the engine; Phase 1a
runs it (`internal/app/engine.go`) behind the `sidecars.generate` job
(`internal/generate`), which writes the results into the book's `.audiosilo/`;
Phase 1c adds the description step and the `sidecars.describe` job beside it;
Phase 1d finds a chapterless book's chapters in its narration (`chapter_detecting`,
`internal/chapterdetect`) and adds the `chapters.detect` job.

## Build / test / gate

```sh
# Go (from the repo root). The default build embeds a placeholder page, so it needs
# no Node toolchain and no generated files.
go build ./... && go vet ./... && go test -race ./... && golangci-lint run

# The page (from web/), Node 24 (export PATH="$HOME/.nvm/versions/node/v24.16.0/bin:$PATH")
cd web && npm run typecheck && npm run lint && npm run format && npm test
# NOT `npx tsc --noEmit`: the root tsconfig is references-only, so that checks nothing.

scripts/build-web.sh            # builds web/, syncs it into internal/web/dist, builds bin/ (-tags embedui)
./bin/audiosilo-bridge --data ./data   # first start prints the page's password ONCE

docker build --target slim .    # slim | cuda | agent | full (the default target)
```

**Before a change is done, run the gate of each side you touched**, plus an image
build for a Dockerfile change. golangci-lint is **v2** at a **green baseline**: fix
new findings, don't widen excludes. Go 1.26; a Go bump is three edits (go.mod,
`.go-version`, the Dockerfile's `golang:` tag). CI (`.github/workflows/ci.yml`) runs
both gates on every PR; `image.yml` builds the images on `v*` tags and dispatch.

> Before adding code, read the workspace **[CODE-HEALTH.md](../CODE-HEALTH.md)**:
> keep logic out of the transport layer, ship a test with every feature, and give
> security-critical code an allowed AND a denied test.

The `-tags embedui` seam: `go:embed` can't reach the repo-root `web/dist`, so the
default build embeds `internal/web/dist-placeholder/` and `-tags embedui` embeds
`internal/web/dist/` (gitignored), which `scripts/build-web.sh` fills. Never commit
`internal/web/dist/`.

## Package layout

Compact map; the per-package rules and history live in [PACKAGES.md](PACKAGES.md):
**read a package's entry before changing it.**

```
cmd/audiosilo-bridge/  serve (default; --data, --listen), version, agent-runner, agent-login,
                       worker [install|uninstall] (Phase 1b; worker.go)
internal/
  -- the shell (Phase 0) --
  app/        wiring + the page API's Backend: pairing (Conn), heartbeat (+ estimates),
              mapping checks; engine.go runs the engine (Phase 1a)
  api/        the page's HTTP API (transport only; LAN-only guard; password always;
              Phase 1a: one job, retry, engine)
  link/       client of the server's Bridge routes; hand-mirrored wire types
  mapping/    library mappings: SafeJoin, size comparison, write probe, Registry
  jobs/       generic runner: lanes (light, pipeline), claim, lease keepalive, cancel,
              resume, park/unpark, server identity, Abandoner, reports
  verify/     bridge.verify, the proof job
  logring/    recent log lines for the page (secrets redacted)
  web/        go:embed of the page (build-tag selected)
  secrets/    sealed-file secrets keyed by AUDIOSILO_BRIDGE_SECRET_KEY or secret.key
  -- Phase 2b --
  ghauth/     the GitHub App device flow, refresh, revoke (injectable base URLs)
  accounts/   the Accounts tab's GitHub sign-in and agent credentials (vault-sealed)
  setup/      the setup checklist the heartbeat and the Status tab show
  correct/    meta.correct: an admin's corrections as one correction-batch issue
  config/     config.yaml + AUDIOSILO_BRIDGE_* (bridge.go: the Bridge's own settings)
  store/      SQLite, append-only migrations (0001 the engine, 0002 server_jobs,
              0003 generate_books, contrib_mode, server_id, deleted_stage_spend,
              0004 workers, 0005 books.goal, 0006 accounts)
  auth/       the page's single password + hashed sessions + login limiter
  -- Phase 1a --
  generate/   sidecars.generate: server job -> engine book -> .audiosilo/ + rescan;
              sidecars.describe (Phase 1c): a description from a finished run's notes;
              chapters.detect (Phase 1d): a chapterless book's chapters, no agent;
              sidecars.contribute (Phase 2b): share kept results with the community
  agentproto/ the staging protocol shared with the agent container (a leaf)
  agentbox/   the Bridge's side: an agent.Runner over the staging dir
  agentrun/   the agent container's side: `agent-runner` (claim, run, harvestable out/)
  -- Phase 1b --
  workerproto/ the worker wire protocol (routes, version, task/result shapes; a leaf)
  workers/    the Bridge's side: the segment queue + leases + registry (Hub) and the
              engine's ASR backend (Router: remote, else local or a wait)
  worker/     `audiosilo-bridge worker`: client, run loop, spool, launchd/systemd install
  -- the engine (from audiosilo-sidecars; app/engine.go runs it) --
  toolfetch/ audio/ asr/ transcript/ ebook/ scratch/ qa/ spelling/ agent/ repair/
  pipeline/ contrib/ state/ eta/ scheduler/ supervisor/ metaops/ events/ pricing/ fsutil/
  chapterdetect/ (Phase 1d: chapters from the narration; pure, imported by pipeline)
web/          the page: Vite + React 19 + TS + Tailwind v4; pure logic in src/lib
docker/       entrypoint.sh (own /bridge-data, umask 022, drop to PUID:PGID)
Dockerfile    slim | cuda | agent | full; compose.example.yaml: server, bridge, agent
```

**Dependency direction:** `app -> {api, jobs, link, mapping, verify, generate,
agentbox, workers, accounts, setup, correct, store, secrets, config, auth, logring,
web}` plus the engine it wires; `accounts -> {agentproto, ghauth, secrets, store}`; `correct ->
{contrib, jobs, metaops}`; `setup -> link`;
`api` calls the injected Backend and the injected `auth.Manager` (login, sessions,
the password), with types from jobs/link/mapping/store/logring; `verify -> {jobs,
link, mapping}`; `generate -> {jobs, link, mapping, store, scheduler, pipeline, ...}`
through its own Engine/Server interfaces; `jobs -> {link, store}`; `mapping -> link`;
`link`, `agentproto` and `workerproto` are leaves; `workers -> {asr, fsutil,
scheduler, state, store, transcript, workerproto}`; `worker -> {asr, fsutil, workerproto}`
(never `workers`; its tests drive the real `api` + `workers` in-process); `api`
reaches the hub through its own `WorkerHub` interface and `workerproto`; `agentbox` and `agentrun` each import only
`agent` + `agentproto` (agentrun also `fsutil`), never each other, and the engine
never knows either (agentbox is just an `agent.Runner`). The engine's directions are
in PACKAGES.md.

## Conventions

- **`internal/api` is transport only.** It validates and calls the Backend; logic
  lives in `app`, `jobs`, `mapping`, `verify`, where it is unit-tested.
- **The wire to the server is hand-mirrored** (`internal/link`), as every client of
  the server does: a server change to `internal/bridge` or `handlers_bridge.go` is a
  change here, with a test on both sides (CROSS-REPO, the Bridge section). The
  `X-AudioSilo-Client` value must parse with the server's `auth.ParseClient` (a test
  holds its regex).
- **Writes into a library** (Phase 1a on): only typed operations, every path through
  `mapping.SafeJoin` against a CHECKED mapping (mapped AND writable at the mount in
  use, checked again before each write), temp file then rename, umask 022, a
  targeted rescan after, deletes to `.audiosilo-trash/`. A library that isn't mapped
  and writable refuses a new write job (a resumed one parks and waits). Today the
  only writer is `generate` (its three jobs): `characters.json`, `recaps.json`,
  `description.json` (Phase 1c), `chapters.json` (Phase 1d) and `bridge.json` in the
  book's `.audiosilo/` (a single-file book's prefixed `<stem>.`), nothing else.
- **The agent container's files are untrusted** (and the Bridge's are to it): every
  read goes through `agentproto.OpenRegular`/`ReadRegular` inside an `os.Root`, the
  harvest is capped, and the container's own credential never leaves it (only its
  kind is reported).
- **Secrets are never logged or echoed.** The page's one-time password is printed
  once at the first start; the server token, the GitHub sign-in (the Accounts tab's
  device flow: the token pair sealed as one value) and the agent credentials set on
  the Accounts tab are sealed in `<data>/secrets`, and the page API never returns
  one. `AUDIOSILO_BRIDGE_GITHUB_TOKEN` is read live and no longer sealed (a start
  deletes an earlier Bridge's `github_pat` copy). An agent credential leaves the
  Bridge only as one run's `credential/credential.json`, which the runner deletes
  before the CLI starts, and only when the agent container has none of its own;
  the log ring redacts secret-sounding attributes.
- **The page answers the local network only and always asks for its password**,
  loopback included (spike 2: an agent on the same network could reach it).
- **The worker routes (`/worker/v1/*`, Phase 1b) take only a worker token**
  (`asw_...`, made on the page, kept as its sha256 in `workers`) and the protocol
  header; the page's routes never take a worker token. Both sides are tested
  allowed and denied. A protocol change an installed worker can't follow bumps
  `workerproto.Version`; an added field doesn't (the worker routes accept unknown
  JSON fields).
- **Every feature ships with a test**; security-critical code (the LAN guard, auth,
  SafeJoin, the vault, the claim/lease handling) has allowed AND denied tests.
- **Hyphens, never em dashes**, in docs, comments, UI copy and generated text.
- **Commits** as `KodeStar <kode@audiosilo.app>`, no trailers.

## Status

**Phase 0 (foundations), merged:** the engine import and its cleanup (the scan,
overrides and sightings removed; migrations squashed; runtime renamed; the keychain
replaced by the sealed vault), and the shell: config, store, the server link, the
mapping checks, the generic runner, the page and its API, `bridge.verify`, and the
slim/full/cuda images.

**Phase 1a (generated characters and recaps), merged (spike 1 passed, 6 of 6 books):** the engine
running behind `sidecars.generate` (the pipeline lane, parks reported to the server
and retried from the page, the job following its book's moves, results written to
`.audiosilo/` and an attachments rescan), the agent container (`agentproto`,
`agentbox`, `agentrun`, the `agent` image and compose service, credential refusals
parked as `agent_credential_refused`), the monthly budget, the transcript retention,
supervisor recoveries on by default, the heartbeat's estimates, and the page's
engine card and job view.

**Phase 1b (the native ASR worker):** `audiosilo-bridge worker` (connects out to the
Bridge's page, long-polls SEGMENTS, downloads the FLAC, decodes with `internal/asr`,
uploads the raw transcript; resumes per segment from its spool), `worker
install`/`uninstall` (launchd agent, systemd user unit), worker tokens on the page,
the protocol-version refusal, and the engine's ASR as `workers.Router`: remote when a
worker is connected, else `asr.worker_fallback` (`local`, the default, which yields
to a worker that connects mid-decode; or `wait`, which parks `asr_worker_waiting`
until one connects).

**Phase 1c (own-words descriptions):** the description step
(`pipeline.Executor.Describe`, not an engine stage: its own `describing` stage run
on the book, which the reconcile and the supervisor skip, run under the
scheduler's `Hold` on the book, or inside the contributing stage) writes a spoiler-free
description from the fact notes of the book's opening chapters ONLY (the staged dir
holds `opening.md` and nothing else: no transcript, no `knowledge-final.md`, no later
chapter), validated (shape, length, voice, meta's description schema, the n-gram
check against whatever source text is left: the retention removes an audio book's
transcripts, and an ebook's text goes as it reaches done, so the contributing stage
of a book a `sidecars.generate` job follows runs it first, best effort and once,
while that text is there) and composed with the run's work
and sources. `sidecars.generate` runs it at its finish, after the characters and
recaps are saved (reusing the contributing stage's for free, its check kept; once
more at done when it first ran at a contributing park), and
writes `description.json` with them, but never fails, parks or holds for it (the
result says `description: false` and why; a regenerate that gets none keeps the
earlier file vouched for in `bridge.json`); `sidecars.describe` (advertised beside
`sidecars.generate`) writes one for a book already generated here, parking for the
agent's parks. Local only: a contribution never carries it.

**Phase 1d (chapterless books):** a single file with no usable chapter markers (none,
or one covering the whole file) gets a WINDOWS manifest at inspect (windows of about
25 minutes cut in silences, straight to splitting, which hears each window's
silences in the same decode), is transcribed window by window, then
`chapter_detecting` (after asr, mechanical) finds the chapters in the narration
(`internal/chapterdetect`: spoken headings after a pause, the spike's rules), moves
the window layer into `windows/`, writes the DETECTED manifest, one synthesized raw
per chapter and `chapters_detected.json` (the player's list, grouped by logical
position), cuts the chapter FLACs from the window FLACs (the source only for what
they can't give), and the book carries on as an ordinary chaptered one. A detection
that isn't confident parks `chapters_not_detected` (human only).
`books.goal = chapters` (migration 0005) stops a book there: the `chapters.detect`
job (speech recognition and detection only, no agent, no series lock) writes
`chapters.json` at its done; `sidecars.generate` writes it as soon as the chapters
are found and continues a chapters run rather than transcribing again
(`store.ContinueChapters`, with the job's identity): a finished one from splitting, or
one cancelled or failed at `chapter_detecting` (the server cancels a parked
`chapters.detect` job as it queues the generate job, which waits for it to give way)
going on there to detect again from its kept windows. **Entry k of the list is logical
position k by construction**: the classification it is grouped by is kept
(`positions_detected.json`) and is the one the later stages use, its positions the
entries' ordinals. chapters.json is never written beside characters and recaps made
without chapters found in the narration (their positions would move), and a
chapters job never clears a finished whole run (nor runs on a file it can't window:
it fails saying why, one marker over only part of it or no length). A chapter's
heading number is the detector's, whatever follows it. Titles found in the
narration are never verified spelling evidence (a detected manifest's
`marker_titles.txt` is empty, and the manifest staged to the spelling agent goes
without them).

**Phase 2b (contributions, Bridge side):** the Accounts tab (`#/accounts`): a GitHub
sign-in with the audiosilo-contributions App's device flow (`ghauth`, `accounts`;
expiring tokens refreshed single-flight and persisted before use, sign-out revokes)
that contributions file with (env, then the sign-in, then `gh`), and agent
credentials handed to the agent container for each run. Two light jobs that write
nothing into a library: `sidecars.contribute` (share a book's kept characters and
recaps) and `meta.correct` (an admin's corrections as one meta correction-batch
issue, filed once per job: a resumed job adopts the issue it made). The setup
checklist (`setup`) on the Status tab's "Get set up" card and in the heartbeat,
sent only once the server's reply `accepts` it. The monthly agent budget is
editable on the page and applies live.
See BRIDGE-PLAN.md's Status table for what is next.

Operative caveats carried from sidecars (PACKAGES.md has the detail):
- **audiosilo-meta is pinned** and the schema drift guard
  (`internal/pipeline/schema_drift_test.go`) is strict.
- **whisper.cpp binaries ship on their own cadence** from audiosilo-sidecars'
  releases (`toolfetch.WhisperCLIReleaseTag`; the images pin the same tag and
  check each asset's sha256 against that release's `checksums.txt`, which is
  not a pin: a release replaced whole would pass). Move them to this repo's
  releases when it goes public.
- **The engine's book identity is `books.source_path`** (an absolute Bridge path):
  `generate` maps the server's `(library_id, rel_path)` onto it through the checked
  mount, and `generate_books` maps the server job to the engine book (a resumed job
  follows the same one; a mapping is a job's own only for the same library and
  path, since server job ids repeat across servers).
- **Server job ids are only meaningful on their server:** every claim records the
  pairing's `server_id`, and a start drops another server's open jobs.
