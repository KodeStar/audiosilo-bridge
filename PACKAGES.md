# PACKAGES.md - AudioSilo Bridge per-package internals

The full annotated package map: per-package invariants, incident history, and
load-bearing rules. **Read the entry for any package you touch before changing
it.** CLAUDE.md keeps the compact tree and the dependency direction; this file is
the detail.

The engine packages (toolfetch through supervisor, metaops, events) came from
audiosilo-sidecars with their history (`git log --follow` reaches it), and their
entries keep sidecars' milestone names (M1-M9) and incident history: "the daemon"
there is the Bridge now, and the folder scan, its overrides and the library
sightings they mention were not carried over (the server's index is the library).
The shell (link, mapping, jobs, verify, app, api, logring, web, cmd) is new, and so
are Phase 1a's generate (the sidecars.generate job) and the agent container's three
(agentproto, agentbox, agentrun).


```
cmd/audiosilo-bridge/     entrypoint, its subcommands: `serve` (the default; --data
                          ($AUDIOSILO_BRIDGE_DATA, else ~/.audiosilo-bridge), --listen; prints the
                          page's one-time password once on the first start, never logged),
                          `version`, `reset-password` (--data; app.ResetPassword: a new
                          generated password printed once and every session signed out,
                          the pairing and secrets kept; refuses a folder with no bridge.db
                          and root in a folder another user owns), `agent-runner` (the
                          agent container's entrypoint: agentrun.Main; --staging /staging,
                          --home /agent, --slots 1) and
                          `agent-login claude|codex` (agentrun.Login, run once inside the agent
                          container; --home /agent). `help` prints the usage.
internal/
  config/   config.yaml in <data>/ + AUDIOSILO_BRIDGE_* env overrides; Load/Save/Validate.
            Phase 2c-1: podcasts.allow_networks (CIDR strings, default empty;
            AUDIOSILO_BRIDGE_PODCASTS_ALLOW_NETWORKS comma-separated) and
            podcasts.max_feed_mb (1-64, default 32;
            AUDIOSILO_BRIDGE_PODCASTS_MAX_FEED_MB). Both apply at restart.
            Phase 2c-2a: podcasts.max_episode_mb (1-4096, default 1024;
            AUDIOSILO_BRIDGE_PODCASTS_MAX_EPISODE_MB) is a separate audio cap.
            The Bridge's own settings are bridge.go: server.url, public_url (the page
            as the LAN reaches it, sent as the heartbeat's ui_url), libraries (library
            id -> the folder the Bridge sees it at; MountFor falls back to the
            server's own root path, so a compose stack mounting the libraries at the
            same path needs none) and mapping.check_minutes (default 10: a check
            stats files and writes a probe, so not every 30 s heartbeat, which would
            keep Unraid array disks awake). AUDIOSILO_BRIDGE_LIBRARIES is
            "<id>=<path>;..." (a path may hold a comma). A value the environment sets
            is recorded (EnvSet) and locked on the page; SaveFile edits config.yaml's
            OWN values (loaded without the environment), and an edit (app's
            SaveSettings) never assigns a locked setting, which the page sends back
            unchanged, so a save never writes an env value into the file. A
            malformed env value fails the start rather than being ignored; an EMPTY
            one (compose's ${VAR} of an unset variable, a blank template field)
            counts as unset: it neither locks the page nor fails the start. The
            engine's settings below are restart-to-apply, except asr.worker_fallback
            (Phase 1b, AUDIOSILO_BRIDGE_ASR_WORKER_FALLBACK, local|wait, default
            local, normalized lower-case): the router reads it per segment and the
            page edits it.
            Engine history: metadata.base_url; agent.queue_concurrency controls
            agent-lane books and
            agent.max_agents_per_book controls safe within-book fan-out; legacy
            agent.concurrency retains its historical global cap. M2 added tools.
            {ffmpeg_path,ffprobe_path,auto_download} - the SINGLE source of truth for
            tool paths. M3a made asr.* live: backend
            (auto|mlx-whisper|whisper-cpp), model, language, whisper_cli_path (the
            full and cuda images set it to their baked /opt/whisper/whisper-cli). There
            is no asr.device knob (no backend honors an override yet). Changing
            asr.backend or the tool paths takes effect only on a RESTART (the backend
            is resolved once at startup). M5 made agent.*
            LIVE: backend (""|claude|codex), capacity, claude_path/codex_path,
            timeout_minutes, and the per-stage claude/openai model maps (keys are agent
            stage names, and Phase 1c's description step, "describing":
            state.RoutesModel). Validate rejects an unknown backend, an unknown model-map key,
            or timeout_minutes < 1; Default() seeds the claude map ("describing":
            sonnet). M7 added
            contribution.{mode [issue|local], core_repo (add-work), community_repo
            (sidecars), auto_purge, poll_minutes, api_base_url} (restart-to-apply;
            api_base_url for tests/GHE). Retired settings load via
            foldLegacyContribution + Config.Deprecations: `repo` -> core_repo (an
            explicit core_repo wins), mode `pr` -> issue. The reliability round added
            agent.book_budget_usd (default 75; 0 in yaml normalizes to the default, so
            set a very large value to effectively disable; Validate rejects negative;
            env AUDIOSILO_BRIDGE_AGENT_BOOK_BUDGET_USD; restart-to-apply like the
            rest of agent.*) - the per-book agent-spend cap runAgent enforces.
            Phase 1a (bridge.go, each with an AUDIOSILO_BRIDGE_* variable that locks
            it like the rest): agent.staging_dir (the dir shared with the agent
            container; absolute or "", which AgentStagingDir reads as <data>/staging
            without writing it into the config, so a saved file doesn't pin the data
            dir), agent.claim_timeout_seconds (default 120, 10-3600: how long a run
            waits for the container to claim it before the book parks
            agent_unavailable), agent.monthly_budget_usd (default 100, decision 7; 0
            is NO cap and is kept as written, unlike book_budget_usd; negative, NaN
            and Inf refused) and transcripts.retention_days (default 30, 0-3650; 0
            deletes a finished book's transcripts when its job ends). An env integer
            that doesn't parse becomes -1, which Validate refuses (envInt).
            agent.backend "" now means claude (the app maps it; the engine's own
            agent.Select, which read "" as auto, is never called on the Bridge).
            supervisor.automatic_actions defaults ON (the Bridge's jobs are pressed and
            left to run; each recovery is capped per book per day), and
            normalizeBridge forces supervisor.model_assisted,
            model_automatic_actions and allow_backend_failover OFF whatever the file
            or the environment says (decision 15: each would run an agent CLI in the
            Bridge), so what the config shows is what runs. On the Bridge the agent
            concurrency comes from the agent container's --slots (runner.json), so
            agent.concurrency/queue_concurrency/max_agents_per_book and
            claude_path/codex_path are read and validated but inert, and
            contribution.mode is overridden per book by the job's contribute param.
            Each inert setting (those, the three forced-off supervisor switches and
            the settings only the model lanes read) set to a non-default value is a
            Config.Ignored notice ("<key> is ignored on the Bridge: <why>",
            ignoredOnBridge, taken before normalizeBridge), which app logs once at
            the start beside Config.Deprecations.
  toolfetch/ fetches the three external artifact families, all gated by
            tools.auto_download and confined to <data>/tools: ffmpeg/ffprobe static
            builds (explicit path -> next to the binary -> $PATH -> HTTPS download,
            in-process extraction w/ per-entry name sanitization, -version
            self-check; ported from audiosilo-server); the whisper-cli release
            binaries (M3b: EnsureWhisperCLI - platform+device asset table over the
            pinned WhisperCLIReleaseTag, sha256 verified against the release's
            checksums.txt, temp-dir extraction under a hard size budget, --help
            self-check, atomic install + .meta written last; CPU fallback on
            accelerated self-check failure, device-aware cache hit, stale-cache
            degrade when a refresh fails); and ggml ASR models (EnsureModel: size
            floor + .meta sidecar + atomic rename). LocateBinary is the shared
            no-download lookup. A missing artifact degrades gracefully (the stage
            parks/fails that book; the daemon keeps working).
            A downloaded binary's first run never fails with ETXTBSY (bridge#12,
            #16): every tool is written unlocked (writeMode; ffmpeg/ffprobe copied
            out of the temp extract dir by copyExec, a plain atomic copy;
            whisper-cli chmodded where it was extracted), and runSelfCheck, every
            downloaded tool's first run, retries a run refused as busy
            (busyRetries) until the child forked mid-write has exec'd. No install
            holds the fork lock: every fork in the process would wait for it.
  execfile/ writes files the process will exec at once with syscall.ForkLock
            held for reading from open to close, so a concurrent fork can't hold
            the write descriptor and make the exec fail with ETXTBSY (WriteFile).
            A leaf; every test's fake tool uses it (it replaced the test-only
            testexe). Keep the locked write small, never a download or a large
            binary: every fork in the process waits for it. fsutil.WriteFileAtomic
            is not fork-safe for an executable run at once. execfile/forktest is
            the shared fork-stress test (execfile's and toolfetch's).
  audio/    the mechanical audio stages: Inspect (ffprobe -> probe.json + normalized
            manifest.json; marker parsing + contiguity ported from audio_extract.py;
            single-file marker books AND multi-file "files" books) and Split (ffmpeg
            each chapter -> mono/16k FLAC under chapters/, resumable via temp+rename,
            per-chapter progress, ctx-cancel clean). A marker-style split first
            STAGES the source m4b into the work-dir split-source/ directory
            (stageMarkerSource: one sequential 4MB-buffered copy, atomic rename, a
            complete staged copy is reused by a retry only while its size still
            matches a statable source - an unreachable source trusts the cache, a
            REPLACED source re-stages - skipped entirely when every chapter is
            already split, removed after success; the DIRECTORY is registered in
            scratch's artifact table so a failed/cancelled split's copy is
            reclaimable; a staging failure DEGRADES to splitting directly from the
            source - staging is an optimization, never a precondition) - ffmpeg
            otherwise reopens the same often-SMB-mounted file once per chapter, and
            enough rapid random-access opens can wedge macOS's SMB client. Both
            halves are TIME-BOUNDED so a wedged mount cannot hang invisibly behind
            the liveness heartbeat: each chapter conversion gets max(10min, 2x its
            audio duration) and the staging copy max(15min, a size-derived budget) -
            a bound hit is a loud, non-transient error, mirroring
            asrChapterDecodeBound. IsTransientSourceErr owns the transient
            FUSE/Nextcloud EINTR shape the pipeline's split retry classifies with.
            Pure/tool-driven, no scheduler deps.
            chapterFromMarker's vocabulary covers "Chapter N", with the title introduced
            by punctuation, an underscore, or NOTHING BUT WHITESPACE ("Chapter 1
            Suffering from Success", "Chapter 1 (Series Name)", "Chapter_1" - the literal
            word "Chapter" is what makes a loose tail unambiguous), digits or
            spelled-out, plus "N. Title" and bare-number tables ("001".."064", "-1-").
            The tail still REQUIRES a separator, which is what keeps "Chapter 10a"
            unrecognized: a split chapter announces one number twice and no contiguous
            manifest can express that, so those books belong with the agent.
            ROUTING IS MarkerStats.Usable() = Contiguous AND Complete, and both halves
            are load-bearing. Contiguous is the numbering check. Complete is COVERAGE:
            UnmappedSpans reports every run of recording time no chapter covers, with the
            marker titles occupying it, and an interior run >= MaxInteriorGapSec (60s) or
            an edge run >= MaxEdgeGapSec (180s) means narration was lost. Numbering alone
            said nothing about it, so 27 books silently dropped 61 hours of Interludes,
            Side Stories, Prologues and Epilogues - never split, never transcribed, no
            park and no note, their sidecars written as though that audio did not exist.
            The two thresholds are measured, not guessed: over the 294-book corpus
            interior holes are bimodal (one 9s encoder artifact, then nothing until 211s,
            above which all 173 are real narration), while the edges are where credits,
            bloopers and retailer samples legitimately live. A title-only marker table
            (no marker states any number) that TILES the recording is numbered by its own
            time order - positionalChapters, MarkerStats.Positional - exactly as a
            multi-file book is numbered by file order; refusing only for the single-file
            case was an inconsistency, not a safety property. A GAPPY title-only table is
            still not numbered: the hole is precisely what must reach a human.
            ReparseMarkerManifest recovers a draft for free after a parser upgrade (gated
            on the draft being non-contiguous, NOT on coverage - an agent-harvested map
            legitimately has edge holes where it excluded credits, so widening the gate
            would re-derive over the agent's work; it preserves the draft's Title AND
            Duration, since the stage bounds the agent's intervals against that duration).
            Inspect/Reparse return MarkerStats {Seen,Recognized,Contiguous,Positional,
            Unmapped,Complete}: Seen>0 with Recognized==0 is a parser vocabulary gap, NOT
            a markerless file - the two were indistinguishable in the metrics for three
            dialects running. Recognized stays the honest PARSER count, so a positionally
            numbered table reports Recognized 0 and Positional true rather than claiming a
            dialect was understood. A StyleFiles book reports Seen/Recognized 0 (its
            probe.json holds no marker table at all) and Complete true (its chapters are
            laid end to end from each file's duration), so markers_seen never fabricates a
            dialect signal. The deterministic reparse records NO RateSample - it is a
            sub-millisecond re-derivation of a stage whose real cost is the agent round
            it skipped, so observing it would collapse the markers_normalizing EWMA (the
            same rule as repair's free known-failed skip).
            Phase 1d (windows.go): a single file with NO usable markers (none, or one
            covering >= 95% of the file: a title, not a table; markerless) gets a
            WINDOWS manifest (StyleWindows): windows of about 25 min (target 1500 s,
            each cut at the middle of the longest silence whose middle is within
            120 s of the target, one 240 s silencedetect probe per cut, time-bounded;
            no silence or no ffmpeg cuts at the target, and a probe that fails (its
            bound ran out on a wedged mount) ends the probing, the rest at their
            targets (windowCuts); a file under 1800 s is one window; a cut that
            would leave under 300 s isn't made). Inspect takes
            ffmpegPath for the probes; its stats are Usable, so the book goes
            straight to splitting (the manifest's Style says windows; no stats flag).
            Several markers in an unknown dialect are still a vocabulary gap
            (markers_normalizing); ReparseMarkerManifest recovers a book an older
            parser parked there with no chapters and no usable markers into windows
            (cut at the targets: the reparse reads no audio). Split cuts a windows
            manifest like a markers one
            (staging: any style but files), and HEARS ITS SILENCES IN THE SAME DECODE
            (encode's listen: -af aformat 16 kHz mono + silencedetect, info-level log
            parsed before the FLAC is published): chNNN.silences.json beside each
            window FLAC, on the file's timeline, keyed by the window's span.
            WindowSilences reads them (a span that differs is never used), else
            hears the window FLAC once and keeps them the same way (WindowHeard:
            whether either is there, without hearing anything). ProbedSize is the
            size probe.json states for a single file (ffprobe's format.size; 0
            unknown). CutFromWindows
            cuts a detected manifest's chapters from the window FLACs (moved to
            WindowAudioDir, windows/chapters/): a chapter inside one window a seek in
            it, one across window edges its trimmed parts joined by the concat filter
            (exact to the sample: tested to the millisecond), a complete FLAC skipped,
            a chapter whose window is gone left (counted) for Split from the source.
            DetectSilences runs silencedetect (-40dB, 0.4 s) over a span of a file and
            returns silences on the FILE's timeline (ffmpeg reports them relative to
            the seek; an ffmpeg before 7 prints a tiny time with an exponent, which the
            parser takes). Every chapter, clip and window FLAC shares flacOutputArgs.
            Manifest.Detected marks the markers-style manifest
            chapter_detecting writes; WindowsDir ("windows") is where it moves the
            window layer. ParseNumberWords exports the marker parser's number words
            (chapterdetect reads spoken headings with it).
            Phase 3c-1: InspectWith(opts) with InspectOptions.WindowUnusable (goal
            readalong): a single file whose marker table isn't Usable() gets the WINDOWS
            manifest a markerless one does (chapter boundaries don't matter to an
            alignment, and markers_normalizing is an agent stage); Inspect is
            InspectWith with none. CutSpan(parts) cuts one mono/16 kHz FLAC across
            chapter FLACs of one file (the heal's clip; cutParts shared with the
            windows).
  asr/      the ASR backend abstraction (M3a/M3b): Backend{ID,Detect,EnsureReady,
            Transcribe} over a normalized Job (audio/outDir/chapter/prompt/language),
            producing RAW per-chapter output byte-for-byte. Two backends behind
            Select (auto|mlx-whisper|whisper-cpp): mlxwhisper (darwin/arm64; manages
            a pinned venv under <data>/tools/mlx-venv, model self-downloads via HF)
            and whispercpp (all platforms; whisper-cli resolves local-first via
            toolfetch.LocateBinary, else the toolfetch auto-download cache - Detect
            is optimistic when auto-download can supply a binary, EnsureReady(ctx)
            performs the fetch + downloads ggml-large-v3-turbo; an explicit
            whisper_cli_path that does not resolve is a loud error). One job at a
            time is the scheduler's job (Lane A cap 1); this package doesn't
            self-serialize. Never seeds the initial prompt with a guess. Gated live
            smoke: -tags asrlive. Phase 1b: Job.Start/Duration place the audio in
            the book (informational for a local backend; a worker task carries them);
            the Bridge's engine runs every Job through workers.Router.
  transcript/ the normalized transcript contract (audiosilo-transcript/v1) + Sanitize
            (NaN/Infinity->null, string-aware) + format-detecting adapters (openai-whisper
            /mlx AND whisper.cpp -ojf) + Complete (resume/skip test, ports
            transcript_is_complete) + writers (transcripts-json/ normalized,
            transcripts-text/ concatenated text). NEVER writes transcripts-raw/.
            Phase 1d: EncodeOpenAI, the openai adapter's inverse through its own
            types (round-trip tested): chapter_detecting renders its re-timed raws
            with it (the pipeline writes them, not this package).
  chapterdetect/ Phase 1d: a chapterless book's chapters from its narration. PURE (no
            I/O, no clock), table-tested. Detect(words on the whole-book timeline,
            silences, duration) -> chapters (contiguous 0..duration, each with spoken,
            heading, heading_at, pause, and number: what its "chapter" heading says,
            of a merged pair the chapter one's, else 0) + a confidence verdict. The
            spike's rules:
            a heading word (chapter/part/book/stave/day/volume/episode + a number in
            the next 1-5 words: digits 1-999, ordinals first..tenth, audio's number
            words, never past a word that ends in punctuation ("Chapter One. Two days
            later" is 1) and several words only as one English cardinal ("one hundred
            and twenty one" is 121, "one two" no number: the marker parser would add
            them up); or prologue/epilogue/interlude/preface/introduction/afterword/
            foreword/intermission alone; never "dedication") after a pause >= 1.5 s
            (the gap from the word before, or the longest silence ending within
            [t-3, t+0.6], silences that touch within 0.05 s joined first: a window
            cut splits a pause in two; the book's start counts); headings at the
            same time in transcript order; a number that runs straight on into
            "of" is no heading ("Book 10 of The Messenger" in Cosmic Ride's
            credits, the spike's one invented boundary; "Chapter 3. Of Mice and Men"
            after a break still is); per numbered word the longest strictly
            increasing run (numbers may skip); headings within 6 s merged ("Epilogue.
            Day 101"); boundary = the heading word's start - 0.2 s, never before the
            word before; title = the words up to the first 0.8 s gap within 10 words
            (never shorter than heading + number), punctuation trimmed, all-lower or
            all-upper title-cased, <= 200 bytes; audio before the first heading under
            180 s joins chapter 1, longer is "Opening". Not confident (books of 60 min
            or more): under two headings, a chapter over max(90 min, 3 x median), or
            a numbered run (not "day") missing over a quarter of its numbers; each
            with a plain reason. A shorter book with no heading is one chapter.
            Silence is audio.Silence (an alias); MaxTitleBytes / ClipTitle are the
            one 200-byte title bound every composed title keeps to (generate's
            chapterTitle, the pipeline's joined titles).
            Corpus (the env-gated pipeline test, 2026-10-17): every spoken story
            boundary of A Christmas Carol, The Emperor's Soul, The Black Gate,
            Cosmic Ride and Past Tense within 0.66 s (median 0.06-0.14 s), nothing
            invented but Emperor's Soul's spoken headings its publisher merges. Past
            Tense's reference is another master's list (6.5 s longer): the test maps
            it onto the file by a fitted line (about -3.8 s, +59 ppm) first.
  epubcfi/  Phase 3c-1: a Go port of foliate's epubcfi.js (pin 78914aef, the frontend's
            FOLIATE_PIN.txt), a leaf over x/net/html and x/text: a minimal DOM
            (ParseXML: encoding/xml non-strict with HTML entities for
            application/xhtml+xml; ParseHTML for text/html; comments and PIs dropped,
            CDATA as text, whitespace text kept),
            FromRange/ToRange/FromElements/Parse/Compare, offsets in UTF-16 code units
            as the DOM's, and WalkText (epublib.mjs's walkText: script/style skipped,
            the BLOCK set, a source newline a space) with CFI(start, end) over its rune
            offsets. Held to golden CFIs foliate itself wrote (testdata/golden.json from
            scripts/cfi-golden/gen.mjs, Node 24 + exact-pinned jsdom, not in CI; Alice
            plus synthetic sections: CDATA, entities, comments, PIs, adjacent text
            nodes, ids, astral characters, a text/html item, deep nesting) in both
            directions; a test checks the copied epubcfi.js's sha256 against the pin
            line.
  readalong/ Phase 3c-1: the read-along aligner, PURE apart from reading the EPUB (no
            clock, no exec): a port of the spike's Python (.readalong-spike/tools/align)
            held to it by an env-gated parity test (AUDIOSILO_READALONG_SPIKE=<spike
            dir>: sentences within 2%, body coverage within 0.5 pp, shared
            sentence-start median under 0.05 s, every CFI resolving;
            AUDIOSILO_READALONG_MAPS=<dir> writes the five corpus maps in the v1
            format). OpenEPUB/ReadEPUB (regular file <= 256 MiB, DRM refused:
            META-INF/rights.xml or an encryption.xml beyond font obfuscation, sha256 of
            the bytes; the spine with each section's CFI), LoadText (every content
            document walked; Sections() the linear ones, a linear="no" item is a note
            not narrated in place), textnorm (TOKEN_RE, NFKD, apostrophes out, digits
            spelled out with years in pairs; SplitSentences), Align (interned tokens,
            unique 5-gram candidates, RANSAC + LIS placement of up to 8 clusters per
            section, anchors resolved, gaps filled by an int32 Needleman-Wunsch with 1:1
            to 1:5 moves under 4M cells, then sentences; chaining resets per placed
            unit, and the second cluster's threshold is max(20, 0.02 nB) as the spike
            meant), Compose (whole-book time = the SERVER's file durations in its order
            + the in-file chapter start + the word time, never the manifest's cross-file
            starts), HealWindows ([prev.end - 0.5, next.start + 0.5] per unaligned run,
            2-600 s; a section's TRAILING run from prev.end - 0.5 for its text at 10
            characters a second + 10 s, never past its chapter's end nor the next
            section's first aligned sentence, at most 600 s (a chapter-end loop, CotW
            in the 3c-1 live run); cut at file boundaries, plus the extra windows
            (tail_verdicts, LoopWindow: the transcript check's loop ranges, 2-600 s),
            overlapping or touching ones merged), Onset/Snap (10 ms frame energies on one grid per
            chapter, p10 + 0.3 (p95 - p10), after-pause/run-start/mid-flow; start =
            onset - 0.1 s, never past end - 0.2 s nor before raw - 0.5 s), Build/Encode
            (audiosilo-readalong/v1, keys in order, times clamped to the book and
            rounded to 0.01, file durations to 0.001, book-relative paths, coverage
            {text: the read body's sentence characters from the first aligned sentence
            to the last, audio}, audio_gaps over 5 s; refused over MaxMapBytes 16 MiB).
            workdir.go names the stage's work-dir files (readalong/request.json,
            words.json, heal/, map.json, align_report.json, repaired) so scratch can
            name them too. testdata/alice.readalong.json is a regenerated spike map.
  ebook/    M9: the epub front half. BuildUniverse turns audiosilo-meta pkg/extract's
            per-section split into the LOGICAL chapter universe the sidecars publish
            positions against, then WriteChapterText/WriteManifest materialize it.
            Two rules are load-bearing: quarantine is POSITIONAL (everything outside
            the first..last numbered section is excluded regardless of length -
            a trailing promo excerpt of ANOTHER book is a full chapter, so any
            word-count rule waves it through), and a Loose label reading is accepted
            only when the whole book's labels form a contiguous run (Contiguous),
            because "I An Irate Neighbor" and "MIX" are ambiguous per-label. Find
            walks a library for .epub files and reads each OPF for identity, never a
            content document. Pure + table-tested; a 33-book corpus test is env-gated
            on AUDIOSILO_EPUB_DIR.
  scratch/  per-book DirSize gauge + Purge (removes chapters/ and split-source/,
            keeps durables), confined to the work root. Reclaimed manually (purge-scratch), by M7's
            auto-purge when a book reaches done (contribution.auto_purge, default on),
            and by the startup GC (async, reservation-guarded, done books only).
            A purge also invalidates the split sentinel (scheduler.purgeInvalidatedStages)
            so a later retry re-splits rather than skipping into an empty chapters/.
            Phase 1a: PurgeTranscripts is the transcript tier of the Bridge's retention
            (decision 5; WHICH books and WHEN is generate's policy): Purge, plus every
            transcripts-* layer (transcripts-corrected/ included, a durable to Purge),
            the root files that quote the transcript (spelling candidates and reference
            matches, the QA reports, qa_plan.json and qa_accepted.json, tail_verdicts,
            corrections.log, repairs.log) and the predecessor's *.txt chapter copies
            under spelling-refs/. It KEEPS knowledge-final.md (a sequel's fact pass
            inherits it), the spelling ledger and rules, the prior-* carryover and the
            series glossary, the manifest, the _done/ sentinels and the sidecars: a
            finished book keeps its work dir because series carryover reads it.
            Confined like Purge (the denied case is tested); the file names come from
            the owning packages' constants (spelling.RefsDir/PriorPrefix/
            SeriesGlossaryFile/CorrectionsLogFile, qa.AcceptedFile), never literals.
            Phase 1c: it keeps the description step's chapter_positions.json and
            sidecars/description.json (neither quotes the transcript). Phase 1d: it
            removes windows/ (a chapterless book's window raws and detection) and
            keeps chapters_detected.json (titles only; chapters.detect reuses it).
            Purge reclaims windows/chapters/ (audio.WindowAudioDir, no sentinel): the
            window FLACs chapter_detecting removes itself once the chapters are cut,
            left only by a run stopped in between (a re-run splits from the source).
            Phase 3c-1: PurgeTranscripts also removes readalong/words.json and
            readalong/heal/ (the heal's raws), keeping readalong/map.json (CFIs and
            times only, so a write a park held up still finishes), align_report.json and
            request.json.
  qa/       M4: the mechanical transcript-QA degeneration sweep, a faithful Go port
            of the historical Python detectors (qa_sweep/cross_segment/
            within_segment/multi_loop/tail_rate scans - thresholds and per-detector
            chapter-0 asymmetries are CONTRACT, golden-tested against 2 real books).
            Reads transcripts-json/ (+ transcripts-repaired/ for multi-loop); writes
            qa_report.json (stable enum contract for the M5 adjudicator/UI) +
            qa_report.md (byte-compatible with the Python report). CrossSegment(input): that
            detector alone (the read-along's heal windows). LoopWords(text):
            the words a 6-gram repeated 5+ times covers (the read-along's adoption
            rule). Report.Clean()
            drives the QAClean branch; the retranscribe queue = wph outliers +
            mid-chapter loops. Loud errors on manifest/transcript divergence,
            wrong-schema files, and an empty transcript set.
            Phase 3c-1: MechanicalPlan(rep) is a read-along book's loop repair with no
            agent: every chapter in the retranscribe queue or with a repeated run, a
            multi-loop or a tail-rate hit is re-transcribed whole (never a tail or mid
            clip: their spliced text has no word timings), every other allowed chapter
            accepted; nil for a clean report.
  spelling/ M4: the corrections/spelling ENGINES ported from apply_corrections/
            check_corrections/generate_spellings/check_first_use.py. Engine-vs-data
            split: per-book data is corrections.json + spellings.json in the work
            dir (M5 agents generate them). Rules apply in array order via regexp2
            (RE2 lacks the lookbehinds the historical rules need; $1 replacement
            syntax, validated gate-compatible at load); Occurrences is a lookaround
            boundary scan, never \b (the d'Daston apostrophe pitfall). Check's four
            gates guard the historical forgeries (Owalyn gate 3, phantom nobles
            gate 4, the Book-2 cascade gate 1); GenerateSheets emits the
            CHUNK_ENDS-gated spoiler-safe sheets. Attestation sources are purely
            data (reference_files) - nothing implicit. Wired into the pipeline in M5:
            the spelling_research agent generates corrections.json/spellings.json and
            the correcting stage runs Apply/Check/GenerateSheets over them.
            Post-M8 spelling-cost round: ExtractCandidates (candidates.go) distills
            the transcript into spelling_candidates.json - a deterministic, capped
            (never silently: Truncated + a stage note) shortlist of likely proper
            nouns (multi-word capitalized phrases, non-initial capitalized singles,
            non-dictionary capitalized singles at count>=3, lowercase variants, rare
            non-dictionary lowercase; Unicode-aware tokens incl. d'Aston internal
            capitals) with counts/chapters/up-to-2 snippets - which the stage stages
            to the agent INSTEAD of the ~600KB transcripts-text/ (the agent may only
            write rules targeting listed forms). commonwords.go is a generated
            (scripts/gen_commonwords.py) license-safe common-word list feeding the
            non-dictionary heuristics. DeadRules (deadrules.go) is the validator-side
            dead-rule scan: a rule whose pattern matches nothing in the ORIGINAL
            (pre-Apply, repaired-preferred) layer is rejected with the pattern named
            - Check's four gates miss that shape whenever the RHS is attested
            elsewhere, and it is deliberately NOT a fifth Check gate (Check is a
            contract-frozen golden-tested port).
            reference.go is the OUTSIDE-EVIDENCE pre-pass (BuildReferenceMatches ->
            spelling_reference_matches.json): it edit-distance-matches each candidate
            against reference vocabularies and reports transcript forms no reference
            spells that way. It exists because the agent's only native signal is
            intra-transcript disagreement, which a CONSISTENTLY misheard name never
            produces - "Torrin" 369 times and "Toren" never reads as settled, so a
            whole Wandering Inn series published Torrin/Floss/Terriarch/Goddard.
            ReferenceSource.Authority is the load-bearing part: VERIFIED (the meta
            series glossary, the publisher's marker titles) comes from outside the
            recording and may contradict it; CARRYOVER (the predecessor's ledger)
            may not. Only a verified source marks a form "known" (and a known form
            is never proposed against) - letting carryover settle a form would make
            a propagated error immunise itself, which is exactly how one mistake
            travels down a series. Bounds (maxMatchDistance 2, also <= len/3, first
            letter must match) are calibrated against the six real misspellings;
            NamesFromTitles mines marker titles ("Chapter 9: Toren") and yields
            nothing for the bare-number tables real volumes often ship.
  agent/    M5: the agent-runner abstraction (Runner{ID,Detect,Run} over a normalized
            Request/Result/Usage) + claude and codex headless-CLI backends (prompt on
            STDIN never argv, --output-format json / codex --json JSONL, usage capture,
            typed RateLimitError/NotAvailableError), Select (auto|claude|codex, explicit
            path knobs, loud on an unresolvable explicit path), ModelFor per-stage
            routing, RunWithRetry (invalid-output <= 2 PATCH-style retries: the staged
            cwd + out/ persist across attempts, so the retry prompt carries the
            validator error and instructs the agent to fix/delete the offending
            entries in its prior out/ files, never regenerate from scratch;
            rate-limit backoff), staging.go (per-attempt staged dir under
            _runs/, 0444 copied inputs + out/, Harvest with traversal + size-cap guards),
            and go:embed prompts/ (one template per stage + a vendored authoring.md from
            audiosilo-meta). The child env injects an API key from secrets that NEVER
            appears in argv/logs/errors (tested). Reliability round: RunWithBackoff also
            retries a transient NotAvailableError (15s, 60s - the claude CLI auto-update
            briefly breaks exec.LookPath, which stranded a real book mid-fact_pass);
            RateLimitError carries a best-effort ResetAt via ParseResetTime (the
            |<epoch> suffix within (now, now+48h], or "resets at H[:MM] am/pm" as the
            next host-local occurrence). Post-M9 round: isRateLimit no longer
            treats a bare "429" as a rate limit - RFC3339 fractional seconds and
            usage counters contain those digits (".294295Z",
            "output_tokens":429), so a timestamped stderr line misclassified a
            validation failure; the status code now requires nearby protocol/error
            context, wide enough for the JSON and status_code=429 shapes the old
            substring caught. runCLI bounds Cmd.Wait with a 5s WaitDelay
            (cliPipeWaitDelay) so a detached CLI helper holding the inherited
            stdout/stderr pipe cannot hang a finished stage; ErrWaitDelay after a
            successful exit is treated as success. SleepCtx is the shared
            ctx-aware backoff sleep (pipeline's split retry reuses it).
            Phase 1a split each backend's Run into the CLI run and its
            interpretation, so a run in the agent container is classified by the same
            code: CLIOutcome {Stdout, Stderr, ExitErr, TimedOut, Timeout, LastMessage}
            is what a run left behind wherever it ran (outcomeOf maps runCLI's return:
            its timeout error is TimedOut, anything else ExitErr), and
            ClaudeResult/CodexResult(model, outcome) parse it, classify a rate limit
            first, and keep the reported usage on the Result even beside an error
            (spend is captured on a failed run too); Run returns ctx.Err() for a
            parent cancel. ClaudeArgs(req) and CodexArgs(req, lastMsgFile, sandbox)
            are exported so agentrun builds the very same argv (CodexSandboxDefault
            keeps read-only/workspace-write; CodexSandboxOff, danger-full-access, is
            the container's, where codex's sandbox can't start under Docker's
            seccomp and the container is the boundary). A REFUSED CREDENTIAL
            (authFailureSignatures: "failed to authenticate", "oauth access token is
            invalid", invalid_api_key, invalid x-api-key, "api_error_status":401, "401
            unauthorized") is a *NotAvailableError with CredentialRefused set, looked
            for ONLY in a run that failed (a clean run may quote a 401 from a web
            source or the book); RunWithBackoff returns it at once (the same
            credential is refused again) and the pipeline parks
            agent_credential_refused. codex's turn.failed error is an OBJECT
            {"message": ...} (the SDK's ThreadError), a bare string read too
            (codexEvent.errorText); a top-level {"type":"error"} event's message
            stands in when the turn.failed carries none, but is never a failure on
            its own (codex sends them for reconnects it gets past).
            Availability.RetryAfter marks an unavailable runner that comes back by
            itself (the agent container restarting): the preflight park then gets a
            retry_at instead of waiting for a human.
            Phase 1c: prompts/description.md is the description step's prompt (the
            AUTHORING.md `description` contract restated: setup only, grounded only
            in opening.md, own words, neutral voice, 200-1500 characters, hyphens,
            out/description.json as exactly {"text": ...}); a prompts test renders
            both source branches and holds the output shape.
  benchmark/ private, provider-neutral post-ASR evaluation: allow-listed corpus
            preparation with input digests; fresh per-run work trees and databases;
            stage-specific model/effort routes; the real fact/synthesis/audit/fix
            pipeline; independent holdout audits; hard-gated aggregate/Pareto reports;
            accepted-reference judge calibration; and read-only historical telemetry
            over a caller-supplied DB snapshot.
            Transcript corpora and generated results must remain outside the repo.
  repair/   M5: the mechanical tail-clip + adoption machinery (ports the historical
            tail_clip_check/adjudicate_tails/build_repairs): ClipAndSplice (locate the
            tail loop, cut+re-transcribe the window prompt-free, health-check, rotation-
            adjudicate FABRICATED/BENIGN/CLIP-REDEGENERATED, splice into
            transcripts-repaired/ + repairs.log/tail_verdicts.json) and AdoptFresh
            (never-blindly-adopt full-chapter plausibility). AdoptFreshReadalong is goal
            readalong's rule (Phase 3c-1): adopt the NoContext re-run unless its word
            rate is implausible (AdoptFresh's check: the heal takes that span), it loops
            over more words (AdoptStats.LoopWords, qa.LoopWords), it is shorter than a
            loop-free original, or it keeps under 80% of a looped original's words
            outside its loops; a looped original's extra words never count for it
            (AdoptFresh keeps it for them). No agent; injected a
            ClipCutter (ffmpeg) + a Transcribe func so it stays unit-testable. The
            re-transcription (full-chapter AND clip) runs NoContext (asr.Job.NoContext)
            so a context-conditioned repetition collapse cannot replay identically.
            ClipSpliceRequest.StartOverrideSec accepts an agent-supplied window start
            (0 = derive as before, byte-identical). Clip windows are CHAPTER-RELATIVE
            seconds and bounded by qa.ClipStartInRange (the shared floor predicate,
            qa.ClipStartFloorSec): the plan validator rejects an out-of-range value at
            authoring time with retry feedback, the retranscribe dispatch skips it as
            clips_unlocatable BEFORE any cut or destructive prep (with an
            action-appropriate "out of range" note), and the repair layer is defense in
            depth - the located path IGNORES an out-of-range override (keeps the derived
            start), the directed and MID paths no-op. This closed the live incident where
            an absolute source-file timestamp cut a negative-length clip and hard-failed
            the book forever. The cut clip file is keyed on
            chapter+effective window start (t%03d-<start>.flac), so a relocated window
            forces a fresh cut instead of reusing the prior window's audio. A window whose
            clip_start already carries a CLIP-REDEGENERATED verdict (within ~1s) UNDER THE
            SAME decode params is SKIPPED without cutting/re-transcribing
            (SkippedKnownFailed), so a re-queued identical tail_clip is free instead of
            minutes of ASR - the verdict records a DecodeTag, so a legacy verdict written
            under different (context-conditioned) params never blocks the chapter's one
            fresh NoContext attempt. MID-CHAPTER interior repair (ClipAndSpliceWindow, for
            an interior loop with real narration resuming AFTER it): the agent supplies a
            bounded [clip_start_sec, clip_end_sec] window (EndOverrideSec), which
            snapWindow snaps OUTWARD to segment edges (no straddling content lost), the
            stage cuts+re-transcribes prompt-free/NoContext, ClipHealthy gates it (no
            rotation-adjudication - there is no closing line), and SpliceWindow splices the
            fresh window BETWEEN the intact head (before start) and tail (after end).
            TailVerdict.ClipEnd (>0) records the window as a MID-REPAIRED (or mid
            CLIP-REDEGENERATED) verdict; the mid clip file is m%03d-<start>-<end>.flac
            (both bounds in deciseconds, dot-free, distinct from the tail t-prefix). The
            known-failed skip and the residual auto-accept both key on ClipEnd (a mid
            verdict never blocks a tail re-attempt, and vice versa). LIMITATION (known,
            deferred): the repair model is ONE clip per chapter - a single repaired
            overlay + one verdict per chapter (MergeTailVerdict upserts by chapter) + a
            chapter-level resume guard (tailClipAlreadyDone). So a chapter needing TWO
            windows (two separate loops, or a mis-covered mid window that must be widened)
            cannot compose: a second clip re-splices from the ORIGINAL transcript (losing
            the first) or is skipped as already-done. Mitigated by prompting the agent to
            bound mid windows GENEROUSLY (a mid_clip cannot be refined) and to use
            retranscribe for a multi-loop chapter; the endSec is clamped to the chapter
            duration. A proper fix (per-window verdicts + splice composition) is a
            follow-up. The reliability round added the DIRECTED (run-less) tail path:
            when LocateTailRun finds no loop (a SHORT tail repeat - a 3x phrase is below
            the 6-gram locator's reach) but the plan supplied clip_start_sec,
            clipAndSpliceDirected cuts [override, chend+2] anyway, health-check-gated
            like the MID path (no rotation-adjudication), writing a TAIL-REPAIRED
            verdict (ClipEnd 0, so auto-accept/known-failed behave like a located tail);
            a degenerate override past chend-1 is the Unlocatable no-op, never an error.
            ClipResult.Unlocatable() reports a no-op: the true tail no-op (no loop AND no
            override), a MID window whose start is out of range for the chapter, or a
            dispatch-level out-of-range rejection - all bucketed as clips_unlocatable +
            a note (naming the missing window, or the out-of-range value and the
            chapter-relative rule) so the adjudicator can correct the plan next round.
  pipeline/ composite scheduler.Executor: routes inspecting -> audio.Inspect,
            splitting -> audio.Split, asr -> the per-chapter internal/asr loop
            (resumable: skip complete raws, delete+retry malformed, freeze each raw
            0444, write asr.json provenance, account scratch; each chapter decode is
            bounded by asrChapterDecodeBound = max(10min, 3x audio duration) - a
            whisper repetition-collapse decode that blows past it is killed and retried
            ONCE with NoContext, and a second timeout parks asr_decode_timeout rather
            than running for hours; the ASR/retranscribe heartbeat ticker advances BOTH
            heartbeat_at AND progress_at, since those stages have no mid-chapter output,
            so a live-but-slow decode trips neither the supervisor's stale-heartbeat nor
            its no_progress detector), sanitizing ->
            internal/transcript normalization, qa_sweep -> the internal/qa sweep
            (writes both reports, branches on Report.Clean()). M5 made EVERY remaining
            stage real: markers_normalizing/qa_adjudicating/spelling_research/fact_pass/
            synthesizing/auditing/fixing are AGENT stages (staged dir + rendered prompt
            + validated outputs via the shared runAgent driver, usage recorded onto the
            open stage_run after every invocation), while retranscribing/correcting/
            validating are MECHANICAL (ASR+repair / spelling engine / canonicalize+ngram).
            validating skips the n-gram scan only for a sidecar NGram would refuse
            (ngramGate).
            M7 made contributing real (contrib_stage.go: slug reconcile -> skip-if-
            covered -> submit per contribution.mode, resume-idempotent via the
            contributions rows; export.go composes the download zip + core-proposal
            JSON injected into api) - EVERY stage is now real. Sidecars go to
            Config.ContribCommunityRepo (issue mode) or <export>/<slug>/<kind>.json
            (local mode, also the zip layout). resolveWorkSlug adopts a 301 survivor
            (contrib.AdoptLiveWork) and parks core_pending with contrib.ReleaseWaitMsg
            while a merged core row's work is unreleased; a duplicate-answered core
            row parks core_needed with CoreDuplicateMsg.
            spelling_research additionally assembles OUTSIDE spelling evidence before
            it stages anything: referenceSources() ranks the metaops series glossary
            and the publisher's marker titles as VERIFIED against the predecessor's
            ledger as CARRYOVER, and BuildReferenceMatches turns that into the staged
            spelling_reference_matches.json. The metaops dependency is reached through
            the OPTIONAL MetaGlossary interface (type-asserted off the existing
            e.meta), so a nil/!ok client simply runs the stage exactly as before -
            the glossary is evidence, never a precondition.
            markers_normalizing's manifest contract has a COVERAGE half alongside the
            numbering one: validateMarkersManifest checks the corrected map against the
            RAW marker table from probe.json (never the draft - the draft is exactly what
            may have dropped a marker) and rejects any unmapped span the verdict did not
            DECLARE in its `excluded` list (title/start/end/reason, surfaced as a stage
            note). Nine books had interludes dropped by an agent that was consulted and
            answered confidently, because numbering was all it was checked against.
            Declaration rather than prohibition is deliberate: an omnibus file carrying a
            preview or the whole NEXT book genuinely must leave audio out, so the rule is
            that a drop must be stated, not that it is forbidden. markers.md correspondingly
            tells the agent that an unnumbered narrative section (Interlude/Side Story/
            Intermission/Prologue/Epilogue/"Chapter 10a") IS a chapter, that including it
            shifts nothing (positions come from the narration later), and to INCLUDE when
            in doubt. The load-bearing
            invariants live in
            the staging (synthesizing/auditing dirs hold NO transcripts, independent
            fact_pass chunk and QA partition dirs hold only their own chapter range and spoiler-bounded
            spelling sheet, and a separate notes-only assembly writes the compact final
            knowledge sheet). LOGICAL CHAPTER UNIVERSE (edgechapters.go): the manifest
            counts every audio FILE as a chapter, but an Audible "This is Audible"
            intro or publisher-credits outro file is not a story chapter - counting
            them deadlocked the audit loop on two real books (the auditor demanded
            recaps through phantom chapters the fixing stage could never satisfy,
            burning $200+). classifyBookEdges (content-driven, any manifest style)
            probes the first/last 8 chapters' transcript word counts (< 120 words AND
            < 180s duration at the edges only; probe-saturation and all-small books
            degrade to no exclusions) and derives the LOGICAL story-chapter count +
            stage notes. It reads each chapter's OPENING for EVERY chapter, not just the
            edges (256 bytes, unlike the whole-transcript word count) - the numbering
            below is only as good as its coverage, and reading openings at the edges
            alone left a long book's interior with no evidence, so any unnumbered
            section in the middle had to be guessed around.
            Synthesis/audit/audit-verify/fix prompts and the mechanical
            validateSidecars position cap use the logical count (sidecarStageInputs
            deliberately does not expose the raw manifest); the fact-pass CHUNK keeps
            file-numbered `## Chapter N` headings (matching staged transcripts + the
            chunk validator - never renumber), and the ASSEMBLE step is the ONE
            file->spoken renumbering boundary (its note renders the concrete offset
            mapping). The offset is DERIVED FROM THE NARRATION, never from counting
            files: deriveNarratedNumbering reads the chapter number each probed file
            ANNOUNCES (audio.SpokenChapterNumber - digits, spelled-out, optional
            "Chapter" prefix; the whole candidate must parse as one number, so prose
            opening "One more time" is rejected) and takes the offset from a run of
            >= minAnnouncementRun (3) CONSECUTIVE files announcing CONSECUTIVE numbers.
            Both conditions matter: one stray parse cannot shift a book, and files
            repeating the same number (a stuck transcript) are not a numbering. That run
            is the ANCHOR - the only announcements trusted outright; the derivation then
            extends outward, believing a further announcement only while it stays
            monotone, so a stray parse outside the run is discarded rather than allowed to
            renumber the book. Files
            before the first numbered chapter are FrontMatter (the meta schema's
            position 0 - an unnumbered Prologue is NOT chapter 1) and files past the
            last numbered one are EndMatter (an Epilogue/bloopers reel: outside the
            numbered range, its material belongs in the whole-book summaries, never a
            chapter-gated entry). With no agreeing run the classifier keeps the old
            positional behaviour byte-identically.
            The result is a PER-FILE map (edgeClassification.Positions, read via
            PositionOf), not one offset, because a single offset cannot describe a book
            with unnumbered material in the MIDDLE - an Interlude between chapters 9 and
            10, or a chapter split across "10a"/"10b". Such a file takes the number of the
            chapter it PRECEDES: rounding UP is the direction that cannot leak, since
            finishing chapter 10 implies having heard the interlude before it while
            finishing 9 does not. Three cases must not be confused when filling the gaps:
            a file that was read and announces nothing is unnumbered narrative (round up);
            one that announced a number which was NOT believed is still a numbered chapter
            (keep the shift); and one with no transcript at all is too (silence is not
            evidence). Folding the last two in with the first collapsed a 59-chapter book
            onto chapter 55. ConstantOffset() reports whether the map is a single shift,
            so composeAssembleNote keeps its byte-identical "subtract N" sentence for the
            common case and renders an explicit file->chapter run table only when it is
            not - inventing a formula for a book that has none is the original error. This was a real incident: a 64-file
            book (Audible intro, unnumbered Prologue, chapters 1-59, Epilogue,
            bloopers, credits) counted as 62 logical chapters at offset 1, so EVERY
            reveal and recap gate sat a chapter early and the position cap allowed
            gates past the end of the book; the auditor correctly rejected them as
            disclosing later material and the audit/fix loop burned round after round
            chasing one clause at a time. Note the numbering is baked into
            knowledge-final.md, so a book affected by this must be re-run from
            fact_pass - retrying the fix loop only chases symptoms. Exclusions and the
            derived mapping always surface as a stage note. Fact chunks and QA
            chapter partitions run concurrently
            behind the same executor-wide invocation semaphore. New capacity uses
            queue_concurrency * max_agents_per_book; legacy concurrency remains a
            non-multiplied global cap. Spelling reference_files are restricted to the
            daemon-staged carryover. Constructed in app/engine.go with the
            toolfetch-resolved paths, the asr.Select-chosen backend, and the agent
            container's runner (agentbox, never a local CLI: decision 15), with
            Config.AgentRedetect replacing agent.Select as the re-detection (Phase 1b's
            workers.Router always reports itself available, so ASR never re-detects). The sanitizing stage
            deliberately RE-DERIVES all chapters every run (cheap, idempotent, raw is the
            source of truth) rather than tracking per-chapter freshness. Missing tools or
            an unavailable agent backend PARK a book needs_attention (a human-fixable
            precondition Retry re-admits) instead of hard-failing. The qa_adjudicating ->
            retranscribing -> qa_sweep loop has three convergence guards beyond the
            maxQARounds (5) round cap: (1) a progress-based STALL park - the retranscribing
            stage INCREMENTS a retranscribe_stalled counter marker whenever a repair round
            splices AND adopts nothing (no real progress) and removes it on any progress;
            qaAdjudicate parks ParkQANoConverge (naming the stuck chapters) only at count
            >= 2 - a count of 1 grants the agent exactly ONE resolution round, because a
            no-progress round produces precisely the feedback (clips_unlocatable notes,
            known-failed skips, kept retranscribes) its terminal accept-or-direct decision
            needs (live-verified: the one-round park fired before the adjudicator could
            ever use the unlocatable feedback). The adjudicator's staged transcripts cover
            qa.AllowedChapters (every disposable chapter), not just the required
            FlaggedChapters - a textless tail-rate-only chapter made the agent queue
            conservative clips it could not verify. This REPLACED an earlier report+ledger sha256 fingerprint, which
            thrashed on the exact incident it was meant to catch: a re-degenerating tail
            clip rewrites tail_verdicts.json every round (each CLIP-REDEGENERATED verdict
            relocates its clip_start), so the fingerprint changed every round, the fixed
            point never fired, and the book burned its whole round budget (~$1.5/round). The
            marker is DELETED on ANY qa_adjudicating park (the stall park and the round-cap
            park) and on the done==0 reset (a Retry/purge-rewind must not inherit a stale
            marker), so a Retry always gets exactly ONE fresh agent round before it can
            re-park. A mid_clip splice increments spliced (it reuses the tail buckets), so
            an interior repair counts as progress too; (2) a WIDENED residual auto-accept -
            tailOnlyChapters (via spanCovered) accepts a repaired chapter whose
            cross-segment / multi-loop hits are residuals the recorded splice window covers:
            the window is [clip_start, clip_end] for a MID splice (both bounds constrain the
            span within +/-15s) or [clip_start, +Inf] for a TAIL splice (only the start,
            with a position>=95% fallback when the hit has no usable time). An UNTIMED
            cross-segment hit (no first_sec, written as pos -1) is the characteristic shape
            of a tail loop re-reported off the untouched transcripts-json/ layer, so that
            positional fallback can never fire for it; crossHitTailCovered therefore falls
            back to a phraseResolver (repairedPhraseResolver, injected so the rule stays
            unit-testable) asking the decisive question the window arithmetic cannot: is the
            looped phrase still in transcripts-repaired/? Gone = the splice resolved it;
            still present = the repair under-covered and the chapter stays with the agent.
            Without this EVERY untimed residual went to the agent, and adjudicate.md tells
            the agent NOT to disposition a chapter a prior tail_clip round already repaired -
            so it omitted one ("intentionally omitted" in its notes), the plan validator
            rejected the plan for a missing required entry, and the stage failed identically
            on every retry until the book parked agent_validation_exhausted. adjudicate.md
            now also states that the auto-accepted list is EXHAUSTIVE - any other flagged
            chapter the agent believes is already repaired still needs an explicit accept,
            because an omission is not a disposition. A MID-CHAPTER
            multi-loop is covered ONLY by a recorded MID window (never a tail window); a mid
            window with an untimed hit is conservatively NOT covered; (3) plan
            clip_start_sec/clip_end_sec (per tail_clip/mid_clip entry) feeds the repair
            known-failed skip above, and (*qa.Plan).Validate(rep, durations) bounds every
            clip window against its chapter's manifest duration (chapter-relative
            seconds; an at-or-past-duration value is rejected with retry feedback naming
            the chapter, value, and duration - adjudicate.md states the timebase). wph outliers, within-segment hits, non-end-fade runs, a
            MID-CHAPTER multi-loop NOT covered by a mid window, and spans that straddle
            mid-chapter into the tail still always disqualify a chapter from residual-only.
            The repair re-transcription is decode-tagged
            (retranscribe/decode_params marker): a stale pre-NoContext fresh raw is discarded
            so the chapter is re-transcribed under the current params rather than reused, and
            a free known-failed skip is excluded from the stage's rate sample. The
            reliability round added (4) a DURABLE accepted-chapters ledger
            (qa_accepted.json): every accept (agent's and auto-accept's) persists and is
            mechanically re-accepted in later rounds without re-invoking the agent - most
            detectors read the stale unrepaired layer, so repaired chapters re-flag
            forever and were being re-verified at full agent cost every round. The ledger
            is deliberately NOT cleared by the done==0 reset or Retry (repairs only touch
            planned non-accept chapters, so accepted decisions stay valid). The AUDIT
            loop likewise now terminates by ACCEPTING: audit_rounds.json records each
            round's {blocker,fix,nit}; when a non-passing round is CONVERGING (blocker 0,
            validation clean, round >= 2, 0 < fix <= auditAcceptMaxFix(2), actionable
            findings not growing (so BLOCKER -> FIX is progress), fix budget left) the
            stage writes audit_accepted.json (the residual
            findings), routes to ONE final fixing round, and the re-entry runs a focused
            semantic verifier over those exact accepted findings when validation is clean -
            so a mechanically valid but unapplied FIX cannot ship and a
            90-chapter book (where a fresh adversarial pass finds ~1 new small defect
            forever - a sampling process that never reaches zero) finishes instead of
            parking fix_loop_exhausted with round-N's findings still live. Blockers or a
            growing fix count still park, with the fix-count trajectory in the park
            message (StageResult.ParkMessage). Both audit.json writers (audit.md AND
            audit_verify.md) feed the same DisallowUnknownFields reader, so EACH must
            state the exact two-field (`pass`/`findings`) shape and name the `nit` key
            agents reach for - a prompts drift-guard test (auditJSONPrompts) pins that.
            audit_verify.md had drifted to "the normal audit shape" while inviting NIT
            reporting, so a PASSING verify emitted {"pass":true,"nit":0,"findings":[]}
            and the reader rejected it on every retry: a finished book parked
            agent_validation_exhausted one cosmetic key from done. A nit is a findings
            entry with severity NIT, never a top-level key.
            contrib appends an acceptance note to the
            contribution rows (process metadata only - the public issue/PR payload is
            unchanged). runAgent also enforces agent.book_budget_usd (default 75) as a
            preflight: summed stage_runs cost (superseded rows included, so Retry can't
            duck it) >= budget parks ParkBudgetExceeded before spending more.
            Post-M9 round: the split stage retries a transient
            audio.IsTransientSourceErr in-place (3 attempts, resumable - completed
            chapter FLACs are skipped; agent.SleepCtx backoff) instead of failing
            the book, and runs under the shared stage heartbeat
            (runWithStageHeartbeat, the generalization transcribeWithHeartbeat now
            wraps) so the supervisor cannot kill a slow single-chapter ffmpeg
            conversion between progress reports; the split RateSample starts at the
            first progress report (audio.Split emits it after staging) and
            subtracts the backoff slept, keeping the staging copy and retry sleeps
            out of the learned EWMA. validateMarkersManifest's coverage check
            accepts adjacent per-marker exclusion declarations whose gap-free union
            covers one coalesced unmapped span (exclusionsCover, same 1s tolerance,
            never bridging a real undeclared gap) - UnmappedSpans coalesces
            adjacent markers while the verdict schema asks for per-marker
            declarations, so requiring one coarse declaration rejected the more
            precise correct verdict.
            Phase 1a: the contribution mode is PER BOOK (contribModeFor: books.contrib_mode,
            which a sidecars.generate job always sets from its contribute param, else
            the configured contribution.mode). runAgent's budget preflight gained the
            MONTHLY half (checkMonthlyBudget, Config.MonthlyBudgetUSD): this calendar
            month's (UTC, MonthBounds) spend across every book, superseded runs and
            deleted books' runs included (store.SumBudgetCostSince), at or over the cap
            parks budget_exceeded WITH a retry_at of the 1st of the next month; the
            per-book park still carries none. MonthBounds is the one month the
            estimates and the page count too. A preflight-unavailable runner whose
            Availability carries RetryAfter (the agent container) parks
            agent_unavailable with that retry_at and the runner's own Detail as the
            reason (agentUnavailableReason; ensureAgent keeps the freshest Detail);
            one without still parks for a human. A run's CredentialRefused
            NotAvailableError parks agent_credential_refused with a retry_at 30 min
            out (credentialRefusedReadmitDelay: each look costs one refused call, no
            tokens). SetAgentConcurrency(n) resizes the global invocation cap and the
            per-book fan-out while the executor runs (agentWorkers is atomic,
            agentSlots a resizable slotPool: runs past a lower cap finish, new ones
            wait), so the engine follows the container's slots. FinalSidecarsPaths(workDir)
            names the validated characters.json/recaps.json generate copies into the
            library (SidecarsView reads the same two). ManifestChangedMsg now says to
            cancel the job and generate again.
            Phase 1c, the DESCRIPTION STEP (describe_stage.go): Executor.Describe(ctx,
            bookID, noWait) writes a finished book's own-words description.
            It is NOT an engine stage (the state machine, scheduler and supervisor
            never run it): the Bridge's sidecars.generate (at its finish) and
            sidecars.describe call it, under the scheduler's Hold on the book, and
            the contributing stage calls it first (describeAtContributing, at the
            stage's start, outside its rate sample, under its heartbeat, no hold:
            the book is the scheduler's in-flight book), while the source text is
            still there: an ebook's text goes as the book reaches done, before the
            job's pass can ask, so a description first written then is checked
            against nothing (a 13-word run copied from the epub was accepted). Best
            effort: only for a book an open sidecars.generate job follows
            (store.GenerateBookFollowed), only when the book has no describing run
            yet (ok or failed: a contributing park runs the stage again and again,
            and never pays again), noWait, any error logged and never the stage's.
            The job's pass then reuses it (below), keeping its checked_against. The
            SPOILER BOUND is the staging: the agent's dir holds ONLY opening.md (and
            out/), built mechanically from the fact pass's chunks (chunk_plan.json,
            named by factsChunkName) covering the chosen files: the `## Chapter N` sections (file numbering kept,
            never renumbered, a "Chapter 0" too) of the files whose LOGICAL position
            (classifyBookEdges -> PositionOf) is 0 (front matter) or among the
            book's first K story positions, K = max(2, ceil(n/4)) capped at n-1
            (never the last chapter: a book of two gets its first, a book of one no
            description, ErrOneChapter), n the count of distinct story positions the
            files hold capped at the logical count (never LogicalCount alone: narrated numbering takes it from the
            highest announcement believed, so a misheard "Forty." or a year, or a
            sequel numbering on from 45, would widen or empty the window);
            excluded edge files (an Audible intro, credits) and end matter never. No transcript, no knowledge-final.md (it
            carries FINAL/ENDING), no later chapter, no publisher text (a test lists
            the staged dir). The classification reads the transcripts, which the
            retention deletes, so each one is kept as chapter_positions.json (keyed
            by manifestFingerprint; an unchanged one isn't rewritten;
            PurgeTranscripts leaves it) and a book whose
            transcripts are gone uses it, else its FILE numbers with the logical count
            from the recaps' last through (a file's number is never below its story
            chapter, so that bound is safe, only less generous); an ebook classifies
            from its manifest alone. The run's characters.json supplies the work and
            sources (a run whose own work/sources can't make a valid description
            fails before any spend); the per-book and monthly budget preflight
            (budgetPreflight, which runAgent runs for every other stage and
            runAgentWith leaves to its caller) runs after the preparation (local
            reads, which keep chapter_positions.json even for a call the budget
            parks) and before the stage run, so a budget-parked retry costs nothing
            and opens no run. The model is config's route for "describing"
            (state.Describing, which agent.claude/agent.openai accept; agent.claude
            defaults it to sonnet). The validator (patch feedback on a retry):
            strict {"text"} decode (decodeSidecarBytes) with a 64 KiB cap; trimmed text 200-1500 runes; no
            control character (nor U+2028/U+2029) but newline (the server refuses
            control characters); no em dash; at most two paragraphs; no second
            person, no sentence ending in "?", no sales words, with word edges by
            Unicode letter and the book's own title and series exempt (a book may
            be "Me Before You" or end in "?") (descriptionProblems); then the composed model.Description
            (license CC-BY-SA-4.0) against meta's description.schema.json
            (sidecarSchemas now compiles it too), then extract.NGram against the
            transcript text layers through ngramScan (ngramCheck's scan without its
            "no layer" refusal): a hit is a retry naming the run; a local fault of
            the check (the disk, a transcript that can't be read) is no agent
            retry: the answer is taken and the run fails with a plain error; no layer at all
            (the retention purged the transcripts; an ebook's text goes as the
            book reaches done, so only a later sidecars.describe meets an ebook
            without it) skips the check and records checked_against "none". Spend: the call opens its own stage_run (DescribeStage
            "describing", numbered store.NextStageAttempt as the scheduler's runs
            are, so each run has its own _runs/describing-aNN) on the book, so
            invocation tracking,
            SumStageRunBudgetCost, the monthly sum and the estimates see it, and
            finishes it ok/failed with {chapters, logical_chapters, basis,
            checked_against, usage}; runAgentWith is runAgent after the preflight with
            the rate-limit backoff given (noWait passes an empty schedule: a rate
            limit parks at once), stageMaxTurns gives it 10. Its parks come back
            as *scheduler.ParkError (the caller decides; a budget park's reason is
            every stage's, budgetExceededMsg). The composed file is kept
            as sidecars/description.json (FinalDescriptionPath, canonical JSON); a
            later call reuses it (re-composed with the current work/sources, nothing
            spent) when a successful describing run recorded how it was made
            (store.LatestOkStageRun), its chapters and checked_against read from
            that run's metrics (the check it was made with, never one run again
            against layers since gone). ErrNoNotes: no characters.json, no manifest, or no notes for the opening.
            Phase 1d, CHAPTER_DETECTING (chapterdetect_stage.go): runs after asr when
            its StageResult says Windowed (the manifest is a windows manifest). It
            loads the window raws onto the whole-book timeline (loadWindowTimeline:
            window start + whisper's times, whisper.cpp sub-word tokens glued into
            words at their lowest probability, a wordless segment as one word for
            the detection only (its synthesized raw stays wordless), nothing timed
            past the window's own audio: a hallucination there would interleave with
            the next window's words), takes each window's silences
            (audio.WindowSilences: the ones split heard and kept beside the FLAC,
            else one bounded silencedetect over it, kept too, so a Retry decodes
            nothing) and runs chapterdetect.Detect. Not confident: parks
            chapters_not_detected (ChaptersNotDetectedPrefix + the reason) with
            nothing moved, so a Retry detects again. Confident: the detection and a
            copy of the windows manifest go to windows/ FIRST, then chapters/ (the
            window FLACs, whose names are the detected chapters' too) is renamed to
            windows/chapters/ and transcripts-raw/ to windows/transcripts-raw/, after
            which the move counts as done and windows/ is the source of truth (a
            re-entry reuses windows/detection.json, never needing the FLACs). Then
            the DETECTED manifest (StyleMarkers, Detected), tl.split ONCE (a segment
            goes to the chapter of its first word, a boundary inside one splits it at
            the word, times chapter-relative) feeding both one raw per chapter
            (transcript.EncodeOpenAI, frozen 0444, a complete one kept and never
            encoded) and the list; asr.json with the new fingerprint (computed once,
            passed on) and empty chapters, SetBookChapters, the chapter FLACs CUT
            FROM THE WINDOW FLACs (audio.CutFromWindows: local, decoded already; the
            source split only for a chapter whose window is gone, a crash after they
            went, with a note), windows/chapters/ removed, and chapters_detected.json
            (DetectedChapters: manifest_sha, source_size (0: it couldn't be looked
            at, never compared), duration_ms, the entries), after the classification
            it was grouped by (positions_detected.json, with the manifest's
            fingerprint). detectedClassification classifies with classifyEdgeChapters
            over the synthesized transcripts in memory through edgeChapters, the
            probe-window rule classifyBookEdges uses too (a chapter whose "chapter"
            heading the detector numbered, chapterdetect.Chapter.Number, opens with
            that number announced: the classifier reads only a bare "Chapter Four.",
            and "Chapter Four, The Storm." would join the next chapter's entry as an
            unnumbered section), and groups the entries by it
            (groupEntries): front matter and an intro join the first story chapter,
            an interior unnumbered section the chapter whose number it takes, credits
            the entry before them, end matter stays its own ("Prologue / Chapter 1",
            bounded by chapterdetect.MaxTitleBytes). Entry k IS logical position k BY
            CONSTRUCTION: rankPositions makes the classification's positions the
            entries' ordinals (where the narration's numbering skips one, a heading
            not heard, the entries would otherwise run a position behind), and
            classifyBookEdges takes the kept classification for a Detected manifest
            (loadDetectedPositions) instead of classifying again from the text layer
            QA leaves (a re-transcribed opening, a loop repaired past the edge rule,
            moved every position off the list). DetectedChaptersPath /
            ReadDetectedChapters / CheckCurrent (manifest fingerprint, the source's
            size and mtime (source_mtime_ns, within sourceModSlack, 2 s, for a
            mount's rounding) at the path the caller says the book is now (a moved
            book; "" the manifest's); fails closed: a stat error is never current
            and is returned, so a caller asks again rather than clearing a run; a
            list with no mtime recorded falls back on inspectedSource's rule;
            writeDetectedChapters records the size and mtime only while the file
            is the one inspect read (inspectedFile: probe.json's size, not modified
            since), so a file replaced mid-transcription
            leaves a list never current for it) /
            DetectedStamp (local stats, so a poller re-reads only a changed list) are
            generate's, as is TranscriptionContinuable (a book stopped at asr can go
            on from what it kept: the windows manifest in place, not moved, asr.json
            naming no other manifest, the file the one inspected, inspectedSource,
            and while splitting's sentinel would pass it on, every window heard and
            a FLAC for each one not transcribed yet), and DetectionRerunnable (a book stopped at chapter_detecting
            can run it again from what it kept: the window layer with the confident
            detection kept for it, or in place with every window's silences to be
            had; every window's raw complete; the file the one inspected: the size
            probe.json states, which must be known, and not modified since probe.json
            was written (sourceClockSlack, 2 minutes, for a library on a clock a
            little ahead), as a tag editor adding markers can keep the size), for its
            continue. The stage
            reports one progress unit, as every book stage (the
            ETA charges a unit its learned per-book time). inspect (its windows'
            silence probes under the stage heartbeat; the marker-format note only for a
            manifest that isn't windows) fails a goal-chapters book whose manifest
            isn't windows with what is true of it (chaptersRefusal:
            ErrHasMarkerChapters / ErrHasFileChapters, or for a file with no chapters
            that can't be windowed ErrPartialMarker, one marker over part of it, /
            ErrUnknownLength, no length to cut by; ChaptersRefusals, sentences the
            console shows as they are; generate reads them from the inspecting run's
            own error, which a supervisor escalation never rewrites). marker_titles.txt only ever holds
            the publisher's titles: ensureMarkerTitles writes a Detected manifest's
            EMPTY (titles heard in this transcript would make a misheard name attest
            itself), so referenceSources, the dry-run corpus and allowedReferenceFile
            never see them, and the manifest staged to the spelling agent goes without
            them (stagedManifest).
            The env-gated corpus test (AUDIOSILO_CHAPTERS_SPIKE_DIR,
            chapterdetect_corpus_test.go) runs the detection through the stage's own
            loading on the spike's real raws.
            Phase 2b: share.go, Executor.Share(bookID, workID, dims) is
            sidecars.contribute's step, outside the engine's stages (the caller holds
            the book; no agent, no spend): the book's kept characters/recaps filed as
            intake issues the way the contributing stage files them in issue mode.
            shareWork verifies the job's work id (a retired slug followed to its
            survivor and recorded on the book), else the book's match, else its exact
            identifiers; unmatched is ErrNotMatched (a share never proposes a new
            work). shareSettled: only submitted, pr_open or merged rows count as
            filed (a local export, a covered verdict or a closed issue is filed
            again); a dimension already filed is kept and reported. Missing sidecars
            is ErrNoSidecars. Config.MonthlyBudget (a func) is read live, so a budget
            saved on the page applies without a restart.
            contrib_filing.go: every issue-mode submit (the stage and Share) files a
            dimension once (fileIssue). Before each create call (the gist,
            the issue) it records a filing marker in the settings KV
            (contrib.filing/<book id>/<kind>: the payload's sha256, the GitHub
            login, gist_since, the gist's raw URL once made, issue_since and the
            sha256 of the title and body the issue POST sends; times in GitHub's
            clock, contrib.Client.Now), since a POST that outlives the client's
            15 s timeout may still have made it. The next submit looks for a
            pending issue first, whatever the sidecar is now: IssuesSinceBy with
            the marker's login (else the signed-in one; a 401 reading it is the
            lookup's error, so the share says the sign-in was refused) from its
            time less 2 minutes, through the rows of pages read before one that
            failed too, the same title and
            body, never the issue the row already names (a closed one a share
            files again). Then, for the same payload, a pending gist: GistsSince,
            one file of that name, the description naming the payload's sha256.
            It adopts a match and creates again only when the list is complete,
            holds none and (for an issue) the marker is a minute old; else it
            fails with ErrFilingUnknown (the share job words it and logs the
            cause) and keeps the marker. Only a list that can't reach back to a
            marker over a day old by GitHub's clock (contrib.ErrListIncomplete,
            which only grows, and contrib.FilingStale: refileStale) files again
            (meta's intake catches a duplicate it has applied, and nothing on
            the page clears a marker); a list that can't be read (an outage, a
            refusal, a rate limit) stops it at any age, since a later submit
            can read it. A refusal or a request that never left
            (contrib.NothingFiled: a 4xx, a rate limit, a failed dial) clears
            the pending time; a made gist is kept for the next try; the marker
            goes once the row is recorded.
            Phase 3c-1, readalong_stage.go: readalong_aligning reads
            readalong/request.json (WriteReadalongRequest: the ebook's Bridge path, the
            server's files book-relative in its order with their durations, the Bridge
            version), composes the timeline from the manifest (files style by
            Rel(manifest.Source, file), the single-file styles by start), cuts again
            every chapter FLAC a purge took (splitWithRetries on the manifest), reads
            transcripts-json/ only, opens the EPUB (its sentences as the job's
            refusals), aligns, heals (the windows: HealWindows' plus tail_verdicts' plus
            the located loops of a qa.CrossSegment over the transcripts it reads,
            readalongLoopWindows; each window a clip cut across its chapter FLACs
            into readalong/heal/<f###_start_end>.flac, transcribed NoContext through the
            engine's backend: a worker when one is connected; a complete raw reused, the
            clip deleted, an empty answer keeps the words it had), aligns again, snaps
            on energies from an ffmpeg f32le pipe per chapter FLAC (only the energies
            kept), and writes readalong/map.json, words.json and align_report.json
            (coverage, first-pass coverage, heal windows and seconds, snap stats, ebook
            sha256, resplit). The map's generator.asr is path.Base of asr.json's model.
            qa_sweep for goal readalong (readalongSweep): a dirty report writes
            qa.MechanicalPlan and goes to retranscribing ONCE (retranscribing writes
            readalong/repaired for goal readalong; ResetReadalongRepair forgets it),
            then on to aligning clean or not with a note naming the chapters left.
            retranscribeChapter cuts a missing chapter FLAC again (resplitChapter, any
            goal). inspect for goal readalong uses InspectWith(WindowUnusable) and fails
            ErrReadalongNoLength for a marker file it can't window.
            TranscriptionContinuable now covers files and markers manifests too
            (chaptersContinuable: raws of this manifest, AudioCurrent, a raw or a FLAC
            per chapter once splitting's sentinel is there). AudioCurrent(workDir,
            source) is the reuse table's audio check: a single file probe.json's size
            and not modified after it (sourceClockSlack), a folder the same files by
            name and order none modified after probe.json; an error when it can't be
            looked at. ReadalongMap/ReadReadalongReport are the job's readers.
  contrib/  M7: everything GitHub-facing for contribution. TokenSource (Phase 2b:
            NewTokenSource(device, onChange) resolves AUDIOSILO_BRIDGE_GITHUB_TOKEN
            read LIVE from the environment first (never sealed any more), then the
            Accounts tab's device sign-in, then `gh auth token`; Resolve returns the
            token and where it came from (FromEnv/FromDevice/FromGH), else
            ErrNoCredential, or ErrSignInUnrefreshed (an ErrNoCredential too) when
            the only credential is a sign-in GitHub couldn't be reached to refresh
            (accounts.ErrUnrefreshed), which callers word as "once GitHub can be
            reached" rather than "sign in"; a 401 on a device token marks it refused
            (a call that works with one moves the sign-in's last_ok_at: NewClientFor
            wires both hooks) - the token NEVER enters argv/logs/
            errors, leak-canary tested). NewClientFor records where the token came
            from: AppFiled() (a device token) skips the labels check, since meta's
            routing workflow labels an issue the App filed. corrections.go (Phase
            2b): CorrectionBatchIssue composes meta's correction-batch issue
            ([corrections] <title>; labels data + data:correction-batch; Record,
            Changes as fenced JSON {v:1, source, changes}, Evidence / source, the CC0
            ticked box), the golden body being meta's TestBatchGoldenBody except that
            every change names its record. MapCorrections maps the server's fields
            (title, author, series, series_index, more_series on the work;
            published -> release_date, isbn, narrator on the recording, addressed to
            the work and left to a maintainer when no recording id is known);
            anything else, a duplicate field (two distinct ISBNs excepted) or a value
            past the bounds (25 changes, 500 characters, 2000 of evidence) is
            ErrInvalidCorrection for the whole issue. A stdlib REST client (injectable base URL; typed RateLimitError; APIError
            carries status + trimmed body only), composers that render the meta repo's
            issue-form markdown VERBATIM to metaissue's parser contract (headings +
            ticked checkbox items pinned from the form YAML; env-gated round-trip test
            AUDIOSILO_META_DIR runs the real `go run ./cmd/metaissue` -> verdict ok;
            >60000-byte bodies fall back to a secret gist link, which metaissue's
            attachment allowlist accepts), CoreProposal (+Validate: title/authors/
            language/narrators/sources required; an ASIN without a region is rejected,
            never silently dropped), Service (SubmitCore: per-book mutex, reuses an
            already-recorded core issue, persists the row BEFORE the park flip;
            SetWork validates the slug upstream), and the poller. Where things live:
              poller.go  row lifecycle (FindIntakePR -> pr_open -> merged/closed),
                         intake verdicts as the note's last segment (re-checked at
                         most hourly; comments re-read only when the issue changed,
                         newest bot comment wins), learnCreatedWork (base.sha...head.sha compare,
                         works-pack ENTRY KEYS at merge base vs head), the release
                         gate (admitWhenLive / releaseCorePending)
              notes.go   JoinNotes, AdoptLiveWork (shared with the stage)
              github.go  the REST client: issues, gists, pulls, compare, contents;
                         the filing lookups' lists (IssuesSinceBy, GistsSince) read
                         page by page, at most 20 pages (ErrListIncomplete past
                         them); Now is GitHub's clock, from the Date of its last
                         answer (GET /rate_limit, free, when there was none);
                         StaleFiling ages a marker by that clock (false while
                         GitHub hasn't answered: it never asks, nor falls back
                         on the Bridge's clock)
            ServiceDeps.CoreRepo is where SubmitCore opens add-work issues; every
            other call follows the repo a row records.
            Imports neither scheduler nor api - reaches them via injected
            Readmit/Publish seams.
  auth/     single admin password (argon2id, generated + printed once on first run),
            opaque SHA-256-hashed session tokens, a per-IP login rate limiter; the
            Store interface is storage-agnostic (MemStore for tests; the SQLite
            store.AuthStore in production - the M0 JSON store was removed in M1).
            ReplacePassword (a change, a reset) and CreatePassword (the first run)
            are the only password writers: the new hash and the sign-out in one
            transaction (bridge#11). A change passes the hash it verified and
            replaces only that one (an UPDATE ... WHERE value=), so a change racing
            a reset or another change is refused (ErrPasswordReplaced, the page's
            409) instead of overwriting it; a reset replaces whatever is stored;
            the first run's EnsureAdmin creates one only while none is stored (an
            upsert that updates only an empty value), so a reset-password run
            that landed first wins and nothing is printed (bridge#16). Each check
            and write is ONE statement: the two-process race tests
            (authstore_race_test.go) fail if one is split into a read and a write. AddSession takes the hash the
            sign-in verified and stores the session only while it is still the
            stored one (one INSERT ... WHERE EXISTS), so a sign-in racing a change
            or the reset-password process is refused and leaves no session.
            Every argon2id hash and verify goes through argonKey, which schedules
            memoryReleaser (debug.FreeOSMemory once hashing has been quiet 1 s, on
            its own goroutine; the server's server#244 port): without it one login
            took an idle Bridge from 30 MB to 97 MB RSS for minutes.
  ghauth/   Phase 2b: the GitHub App user sign-in (the audiosilo-contributions App,
            DefaultClientID, AppID 5387686; the client ID overridable in config):
            RequestCode / Poll / Wait (the device flow: pending and slow_down arrive
            as a 200 with an error code; slow_down adds 5 s; expired and denied end
            it; a poll GitHub doesn't answer, unreachable or a 5xx/429, adds 5 s and
            polls on until the code expires), Refresh (client_id only; the old pair
            dies at once, so the caller persists the new pair before using it, and
            ANY error GitHub answers is ErrBadRefresh: a spent refresh token answers
            incorrect_client_credentials),
            Revoke (POST /credentials/revoke, unauthenticated, 202) and User (/user).
            Every base URL injectable; fake-server tests for each outcome.
  accounts/ Phase 2b (decision 19): the Bridge's external accounts. GitHub: Start
            runs one device flow at a time in the background (Pending shows the
            code), the token pair sealed as ONE vault value (github_user_token) so
            a refresh replaces it in one write; Token refreshes single-flight
            before expiry and persists first, detached from its caller (a page
            request that goes away mid-refresh would lose the rotated pair); only
            ErrBadRefresh refuses the sign-in, a refresh without GitHub's answer
            keeps the pair (the access token serves until it expires, judged
            after the refresh, which can take ghauth's request timeout; then Token
            answers ErrUnrefreshed) and is tried again a minute later; a device flow ended meanwhile (sign-out, a newer
            one) stores nothing; Refused marks the account refused (a 401 on a
            token a refresh has since replaced, or on one past its expiry, is
            ignored: the refresh decides then) and OK moves last_ok_at
            (a REST call that worked with the current token; never ends a
            refusal; a sign-in and a refresh move it too); SignOut ends any device flow,
            forgets the pair locally first (so the Bridge stops using it whatever
            GitHub answers), then revokes it, a failed revoke returned for the
            page. Agent: Set/Delete/Credential for the kinds
            claude_oauth_token, anthropic_api_key, openai_api_key (one printable
            line, sealed in the vault), handed to the agent container per run by
            agentbox; Credential's set time is that kind's own (agent/<kind>), so
            changing another kind never ends a refusal of the one in use. The
            store's accounts table (migration 0006) keeps only what the page
            shows (login, signed_in_at, last_ok_at, refused_at, last_error),
            never a credential.
  setup/    Phase 2b (FIRST-RUN-PLAN F2): Checklist(Inputs) is the setup checklist
            (link.Setup) in its fixed order: paired, libraries, asr, agent, budget,
            github, each {id, done, required, detail}; details cut to 200
            characters and worded for the console (the budget's "$100 a month" or
            "no monthly cap"); Complete is every required item done. The heartbeat
            sends it and the page's Status tab shows it.
  correct/  Phase 2b (plan D5): the meta.correct job (light lane). ParseParams
            is STRICT (an unknown field refused, work_id required). It reads the
            work's live coverage (FreshCoverageForWork: its title, the recording
            by ASIN (and region) or ISBN when the server sent no recording id),
            maps and composes the batch (contrib.CorrectionBatchIssue) and files
            ONE issue on the core repo with the GitHub sign-in. A job's result is
            kept under meta.correct/<server id>/<job id> (a resumed job returns
            it). The filing is guarded per REQUEST: before CreateIssue a marker
            {since (GitHub's time, contrib.Client.Now), sum (contrib.IssueSum of
            the title and body sent), job} goes to meta.correct.filing/<server
            id>/<request> (filingKey: the sha256 of the record, work_id plus the
            recording_id, or else the ASIN, ISBN and region that pick one, in the
            one form ParseParams gives them, and the changes as a sorted set of
            {field, corrected}; NOT the evidence, which the console refills with
            a default line naming the server version in the admin's language each
            time its dialog opens, nor the source stamp, a change's
            current value (it moves when the community applies a fix), the book,
            which can move, or the composed issue, whose title is the work's live
            title), so a NEW job asking for the same corrections (the admin
            pressing Suggest again after a lost answer, reworded or reordered,
            whatever the book is called by then) sees it too and adopts the issue
            the marker's sum names (while the marker is under a day old, below),
            and other corrections never share or overwrite it. It is deleted (not emptied:
            the keys are per request), and only while it is still the one the
            run wrote or adopted (clearFiling compares: an overlapping run's
            newer marker stays), once a filing is confirmed and its result
            recorded (filed, or adopted; a failed result write keeps it, so the
            job resumed adopts the issue), when GitHub answered a 4xx or a rate
            limit or the request never left (contrib.NothingFiled; a 5xx can
            come after the issue was made, and the client never resends a POST),
            or once a person is told the Bridge can't tell (below). A run that
            finds a marker lists the core repo's issues made since
            (contrib.IssuesSince, the rows of pages read before one that failed
            included) and: adopts a [corrections] issue with its own sum or the
            marker's (the same request composed earlier: the work's title
            corrected since, a recording that took the ASIN, or the evidence
            reworded changes what it composes now; a console that names the
            recording by then, or leaves out a change the community applied,
            sends another request, which files), another job the marker's only
            while the marker is under a day old by GitHub's clock
            (contrib.FilingStale: later, the same changes suggested again, with
            a source the maintainers asked for, say, are no retry of the lost
            answer); else fails asking the admin to look first (errFiledBefore)
            for a same-record batch with another body (otherBatch: another
            admin's, its own edited since, or past a day the lost one; the marker
            goes once the list was read back to it whole and it has settled, so
            the next suggestion files) or a marker without a time (which goes
            too); waits (errSettling) when the marker is under a minute old
            (contrib.FilingSettle, Client.Settling: GitHub may still be making
            it); and for an unreadable or incomplete list stops only while the
            marker is under a day old by GitHub's clock (contrib.FilingStale,
            Client.StaleFiling), past which it files again (meta's intake
            catches a batch it has applied). A 401 (contrib.Unauthorized), on
            the lookup (even past a same-record batch on a page read before) or
            the filing, is errRefused; a rate limit on either errRateLimited (a
            lookup's keeps the marker); a lost or 5xx answer errNoAnswer
            (worded: the Bridge checks before filing again). Result {issue_url,
            issue_number, changes, auto, maintainer}: auto and maintainer as
            known at filing; the Bridge doesn't follow the issue (a batch meta
            applies only in part is not reported back, plan D8).
  secrets/  named secrets (the server token, the agent credentials, the GitHub
            sign-in; github_pat only to delete an earlier Bridge's copy) as
            AES-256-GCM sealed files in <data>/secrets (0600, the manager BlobStore's
            format, each bound to its name as additional data so a file renamed onto
            another name doesn't open), keyed by ONE master key:
            AUDIOSILO_BRIDGE_SECRET_KEY (32 bytes, hex or base64; lets the key live
            outside the data volume) else <data>/secret.key (created 0600, O_EXCL, on
            the first start, synced, and removed when it couldn't be written whole:
            an empty key file would fail every later start). A changed key is
            ErrWrongKey, which the start reports in words. No OS keychain: the
            Bridge runs headless in a container. Names are ^[a-z][a-z0-9_]{0,63}$ (a
            name is a file name); Get hands a secret to the code that uses it, and
            nothing echoes one. Phase 2c-1: PodcastJobName(id, attempt) (each
            claim its own: an earlier attempt's end never deletes a later one's)
            and PodcastFeedName(show_id) name sealed podcast credentials; they
            are omitted from the operator-managed Names() list.
  store/    SQLite (modernc, pure Go; single writer + WAL) + append-only migrations,
            bridge.db. 0007 adds podcast_shows, a credential-free cache keyed by
            (server_id, show_id), indexed by server/library/feed hash. An empty
            library cache is rebuilt from feed.json before subscription dedupe.
            0008 adds podcast_artwork_state keyed by server/library/show, storing
            only source hash, failure count and next retry time (1 h to 24 h).
            0001 is sidecars' 13 migrations squashed at the import (the
            later columns folded into their tables; candidate_overrides and
            library_sightings dropped; the 'legacy' batch row still seeded): books,
            stage_runs, progress, events (durable log, 30-day prune), rates
            (per-stage EWMA unit rates), settings KV, sessions, contributions,
            batches, supervisor_runs, agent_invocations. The park_code invariant
            (non-empty iff status is needs_attention) is enforced INSIDE
            SetBookState/SetBookStatus, not by caller discipline; contributions'
            UNIQUE (book_id, kind) makes UpsertContribution the crash-resume
            idempotency guard (it keeps a row's intake-PR pointer and
            issue_seen_at only while the row names the same issue);
            books.retry_at is cleared with status like park_code; a Retry
            SUPERSEDES stage_runs success rows instead of deleting them, so
            SCHEDULING readers (CountStageSuccesses, SucceededStages*) filter
            superseded=0 while MONEY readers (SumStageRunCost, StageRunTotals,
            ListStageRuns) include everything. Plain tested CRUD; AuthStore
            adapts it to auth.Store. Holds the SCHEDULING truth.
            0002 adds server_jobs (serverjobs.go): the server jobs this Bridge
            claimed, keyed by the SERVER's job id, with attempt, state (running |
            done | failed | needs_attention | cancelled | lost), progress, result,
            error and reported. RecordClaim on a later attempt replaces the row;
            OpenServerJobs (running, or finished and not reported) is what a start
            picks up; a lost job needs no report (FinishServerJob marks it
            reported). The pairing lives in settings (pairing.*; the token is a
            secret).
            0004 (Phase 1b, workers.go): workers (name, token_hash UNIQUE, what it
            last said, last_seen_at written at most once a minute, segments /
            audio_seconds / decode_seconds); deleting a row revokes its token.
            0003 (Phase 1a, generatebooks.go): server_jobs.server_id (the server that
            handed the job out, from the pairing's server_id; '' on older rows) and,
            on a RUNNING row, error holds the park message of a parked job
            (SetServerJobReport writes progress and park message in one write, so a
            start resumes the job parked); books.contrib_mode ('' = configured; local |
            issue); generate_books (server_job_id PK -> book_id, ON DELETE SET NULL, +
            library_id, rel_path, created_at, ended_at, swept_at): a resumed job
            follows the same engine book, ended_at starts the transcript retention,
            swept_at stops the sweep looking again; GenerateBookFollowed (an open
            mapping) gates the supervisor's actions; idx_stage_runs_started for the
            month's sum; and deleted_stage_spend (started_at, cost_usd): DeleteBook
            copies a book's stage runs' budget-basis spend there IN THE SAME
            TRANSACTION before the cascade, so a regenerated book's spend still counts
            toward the month. budgetCostSQL is the one budget-basis expression (the
            provider's cost, else a complete estimate, else 0) every sum shares.
            GetBookBySourcePath finds an earlier run of the same book.
            Phase 1c: StageRunBudgetCost(runID) is one run's budget-basis spend (the
            description step reports its own); LatestGenerateBookAt(library, path)
            is the newest generate job's surviving engine book at a path, never
            the audio run beside an ebook run (sidecars.describe finds a moved
            book's run with it); NextStageAttempt
            (one past every recorded run: the scheduler's and the description step's
            run numbers) and LatestOkStageRun (a stage's newest success, or
            ErrNotFound).
            0005 (Phase 1d): books.goal ('' the whole pipeline, 'chapters' stops after
            chapter_detecting; opaque here like State, so the store names no goal:
            its SQL asks goal = '' or goal != ''). ContinueChapters(id, from, to, ...)
            (one statement: only a book with a goal, chapters being the one, still at
            from (the state the caller checked), done there or failed there (a cancel
            is a failure), whose transcripts the retention didn't sweep, else
            ErrNotContinuable) makes it a whole-run book at to, its status cleared (a
            done book's too: a cancel or a park landing as its last stage finished
            leaves one), with the new job's identity
            (title, authors, narrators,
            series and position, asin, isbn and their sources: the book may have been
            matched since), contrib_mode and work_id, and maps the new job to it in
            the same transaction. FinishedBooks leaves goal-chapters books out (no
            agent spend to average). UnsweptGenerateBooks counts a book's retention
            from its LAST job's end (a continued book has the chapters job's mapping
            and the generate job's).
            0006 (Phase 2b, accounts.go): accounts (provider PK: github | agent |
            agent/<kind>, the last keeping when that agent credential was set;
            login, signed_in_at, last_ok_at, refused_at, last_error), what the
            Accounts tab shows; the credentials are in the vault.
            Phase 3c-1: ContinueReadalong(id, from, to, goal, job) moves a stopped
            engine book (done, or failed, parked or paused: the caller holds it and
            checked no other job follows it) on at to with goal, its kind kept, its
            status cleared and the job mapped in one transaction; ErrNotContinuable
            when it isn't stopped at from, is an ebook run, or (going on past
            inspecting) was swept. Going on from inspecting ignores a sweep and clears
            swept_at on the book's mappings (the retention then counts from this job's
            end). GenerateBookSwept(bookID). AudioRunKey(path) is the source_path
            of the audio run beside an ebook run ("audio:" + the path: two engine books
            at one path, never a real path); Book.IsAudioRun, Book.AudioSource (the
            path the audio is read from: inspect and the markers file read it).
            ContinueChapters sets source_path to nb's when nb names one (a generate job
            taking on the audio run as the book's own). LatestOkStageRun(book, stage)
            reads superseded successes too (a history reader).
  state/    per-book pipeline state machine: table-driven states/lanes/transitions,
            CanStart/NextState guards, the audit fix-loop cap. Pure, no I/O. M6 added
            ParkCode (typed park reasons - M7 added contrib_unavailable, core_needed,
            core_pending; the reliability round added budget_exceeded; the ASR
            decode-timeout hotfix added asr_decode_timeout, and M9 added
            ebook_unreadable / ebook_no_chapters / ebook_chapters_not_confident,
            and Phase 1a agent_credential_refused, so 21 now; generate's own
            book_not_writable and library_not_writable are job parks, not ParkCodes),
            MainlineNext (the optimistic mainline
            successor the ETA engine walks - the table's Next ordering is load-bearing:
            conditional/loop target first, mainline continuation LAST),
            ParseSeriesPos + SeriesBreadthRanks (shared scheduler/ETA series ordering),
            and IsParkedWith (the one status+park-code predicate api/contrib share
            instead of hand-rolling it). The post-M9 round added the SourceIO
            Def column + ReadsSource (the stages that open the original library
            item; the scheduler's mechanical-lane serialization and the ETA
            simulation both consume it). Phase 3c-1: SeriesLockExempt(goal, state)
            keeps a read-along (goal readalong, or a whole run at readalong_aligning)
            out of its series' lock beside HoldsSeriesLock (scheduler and ETA).
            Phase 1a: HoldsSeriesLock(state, status)
            takes the status - a FAILED book (a cancel is a failure) holds nothing,
            since the job that wanted it ended and nothing will finish it (a later
            volume runs without its carryover, like a series' first book), while a
            parked or paused one still holds it. Phase 1c: Describing ("describing")
            names the Bridge's description step's stage runs, NOT a state (no table
            row: IsStage/IsAgent false, Order 0), declared here for the one spelling
            pipeline and config share; RoutesModel (an agent stage, or Describing)
            is the model maps' key rule. Phase 1d: ChapterDetecting
            ("chapter_detecting", order 5, mechanical, SourceIO: it stats the source
            and splits from it what the window FLACs can't give) after asr: ASR.Next =
            [ChapterDetecting, Sanitizing] on Outcome.Windowed, ChapterDetecting.Next
            = [Done, Sanitizing] on Outcome.Goal == GoalChapters (the book's goal,
            filled by the scheduler, never a stage). GoalChapters ("chapters") is
            books.goal's one value; PathNext(kind, goal, cur) is the remaining path's
            routing by it (the ETA follows it), so the goal routing lives here once.
            ParkChaptersNotDetected (chapters_not_detected, human only).
            Phase 3c-1: GoalReadalong ("readalong") and ReadalongAligning
            ("readalong_aligning", LaneASR, order after retranscribing, mechanical, Next
            [Done]; NOT SourceIO, nor is Retranscribing:
            TestReadsSourcePinsLibrarySourceStages wants every SourceIO stage in the
            mechanical lane, and the ASR lane's cap of 1 serialises them anyway). The
            goal readalong path is inspecting -> splitting -> asr -> sanitizing ->
            qa_sweep -> [retranscribing -> qa_sweep] -> readalong_aligning -> done:
            inspect never routes it to markers_normalizing, asr never to
            chapter_detecting, qa_sweep to retranscribing on Outcome.RetranscribeNeeded
            (the mechanical round) else readalong_aligning, qa_adjudicating (a continued
            whole run) to readalong_aligning. PathNext follows it; a test walks it for
            every kind and finds no agent state.
  eta/      the PURE ETA engine (no I/O, no clock): per-stage unit kinds
            (chapter/chunk/book) + seed rates from the historical extraction metrics,
            EWMA Observe (alpha 0.3), book ETA = rate x remaining units over the
            optimistic mainline (loops are not predicted - documented), queue ETA = a
            greedy three-lane event simulation (LaneCaps injected from the scheduler,
            series locks via state.HoldsSeriesLock, retranscribe-first then
            breadth-first-across-series ASR ordering). The post-M9 round added
            LaneCaps.MechanicalSourceIO: the simulation charges a mechanical stage
            with state.ReadsSource against that scarce sub-slot (continue, never
            break - a work-dir-only stage still overlaps a library read), so the
            queue ETA models the scheduler's source-IO serialization instead of
            predicting 2x the real inspect/split parallelism. Phase 1a: a parked or
            paused SERIES book that still holds its lock enters the simulation as
            `held` (it runs nothing, but a later volume's carryover stages wait behind
            it, as in the scheduler); a failed one is left out. Phase 1d:
            chapter_detecting's seed {book, 60 s}; the path follows state.PathNext
            with Book.Goal (books.goal): a chapters-only book goes asr ->
            chapter_detecting -> done; the mainline skips the stage like the other
            conditional ones.
            Phase 3c-1: readalong_aligning's seed {book, 180 s}; the goal readalong path
            follows state.PathNext.
  scheduler/ one wake-on-event goroutine over three lanes (ASR cap 1 / agent cap =
            config, series-locked / mechanical cap 2) over an injected Executor +
            _done/<stage>.json sentinels (the CONTENT truth) and crash reconcile.
            The ASR lane is ONE serial MLX slot shared by full-book and corrective
            transcription - the post-M9 round REMOVED the separate corrective slot
            (overlapping decodes pushed the second process into the OOM killer on
            unified-memory Macs); corrective work takes priority when the slot
            frees. The mechanical lane additionally serializes SOURCE-READING
            stages (inspect/split/extract, the state.Def SourceIO column read via
            state.ReadsSource) to sourceIOCapacity 1 - concurrent random access
            through one SMB share can wedge macOS's SMB client - while
            work-dir-only stages keep the second slot.
            Pause/resume/retry/cancel/delete + PurgeScratch (reclaim chapters/ when
            done/paused/failed). Publishes book.state/stage.progress/queue.stats.
            M2 runs the pipeline composite executor (real inspect/split, stubs beyond).
            M6: records EWMA rates from each stage's explicit StageResult.RateSample
            (units processed this run + productive seconds, measured by the stage AFTER
            setup and excluding agent rate-limit backoff - first-run tool/model
            downloads never contaminate learned rates; a nil sample records nothing),
            recomputes ETAs on every dispatch pass (idle-gated: no active books = no
            query/sim) and publishes deduped eta.update SSE (queue_seconds is null when
            idle); ETASnapshot/ETASeconds feed the books API. Progress reporting is
            display-only and resume-aware (a resumable stage's FIRST report is the
            already-complete baseline; skipped units never tick). M7: auto-purge -
            a book advancing to done reclaims its scratch in-line (the worker still
            holds the inflight slot; accounting runs under context.WithoutCancel so
            a shutdown-timed purge can't leave a stale gauge) and an async,
            WaitGroup-tracked startup GC purges done-with-scratch books after
            Reconcile (per-book reservation so a concurrent Delete sees busy); both
            gated by the autoPurge constructor param (from contribution.auto_purge).
            Reliability round: Retry's core was extracted into readmit(), shared by
            manual Retry, the contrib poller, and the new autoReadmitDue pass (each
            dispatch tick re-admits books parked agent_unavailable/agent_rate_limited
            whose retry_at is due, publishing a durable stage.note). readmit supersedes
            the CURRENT stage's successes ONLY for the round-cap park codes
            (qa_no_converge, fix_loop_exhausted - the latter also superseding fixing +
            wiping the audit trajectory files) so an availability park never destroys a
            loop's round history; parks with a plain code just clear status.
            ParkError.Cause carries the error behind a park (contrib_unavailable's
            credential error) for a caller that words the park (the share job); it is
            deliberately no Unwrap, since runStage's errors.Is(err, context.Canceled)
            would see through a park and a cancel sets no status. Timed parks
            come from ParkError.RetryAfter (ParkWithCodeAfter): the rate-limit park uses
            the parsed reset + 2min (agent.ParseResetTime - epoch accepted only within
            (now, now+48h]; clock form host-local by documented assumption) floored at
            now+5min, else a 30min fallback; the mid-run transient NotAvailable park
            uses now+10min; the PREFLIGHT no-backend park carries NO retry_at (human
            only, so an unconfigured daemon parks once instead of churning). retry_at
            rides bookView and the book.state SSE frame (the web patch clears it when
            absent); pre-migration parks (retry_at '') never auto-readmit.
            Phase 1a: autoReadmitCodes adds agent_credential_refused (looked at again
            every 30 min in case the credential was replaced) and budget_exceeded
            (the monthly park's retry_at is the 1st; the per-book park has none, so it
            waits for a raised cap and the restart that applies it) - ONLY a park
            carrying a retry_at is ever re-admitted by the timed self-resume. Bridge
            2b: restartReadmitCodes (the parks a daemon start re-admits once) add
            budget_exceeded, since a cap
            raised in config.yaml or the environment applies at the start, unless the
            executor's budget preflight gives the park back as it is (budgetParkHolds:
            the optional budgetChecker, pipeline Executor.BudgetPark, parks it with the
            same reason), which stays: no stage re-run, no job reported running in
            between. A book a changed cap still leaves over its budget parks again in
            runAgent's budget preflight, before any spend (pipeline
            TestRestartReadmitsABudgetParkThatParksAgainBeforeSpending). IsCancelled(b) tells Cancel's failure from a stage's by its
            reason (cancelledReason "cancelled by user"; the status enum has no
            cancelled state). runStage closes a run on ANY error once the worker ctx
            ended (a killed ffmpeg is "signal: killed", not ctx.Err()): whoever
            cancelled set the status already. lockHolders/seriesLockOwners pass the
            status to state.HoldsSeriesLock (a failed predecessor frees its series).
            Phase 1b: ReadmitParked(msg, code) re-admits every book parked with code
            (store.ListBooksParkedWith) through readmit: the Bridge's asr_worker_waiting.
            SetAgentCapacity(n) changes the agent lane's book capacity (atomic,
            clamped to 1, notifies on a change) while it runs: the Bridge follows the
            agent container's slots.
            Phase 1c: reconcileBook skips a succeeded stage the state machine doesn't
            know (the description step's "describing": no sentinel, state.Order 0,
            so it would rewind a contributing-parked book to a state nothing runs).
            Hold(id) (release, ok) is PurgeScratch's reservation for work outside the
            stages (the Bridge's description step, its transcript purges): no stage
            dispatched, Delete/PurgeScratch busy, the re-admits and startup GC skip
            it, a supervisor terminate finds no worker; ok false when busy already;
            release is idempotent. runStage numbers a run store.NextStageAttempt.
            Phase 1d: StageResult.Windowed (asr) rides the sentinel into
            Outcome; advance fills Outcome.Goal from books.goal, and the ETA input
            carries it (eta.Book.Goal) as it is. A goal-chapters book reaching done is auto-purged like
            any other (its chapter FLACs go; a continue re-splits).
            Perf quick wins (2026-10): the dispatch pass reads dispatchBooks, the
            unfinished books in full (store.ListUnfinishedBooks: all but state done
            with no status) and, apart from them, the finished books of a series
            that still has an unfinished volume as []state.SeriesQueueItem
            (ListFinishedSeriesBooks, its series trimmed of exactly what
            strings.TrimSpace trims, bridge#31), since a finished volume still
            counts in the series breadth ranks (asrBreadthRanks, eta.QueueETA's
            finished); the ETA progress read is ListUnfinishedProgress.
            The two reads are two statements, so dropFinished keeps a book that
            reached done between them once, as its finished identity.
            A book that leaves done is read in full at the next pass. Reconcile,
            the startup GC and the restart re-admit still read every book.
  supervisor/ the health-tick babysitter (config supervisor.*): classifies incidents
            over book/stage-run snapshots (deterministic classifiers + optional
            model-assisted lane), decides bounded recovery actions, and applies them
            through the scheduler seam. Three invariants are load-bearing (all fixed
            after live park/readmit ping-pongs): (1) the parked-recovery
            FINGERPRINT is stationary - derived from park code + stage + the latest
            non-superseded stage-run error, never from book.Error, which every
            park_escalate rewrites (state.SupervisorMessagePrefix prose is stripped);
            (2) a durable cross-Kind AUTO-RECOVERY CAP: at most MaxAttempts automatic
            retry/readmit/supersede_rerun/requeue/fallback_backend actions per book
            per rolling 24h (store.CountAutoRecoveriesSince; terminate_requeue is
            deliberately excluded as restart hygiene), enforced on BOTH the
            deterministic and the model-assisted lanes - past the cap the decision
            converts to park_escalate + approval_required (human review); (3) the
            AttemptGrowthFactor checks compare against a stage's HIGH-WATER MARK over
            all its successful attempts (priorSuccessBaseline - max duration/tokens/
            cost, cost only across attempts of the same comparableCost kind), never
            whichever attempt happened to finish last, AND the duration check stays
            silent below the absolute minGrowthElapsed (20min) floor. An agent stage's
            duration tracks the work it was handed - a fixing round carrying three
            blockers legitimately runs several times longer than one carrying a small
            edit - so a loop stage's recent short round is an arbitrary sample, not a
            budget. Without both guards a healthy 3m13s fixing run was stopped
            (stop_budget) against a 62s predecessor at 1.8% of MaxStageDuration, and
            the resulting park then spent the book's whole auto-recovery budget.
            Post-M9 round: a dead recorded process is classified missing only past
            processExitGrace (90s - it is measured against a stage-run heartbeat
            refreshed on a 60s cadence, so the grace must exceed that cadence plus
            the runner's 5s pipe-wait; a 15s draft failed for ~75% of child exits)
            from the freshest heartbeat/start - the child's exit and the runner's
            durable process_active clear are sampled independently, and the gap
            between them read as a disappeared worker on a successfully completing
            stage; and collectArtifactStatuses trusts an OPEN stage run over the
            stale book snapshot (the scheduler can advance the state between the
            two reads), so an intentionally absent sentinel for an in-flight rerun
            is not reported broken. The tick reads only the unfinished books
            (ListUnfinishedBooks, StageRunsUnfinished, ListUnfinishedProgress; perf
            quick wins 2026-10): a finished book (done, no status) can raise only a
            protected artifact incident no action may apply to, and reading it cost a
            sentinel read and artifact stats per finished stage every 30 s, forever;
            one that leaves done is read from the next tick, and a done book with a
            status still is. collectArtifactStatuses keeps its done+splitting skip
            for the confirmation's fresh read. The batch reads predate
            the pass's own per-book filesystem work, so that classification is only a
            CHEAP FILTER: every incident is confirmed by re-reading just that book
            (GetBook + ListStageRuns), rebuilding its snapshot through the shared
            buildSnapshot, and re-CLASSIFYING before any recovery is recorded. One
            invariant for every incident kind, so protection rules stay owned solely
            by Classify and the acting layer holds no per-kind freshness patch; a
            book deleted mid-pass is skipped, never a failed tick. That re-read makes
            a book's runs NEWER than the tick's single runtime occupancy sample, so
            passReads carries the moment that sample was stamped and Classify ignores
            an open run admitted no earlier than it (admittedAfterRuntimeSample, an
            INCLUSIVE boundary - the wall clock is microsecond-granular): the
            scheduler registers a worker in-flight BEFORE it opens the run row, so a
            book dispatched mid-pass would otherwise be terminate_requeued as an
            orphan. The occupancy/invocation/progress figures themselves stay as of
            the prologue by design; they are what the sample timestamp qualifies.
            classifyFailures reads only a failed or parked book's last failed run:
            every scheduler failure path sets one of the two, so any other book was
            re-admitted since (perhaps then paused) and waits for its next attempt.
            A cancelled book (scheduler.IsCancelled) is skipped as well: Cancel fails
            a book without a run of its own, so one cancelled while it waited still
            has that answered failure as its last run.
            Spike 1 (2026-10-16): after a refused credential was replaced, the books
            re-admitted but waiting for an agent slot were escalated from their old
            401 (authentication_failure -> park_escalate) and needed a human Retry.
            On the Bridge (app/engine.go) only the deterministic lane runs (config
            forces model-assisted and backend failover off), automatic actions are on
            by default, and the Apply hook (supervisorApply) refuses three things: a
            fallback_backend (there is none), a stop_budget on an ASR-lane stage (asr
            and retranscribing bound themselves chapter by chapter; a whole-stage
            time limit would stop a long CPU transcription again and again), and any
            action on a book no open sidecars.generate job follows
            (store.GenerateBookFollowed: it would run, and spend, for nobody).
            Phase 1c: engineRuns drops the runs of a stage outside the state machine
            (the description step's "describing") at the run reads (engineRunsAll for
            the tick, engineRunsOf for the confirmation and Ask; the input slice back
            as it is when nothing is dropped): an open one has no scheduler worker
            and a done one no sentinel, so they read as a missing process or a
            broken artifact on a finished book.
            Phase 1d: chapters_not_detected is decided like markers_not_confident (a
            person's call: park_escalate for approval, never an automatic retry).
            bridge#24: classifyError reads 401, 429 and 503 only as status codes an
            error states (statusPattern: after status, status code, HTTP[/1.1],
            error[_code] or code, `"status":429` included), never bare digits, which
            work-dir names, file names and counts carry; its phrases still come
            first (agent's own "refused the credential" among them). handleIncident
            records no row for an observe decision left for approval on a
            SELF-RESUMING park (Incident.SelfResuming: the book carries a retry_at,
            so the scheduler's timer re-admits it) when the book's newest supervisor
            run is that same observation of the same incident family
            (unchangedObservation: its incidentKey under that run's stage run; kind,
            book, stage and fingerprint, any stage run), so a park that resumes
            itself every 30 minutes and parks again the same way (a refused
            credential) is recorded once, not once per resume. A park only a person
            or a restart lifts (no retry_at) is recorded each time it comes back: a
            Retry records no supervisor run, and the park after it is news.
  metaops/  meta.audiosilo.app client (coverage/lookup, capped 1h TTL caches,
            graceful degrade) + the PathAllowed/AllowedPath containment checks
            (Phase 1a passes the mapped mounts as the roots). glossary.go adds
            SeriesGlossary (work -> its series -> the OTHER volumes' contributed
            character names/aliases, <= 12 siblings, own TTL caches), the canonical
            spelling evidence the spelling stage checks a transcript against; it
            EXCLUDES the book's own sidecar (a book checked against itself makes its
            mistakes self-attesting) and drops descriptive placeholder labels ("The
            missing young gnoll child") since those are prose, not spellings. Every
            no-data path (disabled client, no series, outage, uncontributed
            siblings) returns an empty glossary and a NIL error - a metadata outage
            must never park a book. "Its series" for works/{id} is readingSeries'
            choice in coverage.go (the first membership with no `ordering_of`, else
            the first), read by Coverage.Series, the predecessor search and the
            glossary cut; a search card's series arrives already chosen by metaserve
            under the same rule. Coverage resolves
            asin -> isbn -> a fuzzy title-search fallback scored by
            audiosilo-server's pure-stdlib pkg/match (Coverage carries matched_by
            "asin"|"isbn"|"search"|"manual" + work_title provenance). The search
            fallback walks the ladder.go RETRIEVAL LADDER: over a real 1147-book
            library the dominant failure was retrieval, not scoring - a decorated
            shelf title ("Supermage : Rise To Omniscience, Book 1", "Halo:
            Primordium (Unabridged)", "Artemis Fowl 4 - The Opal Deception")
            retrieves NOTHING even when the work is indexed under a clean title. The
            ladder is 9 ordered, de-duplicated query shapes (raw title, punctuation-
            normalized, CleanTitle, pre-subtitle, post-separator tail, bare trailing
            volume stripped, title+author, folder leaf, parent folder), the raw title
            FIRST and ALWAYS (the minimum query length is a floor on the DERIVED
            rungs only - a book called "It" must still search something) and an early
            exit at the first rung match.Best accepts, so a book that already
            resolved still costs one request. Each rung carries its own matchTitle,
            and the de-duplication key is the WHOLE step: rung 5 and rung 8 can send
            the same query scored against different titles, and dropping the second
            deletes the leaf-scored variant that is the point of the rung. The FOLDER
            LEAF rung queries the tail behind a shelf prefix ("RO07 - Sandqueen") or
            else the whole leaf name, and is SCORED against that leaf only when the
            tag title does not contradict it (empty, spells the leaf out, or is a
            bare shortcode with no real word - leafScorable); a mis-shelved folder
            naming another book by the same author otherwise mints a confident match,
            and a search verdict becomes books.work_id, which the contributing stage
            attaches sidecars to. A wider query also retrieves the right SERIES and
            the wrong volume - titleTokens drops pure numbers, so book 1's card is a
            perfect title match for "The Wandering Inn - 7" - so an accept is VETOED
            when the card's series position disagrees with the volume the book claims
            (its series-position tag, else a trailing number or "Book N" marker in the
            title), or when the accepting rung dropped a number the scored title
            carried and the card states no position at all; the walk then continues.
            NARRATOR EVIDENCE is not a second pass: the book's narrators ride the
            match.Query and each card's ride its match.Book, and pkg/match's person
            gate accepts author<->author, query-author<->card-narrator and
            query-narrator<->card-author in ONE Best call (a shelf routinely credits
            the narrator as the author). Coverage.ApplyContributed is
            the read-time repair of a FROZEN verdict: it patches a KNOWN verdict
            additively with what this Bridge has since contributed, and ONLY when the
            book's work_id agrees with the verdict's. (sidecars' folder-scan job
            manager, candidate overrides and library sightings were not imported.)
            Deps: stdlib
            HTTP + the meta module + audiosilo-server/pkg/match.
  events/   SSE hub: Publish -> monotonic-id fan-out, ring-buffer replay from
            Last-Event-ID, ephemeral heartbeats, slow-subscriber eviction, optional
            durable-sink persister (feeds store.events)
  link/     the client of the server's Bridge routes (pairing exchange, heartbeat,
            long-poll claim narrowed by ?kind= to the kinds with room, job updates,
            a book's files, targeted rescan). Phase 2c-1: Library.Kind (missing
            means books), Marker (no_marker -> ErrNoMarker), RescanPodcast
            (path and episode count). Every request sends X-AudioSilo-Client:
            "AudioSilo Bridge/<version> (<os>)", inside the server's auth.ParseClient
            grammar (a test holds the regex). Wire types mirror the server's
            internal/bridge BY HAND (no codegen): a change there is a change here,
            with a test on both sides. Answers map to ErrUnauthorized (401: the token
            was revoked), ErrNotHeld (409 not_held/finished), ErrNoHeartbeat,
            ErrNoBook, ErrNotIndexable (a rescan's 404 not_indexable), else *APIError
            (an HTML proxy page never becomes a message). Phase 1a: Paired carries
            the server's server_id (the job runner's server identity); the book
            lookup's answer carries a `book` block (BookInfo: title, authors and
            narrators as credits, series, series_index, asin, isbn, duration, and the
            work_id of the server's metadata cache) when a path was asked for, never
            on the sample-book answer; the heartbeat's Status carries
            estimates.sidecars (SidecarEstimates: asr_seconds_per_audio_hour,
            agent_usd_per_audio_hour / agent_usd_per_ebook (null until a finished book
            gives one), samples, agent_backend, agent_ready, subscription,
            monthly_budget_usd, month_spent_usd and can_contribute; omitted until the
            engine has started); RescanAttachments(library, path) is POST
            /bridge/rescan with mode "attachments" (the book's .audiosilo/ only, never
            its audio), answering which of characters/recaps/description (Phase 1c;
            false from an older server)/chapters (Phase 1d, likewise)/bridge are
            indexed; and
            a job update's answer carries the job's current path, which the server
            moves with its book.
            Phase 2b: Status.Setup (the setup checklist, plan D1) is sent ONLY
            after a reply's StatusReply.Accepts lists "setup" (Accepting; an older
            server would refuse an unknown field); app kicks a beat when it first
            appears, resends once without it on a 400, then ignores accepts for an
            hour, and resets that on every pair or unpair.
            Phase 2c-2a mirrors PodcastRetention and CheckPodcastRetention.
            Phase 2c-2b: Heartbeat records claim_min_priority per Client;
            SupportsClaimMinPriority and ClaimMinPriority expose the negotiated
            int32 filter. Ordinary Claim never sends it. Missing accepts disables
            the option, unknown reply keys are tolerated, and a new Client starts
            unnegotiated. WithdrawClaimMinPriority clears it after an ignored
            filter or pairing change; a later heartbeat must advertise it again.
            Deletion snapshots require all protocol fields, tolerate new keys,
            and expose no listener identities.
            Phase 3c-1: BookFile.Duration (the server's indexed seconds; a single-file
            book's is the book's), BookInfo.Ebook {path, size} (the server's pick of the
            book's ebook, omitted when none) and Attachments.Readalong (the rescan read
            readalong.json).
  mapping/  where the Bridge sees each library, CHECKED (decision 13): Check stats the
            server's sample book's files through the mount (SafeJoin: no absolute,
            "..", NUL; a backslash is a name character except on Windows) and
            compares SIZES (the only thing that catches a stale copy, spike 3), and
            only then probes writability with an exclusive dot-named probe file (a
            crash leaves nothing the server's scan walks). A library with no indexed
            book is NEVER mapped: with nothing to compare, a wrong mount would look
            right and a later write job would land in the wrong folder. Podcast
            libraries use CheckPodcast instead: a root .audiosilo/library.json
            marker (version, matching library id, random nonce) is read back by
            the server. Symlinked marker folders/files and invalid markers are
            refused; a newly created marker is removed after a failed check. A
            marker naming another library is refused while the server lists that
            library, and replaced once it doesn't (the library was deleted, or
            the folder added again under a new id). Missing marker/nonce mismatch is definitive;
            network/401/503/unexpected errors say verification failed and keep
            existing markers. Only artifacts created by that check are removed.
            Registry dispatches by the heartbeat's kind and holds the
            server's libraries from the last heartbeat and each check; Check(ids...)
            checks some or all, one library at a time, and never overlaps itself: a
            check asked for while one runs is done right after it by that call, with
            the NEWEST caller's link (after a re-pair the old one is revoked), so a
            mapping edited mid-check is never missed; that queued Check returns false
            (its caller beats for nothing). Due is false while a check runs (it
            refreshes what is due): a heartbeat during a long check (spun-down disks)
            asking again and beating again was a tight loop of heartbeats. A mapping
            edit re-checks only its library (each check stats files and writes a
            probe). Status(id) is one library's last check (a write job needs it
            mapped AND writable at the mount it uses now).
  jobs/     the generic runner. Phase 2c-2b: dispatch.go owns ordinary and
            reserved dispatchers. InteractiveHandler opts a kind into the reserved
            pool (1), claimed only with negotiated min_priority=100. Download is
            1 with negotiation, 2 without it; light=2 and pipeline=25 stay fixed.
            The reserve never borrows background work; ordinary workers accept
            every priority. Each outstanding claim reserves admission in all
            requested lanes and transfers its selected slot atomically to its run.
            Eligibility changes (kinds, pairing, capability or capacity) cancel
            and join a long poll before reissue; unchanged wakes leave it alone.
            A job racing cancellation runs under the original link and admission;
            one racing the runner's stop is still recorded, so the next start
            resumes it. An answer outside the requested admission is owned under
            its original claim and waits for its kind's ordinary lane, never the
            reserve. An ignored reserved filter immediately withdraws negotiation,
            restores legacy capacities and wakes dispatchers to cancel affected
            claims. The reserved dispatcher waits for a heartbeat to re-advertise
            support, and while a job it took outside admission still waits it
            claims nothing (a server re-advertising a filter it ignores holds one
            waiter, not its queue). An ordinary answer outside the requested kinds
            backs off like a failed claim. Running promotions keep their
            pool. Recovery uses ordinary admission because old rows lack priority.
            Both recovery and out-of-admission jobs renew leases while waiting
            for room, queue behind jobs already waiting, and take
            precedence over fresh claims in that lane. Live parks hold
            admission; terminal parks and releases wake the dispatcher.
            Phase 2c-2a adds the strict Reporter.ConfirmLease publication check
            (network errors deny commit). Phase 2c-1: optional ParamSealer.SealParams runs
            BEFORE RecordClaim. A failure records sanitized empty params, fails
            closed without Run, and calls Abandon for any secret already sealed;
            a *SealFailure is reported as its code and message, any other error
            only as a generic write_failed (its text may hold the secret). A claim
            the store can't record is abandoned (the server hands it out again)
            only when its handler seals; other kinds run anyway, as before.
            Handlers declare a kind and a lane (lanes: light=2,
            and Phase 1a's pipeline=25 for the jobs that follow an engine book: the
            engine's scheduler does the throttling, so such a job sits in its lane
            while its book is queued or parked, and the lane only bounds how many
            are followed); the claim loop asks only for kinds whose lane has room, and
            waits (no claim) while unpaired or full. A running job reports at least
            every Keepalive (DefaultKeepalive 30 s, 10 s before the 2026-10 perf
            quick wins: every report is a server write that empties its generation
            caches; four still fit in the lease), renewing the server's 2-minute
            lease, so a cancel is heard within about 32 s (the keepalive is checked
            on the 2 s ProgressEvery tick). After a report that didn't reach the
            server the next goes after ReportRetry (DefaultReportRetry 10 s) instead,
            until one gets through, then Keepalive again: at 30 s alone one report
            through and three failed would lapse the lease. Only an answerless
            failure, a 5xx, 408 or 429 counts (unreachable); any other 4xx would be
            refused again however soon, so it waits the Keepalive. The first failed
            report warns, the rest log at debug, and the first one through after
            them logs one Info (bridge#31); a ConfirmLease or Refresh that gets
            through clears failing too (under the Reporter's lock: they run on
            the handler's goroutine). Reporter.Refresh (v0.5.0, BRIDGE-01) reports
            the job now, parked as it is, for another job it stands in the way
            of: nil while the server holds it, ErrStopped when the answer (a
            cancel, a lost claim) ended it. A reply
            with cancel_requested ends its context (errCancelRequested -> reported
            cancelled), a 409 ends it as lost (nothing more sent), a 401 is handed to
            the app WITH the link that got it (onUnauthorized(srv): a job of the
            previous pairing still reporting with its revoked token must not refuse
            a new pairing). A stopping Bridge (base context ended) leaves the row
            RUNNING, so the next start resumes it under the same claim; a job a 401
            stopped is left running too and resumed under its claim as soon as a
            link to its server (the same address) works again, a refusal ending or
            a re-pair (resumeStranded; another server's link records it lost, its
            ids being the old server's). If the lease ran out meanwhile, the first
            report gets 409 and the row is lost, and the claim loop takes the job
            again as the next attempt (the kill test in the handoff log). release
            frees only its own run (a later claim of the job may run under its id).
            Job.Resumed (Phase 1c) tells a handler the runner resumed the job under
            its claim (a restart's recovery, resumeStranded) rather than claimed it
            now, so an earlier run of it may have spent already.
            Recovery runs once the link is up: a kind no longer run is failed.
            Nothing is sent that the server refuses for good: a result that isn't a
            JSON object of at most 64 KiB fails the job (errUnreportable) and
            progress text is clipped, since a refused report was retried until the
            lease ran out and the job ran again. The final report carries the last
            progress (a throttled step may never have been sent) and is retried
            every 30 s until the server takes it or says it moved on (only while one
            is owed: a flag, not a poll); old
            finished jobs are pruned hourly. Progress only RECORDS: the job's
            reporting goroutine sends (a new step at once, other changes within
            ProgressEvery, a keepalive otherwise), so a handler never waits on the
            network. Cancel is the page's own cancel. LocalReporter is a
            send-nothing Reporter for handler tests.
            Phase 1a: PARK/UNPARK. Reporter.Park(code, msg) reports the job
            needs_attention with msg as its error while it KEEPS RUNNING here (the
            server drops a parked job's lease, as needs_attention has none, and renews
            it once it reports running; the job goes on reporting under its claim, so
            a server cancel still ends it; a Bridge gone for good leaves the job
            needs_attention until an admin cancels it);
            Unpark reports it running again; both are sent at once. Every report
            carries the error, "" when unparked, which clears a park's message (even
            one a restart interrupted); the park message is stored with the progress
            (store.SetServerJobReport) and a resumed job starts parked with it, so it
            never reports running between two parks. The same progress again changes
            nothing (DeepEqual), so a handler may report on every poll. Progress.Extra
            is a kind's own fields (sidecars.generate's stage, eta_seconds, cost_usd,
            source, park), marshalled FLAT beside step/message/done/total (those four
            win over an Extra key of the same name); an object over the server's 16
            KiB is sent without Extra rather than refused for good. Reporter.Path()
            is the job's path as the server's last answer gave it ("" until one did):
            the server moves an open job with its book. SERVER IDENTITY:
            Options.ServerID (the pairing's server_id) is recorded on every claim, and
            a recorded job of ANOTHER server (unknown on either side counts as the
            same) is dropped at recovery (a running one lost, an unreported end never
            sent) and never retried by retryReports. ABANDONER: a handler whose jobs
            hold work beyond Run (generate's engine book) implements Abandon(ctx,
            job), which the runner calls when it ends a job WITHOUT running the
            handler (a resumed job's first report answered with a cancel or a lost
            claim, another server's job dropped), so that work is released as the
            handler's own end would. Resumes(ctx) tells a handler whether its ended
            context will run again under the claim (the Bridge stopping, a refused
            token) or ended for good (errCancelRequested, errLost).
  verify/   bridge.verify: params {hold_seconds 0-600}; resolves the library from the
            registry, asks the server for the book's files (path "" = the sample
            book), CompareFiles through the mount, probes the BOOK's folder for
            writing (spike 3: a root probe passed while a 0555 book folder refused),
            then holds, reporting each second. A mismatch is a failure WITH a result.
            Result {library_id, mount_path, path, files[{path,size,expected_size,ok}],
            writable, writable_error, elapsed_ms}: the server's console renders it.
  netguard/ Stream adds a one-attempt audio GET over the same guard and transport,
            identity encoding, caller-owned body, inactivity bound and cross-origin
            credential stripping shared with buffered feed/artwork GETs, including
            later hops in a redirect chain. The handler owns the combined
            three-attempt retry/resume budget; ResumeAction validates exact strong identity.
            Phase 2c-1 feed/artwork client: explicit DNS resolution and a socket
            Control check of the exact IP, public addresses or configured CIDRs
            only (including NAT64 embedded IPv4 checks), no environment proxy.
            Each resolved address is dialed in turn with its share of the 10 s
            connect bound (at least 2 s), so one that drops packets can't leave a
            working one untried. GET, gzip only, 10 redirects (Permanent only when
            one was followed and all were 301/308), connect/header/body-stall
            bounds and decoded size caps; two jittered retries for transient
            failures, respecting Retry-After. Bad gzip data (empty, header,
            checksum, stream) fails at once as not_a_feed; a read the connection
            broke is retried. Errors never contain remote text or URLs.
  podcast/  pure feed.json v1 types, rooted regular-file IO with capped atomic
            writes, URL normalization/hash/privacy/redaction, NFC show names,
            stable episode paths, identity merge and adaptive polling. Spike 5
            selected gofeed Extensions, with prefix declarations resolved by URI
            (a legacy-charset feed decoded as gofeed decodes it); x/net/html
            produces plain text, keeping what only looks like a tag (a name that
            is no HTML element, "Vec<T>", or a tag left open at the end, "x<y").
            itunes:duration is S, M:S or H:M:S, a fraction of its seconds dropped.
            Merge: every item first claims the kept episode of its own identity
            or id, then the items left try the weak matches (enclosure, title and
            date), so a weak match never takes an episode whose own item is still
            listed. EnclosureBase drops the query only when the path names an
            audio file (a download link's query tells episodes apart). Over the
            20,000-episode cap the removed, never-fetched episodes gone longest
            are dropped first. Private feeds scrub URLs before each write. Golden
            fixtures and URL vectors are shared with the server.
            Phase 2c-2a adds Fetch validation/generation transitions, bounded
            chronological ID3 CHAP attachments and pure install/delete journals.
            Journals are serving barriers. Delete recovery aborts when audio has not
            moved to owned trash; only a moved deletion is completed without a fresh
            aggregate. Exclusive hard-link then unlink installs audio without
            overwriting a concurrent foreign file; a mount without
            hard links (SMB/CIFS, some NAS/FUSE shares) falls back to a no-replace
            rename (renameat2 RENAME_NOREPLACE on Linux, else an absence check
            under the show and path locks; install*.go). Committed delete
            manifests identify trash for cleanup after rescan or after 24 h.
            Invalid Fetch keeps its episode and grants no file authority. Shared
            phase2c2 fixtures are copied byte-for-byte, checked by manifest hashes.
  podcastjob/ podcast.fetch opts into the interactive reserve (2c-2b), with
            download as its ordinary lane. Subscribe, poll and retain do not opt in.
            podcast.subscribe and podcast.poll (light lane): require the
            heartbeat's podcasts kind and the checked writable mount at each
            write. Per-library subscribe and per-show write locks, hash dedupe,
            cache rebuild, artwork sniffing, permanent moves and targeted rescan.
            Subscribe implements ParamSealer and Abandoner: job credentials are
            sealed before persistence, resumed from the vault, resealed under
            the show id on success, and removed at every terminal job end.
            A 304 never rewrites feed.json or artwork. Both handlers preserve retained fetch metadata and recover show journals before
            writes. Fetch downloads outside the show lock, rehashes proven partials,
            seals validators, probes local descriptors and confirms its lease before
            publication. Per-path process and OS file locks prevent overlap across
            reclaimed jobs. Retain hashes local candidates and fails closed on an
            unavailable, stale or protected aggregate. Snapshot freshness lasts five
            seconds from request dispatch on the Bridge's monotonic clock;
            checked_at stays on the wire and does not time freshness. Old cleanup
            uses only owned checkpoint/trash records, checked mappings and no live matching job. A job claimed
            before its library's mapping check (a start or re-pair) waits for it
            (CheckPoll). A subscribe's 3-minute ceiling starts once it holds the
            library lock; artwork gets at most a minute and half the time left; a
            follow that fails before feed.json takes back the folder it made. A
            poll asks for a rescan of any show the server hasn't indexed since
            this start (one a failed job left owed), 304 or not. An
            itunes:new-feed-url never names a URL the show moved away from nor
            takes https to http. A cover deleted by hand is fetched again, subject to durable artwork backoff.
            No failure-only feed timestamp churn and no artwork work on 304.
  generate/ sidecars.generate (Phase 1a): turns a server job into an ENGINE BOOK, follows
            it to the end reporting its stage, ETA, spend and parks, then writes the
            validated characters and recaps into the book's .audiosilo/ and asks the
            server to re-read them. Pipeline lane: the engine's scheduler does the
            throttling. Params {source auto|audio|ebook, contribute local (default) |
            issue, work_id <= 200 bytes, no control characters} are decoded leniently
            (a field a later server adds is ignored) but checked strictly. A NEW job
            with contribute=issue fails BEFORE ANY SPEND when no GitHub token resolves
            (Options.ContribCredential; the contribution is the last stage), worded
            apart when the sign-in only couldn't be refreshed (errCantContributeUnrefreshed);
            so does a resumed one that makes a new engine book after all (its id
            reused from another job's mapping, its book deleted or swept: create
            checks, cantContribute). prepare: the library from the
            registry; waitMapped needs its check mapped AND writable at the mount the
            Bridge uses NOW (a new job fails otherwise; a resumed one parks
            library_not_writable and waits, its engine book going on); fetchBook waits
            out an answer that may change (unreachable, 5xx) with a backoff to 1 min
            and fails only on a definite one (no_book, another 4xx), so a restart
            while the server is down doesn't end a long run; CompareFiles through the
            mount must match; locate SafeJoins the path (a folder book, or a
            single-file book whose attachments get a "<stem>." prefix); waitWritable
            probes the BOOK's own folder (spike 3), parking book_not_writable and
            checking again every minute or on Retry. create: chooseSource (auto takes
            exactly one readable .epub directly in the book's folder, dot files
            skipped, or "<stem>.epub" beside a single file; readable = its OPF parses,
            so a DRM-wrapped file falls back to the audio with a note; several .epub
            files fail an ebook job and make auto read the audio), both paths
            contained by metaops.AllowedPath under the mount; an earlier engine book at
            the same source_path is deleted first unless another open job follows it
            (refused: cancel it first) or a stage of it runs (scheduler.ErrBusy: try
            again in a minute); the new book takes its identity from the server's book
            block (identity_sources "server"), its work id from params else the
            server's metadata cache, kind ebook with an epub, contrib_mode from the
            params; generate_books maps the job to it. RESUME: a mapping is the job's
            own only for the same library AND path (the server's ids repeat across
            servers and reinstalls, and following another book's run would write its
            sidecars into this one); otherwise the old book is stopped and a new one
            made. attach re-admits a book this job's own cancel stopped (engine
            Retry), unless its transcripts were swept meanwhile, which starts over,
            as a deleted book does. follow polls 3 s while a stage runs, 30 s while
            queued/paused/parked, at once on Retry; describe() maps the book to the
            job's progress: the stage as step ("waiting" while queued), a plain-words
            label (stageLabels), the stage's done/total, and Extra {stage, source,
            cost_usd (budget basis, rounded to the cent), eta_seconds (not while parked
            or paused), park {code, hint, retry_at}}; an engine park is
            Reporter.Park(code, error), its end Unpark. A book parked AT contributing
            (core_needed and co.) still gets its local files written. FINISH: the
            sidecars (pipeline.FinalSidecarsPaths) must decode into the meta model and
            fit the server's 4 MiB cap; .audiosilo/ is made 0755 (refused when a link
            or not a folder), and characters.json, recaps.json and bridge.json
            (provenance: generator, version, kind, job_id, generated_at, source,
            work_id, inputs [{path, size, mod_time}] relative to the library) are
            written 0644 through fsutil.WriteFileAtomic, ONLY those names, once per
            content hash, at the path the book is at NOW (Reporter.Path: the server
            moves the job with its book; the move is recorded in generate_books). The
            mapping is checked again before each write; a failed write on a done book
            parks book_not_writable and tries again every minute or on Retry (the
            sidecars stay in the work dir, never lost to a failed job). Then
            RescanAttachments, 3 tries, a failure only logged (the server's next scan
            reads them). Result {library_id, path, source, files, characters, recaps,
            cost_usd, work_id, contributed {characters|recaps: issue url}}. END: unless
            jobs.Resumes (the Bridge stopping, a refused token), the job's end is
            recorded (ended_at), a book that didn't finish is cancelled (stopBook
            leaves a done or failed one alone: a cancel would overwrite its error),
            and retention 0 purges its transcripts now; a later attempt of the same
            job running here (Attempt higher) is left alone. Abandon (jobs.Abandoner)
            does the same for a job the runner ended without running it, waiting for
            the engine to start when it hasn't, and waitEngine for a run cancelled
            (or taken back) while it waits for the engine, which never reaches end(). Retry(job): the engine's Retry for a
            parked engine book, else a poke for a job waiting in its own park
            (ErrNotParked, store.ErrNotFound otherwise). ParkHint words every park code
            for the console and the page (a fresh start says to cancel this job first:
            the server keeps one open job per book). The budget_exceeded hint and the
            pipeline's budget park reasons (budgetExceededMsg, monthlyBudgetExceededMsg)
            read the same for every job kind and stage: a monthly cap resumes on the
            1st or at once when raised on the Settings tab; a cap set in config.yaml
            or the environment is raised there and the Bridge restarted, then the job
            carries on with no Retry (a start re-admits an engine book's budget park,
            scheduler restartReadmitCodes, and runs a describe job again). The reasons
            speak of "a job paused at the cap": a generate job's description and the
            contributing stage's only log the description step's. Estimates (the heartbeat's): the
            ASR rate (seconds per chapter) scaled by the finished audio books'
            chapters per hour, else SeedASRSecondsPerAudioHour 900; agent spend per
            audio hour summed over finished audio books, per ebook averaged, nil until
            one finished; this month's spend (MonthBounds, deleted books included).
            Retention (decision 5) is policy here: SweepTranscripts (daily) purges
            each finished (done, or failed/cancelled) book whose job ended at least
            retention_days ago and wasn't swept, through scratch.PurgeTranscripts,
            then marks it swept. Phase 1c: each purge takes the engine's Hold on the
            book (Engine.Hold, the scheduler's reservation); the sweep leaves a held
            book for the next one, and a job end's purge (retention_days 0) waits a
            few polls for it (a stage the end just stopped) before leaving it too.
            Under the hold the book is looked at again (purgeHeld): one no longer
            finished, or that an open job follows again (a later claim of its job
            re-opened and re-admitted it while the purge waited), keeps its
            transcripts and stays unswept.
            Phase 1c, DESCRIPTIONS (describejob.go): Options.Describer is the
            engine's description step (pipeline.Executor.Describe). A generate job's
            write() saves the characters and recaps first, then runs it
            (describeOnce, with noWait: the step can still wait for an agent slot
            behind other books, so it never holds the files back) when the sidecars
            are final (done, or parked at contributing; a contributing park is
            reported before the write), and a description it got follows in a
            second save. It asks once per run, and once more at done when it first
            asked at a contributing park: the contributing stage may have resolved
            the work and sources since, and the step's free re-compose makes the
            description name the work the characters do; a try at the park that
            failed without spending gets its second go there too. The engine's own
            try at the start of contributing is what it usually finds (reused,
            nothing spent, its checked_against kept); when the book's latest
            describing run failed after spending (that try's, or an earlier run of
            this job's before a restart), it asks no more (describeFailedPaid: no
            second payment, and an ebook's second try at done would be checked
            against nothing). The
            description NEVER fails, parks or holds the job: any error (the engine
            keeping its hold, see below, included) is logged and
            the result says {"description": false, "description_error": "<short
            plain reason>"} (descriptionErrorText), else "description": true and
            description.json is written beside the others in the same finish (same
            rules: known names only, once per content hash, at the book's current
            path, mapping re-checked) with bridge.json's description object
            {job_id, generated_at, from "opening_notes", chapters,
            checked_against}. Only the description the run's step read back under
            the hold is written (the work dir's file is never read again for it);
            the cost is read again after the step (a failed try's spend included).
            The contribution never carries it. A regenerate whose step got none
            leaves an earlier run's description.json in place (nothing deletes into
            a library yet), and its bridge.json carries over the existing one's
            description object (keptDescription: only beside a description.json
            that is a regular file, read through readProvenance), so the kept file
            stays vouched for.
            sidecars.describe (DescribeJob, DescribeKind, pipeline lane): params
            decoded leniently (any JSON object). prepare as generate does, strict
            when new (nothing spent yet) and not when resumed (jobs.Job.Resumed: an
            earlier run may have paid; it parks library_not_writable like a
            resumed generate job); notesBook finds the engine book (the latest at the
            book's source path, else LatestGenerateBookAt for library+path, else the
            generate run the book's own bridge.json names, for a book that moved
            after its job ended (that file moved with it, the Bridge's records did
            not; the job's mapping then follows the book); never a run whose source
            lies outside this library's folder: library ids and paths repeat across
            servers): none is
            errDescribeNoNotes ("The Bridge has no notes for this book ..."), a
            single-chapter book errDescribeOneChapter, one an
            open generate job follows or the engine still has queued/running is
            errDescribeGenerating. Both jobs describe through describeHeld: the
            scheduler's Hold on the engine book (holdBook tries every poll for
            holdTries polls, else errBookBusy, "The engine is busy with this book;
            try again in a minute.") for the call and the read-back of the file, so
            no stage, Retry's dispatch, Delete, purge, sweep or supervisor terminate
            acts on the book meanwhile (a new generate job waits giveWayTries polls for
            a held earlier run, deleteEarlier: the server asks a describe job to give
            way to it, which it does at its next report; then "try again in a
            minute"); a park waits with the hold let go (the job's in-process
            rate-limit backoff, noWait false, does not). The job sets r.bookID once
            it has its engine book, so its own parks are lifted by the save that
            clears them, never between two tries. describeParked runs it: an agent park
            (rate limit, budget, credential, unavailable) is the job's park (code,
            hint, retry_at in progress) and it tries again at retry_at or on Retry
            (the shared pokes, so the page's Retry and Handler.Retry reach it;
            WakeParked(code) pokes every describe job parked with code, which a
            monthly budget saved on the page does for budget_exceeded and an
            agent credential changed on the Accounts tab for
            agent_credential_refused and agent_unavailable, since these wait in
            their own loop, not as engine books: each park is registered in
            describeParks with its run's own channel and removed only by that
            run, and every wake is numbered (wakeSeq, woken per code) so a try
            that parks with a code woken while it ran tries again at once rather
            than lose the wake to the drain below),
            staying parked while it tries so a park that holds isn't announced
            again (a Retry pressed during a try at a timed park is answered by that
            try; the success lifts the park, one the runner restored after a restart
            included); validation exhausted fails it ("Couldn't write a description
            that passed the checks."). Handler.Retry only pokes a sidecars.describe
            job (its id may map to another server's generate run in generate_books).
            saveDescription writes description.json and
            bridge.json (describedProvenance: the existing file's fields kept, its
            description object added or replaced; a fresh one when there is none or
            it isn't a regular file holding a JSON object; readProvenance reads it
            inside the .audiosilo as an os.Root, the opened file the one looked at,
            non-blocking and capped, as agentproto.ReadRegular does) through save, the write
            loop generate's write() uses too (saveSpec: the files composed once the
            book is located, relocate following a moved .epub, parking
            book_not_writable until it can, adoptMove following a move: no mapping
            to record), then RescanAttachments. Progress {step describing|writing|
            rescanning|done, message, done, total, stage "describing", source,
            cost_usd}; result {library_id, path, description: true, files, cost_usd,
            checked_against} ("description" is always true; the server console's
            shared result parser reads it).
            Phase 1d, CHAPTERS (chapters.go): .audiosilo/chapters.json (ChaptersFile:
            version 1, generator, generated_at, from "narration", files [{path
            relative to the book's folder (a single file's name), size, duration_ms}]
            in the server's order, chapters [{title, start_ms, length_ms}] contiguous
            from 0) composed from the engine book's pipeline.DetectedChapters when it
            is current for its manifest (composeChapters: no file for one chapter or
            over 2000; titles control-free, chapterdetect.ClipTitle's bound, "Chapter
            N" for none); bridge.json's chapters object {job_id, generated_at, from,
            chapters}. refreshChapters reads the list and returns (the list, current):
            it reads and checks it again only when pipeline.DetectedStamp (local stats
            of chapters_detected.json and the manifest) changed since the last look,
            so the poll it runs on costs a stat or two (checked against the book's
            audio where the job has it now, bookFileNow: the path the server's last
            answer moved the job to, which adoptMove takes up only at the next save,
            else the run's place; one that can't be looked at is not current,
            detectedUnseen, and asked again at the next look, never kept so). A
            write never goes without the run's chapters only because their audio is
            out of sight (awaitChapters: the done writes park book_not_writable and
            look again every Recheck; the contributing park's write and
            chaptersFromEarlier return errAudioUnseen, the latter failing the job
            rather than clearing the earlier run unseen); saveChaptersEarly saves
            only a list current now, and a write with no chapters.json keeps the
            chapters object of the one in place (keptObject), never the run's own
            beside no file; chaptersJSON composes the file
            at each write, from the book's files and path as they are then (the server
            moves a job with its book: a list naming the old file would never apply),
            at the time the run first noted the list. sidecars.generate (r.kind ==
            Kind: startRun keeps every run's kind) saves them as soon as the engine
            has them (saveChaptersEarly from step(), once per detected list; it never
            waits: not while the library isn't checked mapped and writable (mappedNow),
            a failure tried again a Recheck later) and with the rest at the finish
            (files lists it); never beside characters and recaps made without
            chapters found in the narration (sidecarsWithoutChapters: no chapters
            object in bridge.json), whose positions the server would then gate by this
            list. A run with none keeps an earlier chapters object beside a
            chapters.json in place (keptObject[T], the description's rule too; the
            merge, mergedProvenance, takes fresh's object by its key). create()
            CONTINUES a goal-chapters engine book at the same source path (not swept,
            no open job, the audio read): a done one whose manifest names the book's
            one file and whose list is current for it (CheckCurrent; the file out of
            sight fails the job with errAudioUnseen, the run kept), from splitting; one cancelled or failed at chapter_detecting
            whose kept windows can run it again (pipeline.DetectionRerunnable), going
            on there, so the detection runs again from the window layer with the
            whole pipeline after it and nothing is transcribed again; or one
            cancelled or failed at asr (a chapters job parked asr_worker_waiting,
            cancelled as a generate job is queued, bridge#24) whose windows manifest
            is in place for the file inspected (pipeline.TranscriptionContinuable),
            from splitting (its kept sentinel passes it on; a purge took that too),
            asr transcribing only the windows it hadn't finished; each only
            while the book is still the one file it was (audio.ResolveSource: a
            folder that gained an audio file is a multi-file book now)
            (continueChapters -> store.ContinueChapters with the state it checked and
            the one to go on at, under the engine's Hold, waited for as the delete
            would (holdBook: a stage the cancel just stopped may still be finishing;
            one that stays busy fails the job, errEarlierRunBusy, rather than the
            run being deleted unseen) and the book read again once held (that stage
            may have finished: done), the job's identity, the cleared status and the
            mapping in one transaction on the job's context). The server queues a
            generate job beside a chapters.detect
            job only once that one is cancelled (a PARKED one it cancels as it queues
            it; a working one refuses it, 409 detect_open, until an admin cancels
            it), and the Bridge hears that cancel at the chapters job's next report,
            so create() first waits up to giveWayTries polls (awaitChaptersGivingWay,
            40: two minutes at the 3 s Poll, a 30 s keepalive or two lost)
            while that chapters job still follows the book, whatever the book is
            doing, or no job does but the book isn't stopped yet (its job's end is
            stopping it; one still not stopped when the wait runs out is stopped
            there, stopBook), handing back the open job it last saw (firstOpenJob); a
            job that stays open is refused as before. stopBook leaves alone a book
            another open job follows (a late end of the chapters job, Abandon's once
            the engine starts, would stop the continued run). chapters.detect
            (ChaptersJob, ChaptersKind, pipeline lane, Abandoner; params any JSON
            object): the same resume and follow as generate (resumeMapping +
            runEngineBook: prepare strict when new, a resumed job's own book found
            again, one that moved while the job was away followed and recorded,
            another book's run stopped); new, refused beside characters and recaps
            made without chapters (errChaptersBesideSidecars: generate it again), an
            engine book at the source path with a current list is written from (no
            run, runEngineBook's fresh hook; the job takes that book to report on, so
            its own park is never lifted between two tries, and records no move); one
            an open generate job follows is refused (errChaptersGenerating); a
            finished whole run is never cleared for it (errChaptersKeepsRun); else an
            engine book with goal chapters and no series (createChapters: clearEarlier
            and queueBook, generate's create tail; a chapters-only run makes nothing
            a later volume carries on from, so it never holds the series' lock),
            followed like generate (progress without cost_usd), written at done
            (chapters.json + bridge.json merged: mergedProvenance keeps every other
            field) and rescanned; an inspect that refuses the run (the book
            has chapters, or a file whose chapters can't be found in the narration:
            pipeline.ChaptersRefusals) fails the job with that sentence, read from the
            inspecting run's own error (hasChaptersFailure: a supervisor escalation
            of the failure into a park ends it the same way). Result {library_id, path, chapters, files, source:
            "audio"}. Retention as generate's (its mapping in generate_books).
            ParkHint words chapters_not_detected (add chapter markers, then cancel:
            the server reads them, a new generate job works from them; a recording's
            chapter list reaches the server, never the Bridge); stageLabels
            chapter_detecting.
            Phase 2b: contributejob.go, sidecars.contribute (light lane; "Share
            with the community"): ParseContributeParams is LENIENT (a later
            server's field ignored; dimensions characters/recaps, each once; a
            work id one line of at most 200 characters). prepareRead resolves the
            book without a writable mapping (it writes nothing into the library),
            notesBook finds the engine book that keeps the results, holdBook holds
            it, and Options.Sharer (pipeline.Executor.Share) files them. Failures
            in words: not signed in, a sign-in GitHub couldn't be reached to
            refresh (Options.ContribCredential answering contrib.ErrSignInUnrefreshed,
            or a contrib_unavailable park whose Cause is it), refused, nothing kept
            here, a generate job still running, not matched. Result {library_id, path, work_id,
            contributed, covered}. The hints for agent_unavailable,
            agent_credential_refused and contrib_unavailable point at the Accounts
            tab.
            Phase 3c-1, readalongjob.go: readalong.align (ReadalongJob, the pipeline
            lane, Abandoner; generate's strict prepare). createReadalong refuses before
            any work a book the server names no ebook for ("this book has no ebook"),
            one outside the mapping, not a regular file, over 256 MiB, DRM or not an
            EPUB (readalong.OpenEPUB). The earlier engine book is audioRun's: the
            book's own run, unless that is an ebook run, which a read-along or chapters
            job never continues, converts nor clears (its kind, ebook path, manifest and
            notes are describe's and a contribution's): then the audio run beside it
            (store.AudioRunKey), a new one made there. One another open job follows
            refuses the job (a generate job's in words); one a
            stage holds says try again in a minute; else, held, readalongReuse (decision
            6, pure, table-tested) picks: a whole run past qa_adjudicating (or stopped
            at readalong_aligning) -> readalong_aligning, goal kept; a read-along done or stopped at aligning ->
            aligning; transcribed but not repaired (chapters done; stopped from
            sanitizing to retranscribing) -> sanitizing, goal readalong; stopped at asr
            and TranscriptionContinuable -> splitting; a finished whole run swept or
            with other audio -> inspecting, goal readalong, its transcripts purged
            first when another recording's, its notes kept (a read-along with a fact
            pass behind it counts as a whole run: holdsWholeRun, which createChapters'
            errChaptersKeepsRun applies too; read from the notes on disk, else any fact
            pass that ever succeeded, so a Reconcile rewind doesn't lift it); else cleared for a
            new book (goal readalong, no series, no contribution). An audio file that
            can't be looked at now says try again, never "the audio changed"; a file the
            server has no length for fails before any work (rescan). prepareContinue
            removes the sentinels of to (not splitting's) and of the read-along stages
            after it and retires their successes and those of every stage a purge took
            the sentinel of (a restart's Reconcile would rewind to them), drops asr.json
            going on from inspecting (pipeline.ForgetTranscription: a new manifest), and
            resets the repair round before qa_sweep. A generate job reading the audio
            takes on the audio run beside an ebook run (earlierRun for both; the ebook
            run cleared first, then the audio run continued, re-keyed to the book's
            path, or cleared); one reading the ebook leaves it alone, and describe's
            latestGenerated skips it (no notes). A generate job never continues a
            read-along transcribed in windows (its chapters were never found). The request is written
            before the book goes on, and again (readalongAsk) when a resumed run first
            looks or the server moved the job with its book. At done: the map under 50%
            text coverage fails ("the ebook and the narration don't match: N% lined
            up"), an ebook whose sha256 now differs from the map's fails ("the ebook
            changed while aligning; run it again"), else readalong.json
            (<stem>.readalong.json) is saved where the book is now (save:
            book_not_writable park until it can), bridge.json untouched, then
            RescanAttachments. Result {library_id, path, file, coverage, sections,
            spans, audio_gaps, heal_windows, transcribed (an asr run since the job took
            the book), ebook_sha256}; no cost_usd in its progress. The job's end records
            ended_at (release), the retention's clock. A generate job continues a done
            read-along run at qa_sweep with goal "" (continueReadalongRun via
            ContinueChapters, decision 7); a chapters job clears a read-along run rather
            than refusing it as a whole run.
  agentproto/ the staging protocol between the Bridge and the agent container (decision
            15), shared by both sides so they can't drift; a LEAF (standard library
            only). Protocol 1 (both sides refuse another). A job dir <staging>/<16 hex>/:
            the Bridge writes work/ (the staged copy, work/out/ writable by the agent
            uid), prompt.txt, then request.json.tmp renamed to request.json (ready);
            the runner takes `claim` (an atomic mkdir: whichever side makes it owns
            the job), touches `heartbeat` every 5 s, writes stdout.txt, stderr.txt,
            last_message.txt (codex) and exit.json, opens work/out/ for the Bridge,
            and renames done.tmp to done; the Bridge may write `cancel`, harvests
            after done and deletes the dir. runner.json {protocol, version,
            heartbeat_at, slots, clis {claude|codex: {version, credential KIND
            (oauth_token | login | api_key | ""), credential_since}}} is rewritten
            every 15 s and counts as up while under 30 s old (two intervals, so one
            slow write doesn't flap it); credential_since moves when the credential
            is replaced, never a token. Request is TYPED {protocol, cli, model,
            effort, web, no_tools, max_turns 0-1000, timeout_seconds 1-86400}: the
            Bridge never sends argv; DecodeRequest refuses an unknown field, trailing
            data and over 64 KiB; a model name can't start with a dash and an effort
            is letters only (it lands in a codex -c override). Exit {exit_code,
            timed_out, cancelled, duration_ms, error, unavailable}: error is a job
            the runner couldn't run or stopped itself, unavailable the kinds the
            Bridge treats as the agent unavailable rather than a failed run. Size
            bounds for each file both sides read (prompt 32 MiB, stdout 64 MiB, stderr
            8 MiB). Nothing is trusted by the side that didn't write it:
            OpenRegular/ReadRegular read inside an os.Root, refuse a link (Lstat
            first), open O_NONBLOCK (a FIFO swapped in never blocks), require the
            opened file to be the one Lstat saw, and enforce the size limit on the
            open file and again while reading; Exists Lstats a marker, never
            following a link.
            Phase 2b (decision 19, the agent credential hand-off): runner.json's handoff says the runner
            takes the Bridge's credential; the Bridge then writes
            credential/credential.json (Credential {cli, kind, value}) in the job
            dir, in its own NON-sticky subdirectory so the runner (another uid) can
            delete it, before request.json. DecodeCredential is strict (unknown
            field, trailing data, over 4 KiB, another CLI, a kind that CLI doesn't
            take, or a value that isn't one printable line all refused; the value
            never in an error).
  agentbox/ the Bridge's side of the agent container: an agent.Runner that runs no CLI,
            drops each invocation into the staging dir, waits for agentrun, and
            harvests the outputs back into the pipeline's staged dir; the outcome is
            classified by agent.ClaudeResult/CodexResult, so a container run reads
            exactly like a local one. Status reads runner.json: Available (fresh,
            protocol match, the backend's CLI present WITH a credential not refused),
            Refused (all that, but the provider refused the same credential on the
            last run: runs still go through, a refused call costs nothing and the
            first that works clears it; a credential the runner reports as changed,
            by kind or credential_since, clears it too), Live (fresh, whatever its
            CLIs), each "why not" worded for the page and a park (claude: `claude
            setup-token` -> CLAUDE_CODE_OAUTH_TOKEN; codex: agent-login; and when the
            OTHER CLI is signed in, how to switch agent.backend), never naming a
            Bridge path (a Detail becomes a park reason the server's console and
            bell show; a staging dir that can't be opened is logged with its path,
            once per outage). Detect: Available =
            Available || Refused, RetryAfter ALWAYS 10 min (whatever makes the
            container unavailable is expected to pass by itself). Run: stage() makes
            the job dir 0700 and writes it ONLY through an os.Root opened on it
            (checked to be the dir just made: the agent uid can write the staging dir
            and swap entries), inputs 0444, work/out/ 0777 with files 0666 (a
            validation retry patches them), the dir opened to the agent (sticky 0777)
            only when whole, request.json renamed last. wait polls done every 1 s
            (polling: shfs may not deliver inotify), the heartbeat every 5 s; an
            unclaimed job past claim_timeout is taken back by the Bridge's own claim
            mkdir (if the runner won that race it runs after all) and is a
            NotAvailableError; a heartbeat older than 60 s writes cancel and fails the
            stage; Request.Heartbeat beats every 60 s; req.Process is never called (a
            pid in another container means nothing). A ctx cancel takes back an
            unclaimed job, else writes cancel and waits up to 30 s for done (the
            usage it reported is still returned, nothing harvested). An exit.json
            with unavailable: true (the container stopped or restarted mid-run) is a
            NotAvailableError that still carries the usage its stdout shows (parsed
            by ClaudeResult/CodexResult, nothing else kept), so a paid run cut short
            counts toward the budgets. The harvest follows ANY
            finished run, failed ones too, into a temp dir then renamed over out/:
            regular files only, depth 8, 8 MiB a file, 64 MiB in all, 2,000 entries;
            a broken limit (harvestLimitError) fails the stage and leaves out/ as it
            was. The Bridge's own trouble with the staging dir (can't write the job,
            can't read exit.json back) is NotAvailable, worded without its paths,
            never a failed stage. A request timeout rounds up to whole seconds; none
            is the protocol's 24 h ceiling. New makes the staging dir and chmods it
            0777 (not sticky: the entrypoint's first-start chown must not stop the
            runner replacing runner.json), removes job dirs older than a day and
            RECLAIMS the rest (one Bridge per staging dir, and this one just started:
            a finished or unclaimed dir is removed, its claim taken first; a running
            one gets a cancel). EnforcesNoTools only for claude (codex's read-only
            sandbox still reads); SupportsWeb true.
            Phase 2b: Options.Credential (the vault's credential for a CLI, from
            accounts.Agent) stands in for a container that has none of its own and
            takes a hand-off: Status counts the agent available, billed as that
            kind, its set time standing for credential_since (so replacing it on
            the Accounts tab ends a refusal); stage() writes credential.json
            (0444, synced) in credential/ (0777) before the request. A container
            credential always wins; a runner that can't take a hand-off gets
            nothing and is told to update. A refusal's detail says where to
            replace the credential: the Accounts tab for the handed one, the agent
            container for its own.
  agentrun/ `audiosilo-bridge agent-runner`, the ONLY thing in the agent container (Linux
            only). It sees the staging dir and /agent (secrets/, codex/), never a
            library or the Bridge's data. Main sets umask 077 for the whole process
            (os/exec can't set a child's, and nothing the runner writes relies on the
            default: what the Bridge reads is chmod'ed 0644 explicitly), so whatever a
            CLI or its helpers write is private. --slots 1-8 (each a concurrent CLI
            run; the Bridge follows it as its agent concurrency). The loop: CLIs
            detected at the start and again every 10 min or when a run finds its CLI
            missing (in the background, so runner.json never goes stale); runner.json
            rewritten every 15 s; ORPHANS (claimed, not running here, heartbeat older
            than 30 s: an earlier runner's) finished at the start and with every
            status write, before the Bridge's 60 s stale limit; the staging dir
            scanned every 1 s while a slot is free, oldest request first, everything
            through an os.Root. execute: request.json decoded strictly, a missing CLI
            or credential is Exit unavailable, prompt.txt bounded, the argv built HERE
            with agent.ClaudeArgs / agent.CodexArgs(CodexSandboxOff) (one copy of the
            flags). The child's environment is an ALLOW-LIST: PATH, LANG, TZ, a
            per-run TMPDIR and HOME, DISABLE_AUTOUPDATER=1, the CLI's own config dir
            (a per-run CLAUDE_CONFIG_DIR; CODEX_HOME on /agent/codex, where the login
            lives) and exactly ONE credential, a subscription first
            (CLAUDE_CODE_OAUTH_TOKEN; codex's device login in auth.json), else the
            API key (ANTHROPIC_API_KEY; OPENAI_API_KEY, also passed as
            CODEX_API_KEY), each from the environment else
            /agent/secrets/<lowercase name>; never the other CLI's, never argv, a log
            or a file the Bridge reads. CODEX_HOME is wiped of everything but
            auth.json and config.toml when the first concurrent codex run starts and
            when the last ends (codex writes the whole environment into
            shell_snapshots/, spike 2). credential_since comes from an in-memory
            fingerprint of the credential, never written. spawn: its own process
            group, stdout/stderr capped, the heartbeat touched every 5 s, cancel
            checked every second; a cancel, the timeout or the runner stopping sends
            SIGTERM to the group and SIGKILL 10 s later, and what is left of the group
            when the CLI exits is killed. A run cut short by the container stopping
            is Exit unavailable (the book parks and resumes). finish writes exit.json
            (O_EXCL, never replacing a file), copies codex's last message (a link
            refused), opens work/out/ for the Bridge (files 0644, dirs 0777 so the
            Bridge can empty them, links skipped, never followed) and renames done.tmp
            to done. Login: codex runs `codex login --device-auth` with the
            allow-listed environment and CODEX_HOME on the volume; claude prints how
            to give the container a `claude setup-token` token.
            Phase 2b: runner.json says handoff: true; execute reads the job's
            credential.json (agentproto.DecodeCredential) and DELETES it before
            the CLI starts, and childEnv uses it only when the container has no
            credential of its own for the CLI (claude: the OAuth token or
            ANTHROPIC_API_KEY; codex: OPENAI_API_KEY and CODEX_API_KEY).
  workerproto/ Phase 1b: the worker wire protocol, a leaf (standard library only)
            shared by api (the routes), workers (the dispatcher) and worker (the
            worker), so the two sides can't drift. Version (1) rides every call in
            X-AudioSilo-Worker-Protocol; another is 409 protocol_mismatch with the
            Bridge's version. Bump it only for a change an installed worker can't
            follow (it refuses every installed worker until it updates); a new field
            isn't one (the routes decode leniently). The unit is a SEGMENT (a FLAC,
            its start and duration in the book, the decode options, an opaque id,
            the FLAC's size and sha256), so Phase 1d's duration windows need no bump.
            The result's raw transcript is the request BODY with ResultMeta in the
            query: mlx output may hold NaN, which isn't JSON. The errors
            (ErrUnauthorized, ErrUnknownTask, ErrNotHeld, ErrInvalidResult) are the
            dispatcher's and map to 401/404 unknown_task/409 not_held/422.
  workers/  Phase 1b, the Bridge's side. Hub: the in-memory segment queue (the
            engine's work dir is the truth), 60 s leases renewed by 15 s beats, and
            the registry of workers heard from (connected = a call within 45 s).
            Tokens: asw_ + 64 hex, kept as sha256 in the workers table, deleting the
            row revokes (and requeues what it held); only Authenticate adds a worker
            to the registry (revokeMu orders it against RevokeWorker), so a call in
            flight when its worker is removed is refused, never brings it back. Run(seg) queues and waits;
            ErrNoWorker as soon as the segment sits unclaimed with none connected
            (none was, or the holder's lease ran out). The task id hashes the audio's
            sha256 + options, so a restarted Bridge asks for the same task; a beat on
            a QUEUED task ADOPTS it (a new attempt) for the worker still decoding it;
            a result is taken whoever holds the task (same audio, same options); a
            finished or cancelled id is remembered 10 min so a late beat hears
            not_held (stop), not unknown (carry on). Results must pass
            transcript.Complete. WhenConnected closes on a connection after none (the
            edge is read from the state in touchLocked, never kept): a local decode
            yields, app's followWaiting looks at once. Router: the engine's
            asr.Backend. Remote while a worker is connected (writes the raw where a
            local decode would, fsutil.WriteFileAtomic), else Wait() (asr.
            worker_fallback == wait, read per segment) parks asr_worker_waiting, else
            the Bridge's own backend (prepared lazily, stopped and re-routed when a
            worker connects mid-decode); no usable local backend is a wait too.
            Detect is always available (so the executor never re-detects), CanRun
            says whether a segment would run now (followWaiting's condition). ID/
            Detect report the route NOW (backend "worker", device "remote"), so
            asr.json's provenance is the route the stage started on (per-segment
            provenance is a follow-up).
  worker/   Phase 1b, `audiosilo-bridge worker`: client (token + protocol header on
            every call; the FLAC checked by size and sha256), Run (Select +
            EnsureReady, ffmpeg checked for mlx-whisper; hello; then a loop: send
            kept results, resume the segment in hand, claim), keepLease (beats every
            HeartbeatEvery from the claim on, through the FLAC's download as well as
            the decode, since a big FLAC on a slow link outlasts the 60 s lease; stops
            both on not_held or a refused token, tolerates an unreachable or
            restarting Bridge for unknownGrace = 30 min, follows an adopted attempt)
            and the spool (<data>/spool: current.json + <id>.flac = the segment in
            hand; results/<id>.json + .meta.json = transcripts not yet taken). A
            refused token or another version is retried every 5/10 min and never
            exits (a service manager would loop). install.go: Service renders a
            launchd plist (KeepAlive, ProcessType Interactive, the install's PATH,
            the log in <data>) or a systemd user unit (Restart=always, every word
            quoted with % doubled), Install/Uninstall run launchctl/systemctl
            through an injected Run; the token goes to <data>/worker.token (0600),
            never into the service file. A decoder outlives a kill -9 of a worker
            started by hand (spike 4); launchd and systemd end the job's process
            group/cgroup, so an installed worker doesn't leave one. Install on
            macOS waits for launchd to unload the old job after bootout
            (launchctl print, up to 10 s; bootout returns while it is still
            tearing down) and retries the bootstrap with a growing pause (it
            fails with exit status 5 meanwhile); a final failure names the
            manual launchctl bootstrap command. Sleep is injectable for tests.
            InstalledArgs reads the service file back into its argv (the
            plist's ProgramArguments, strict XML; the unit's first ExecStart,
            "quoted" or bare words with only \\ and \" escapes and %% $$
            undoubled), Render's inverse (a round-trip test holds it); no
            file wraps fs.ErrNotExist, and anything else systemd would read
            differently (a specifier, a $VAR, a prefix, another escape, a '
            in a bare word, a lone ;) is an error, which `worker install`
            prints before any --bridge refusal. `worker install` (cmd
            worker.go carryForward) parses that argv with the same flag set
            that wrote it and keeps each written flag (--bridge, --data, --backend, --model, --whisper-cli; never
            --token) the command line didn't give, so an update keeps a
            customised worker and finds its token in the kept --data; an
            explicit flag wins (an empty one included); a file that doesn't
            parse keeps nothing and says so on stderr. Uninstall names the
            kept --data. The client appends each route to the --bridge path as
            written (EscapedPath), so a proxy prefix's "(" or "!" reaches it
            unescaped.
  workerdist/ the worker's own distribution, a leaf: the gzipped builds the
            images carry in /usr/local/share/audiosilo-bridge/workers
            (DefaultDir; audiosilo-bridge_<os>_<arch>.gz for the fixed Platforms
            darwin/arm64, darwin/amd64, linux/amd64, linux/arm64, the Bridge's own
            VERSION, never embedded in the binary), Open (the file name comes from
            the set, never the request; ErrUnknownPlatform, ErrMissing with the
            releases URL when the folder is absent: a native run), Available, and
            install.sh (go:embed, POSIX sh): Address baked in single-quoted by
            Script. Address validates the request's Host first (ErrAddress for a
            host a script can't carry, whatever public_url says), then takes
            public_url as set (scheme and path prefix, trailing slashes trimmed,
            so it equals the Workers tab's command) when it is a plain http(s)
            address (no user, query or fragment; a plain path) on the request
            Host's host and port: hosts case-insensitive, a missing port on
            either side public_url's scheme's default (a TLS proxy's request
            arrives as plain http with the client's portless Host). The prefix
            may hold RFC 3986's pchar (unreserved, %XX, the sub-delims and
            ":" "@") but never "'", which would end the single-quoted
            bake-in; each sink carries the rest as written (install.sh's
            double-quoted expansions, curl, which globs only "[]{}", the plist
            XML-escaped, the unit quoted with % and $ doubled), a test per
            character. Otherwise the
            request's scheme://Host, reachable from that computer by
            construction. The admin sets public_url; the Host shows the fetching
            computer reached that host. The download URLs are under it, prefix
            included. When install.sh can't be served (no folder, a bad Host) the
            route still answers 200 with ErrorScript: it prints why to stderr and
            exits 1, since `curl -fsSL` drops an error status's body and sh would
            exit 0 on nothing. The script
            (one main function, called on its last line, so a download cut
            short runs nothing) takes --token (required the first time; an
            update reuses the data dir's saved token), --run (says a --run start
            doesn't save a --token it was given; one that lands in the folder the
            installed service runs from says the service keeps the old program
            until it restarts, with the restart command), --dir; detects uname (a Rosetta terminal on
            Apple Silicon takes arm64). With no --dir it reuses the installed
            service's folder (installed_dir: the launchd plist's first
            ProgramArguments string, the systemd user unit's first ExecStart word,
            as `worker install` writes them; only an absolute path to an existing
            folder this user can write, with nothing the file had to escape,
            `\ % $ & < >`, else the default ~/.audiosilo-bridge-worker/bin, which
            `worker install` then points the service at) and says so; an explicit --dir
            wins. It downloads to a temp file in the target dir, checks `version`
            runs, moves it to <dir>/audiosilo-bridge and runs `worker install` (or
            `worker` with --run). The saved-token check of an update looks in the
            installed service's --data (service_args reads the service's
            argv, installed_data takes --data <dir> or --data=<dir> when
            it reads plainly), since `worker install` keeps it; a --run
            start, or a service with none, looks in $AUDIOSILO_WORKER_DATA,
            else ~/.audiosilo-bridge-worker, as `worker` does. Its tests run it end to end with a fake curl
            (and a fake uname for the other OS's service file), each run in a
            temp HOME holding service files rendered by worker.Service, so no
            test reads this computer's own.
  app/      Phase 2c-1 wires the guarded client and both podcast handlers,
            advertises their kinds (2c-2a also fetch/download and retain/light),
            opts fetch into the generic interactive pool (2c-2b),
            uses the resolved engine probe for podcast audio, warns for allowed
            non-public CIDRs at start
            and on Status, and redacts podcast params/results/errors for page
            job lists and details. The warning reflects the running client until
            restart, including after an unrelated settings save.
            The wiring and the page API's Backend: Conn holds the pairing (settings
            pairing.*, the token in the vault) and the link; the heartbeat loop beats
            every 30 s or when kicked, records the server's libraries (under b.mu,
            beatGen-checked, before the link counts as ready), wakes the runner
            to compare negotiated eligibility on every successful reply (2c-2b),
            and starts a mapping
            check (in the background; one asked for while another runs is queued
            and beaten once, by it) when they changed or one is due, then beats
            again with the results. Phase 2b: the beat carries the setup checklist
            (setup(ctx), setup.Checklist) once the server accepts it (see link/).
            accounts.go wires internal/accounts: newAccounts (the GitHub sign-in
            with the token source's onChange, the agent vault), a start deletes an
            earlier Bridge's sealed github_pat once (forgetSealedGitHubPAT, best
            effort: one sealed with another key, or a failed delete, never stops
            the start; the env token is read live), githubChanged kicks a beat
            and re-admits contrib_unavailable books when the Bridge can contribute
            (a sign-in resumes the sidecars.generate job waiting on one; a refresh
            that works again re-admits nothing, so a book parked on a sign-in GitHub
            couldn't be reached to refresh waits for Retry; sidecars.contribute
            never parks: it fails asking for a sign-in, or for GitHub to be
            reachable, and a share again); generate gets contribCredential (why
            no token resolves) as Options.ContribCredential,
            agentCredentialChanged re-admits agent_credential_refused and
            agent_unavailable; the Backend adds Accounts, StartGitHubSignIn,
            SignOutGitHub, SetAgentCredential, DeleteAgentCredential. A budget
            saved on the page (SaveSettings, agent.monthly_budget_usd) records
            BudgetReviewedAt (the checklist's budget item) and re-admits
            budget_exceeded books. meta.correct (correct.New over
            metaops.NewClient(metadata.base_url)) and sidecars.contribute (the
            engine's sharer) are registered, so the heartbeat advertises them. The runner sees the link (server()) only once a
            heartbeat on it went through, so no job, a resumed one included, runs
            before the server's libraries are known. A 401 on the CURRENT link
            refuses it with a reason the page shows (Conn.Refuse(cl) and accept(cl)
            ignore any other link: one a re-pair or an unpair replaced, still in use
            by a job or a call that started before). Refusal, unpairing and
            replacing a pairing withdraw claim_min_priority from the old link;
            each new link negotiates on its own heartbeat. A 401 does NOT drop the token:
            Conn.Refuse keeps it and the heartbeat retries it every RefusedRetry (5
            min), since a demoted admin's Bridge works again once they are an admin
            again (an unpair or a new pairing revoked it for good, so a retry just
            fails); Client() is nil
            while refused, so nothing else calls in. Run LISTENS BEFORE pairing from
            AUDIOSILO_BRIDGE_PAIR_CODE, so a start that can't serve doesn't spend the
            code; a page server that fails stops the heartbeat and the runner, and
            Run returns its error. Edits go through config.SaveFile (one at a time:
            each loads, edits and writes config.yaml) and reload the config.
            Phase 1d: chapters.detect (b.gen.ChaptersJob()) is registered beside
            sidecars.describe, so the heartbeat advertises it.
            Phase 1b: workers.go: the hub (made in New, behind the page server's
            /worker/v1/*), the router (engine.go: the asr.Select backend becomes its
            Local), Workers/CreateWorker/RevokeWorker for the page, linkWorkers for
            the heartbeat (only workers that have connected; platform = GOOS/GOARCH
            plus backend and device), and followWaiting: ONE level-triggered loop that
            re-admits asr_worker_waiting books whenever router.CanRun(), looking
            every 30 s, when a worker connects (hub.WhenConnected), at the engine's
            start and after a settings save (kickWaiting).
            Phase 1a: engine.go wires the ENGINE the way sidecars' daemon did, minus
            the folder scan, and runs it in the background from Run (the first start
            may download ffmpeg; the generate job waits for it): the event hub with
            its durable persister (enqueue never blocks; a full or closed queue
            drops), toolfetch, asr.Select, the agentbox runner as the ONLY agent
            runner, the executor, the scheduler, the supervisor (deterministic lane;
            supervisorApply, see supervisor/), the contribution poller, and two daily
            jobs (prune the 30-day event log, sweep the transcripts past
            transcripts.retention_days). The agent concurrency is the container's
            --slots: read once at the start, then followAgentSlots (every
            RunnerInterval) calls scheduler.SetAgentCapacity +
            executor.SetAgentConcurrency when a live runner.json reports another
            number. New wires generate.Handler into the runner (beside verify) with
            ServerID = the pairing's server_id. AUDIOSILO_BRIDGE_GITHUB_TOKEN was
            sealed into the vault as github_pat at every start until Phase 2b; it
            is read live now (contrib.TokenSource), and canContribute is the token
            source resolving (env, device sign-in, gh) and not refused. The heartbeat carries estimates (nil until the engine
            started): generate.Estimates, cached 5 min or until a job finishes done
            (can_contribute computed with them, since resolving can shell out to
            gh), with the agent's readiness and the budget read fresh each beat. The
            Backend adds Job(id), RetryJob (generate.Retry: ErrNotFound -> 404,
            ErrNotParked -> 409) and Engine (the agent container from
            agentbox.Status, ASR capability, budgets and this month's spend, the
            retention).
  api/      transport-only HTTP of the Bridge's page (/api/*): login (rate limited),
            password (a wrong current one is 403, never 401, which signs the page
            out), status, pair/unpair, libraries + mount edits + check now, settings,
            jobs + one job (GET /api/jobs/{id}) + cancel + retry (POST
            /api/jobs/{id}/retry: 202, 404 when it isn't a sidecars job here, 409
            when it isn't parked), engine (GET /api/engine: the agent container, its
            CLIs' versions and credential KINDS, never a credential; ASR; the budgets
            and the month's spend; the transcript retention), logs, workers (list,
            POST add: the token in the answer only, DELETE revoke). Phase 2b:
            accounts.go, GET /api/accounts (GitHub: signed in, login, refused,
            the pending device code, env_token, from, can_contribute, manage_url;
            the agent: each kind set or not, when they last changed (set_at: a
            save, or a removal that left others) and when each held was saved
            (set_at_by_kind), the container's own
            credential; never a credential), POST /api/accounts/github/device
            (starts a sign-in, answers the code), DELETE /api/accounts/github
            ({accounts, revoke_error}), PUT /api/accounts/agent {kind, value},
            DELETE /api/accounts/agent?kind=; status carries setup and
            setup_complete, settings monthly_budget_usd and budget_reviewed.
            workerdist.go: GET /worker/install.sh and GET
            /worker/download/{os}/{arch} (internal/workerdist), outside
            /worker/v1, behind lanOnly, with NO page password (a curl | sh has no
            session; the worker token in the user's command is the credential),
            install.sh's address from workerdist.Address with the saved
            public_url, the download a plain-text 404 naming the releases when
            the image's folder is absent (install.sh an error script that exits 1
            instead); status carries worker_downloads (the folder has a build).
            lanOnly refuses GET/HEAD /worker/install.sh with that error script
            (200, text/plain: a 403's body would never reach sh), knowing it by
            its path.Clean'd path (//worker/install.sh, /worker/install.sh/,
            /worker/./install.sh get it too; from the LAN they get what the
            mux answers); the address's scheme is always http (the Bridge
            serves plain HTTP; a TLS proxy shows only through public_url); every other
            route, the downloads included, keeps the JSON 403 lan_only.
            securityHeaders wraps lanOnly, so a refusal carries nosniff too.
            Phase 1b:
            workers.go serves /worker/v1/* to the injected WorkerHub: a worker token
            only (a page session is 401, and the page's routes refuse a worker
            token), then the protocol header; lenient JSON; a longer write deadline
            for the audio and read deadline for the result. lanOnly refuses any connection not from a
            loopback, private (RFC 1918, ULA) or link-local address (RemoteAddr only,
            never a forwarded header; CGNAT/Tailscale is NOT local); every route but
            login and health needs a session, loopback included (an agent container
            on the Bridge's network could otherwise reach it, spike 2). Backend errors
            are typed (ErrInvalid 400, ErrRefused 422, ErrNotFound 404, ErrLocked 409
            code locked, ErrConflict 409, ErrUnreachable 502), anything else a 500
            that is logged and never echoed.
  logring/  an slog.Handler that tees Info-and-up into a 2,000-line circular buffer
            for the page's log view (levels kept as slog.Level, encoded by name);
            ids start from the clock (Unix microseconds), so they grow across
            restarts and a page still holding the previous process's cursor gets
            the new lines. Attributes whose FULL dotted key holds
            token/password/code/key/secret/cookie/authorization are redacted in the
            ring (the process log keeps their values), groups (slog.Group, a
            LogValuer resolving to one) flattened leaf by leaf first.
            Phase 2c-1 sanitizes BEFORE BOTH sinks: feed_url values and URLs in
            messages/errors lose credentials, paths and queries. WithAttrs and
            nested groups receive the same treatment.
  web/      go:embed of the page (build-tag selected) + SPA-fallback static serving;
            the page itself (/, a client route, /index.html) always goes out with
            its CSP
web/          the Bridge's page: Vite + React 19 + TS + Tailwind v4 (npm, Node 24); dist/ is
              embedded with -tags embedui. Tooling, versions and the visual language are
              sidecars' web/ (the lock file is sidecars' with the name changed). No router:
              the tab rides the URL hash (src/lib/tabs.ts). src/api/types.ts mirrors the
              page API (internal/api) by hand; client.ts sends the bearer token, turns a
              401 into signed-out (never on login) and a 403 into the "local network
              only" screen (the guard's code lan_only; a wrong current password is a
              403 of its own route, shown as its {error}). src/lib holds the pure,
              vitest-tested logic (time, tabs, status, jobs, verify, settings,
              libraries, logs, token: localStorage audiosilo_bridge_token) plus the
              usePoll hook; components/tabs/ are the seven tabs (Status, Libraries, Jobs,
              Settings, Accounts, Workers, Logs), thin over src/lib. Jobs poll every 3 s while one
              runs (a PARKED job counts as needs attention, so it polls at 15 s: it may
              wait for days), else 15 s; a bridge.verify result renders as a file table.
              Phase 1a: the hash is a Route (routeFromHash: "#jobs", "#/logs", and
              "#/jobs/<server job id>", the server console's link to one job, which
              the Jobs tab shows alone as FocusedJob); src/lib/generate.ts reads a
              sidecars.generate job (parkOf: a RUNNING job whose progress carries a
              park; effectiveState shows it as needs_attention, as the server does;
              canRetry; the ETA, spend and result wording), and a generate job shows
              its stage, count bar, ETA, spend and source, while parked its park, its
              hint and when it resumes by itself, a Retry button, and when done the
              files written to .audiosilo/ with any contribution links.
              Phase 1d: chapters.detect is a sidecars job too (kind label "Chapters",
              its parks and Retry; parseChaptersResult / chaptersSummary word its
              result). Podcast job labels also word Fetching episode and Checking retention.
              The page lists no stage or park names: the hint comes in the
              progress.
              src/lib/engine.ts words the Status tab's "Characters and recaps" card
              (GET /api/engine every 10 s: the agent container and its CLIs'
              credential kinds, speech recognition, the monthly budget bar, the
              per-book budget and the transcript retention, all config.yaml).
              Phase 2b: the Accounts tab (#/accounts, the console's link; after
              Settings): GitHub sign-in with the device code shown and polled
              every PENDING_POLL_MS while pending, sign out (a revoke_error shown),
              refused and env-token states, the manage link; the agent card, one
              password field per kind (src/lib/accounts.ts: KIND_LABELS,
              githubState, checkCredential). The Status tab's "Get set up" card
              (src/lib/setup.ts) shows until setup_complete, each step linking to
              its tab (agent and github: accounts; asr and budget: settings;
              libraries: libraries). Settings gains the monthly agent budget.
scripts/build-web.sh   build the SPA + embed it into bin/ (-tags embedui)
Dockerfile             multi-stage: node build -> go build (embedui, cross-built on
                       $BUILDPLATFORM, only cmd/ and internal/ copied, Go and npm cache
                       mounts) -> whisper-cpu / whisper-cuda (docker/fetch-whisper.sh:
                       sidecars' pinned release asset, sha256-checked; each target
                       fetches only its own) -> four targets: `slim` (debian-slim +
                       ffmpeg), `cuda` (nvidia/cuda runtime + the CUDA whisper-cli,
                       amd64), `agent` (see below) and `full` (slim + the CPU
                       whisper-cli; the last stage, the default). None of slim, cuda
                       and full carries an agent CLI (decision 15).
                       docker/entrypoint.sh starts as root only to own /bridge-data (a
                       chown -R only when the folder isn't PUID:PGID's yet), sets umask
                       022 and drops to PUID:PGID with setpriv. `agent` (Phase 1a):
                       debian-bookworm-slim + tini + the native claude and codex CLIs
                       (pinned CLAUDE_VERSION/CODEX_VERSION ARGs, fetched by `npm pack`
                       of the one platform package, integrity-checked, no install
                       script, no Node at runtime; codex keeps its vendor layout minus
                       the voice runtime) + this binary as `agent-runner`; uid/gid
                       10002, setuid/setgid bits stripped, /agent (+ secrets/, codex/)
                       0700 and /staging 0777, both volumes; the build runs each CLI's
                       --version, so a release that doesn't install fails there.
                       A `workers` stage (on $BUILDPLATFORM, no page, the same
                       VERSION) cross-builds the four worker builds, gzipped, into
                       /usr/local/share/audiosilo-bridge/workers of slim, cuda and
                       full (not agent). release.yml uploads the same four (with
                       the page embedded, so they also run a whole Bridge) as
                       audiosilo-bridge_<ver>_<os>_<arch>.tar.gz + checksums.txt to
                       the tag's release (made, not as a draft, when missing).
                       image.yml builds all four on v* tags and dispatch (amd64+arm64;
                       cuda amd64; agent -> :<tag>-agent, :latest-agent).
compose.example.yaml   three services: the server (libraries :ro), the Bridge (the same
                       folders :rw, AUDIOSILO_BRIDGE_AGENT_STAGING_DIR=/staging, the
                       optional AUDIOSILO_BRIDGE_AGENT_BACKEND and
                       AUDIOSILO_BRIDGE_GITHUB_TOKEN) and `agent` (:latest-agent, user
                       10002:10002, cap_drop ALL, no-new-privileges, read-only root,
                       tmpfs /tmp, its own internet-only network, the credential
                       variables by NAME only, stop_grace_period 20s). The `staging`
                       volume is the only one both mount, outside /bridge-data (whose
                       first-start chown would otherwise reach it); `agent-config` is
                       the agent's /agent.
```

**Dependency direction** (transport-only rule): `cmd -> {app, agentrun, logring,
config}`; `app -> {api, auth, config, jobs, link, logring, mapping, secrets, store,
verify, web, generate, agentbox, agentproto, agent}` plus the engine it wires (asr,
contrib, events, metaops, pipeline, scheduler, state, supervisor, toolfetch);
`api -> {auth, jobs, link, logring, mapping, store}` (types only; it calls the
injected Backend); `verify -> {jobs, link, mapping}`; `jobs -> {link, store}`;
`mapping -> link`; `link` is a leaf. `generate -> {jobs, link, mapping, store,
scheduler, state, pipeline, contrib, chapterdetect, audio, scratch, metaops, ebook, agent, fsutil,
readalong}` (+ the meta
module's extract/model), its engine and server reached through its own Engine and
Server interfaces. The agent container: `agentproto` is a leaf (standard library
only); `agentbox -> {agent, agentproto}`; `agentrun -> {agent, agentproto,
fsutil}`; neither imports the other, and neither the pipeline nor the scheduler
knows about either (agentbox is just an agent.Runner). The engine:
`scheduler -> {store, state, eta, events, scratch}`; `eta -> state` (pure, imported BY
scheduler - never the reverse);
`chapterdetect -> audio` (its number words and Silence only); `readalong -> epubcfi`
(+ x/text) and `epubcfi -> {x/net/html, x/text}`, both pure leaves of the engine;
`scratch -> readalong` (its work-dir names only); `pipeline -> {audio, asr, transcript, ebook, qa, spelling, agent, repair, toolfetch, chapterdetect,
readalong, scratch, secrets, fsutil, metaops, contrib, pricing, store, state, scheduler}` (metaops since M7's
contributing stage, and the spelling stage's series glossary); `contrib -> {store, state, accounts}`
(+ the meta module's model/pack; accounts for ErrUnrefreshed alone, the sign-in reached
through contrib's DeviceTokens interface, so accounts never imports contrib); `agent`/`repair` are leaf helpers (no
scheduler/store deps; `repair -> qa` for the shared Python-compat gram/repr helpers
and the shared clip-floor predicate `qa.ClipStartInRange`/`qa.ClipStartFloorSec`);
`state` is pure. Handlers marshal DTOs and call into the injected packages; they
hold no logic (state transitions live in `state`, dispatch in `scheduler`).
