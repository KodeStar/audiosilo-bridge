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
- **Agents never run in this container** (decision 15): the `agent` container (Phase
  1a) sees only a staging volume. None of the images carries an agent CLI.
- **Private, internal name** (decision 14): the repo stays private until the owner
  says otherwise; rename it with the product before any public release.

Module path: `github.com/kodestar/audiosilo-bridge`. Code is **AGPL-3.0**.

The engine (ASR, the agent pipeline, QA, spelling, contribution, the scheduler and
supervisor) came from **audiosilo-sidecars** with its history (`git log --follow`
and `git blame` reach the reliability fixes). sidecars stays usable until Phase 4,
then is archived with the manager. Phase 0 imports and tests the engine but doesn't
run it; Phase 1a wires it behind the `sidecars.generate` job.

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

docker build --target slim .    # slim | cuda | full (the default target)
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
cmd/audiosilo-bridge/  serve (default) + version; --data, --listen
internal/
  -- the shell (Phase 0) --
  app/        wiring + the page API's Backend: pairing (Conn), heartbeat, mapping checks
  api/        the page's HTTP API (transport only; LAN-only guard; password always)
  link/       client of the server's Bridge routes; hand-mirrored wire types
  mapping/    library mappings: SafeJoin, size comparison, write probe, Registry
  jobs/       generic runner: lanes, claim, lease keepalive, cancel, resume, reports
  verify/     bridge.verify, the proof job
  logring/    recent log lines for the page (secrets redacted)
  web/        go:embed of the page (build-tag selected)
  secrets/    sealed-file secrets keyed by AUDIOSILO_BRIDGE_SECRET_KEY or secret.key
  config/     config.yaml + AUDIOSILO_BRIDGE_* (bridge.go: the Bridge's own settings)
  store/      SQLite, append-only migrations (0001 the engine, 0002 server_jobs)
  auth/       the page's single password + hashed sessions + login limiter
  -- the engine (from audiosilo-sidecars; wired in Phase 1a) --
  toolfetch/ audio/ asr/ transcript/ ebook/ scratch/ qa/ spelling/ agent/ repair/
  pipeline/ contrib/ state/ eta/ scheduler/ supervisor/ metaops/ events/ pricing/ fsutil/
web/          the page: Vite + React 19 + TS + Tailwind v4; pure logic in src/lib
docker/       entrypoint.sh (own /bridge-data, umask 022, drop to PUID:PGID)
Dockerfile    slim | cuda | full; compose.example.yaml beside the server
```

**Dependency direction:** `app -> {api, jobs, link, mapping, verify, store, secrets,
config, auth, logring, web}`; `api` calls the injected Backend and the injected
`auth.Manager` (login, sessions, the password), with types from
jobs/link/mapping/store/logring; `verify -> {jobs, link, mapping}`; `jobs -> {link,
store}`; `mapping -> link`; `link` is a leaf. The engine's directions are in
PACKAGES.md.

## Conventions

- **`internal/api` is transport only.** It validates and calls the Backend; logic
  lives in `app`, `jobs`, `mapping`, `verify`, where it is unit-tested.
- **The wire to the server is hand-mirrored** (`internal/link`), as every client of
  the server does: a server change to `internal/bridge` or `handlers_bridge.go` is a
  change here, with a test on both sides (CROSS-REPO, the Bridge section). The
  `X-AudioSilo-Client` value must parse with the server's `auth.ParseClient` (a test
  holds its regex).
- **Writes into a library** (Phase 1a on): only typed operations, every path through
  `mapping.SafeJoin` against a CHECKED mapping, temp file then rename, umask 022,
  a targeted rescan after, deletes to `.audiosilo-trash/`. A library that isn't
  mapped and writable refuses write jobs.
- **Secrets are never logged or echoed.** The page's one-time password is printed
  once at the first start; the server token and API keys are sealed in
  `<data>/secrets`; the log ring redacts secret-sounding attributes.
- **The page answers the local network only and always asks for its password**,
  loopback included (spike 2: an agent on the same network could reach it).
- **Every feature ships with a test**; security-critical code (the LAN guard, auth,
  SafeJoin, the vault, the claim/lease handling) has allowed AND denied tests.
- **Hyphens, never em dashes**, in docs, comments, UI copy and generated text.
- **Commits** as `KodeStar <kode@audiosilo.app>`, no trailers.

## Status

**Phase 0 (foundations):** the engine import and its cleanup (the scan, overrides
and sightings removed; migrations squashed; runtime renamed; the keychain replaced
by the sealed vault), and the shell: config, store, the server link, the mapping
checks, the generic runner, the page and its API, `bridge.verify`, and the
slim/full/cuda images. Next: Phase 1a (`sidecars.generate`, the agent container,
`.audiosilo/` attachments). See BRIDGE-PLAN.md's Status table.

Operative caveats carried from sidecars (PACKAGES.md has the detail):
- **audiosilo-meta is pinned** and the schema drift guard
  (`internal/pipeline/schema_drift_test.go`) is strict.
- **whisper.cpp binaries ship on their own cadence** from audiosilo-sidecars'
  releases (`toolfetch.WhisperCLIReleaseTag`; the images pin the same tag and
  check each asset's sha256 against that release's `checksums.txt`, which is
  not a pin: a release replaced whole would pass). Move them to this repo's
  releases when it goes public.
- **The engine's book identity is `books.source_path`** (an absolute Bridge path),
  which Phase 1a maps from the server's `(library_id, rel_path)`.
