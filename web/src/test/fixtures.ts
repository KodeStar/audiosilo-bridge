import type { Engine, Library, ServerJob, Settings, Status, Worker } from '@/api/types';

export function statusFixture(over: Partial<Status> = {}): Status {
  return {
    version: '0.1.0',
    pairing: { paired: false, server_url: '', server_id: '', server_name: '', paired_at: '' },
    online: false,
    last_heartbeat: null,
    last_error: '',
    kinds: ['bridge.verify'],
    lanes: [{ name: 'light', capacity: 2, running: 0 }],
    secret_key: 'key file',
    public_url: '',
    server_url_default: 'http://audiosilo:8080',
    ...over,
  };
}

export function pairedStatus(over: Partial<Status> = {}): Status {
  return statusFixture({
    pairing: {
      paired: true,
      server_url: 'http://audiosilo:8080',
      server_id: 'srv-1',
      server_name: 'Home',
      paired_at: '2026-10-15T10:00:00Z',
    },
    online: true,
    last_heartbeat: new Date().toISOString(),
    ...over,
  });
}

export function libraryFixture(over: Partial<Library> = {}): Library {
  return {
    library: { id: 1, name: 'Audiobooks', root: '/audiobooks' },
    mount_path: '/audiobooks',
    configured: false,
    status: {
      library_id: 1,
      mount_path: '/audiobooks',
      mapped: true,
      writable: false,
      checked_path: 'Author/Book',
      error: '',
    },
    checked_at: '2026-10-15T10:00:00.123456789Z',
    locked: false,
    ...over,
  };
}

export function jobFixture(over: Partial<ServerJob> = {}): ServerJob {
  return {
    id: 7,
    kind: 'bridge.verify',
    library_id: 1,
    path: 'Author/Book',
    params: {},
    attempt: 1,
    server_id: 'srv-1',
    state: 'running',
    progress: { step: 'stat', message: 'Comparing the files', done: 1, total: 4 },
    result: {},
    error: '',
    reported: false,
    claimed_at: '2026-10-15T10:00:00.000000000Z',
    updated_at: '2026-10-15T10:00:01.000000000Z',
    finished_at: null,
    ...over,
  };
}

export function settingsFixture(over: Partial<Settings> = {}): Settings {
  return {
    public_url: '',
    mapping_check_minutes: 10,
    asr_worker_fallback: 'local',
    locked: [],
    ...over,
  };
}

// A sidecars.generate job running in the engine's asr stage.
export function generateJobFixture(over: Partial<ServerJob> = {}): ServerJob {
  return jobFixture({
    id: 12,
    kind: 'sidecars.generate',
    params: { source: 'auto', contribute: 'local' },
    progress: {
      step: 'asr',
      message: 'Transcribing (12 of 40)',
      done: 12,
      total: 40,
      stage: 'asr',
      eta_seconds: 5400,
      cost_usd: 3.42,
      source: 'audio',
    },
    ...over,
  });
}

export function engineFixture(over: Partial<Engine> = {}): Engine {
  return {
    started: true,
    agent: {
      backend: 'claude',
      available: true,
      detail: '',
      version: '0.2.0',
      slots: 1,
      heartbeat_at: '2026-10-15T10:00:00Z',
      clis: { claude: { version: '2.1.300', credential: 'oauth_token' } },
      subscription: true,
      staging_dir: '/bridge-data/staging',
    },
    asr: {
      backend: 'whisper-cpp',
      available: true,
      device: 'cpu',
      detail: '',
      worker_fallback: 'local',
      waiting: false,
    },
    monthly_budget_usd: 100,
    month_spent_usd: 12.3,
    book_budget_usd: 75,
    transcript_retention_days: 30,
    ...over,
  };
}

export function workerFixture(over: Partial<Worker> = {}): Worker {
  return {
    id: 1,
    name: 'Mac Studio',
    created_at: '2026-10-16T09:00:00.000000000Z',
    last_seen_at: '2026-10-16T10:00:00.000000000Z',
    version: '0.2.0',
    platform: 'darwin/arm64',
    backend: 'mlx-whisper',
    device: 'metal',
    model: 'mlx-community/whisper-large-v3-turbo',
    segments: 49,
    audio_seconds: 40100,
    decode_seconds: 2005,
    connected: true,
    task: '',
    ...over,
  };
}
