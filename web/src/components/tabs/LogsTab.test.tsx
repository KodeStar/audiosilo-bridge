import { describe, expect, it, vi } from 'vitest';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { LogsTab } from './LogsTab';
import { ApiClient } from '@/api/client';
import { mockFetch } from '@/test/fetchMock';

describe('LogsTab', () => {
  it('shows lines with attrs, follows with after and filters by level', async () => {
    const { calls } = mockFetch({
      'GET /api/logs': ({ url }) => {
        const q = new URL(url, 'http://x').searchParams;
        if (q.get('level') === 'ERROR') {
          return {
            body: {
              entries: [{ id: 5, time: '2026-10-15T10:00:00Z', level: 'ERROR', message: 'broke' }],
            },
          };
        }
        if (q.get('after') === '0') {
          return {
            body: {
              entries: [
                {
                  id: 3,
                  time: '2026-10-15T10:00:00Z',
                  level: 'INFO',
                  message: 'bridge: paired',
                  attrs: { server: 'http://audiosilo:8080', server_name: 'Home' },
                },
              ],
            },
          };
        }
        return { body: { entries: [] } };
      },
    });
    render(<LogsTab client={new ApiClient({ getToken: () => 't', onSignedOut: vi.fn() })} />);

    expect(await screen.findByText('bridge: paired')).toBeInTheDocument();
    expect(screen.getByText(/server=http:\/\/audiosilo:8080 server_name=Home/)).toBeInTheDocument();

    await userEvent.selectOptions(screen.getByLabelText('Level'), 'ERROR');
    expect(await screen.findByText('broke')).toBeInTheDocument();
    expect(screen.queryByText('bridge: paired')).toBeNull();
    await waitFor(() =>
      expect(calls.some((c) => c.url === '/api/logs?after=0&level=ERROR')).toBe(true),
    );
  });

  it('polls with after set to the last id', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { calls } = mockFetch({
      'GET /api/logs': ({ url }) =>
        url.includes('after=0')
          ? {
              body: {
                entries: [{ id: 11, time: '2026-10-15T10:00:00Z', level: 'INFO', message: 'hi' }],
              },
            }
          : { body: { entries: [] } },
    });
    render(<LogsTab client={new ApiClient({ getToken: () => 't', onSignedOut: vi.fn() })} />);
    expect(await screen.findByText('hi')).toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3100);
    });
    await waitFor(() =>
      expect(calls.some((c) => c.url === '/api/logs?after=11&level=INFO')).toBe(true),
    );
  });
});
