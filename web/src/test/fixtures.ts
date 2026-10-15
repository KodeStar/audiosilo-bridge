import type { Library, ServerJob, Settings, Status } from '@/api/types';

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
  return { public_url: '', mapping_check_minutes: 10, locked: [], ...over };
}
