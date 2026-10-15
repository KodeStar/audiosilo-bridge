import { describe, expect, it, vi } from 'vitest';
import { ApiClient, ApiError, LAN_ONLY_MESSAGE } from './client';
import { mockFetch } from '@/test/fetchMock';

function client(token: string | null = 'tok') {
  const onSignedOut = vi.fn();
  const onLanBlocked = vi.fn();
  return {
    c: new ApiClient({ getToken: () => token, onSignedOut, onLanBlocked }),
    onSignedOut,
    onLanBlocked,
  };
}

describe('ApiClient', () => {
  it('sends the bearer token', async () => {
    const { calls } = mockFetch({ 'GET /api/status': { body: { version: '1' } } });
    const { c } = client('abc');
    await c.status();
    expect(calls[0].headers.Authorization).toBe('Bearer abc');
  });

  it('sends no token to sign in', async () => {
    const { calls } = mockFetch({ 'POST /api/login': { body: { token: 't' } } });
    const { c } = client('abc');
    expect(await c.login('pw')).toEqual({ token: 't' });
    expect(calls[0].headers.Authorization).toBeUndefined();
    expect(calls[0].body).toEqual({ password: 'pw' });
  });

  it('maps a 401 to signed out', async () => {
    mockFetch({ 'GET /api/libraries': { status: 401, body: { error: 'sign in first' } } });
    const { c, onSignedOut } = client();
    await expect(c.libraries()).rejects.toMatchObject({ status: 401, message: 'sign in first' });
    expect(onSignedOut).toHaveBeenCalledOnce();
  });

  it('does not sign out on a wrong password at sign in', async () => {
    mockFetch({ 'POST /api/login': { status: 401, body: { error: 'wrong password' } } });
    const { c, onSignedOut } = client(null);
    await expect(c.login('x')).rejects.toBeInstanceOf(ApiError);
    expect(onSignedOut).not.toHaveBeenCalled();
  });

  it('surfaces {error} and the code', async () => {
    mockFetch({
      'PUT /api/settings': {
        status: 409,
        body: { error: 'set by the environment', code: 'locked' },
      },
    });
    const { c } = client();
    await expect(
      c.saveSettings({ public_url: 'http://x', mapping_check_minutes: 5 }),
    ).rejects.toMatchObject({ status: 409, message: 'set by the environment', code: 'locked' });
  });

  it('falls back to the status without a JSON body', async () => {
    mockFetch({ 'GET /api/status': () => ({ status: 500, body: undefined }) });
    const { c } = client();
    await expect(c.status()).rejects.toMatchObject({ message: 'Request failed (500)' });
  });

  it("maps the LAN guard's 403 to the LAN-only screen", async () => {
    mockFetch({ 'GET /api/status': { status: 403, body: { error: 'nope', code: 'lan_only' } } });
    const { c, onLanBlocked } = client();
    await expect(c.status()).rejects.toMatchObject({
      status: 403,
      message: LAN_ONLY_MESSAGE,
      code: 'lan_only',
    });
    expect(onLanBlocked).toHaveBeenCalledOnce();
  });

  it('keeps a 403 without the lan_only code as the answer', async () => {
    mockFetch({ 'GET /api/status': { status: 403, body: { error: 'nope' } } });
    const { c, onLanBlocked } = client();
    await expect(c.status()).rejects.toMatchObject({ status: 403, message: 'nope' });
    expect(onLanBlocked).not.toHaveBeenCalled();
  });

  it("lets the LAN guard's 403 win on the password route too", async () => {
    mockFetch({
      'POST /api/password': { status: 403, body: { error: 'nope', code: 'lan_only' } },
    });
    const { c, onLanBlocked } = client();
    await expect(c.changePassword('old', 'newer-password')).rejects.toMatchObject({
      message: LAN_ONLY_MESSAGE,
    });
    expect(onLanBlocked).toHaveBeenCalledOnce();
  });

  it("keeps the password change's 403 as its own answer", async () => {
    const { calls } = mockFetch({
      'POST /api/password': { status: 403, body: { error: 'the current password is wrong' } },
    });
    const { c, onLanBlocked, onSignedOut } = client();
    await expect(c.changePassword('old', 'newer-password')).rejects.toMatchObject({
      status: 403,
      message: 'the current password is wrong',
    });
    expect(onLanBlocked).not.toHaveBeenCalled();
    expect(onSignedOut).not.toHaveBeenCalled();
    expect(calls[0].body).toEqual({ current: 'old', new: 'newer-password' });
  });

  it('builds the jobs and logs queries', async () => {
    const { calls } = mockFetch({
      'GET /api/jobs': { body: { jobs: [] } },
      'GET /api/logs': { body: { entries: [] } },
    });
    const { c } = client();
    await c.jobs();
    await c.jobs(40, 50);
    await c.logs(12, 'WARN');
    expect(calls.map((x) => x.url)).toEqual([
      '/api/jobs',
      '/api/jobs?before=40&limit=50',
      '/api/logs?after=12&level=WARN',
    ]);
  });

  it('treats 202 and 204 as no body', async () => {
    mockFetch({
      'POST /api/jobs/7/cancel': { status: 202 },
      'POST /api/logout': { status: 204 },
    });
    const { c } = client();
    await expect(c.cancelJob(7)).resolves.toBeUndefined();
    await expect(c.logout()).resolves.toBeUndefined();
  });
});
