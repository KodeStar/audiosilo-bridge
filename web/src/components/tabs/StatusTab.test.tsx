import { describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { StatusTab } from './StatusTab';
import { ApiClient } from '@/api/client';
import { lastCall, mockFetch } from '@/test/fetchMock';
import { engineFixture, pairedStatus, setupFixture, statusFixture } from '@/test/fixtures';

const api = () => new ApiClient({ getToken: () => 't', onSignedOut: vi.fn() });

describe('StatusTab', () => {
  it('warns when podcast feeds may reach configured non-public networks', async () => {
    mockFetch({
      'GET /api/engine': { body: engineFixture() },
      'GET /api/status': { body: pairedStatus({ podcast_allow_networks: ['127.0.0.1/32'] }) },
    });
    render(<StatusTab client={api()} />);
    expect(await screen.findByText(/Podcast feeds can reach/)).toHaveTextContent('127.0.0.1/32');
  });

  it('shows no network warning with the default podcast guard', async () => {
    mockFetch({
      'GET /api/engine': { body: engineFixture() },
      'GET /api/status': { body: pairedStatus() },
    });
    render(<StatusTab client={api()} />);
    await screen.findByText('Version');
    expect(screen.queryByText(/Podcast feeds can reach/)).not.toBeInTheDocument();
  });
  it('leads with "Get set up" until the required items are done, each linking to its tab', async () => {
    mockFetch({
      'GET /api/engine': { body: engineFixture() },
      'GET /api/status': {
        body: pairedStatus({
          setup_complete: false,
          setup: setupFixture(
            {
              libraries: { done: false, detail: '1 of 2 libraries writable', library_ids: [3] },
              agent: {
                done: false,
                detail: "claude has no credential: set one on the Bridge's Accounts tab",
              },
              budget: { done: false, detail: '$100 a month' },
              github: { done: false },
            },
            { can_contribute: false, github_login: '' },
          ),
        }),
      },
    });
    render(<StatusTab client={api()} />);
    const card = await screen.findByRole('region', { name: 'Get set up' });
    expect(within(card).getByText('3 steps left')).toBeInTheDocument();
    expect(within(card).getByText('1 of 2 libraries writable')).toBeInTheDocument();
    const links = within(card)
      .getAllByRole('link')
      .map((a) => [a.textContent, a.getAttribute('href')]);
    expect(links).toEqual([
      ['Open Libraries', '#/libraries'],
      ['Open Accounts', '#/accounts'],
      ['Open Settings', '#/settings'],
      ['Open Accounts', '#/accounts'],
    ]);
    // Done items are listed, without a link.
    expect(within(card).getByText('Speech recognition')).toBeInTheDocument();
  });

  it('links Workers while speech recognition waits for a worker', async () => {
    mockFetch({
      'GET /api/engine': { body: engineFixture() },
      'GET /api/status': {
        body: pairedStatus({
          setup_complete: false,
          setup: setupFixture({ asr: { done: false, detail: 'waiting for an ASR worker' } }),
        }),
      },
    });
    render(<StatusTab client={api()} />);
    const card = await screen.findByRole('region', { name: 'Get set up' });
    const links = within(card)
      .getAllByRole('link')
      .map((a) => [a.textContent, a.getAttribute('href')]);
    expect(links).toEqual([
      ['Open Workers', '#/workers'],
      ['Open Settings', '#/settings'],
    ]);
  });

  it('shows no "Get set up" card once set up, the optional GitHub item aside', async () => {
    mockFetch({
      'GET /api/engine': { body: engineFixture() },
      'GET /api/status': {
        body: pairedStatus({
          setup: setupFixture({ github: { done: false } }),
          setup_complete: true,
        }),
      },
    });
    render(<StatusTab client={api()} />);
    await screen.findByText('Version');
    expect(screen.queryByRole('region', { name: 'Get set up' })).not.toBeInTheDocument();
  });

  it('pairs with the address and the code, and shows a 422 verbatim', async () => {
    const { calls } = mockFetch({
      'GET /api/engine': { body: engineFixture() },
      'GET /api/status': {
        body: statusFixture({
          pairing: {
            paired: false,
            server_url: '',
            server_id: '',
            server_name: '',
            paired_at: '',
            unpaired_reason: 'the server refused the token',
          },
        }),
      },
      'POST /api/pair': {
        status: 422,
        body: { error: 'the code is wrong, used or expired; make a new one' },
      },
    });
    render(<StatusTab client={api()} />);

    const address = await screen.findByLabelText('Server address');
    expect(address).toHaveValue('http://audiosilo:8080');
    expect(screen.getByText(/the server refused the token/)).toBeInTheDocument();

    await userEvent.type(screen.getByLabelText('Pairing code'), ' ABCD-1234 ');
    await userEvent.click(screen.getByRole('button', { name: 'Pair' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'the code is wrong, used or expired; make a new one',
    );
    expect(lastCall(calls, 'POST', '/api/pair')?.body).toEqual({
      server_url: 'http://audiosilo:8080',
      code: 'ABCD-1234',
    });
  });

  it('shows the pairing and lanes, and unpairs after a confirm', async () => {
    const { calls } = mockFetch({
      'GET /api/engine': { body: engineFixture() },
      'GET /api/status': { body: pairedStatus({ public_url: '' }) },
      'POST /api/unpair': { body: statusFixture() },
    });
    render(<StatusTab client={api()} />);

    expect(await screen.findByText('Home')).toBeInTheDocument();
    expect(screen.getByText('Online')).toBeInTheDocument();
    expect(screen.getByText(/0 of 2 running/)).toBeInTheDocument();
    expect(screen.getByText(/Set one in Settings/)).toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Unpair...' }));
    expect(screen.getByText(/Unpair it in the server's console too/)).toBeInTheDocument();
    expect(lastCall(calls, 'POST', '/api/unpair')).toBeUndefined();
    await userEvent.click(screen.getByRole('button', { name: 'Unpair' }));

    expect(await screen.findByLabelText('Pairing code')).toBeInTheDocument();
    expect(lastCall(calls, 'POST', '/api/unpair')).toBeDefined();
  });

  it('shows the engine: the agent container, the budget and the retention', async () => {
    mockFetch({
      'GET /api/status': { body: pairedStatus() },
      'GET /api/engine': {
        body: engineFixture({
          agent: {
            ...engineFixture().agent,
            available: false,
            detail: 'no heartbeat from the agent container',
          },
          month_spent_usd: 40,
          transcript_retention_days: 0,
        }),
      },
    });
    render(<StatusTab client={api()} />);
    const card = await screen.findByRole('region', { name: 'Characters and recaps' });
    expect(await within(card).findByText('Agent not ready')).toBeInTheDocument();
    expect(
      within(card).getByText('Not ready: no heartbeat from the agent container'),
    ).toBeInTheDocument();
    expect(within(card).getByText('claude 2.1.300: subscription login')).toBeInTheDocument();
    expect(within(card).getByText('$40.00 of $100.00 spent this month')).toBeInTheDocument();
    expect(within(card).getByRole('progressbar', { name: 'Monthly budget spent' })).toHaveAttribute(
      'aria-valuenow',
      '40',
    );
    expect(within(card).getByText('Deleted as soon as the job ends')).toBeInTheDocument();
    expect(
      within(card).getByText(
        /monthly budget is set on the Settings tab, where a change applies straight away/,
      ),
    ).toBeInTheDocument();
    expect(within(card).getByText(/^whisper-cpp \(cpu\) on the Bridge/)).toBeInTheDocument();
    expect(
      within(card).getByText('With no worker connected, the Bridge transcribes them itself.'),
    ).toBeInTheDocument();
  });
});
