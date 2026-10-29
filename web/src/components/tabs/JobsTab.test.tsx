import { describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { JobsTab } from './JobsTab';
import { ApiClient } from '@/api/client';
import { lastCall, mockFetch } from '@/test/fetchMock';
import {
  describeJobFixture,
  generateJobFixture,
  jobFixture,
  libraryFixture,
} from '@/test/fixtures';

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

  it("names a job's library as the Libraries tab does", async () => {
    mockFetch({
      'GET /api/jobs': { body: { jobs: [jobFixture()] } },
      'GET /api/libraries': { body: { libraries: [libraryFixture()] } },
    });
    render(<JobsTab client={api()} />);
    const card = await screen.findByRole('article', { name: 'Job 7' });
    expect(await within(card).findByText('Audiobooks · Author/Book')).toBeInTheDocument();
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

  it('shows a running sidecars job: stage, count, ETA, spend and source', async () => {
    mockFetch({ 'GET /api/jobs': { body: { jobs: [generateJobFixture()] } } });
    render(<JobsTab client={api()} />);
    const card = await screen.findByRole('article', { name: 'Job 12' });
    expect(within(card).getByText('Running')).toBeInTheDocument();
    expect(within(card).getByText('Transcribing (12 of 40)')).toBeInTheDocument();
    expect(
      within(card).getByText('about 1 h 30 min left - $3.42 spent - from the audio'),
    ).toBeInTheDocument();
    expect(within(card).getByRole('progressbar', { name: 'Job 12 progress' })).toHaveAttribute(
      'aria-valuenow',
      '30',
    );
    expect(within(card).queryByRole('button', { name: 'Retry' })).toBeNull();
    expect(within(card).getByRole('link', { name: '#12' })).toHaveAttribute('href', '#/jobs/12');
  });

  it('shows a parked sidecars job with its advice, and retries it', async () => {
    let retried = false;
    const parked = generateJobFixture({
      error: 'agent backend unavailable',
      progress: {
        step: 'fact_pass',
        message: 'Needs attention: Reading for the facts',
        stage: 'fact_pass',
        cost_usd: 1.5,
        source: 'ebook',
        park: {
          code: 'agent_unavailable',
          hint: 'Check the agent container is running and logged in.',
        },
      },
    });
    const { calls } = mockFetch({
      'GET /api/jobs': () => ({ body: { jobs: [retried ? generateJobFixture() : parked] } }),
      'POST /api/jobs/12/retry': () => {
        retried = true;
        return { status: 202 };
      },
    });
    render(<JobsTab client={api()} />);
    const card = await screen.findByRole('article', { name: 'Job 12' });
    expect(within(card).getByText('Needs attention')).toBeInTheDocument();
    // The park's code is for the log: the page says it in words.
    expect(within(card).queryByText(/agent_unavailable/)).toBeNull();
    expect(within(card).getByText('agent backend unavailable')).toBeInTheDocument();
    expect(
      within(card).getByText('Check the agent container is running and logged in.'),
    ).toBeInTheDocument();
    expect(within(card).getByText('$1.50 spent - from the ebook')).toBeInTheDocument();

    await userEvent.click(within(card).getByRole('button', { name: 'Retry' }));
    expect(await within(card).findByText('Running')).toBeInTheDocument();
    expect(within(card).queryByRole('button', { name: 'Retry' })).toBeNull();
    expect(lastCall(calls, 'POST', '/api/jobs/12/retry')).toBeDefined();
  });

  it('words a park whose message is only its code, and names who asks for jobs', async () => {
    mockFetch({
      'GET /api/jobs': {
        body: {
          jobs: [
            generateJobFixture({
              error: 'asr_worker_waiting',
              progress: {
                step: 'transcribing',
                park: { code: 'asr_worker_waiting', hint: 'Start a worker.' },
              },
            }),
          ],
        },
      },
    });
    render(<JobsTab client={api()} />);
    const card = await screen.findByRole('article', { name: 'Job 12' });
    expect(within(card).queryByText(/asr_worker_waiting/)).toBeNull();
    expect(within(card).getByText('Start a worker.')).toBeInTheDocument();
    expect(screen.getByText(/a listener in the player/)).toBeInTheDocument();
    expect(screen.queryByText(/Jobs are started from/)).toBeNull();
  });

  it("shows a done sidecars job's result", async () => {
    mockFetch({
      'GET /api/jobs': {
        body: {
          jobs: [
            generateJobFixture({
              state: 'done',
              reported: true,
              finished_at: '2026-10-15T12:00:00Z',
              result: {
                library_id: 1,
                path: 'Author/Book',
                source: 'audio',
                files: ['characters.json', 'recaps.json', 'bridge.json'],
                characters: 41,
                recaps: 38,
                cost_usd: 12.5,
                work_id: 'the-book',
                contributed: {
                  characters: 'https://github.com/KodeStar/audiosilo-meta-community/issues/12',
                },
                description: false,
                description_error: 'the agent account hit its usage limit',
              },
            }),
          ],
        },
      },
    });
    render(<JobsTab client={api()} />);
    const card = await screen.findByRole('article', { name: 'Job 12' });
    await userEvent.click(within(card).getByText('Details'));
    expect(
      within(card).getByText('41 characters and 38 recaps from the audio, $12.50'),
    ).toBeInTheDocument();
    expect(within(card).getByText(/characters.json, recaps.json, bridge.json/)).toBeInTheDocument();
    expect(within(card).getByRole('link', { name: /issues\/12/ })).toBeInTheDocument();
    expect(
      within(card).getByText('No description: the agent account hit its usage limit'),
    ).toBeInTheDocument();
  });

  it('shows a description job running, then its result', async () => {
    mockFetch({ 'GET /api/jobs': { body: { jobs: [describeJobFixture()] } } });
    const { unmount } = render(<JobsTab client={api()} />);
    const card = await screen.findByRole('article', { name: 'Job 14' });
    expect(within(card).getByText('Description')).toBeInTheDocument();
    expect(within(card).getByText('Writing a description')).toBeInTheDocument();
    expect(within(card).getByText(/\$0.04 spent/)).toBeInTheDocument();
    unmount();

    mockFetch({
      'GET /api/jobs': {
        body: {
          jobs: [
            describeJobFixture({
              state: 'done',
              finished_at: '2026-10-16T12:00:00Z',
              result: {
                library_id: 1,
                path: 'Author/Book',
                description: true,
                files: ['description.json', 'bridge.json'],
                cost_usd: 0.25,
                checked_against: 'none',
              },
            }),
          ],
        },
      },
    });
    render(<JobsTab client={api()} />);
    const done = await screen.findByRole('article', { name: 'Job 14' });
    await userEvent.click(within(done).getByText('Details'));
    expect(
      within(done).getByText(
        'A description, $0.25, not checked: the source text was already removed',
      ),
    ).toBeInTheDocument();
    expect(within(done).getByText(/description.json, bridge.json/)).toBeInTheDocument();
  });

  it("opens one job from the console's link", async () => {
    const { calls } = mockFetch({
      'GET /api/jobs/12': { body: generateJobFixture() },
      'GET /api/jobs': { body: { jobs: [] } },
    });
    render(<JobsTab client={api()} focusId={12} />);
    expect(await screen.findByRole('article', { name: 'Job 12' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'All jobs' })).toHaveAttribute('href', '#jobs');
    expect(lastCall(calls, 'GET', '/api/jobs')).toBeUndefined();
  });

  it('says so when the linked job is not here', async () => {
    mockFetch({
      'GET /api/jobs/99': { status: 404, body: { error: 'this Bridge has no job 99' } },
    });
    render(<JobsTab client={api()} focusId={99} />);
    expect(await screen.findByText('this Bridge has no job 99')).toBeInTheDocument();
  });
});
