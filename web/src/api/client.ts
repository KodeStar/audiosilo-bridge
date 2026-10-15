import type {
  JobsResponse,
  LibrariesResponse,
  LoginResponse,
  LogLevel,
  LogsResponse,
  Settings,
  SettingsUpdate,
  Status,
  WorkersResponse,
} from './types';

// ApiError is any non-2xx answer; message is the server's {error} sentence when
// there is one.
export class ApiError extends Error {
  readonly status: number;
  readonly code: string | undefined;
  constructor(status: number, message: string, code?: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }
}

// The message the LAN guard's 403 is shown as.
export const LAN_ONLY_MESSAGE = 'This page answers only the local network.';

// The code the LAN guard marks its 403 with (any other 403, like the password
// change's wrong current password, is the call's own answer).
export const LAN_ONLY_CODE = 'lan_only';

export interface ApiClientOptions {
  // The current session token, or null when signed out.
  getToken: () => string | null;
  // A 401 on an authed call: the session is gone.
  onSignedOut: () => void;
  // The LAN guard refused this address.
  onLanBlocked?: () => void;
}

interface RequestOptions {
  method?: string;
  body?: unknown;
  // false: no token is sent (sign in).
  auth?: boolean;
}

export class ApiClient {
  private readonly getToken: () => string | null;
  private readonly onSignedOut: () => void;
  private readonly onLanBlocked: () => void;

  constructor(opts: ApiClientOptions) {
    this.getToken = opts.getToken;
    this.onSignedOut = opts.onSignedOut;
    this.onLanBlocked = opts.onLanBlocked ?? (() => {});
  }

  private async request<T>(path: string, opts: RequestOptions = {}): Promise<T> {
    const { method = 'GET', body, auth = true } = opts;
    const headers: Record<string, string> = {};
    if (auth) {
      const token = this.getToken();
      if (token) headers.Authorization = `Bearer ${token}`;
    }
    let payload: string | undefined;
    if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
      payload = JSON.stringify(body);
    }
    const res = await fetch(path, { method, headers, body: payload });
    if (!res.ok) {
      const { error, code } = await readError(res);
      if (res.status === 401 && auth) this.onSignedOut();
      if (res.status === 403 && code === LAN_ONLY_CODE) {
        this.onLanBlocked();
        throw new ApiError(403, LAN_ONLY_MESSAGE, code);
      }
      throw new ApiError(res.status, error, code);
    }
    // 202 and 204 carry no body.
    const text = await res.text();
    return (text === '' ? undefined : JSON.parse(text)) as T;
  }

  // --- session ---
  login(password: string): Promise<LoginResponse> {
    return this.request('/api/login', { method: 'POST', body: { password }, auth: false });
  }

  logout(): Promise<void> {
    return this.request('/api/logout', { method: 'POST' });
  }

  // 403 = the current password is wrong; 400 = the new one is too short.
  changePassword(current: string, next: string): Promise<void> {
    return this.request('/api/password', { method: 'POST', body: { current, new: next } });
  }

  // --- status and pairing ---
  status(): Promise<Status> {
    return this.request('/api/status');
  }

  pair(serverUrl: string, code: string): Promise<Status> {
    return this.request('/api/pair', { method: 'POST', body: { server_url: serverUrl, code } });
  }

  unpair(): Promise<Status> {
    return this.request('/api/unpair', { method: 'POST' });
  }

  // --- libraries ---
  libraries(): Promise<LibrariesResponse> {
    return this.request('/api/libraries');
  }

  // "" = use the server's root path.
  setMount(id: number, mountPath: string): Promise<LibrariesResponse> {
    return this.request(`/api/libraries/${id}`, { method: 'PUT', body: { mount_path: mountPath } });
  }

  checkLibraries(): Promise<void> {
    return this.request('/api/libraries/check', { method: 'POST' });
  }

  // --- settings ---
  settings(): Promise<Settings> {
    return this.request('/api/settings');
  }

  saveSettings(s: SettingsUpdate): Promise<Settings> {
    return this.request('/api/settings', { method: 'PUT', body: s });
  }

  // --- jobs ---
  jobs(before?: number, limit?: number): Promise<JobsResponse> {
    const q = new URLSearchParams();
    if (before) q.set('before', String(before));
    if (limit) q.set('limit', String(limit));
    const qs = q.toString();
    return this.request(`/api/jobs${qs ? `?${qs}` : ''}`);
  }

  // 404 = the job isn't running here.
  cancelJob(id: number): Promise<void> {
    return this.request(`/api/jobs/${id}/cancel`, { method: 'POST' });
  }

  // --- logs and workers ---
  logs(after: number, level: LogLevel): Promise<LogsResponse> {
    const q = new URLSearchParams({ after: String(after), level });
    return this.request(`/api/logs?${q}`);
  }

  workers(): Promise<WorkersResponse> {
    return this.request('/api/workers');
  }
}

// readError reads an {error, code} body, falling back to the status.
async function readError(res: Response): Promise<{ error: string; code?: string }> {
  try {
    const json: unknown = JSON.parse(await res.text());
    if (typeof json === 'object' && json !== null) {
      const { error, code } = json as { error?: unknown; code?: unknown };
      if (typeof error === 'string' && error !== '') {
        return { error, code: typeof code === 'string' ? code : undefined };
      }
    }
  } catch {
    // Not JSON: fall through.
  }
  return { error: `Request failed (${res.status})` };
}

// errorText words any thrown value for the page.
export function errorText(err: unknown, fallback: string): string {
  if (err instanceof ApiError) return err.message;
  return fallback;
}
