// The Bridge page's wire format, mirrored by hand from the Go side (no codegen):
// internal/api/api.go (the routes and envelopes), internal/app/conn.go (Pairing),
// internal/mapping/mapping.go (Entry, FileCheck), internal/link/link.go
// (Library, LibraryStatus, Worker), internal/store/serverjobs.go (ServerJob),
// internal/jobs/jobs.go (Lane, Progress), internal/logring/logring.go (Entry),
// internal/verify/verify.go (Result) and internal/generate (the sidecars.generate
// progress and Result). A change there is a change here.

// POST /api/login
export interface LoginResponse {
  token: string;
}

// app.Pairing (inside Status.pairing).
export interface Pairing {
  paired: boolean;
  server_url: string;
  server_id: string;
  server_name: string;
  // RFC 3339, "" when never paired.
  paired_at: string;
  // Why a Bridge that was paired no longer is (omitted when none).
  unpaired_reason?: string;
}

// jobs.Lane
export interface Lane {
  name: string;
  capacity: number;
  running: number;
}

// secrets.KeySource
export type SecretKeySource = 'environment' | 'key file' | 'new key file';

// GET /api/status (also the reply of POST /api/pair and POST /api/unpair).
export interface Status {
  version: string;
  pairing: Pairing;
  // The last heartbeat went through less than two minutes ago.
  online: boolean;
  last_heartbeat: string | null;
  last_error: string;
  kinds: string[];
  lanes: Lane[];
  secret_key: SecretKeySource;
  public_url: string;
  server_url_default: string;
}

// link.Library
export interface ServerLibrary {
  id: number;
  name: string;
  root: string;
}

// link.LibraryStatus
export interface LibraryStatus {
  library_id: number;
  mount_path: string;
  mapped: boolean;
  writable: boolean;
  checked_path: string;
  error: string;
}

// api.Library (mapping.Entry plus locked).
export interface Library {
  library: ServerLibrary;
  // Where the Bridge sees the library.
  mount_path: string;
  // true: set in the Bridge's config; false: the server's root path is used.
  configured: boolean;
  status: LibraryStatus | null;
  checked_at: string | null;
  // AUDIOSILO_BRIDGE_LIBRARIES sets the mappings.
  locked: boolean;
}

// GET /api/libraries, PUT /api/libraries/{id}
export interface LibrariesResponse {
  libraries: Library[];
}

// The settings keys that can be locked by the environment (config.Key*).
export type SettingKey =
  'public_url' | 'mapping.check_minutes' | 'libraries' | 'server.url' | 'asr.worker_fallback';

// What a segment does when no ASR worker is connected (asr.worker_fallback).
export type WorkerFallback = 'local' | 'wait';

// GET/PUT /api/settings
export interface Settings {
  public_url: string;
  mapping_check_minutes: number;
  asr_worker_fallback: WorkerFallback;
  // The keys the environment sets.
  locked: SettingKey[];
}

export interface SettingsUpdate {
  public_url: string;
  mapping_check_minutes: number;
  asr_worker_fallback: WorkerFallback;
}

// The states a job can have on the Bridge (store.Job*).
export type JobState = 'running' | 'done' | 'failed' | 'needs_attention' | 'cancelled' | 'lost';

// A park (a job waiting on something: the engine's park codes, plus the
// Bridge's own book_not_writable and library_not_writable).
export interface Park {
  code: string;
  // What it needs, in words ("" for a code with no advice).
  hint: string;
  // RFC 3339: an availability park resumes by itself then.
  retry_at?: string;
}

// The sources a sidecars.generate run reads.
export type GenerateSource = 'audio' | 'ebook';

// jobs.Progress, with the fields a kind adds beside them (Extra; sidecars.generate
// sends stage, eta_seconds, cost_usd, source and, while parked, park;
// sidecars.describe the same but eta_seconds, its stage "describing";
// chapters.detect the same but cost_usd).
export interface Progress {
  step?: string;
  message?: string;
  done?: number;
  total?: number;
  // The engine's stage (state.State).
  stage?: string;
  // The engine's whole-book ETA, in seconds.
  eta_seconds?: number;
  // Agent spend so far, in US dollars.
  cost_usd?: number;
  source?: GenerateSource;
  park?: Park;
}

// generate.Result (a sidecars.generate job's result).
export interface GenerateResult {
  library_id: number;
  path: string;
  source: GenerateSource;
  // The names written in the book's .audiosilo/ folder.
  files: string[];
  characters: number;
  recaps: number;
  cost_usd: number;
  work_id: string;
  // Intake issue links, with contribute = issue only.
  contributed?: Record<string, string>;
  // Whether description.json was written with the others (absent from a Bridge
  // before Phase 1c); when it wasn't, why, in a few plain words.
  description?: boolean;
  description_error?: string;
}

// What a description's no-verbatim check measured against ("none": the source
// text was already removed: an audio book's transcripts, or an ebook's text).
export type CheckedAgainst = 'transcripts' | 'book text' | 'none';

// generate.DescribeJobResult (a sidecars.describe job's result).
export interface DescribeResult {
  library_id: number;
  path: string;
  description: boolean;
  // The names written in the book's .audiosilo/ folder.
  files: string[];
  cost_usd: number;
  checked_against: CheckedAgainst;
}

// generate.ChaptersJobResult (a chapters.detect job's result).
export interface ChaptersResult {
  library_id: number;
  path: string;
  // How many chapters the narration gave the book.
  chapters: number;
  // The names written in the book's .audiosilo/ folder (none for one chapter).
  files: string[];
  source: 'audio';
}

// store.ServerJob
export interface ServerJob {
  id: number;
  kind: string;
  library_id: number | null;
  path: string | null;
  params: unknown;
  attempt: number;
  // The server that handed the job out ("" on jobs from before Phase 1a).
  server_id: string;
  // Typed loosely (any string still fits) so a state added later still renders.
  state: JobState | (string & {});
  progress: Progress | null;
  result: unknown;
  // A finished job's error, or, while it runs parked, the park's message.
  error: string;
  reported: boolean;
  claimed_at: string;
  updated_at: string;
  finished_at: string | null;
}

// GET /api/jobs
export interface JobsResponse {
  jobs: ServerJob[];
  // Present when the page was full: pass it as ?before= for older jobs.
  next_before?: number;
}

// mapping.FileCheck
export interface FileCheck {
  path: string;
  size: number;
  expected_size: number;
  ok: boolean;
  error?: string;
}

// verify.Result (a bridge.verify job's result).
export interface VerifyResult {
  library_id: number;
  mount_path: string;
  path: string;
  files: FileCheck[];
  writable: boolean;
  writable_error?: string;
  elapsed_ms: number;
}

// logring.Entry
export interface LogEntry {
  id: number;
  time: string;
  level: string;
  message: string;
  attrs?: Record<string, string>;
}

export type LogLevel = 'INFO' | 'WARN' | 'ERROR';

// GET /api/logs
export interface LogsResponse {
  entries: LogEntry[];
}

// api.Worker (store.Worker + what the Bridge knows now): a paired ASR worker.
export interface Worker {
  id: number;
  name: string;
  created_at: string;
  // "" until it first connected.
  last_seen_at: string;
  version: string;
  // GOOS/GOARCH, e.g. darwin/arm64.
  platform: string;
  // The speech recognition it runs (mlx-whisper on metal, whisper-cpp on cpu or cuda).
  backend: string;
  device: string;
  model: string;
  // The segments it finished, their audio and the time it took to decode them.
  segments: number;
  audio_seconds: number;
  decode_seconds: number;
  connected: boolean;
  // The segment it decodes now ("" when idle).
  task: string;
}

// GET /api/workers
export interface WorkersResponse {
  workers: Worker[];
}

// POST /api/workers: the token is shown this once.
export interface NewWorker {
  worker: Worker;
  token: string;
}

// api.EngineCLI: one agent CLI in the agent container (the kind of credential,
// never the credential).
export interface EngineCLI {
  version: string;
  // "oauth_token" | "api_key" (claude), "login" | "api_key" (codex), "" for none.
  credential: string;
}

// api.EngineAgent: the agent container as the Bridge last saw it.
export interface EngineAgent {
  backend: string;
  // The container answers and the backend's CLI has a credential.
  available: boolean;
  detail: string;
  version: string;
  slots: number;
  heartbeat_at: string | null;
  clis: Record<string, EngineCLI>;
  // The credential is a subscription login (costs are notional).
  subscription: boolean;
  staging_dir: string;
}

// api.EngineASR: where speech recognition runs now (backend "worker" and
// device "remote" while a worker is connected).
export interface EngineASR {
  backend: string;
  available: boolean;
  device: string;
  detail: string;
  worker_fallback: WorkerFallback;
  // No segment can run now: books wait for a worker.
  waiting: boolean;
}

// GET /api/engine
export interface Engine {
  // The engine is up (its first start may be downloading ffmpeg).
  started: boolean;
  agent: EngineAgent;
  asr: EngineASR;
  // 0 = no monthly cap.
  monthly_budget_usd: number;
  month_spent_usd: number;
  book_budget_usd: number;
  // A finished book's transcripts are deleted this long after its job (0 = at once).
  transcript_retention_days: number;
}
