// The Bridge page's wire format, mirrored by hand from the Go side (no codegen):
// internal/api/api.go (the routes and envelopes), internal/app/conn.go (Pairing),
// internal/mapping/mapping.go (Entry, FileCheck), internal/link/link.go
// (Library, LibraryStatus, Worker), internal/store/serverjobs.go (ServerJob),
// internal/jobs/jobs.go (Lane, Progress), internal/logring/logring.go (Entry)
// and internal/verify/verify.go (Result). A change there is a change here.

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
export type SettingKey = 'public_url' | 'mapping.check_minutes' | 'libraries' | 'server.url';

// GET/PUT /api/settings
export interface Settings {
  public_url: string;
  mapping_check_minutes: number;
  // The keys the environment sets.
  locked: SettingKey[];
}

export interface SettingsUpdate {
  public_url: string;
  mapping_check_minutes: number;
}

// The states a job can have on the Bridge (store.Job*).
export type JobState = 'running' | 'done' | 'failed' | 'needs_attention' | 'cancelled' | 'lost';

// jobs.Progress
export interface Progress {
  step?: string;
  message?: string;
  done?: number;
  total?: number;
}

// store.ServerJob
export interface ServerJob {
  id: number;
  kind: string;
  library_id: number | null;
  path: string | null;
  params: unknown;
  attempt: number;
  // Typed loosely (any string still fits) so a state added later still renders.
  state: JobState | (string & {});
  progress: Progress | null;
  result: unknown;
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

// link.Worker
export interface Worker {
  name: string;
  version: string;
  platform: string;
  last_seen: string;
}

// GET /api/workers
export interface WorkersResponse {
  workers: Worker[];
}
