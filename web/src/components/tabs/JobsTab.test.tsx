import { describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { JobsTab } from './JobsTab';
import { ApiClient } from '@/api/client';
import { lastCall, mockFetch } from '@/test/fetchMock';
import { jobFixture } from '@/test/fixtures';

const api = () => new ApiClient({ getToken: () => 't', onSignedOut: vi.fn() });

describe('JobsTab', () => {
  it('cancels a running job', async () => {
    let cancelled = false;
    const { calls } = mockFetch({
      'GET /api/jobs': () => ({
        body: {
          jobs: [
            cancelled
              ? jobFixture({ state: 'cancelled', finished_at: '2026-10-15T10:00:05Z' })
              : jobFixture(),
          ],
        },
      }),
      'POST /api/jobs/7/cancel': () => {
        cancelled = true;
        return { status: 202 };
      },
    });
    render(<JobsTab client={api()} />);

    const card = await screen.findByRole('article', { name: 'Job 7' });
    expect(within(card).getByText('Running')).toBeInTheDocument();
    expect(within(card).getByText('stat: Comparing the files (1/4)')).toBeInTheDocument();
    await userEvent.click(within(card).getByRole('button', { name: 'Cancel' }));

    expect(await within(card).findByText('Cancelled')).toBeInTheDocument();
    expect(within(card).queryByRole('button', { name: 'Cancel' })).toBeNull();
    expect(within(card).getByText('Report pending')).toBeInTheDocument();
    expect(lastCall(calls, 'POST', '/api/jobs/7/cancel')).toBeDefined();
  });

  it('says so when the job is no longer running here', async () => {
    mockFetch({
      'GET /api/jobs': { body: { jobs: [jobFixture()] } },
      'POST /api/jobs/7/cancel': { status: 404, body: { error: "that job isn't running here" } },
    });
    render(<JobsTab client={api()} />);
    await userEvent.click(await screen.findByRole('button', { name: 'Cancel' }));
    expect(await screen.findByText("That job isn't running here any more.")).toBeInTheDocument();
  });

  it('shows a verify result as a table, a lost job, and pages back', async () => {
    const done = jobFixture({
      id: 9,
      state: 'done',
      reported: true,
      finished_at: '2026-10-15T10:00:02Z',
      result: {
        library_id: 1,
        mount_path: '/audiobooks',
        path: 'Author/Book',
        files: [
          { path: 'Author/Book/01.mp3', size: 2048, expected_size: 2048, ok: true },
          {
            path: 'Author/Book/02.mp3',
            size: 0,
            expected_size: 4096,
            ok: false,
            error: 'not found here',
          },
        ],
        writable: false,
        writable_error: 'permission denied',
        elapsed_ms: 1250,
      },
    });
    const { calls } = mockFetch({
      'GET /api/jobs': ({ url }) =>
        url.includes('before=8')
          ? { body: { jobs: [jobFixture({ id: 3, state: 'failed', error: 'boom' })] } }
          : { body: { jobs: [done, jobFixture({ id: 8, state: 'lost' })], next_before: 8 } },
    });
    render(<JobsTab client={api()} />);

    const card = await screen.findByRole('article', { name: 'Job 9' });
    expect(within(card).getByText('Reported')).toBeInTheDocument();
    await userEvent.click(within(card).getByText('Details'));
    const table = within(card).getByRole('table');
    expect(within(table).getByText('Author/Book/02.mp3')).toBeInTheDocument();
    expect(within(table).getByText('not found here')).toBeInTheDocument();
    expect(within(table).getByText('4.0 KB')).toBeInTheDocument();
    expect(within(card).getByText(/not writable: permission denied/)).toBeInTheDocument();
    expect(within(card).getByText(/took 1.3 s/)).toBeInTheDocument();

    const lost = screen.getByRole('article', { name: 'Job 8' });
    expect(within(lost).getAllByText('The server took it back').length).toBeGreaterThan(0);

    await userEvent.click(screen.getByRole('button', { name: 'Older' }));
    expect(await screen.findByText('boom')).toBeInTheDocument();
    expect(calls.some((c) => c.url === '/api/jobs?before=8&limit=50')).toBe(true);
    expect(screen.queryByRole('button', { name: 'Older' })).toBeNull();
  });
});
