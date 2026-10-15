import { describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { StatusTab } from './StatusTab';
import { ApiClient } from '@/api/client';
import { lastCall, mockFetch } from '@/test/fetchMock';
import { engineFixture, pairedStatus, statusFixture } from '@/test/fixtures';

const api = () => new ApiClient({ getToken: () => 't', onSignedOut: vi.fn() });

describe('StatusTab', () => {
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
    expect(within(card).getByText('whisper-cpp (cpu)')).toBeInTheDocument();
  });
});
