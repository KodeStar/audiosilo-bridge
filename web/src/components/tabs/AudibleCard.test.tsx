import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AudibleCard } from './AudibleCard';
import { ApiClient } from '@/api/client';
import type { AudibleAccounts } from '@/api/types';
import { lastCall, mockFetch } from '@/test/fetchMock';
import { audibleAccountFixture, audibleFixture } from '@/test/fixtures';

const api = () => new ApiClient({ getToken: () => 't', onSignedOut: vi.fn() });

const REDIRECT = 'https://www.amazon.co.uk/ap/maplanding?openid.oa2.authorization_code=ANcode123';

afterEach(() => {
  vi.restoreAllMocks();
});

describe('AudibleCard', () => {
  it('signs in: opens Amazon, takes the pasted page and the mapping', async () => {
    const open = vi.spyOn(window, 'open').mockImplementation(() => null);
    let state: AudibleAccounts = audibleFixture();
    const { calls } = mockFetch({
      'GET /api/accounts/audible': () => ({ body: state }),
      'POST /api/accounts/audible/sign-in': {
        body: {
          login: 'login0001',
          url: 'https://www.amazon.co.uk/ap/signin?x=1',
          marketplace: 'uk',
          expires_at: new Date(Date.now() + 15 * 60000).toISOString(),
        },
      },
      'POST /api/accounts/audible': () => {
        state = audibleFixture({
          accounts: [audibleAccountFixture({ label: 'Chris (UK)', last_poll_at: '' })],
        });
        return { body: state };
      },
    });
    render(<AudibleCard client={api()} />);
    const card = await screen.findByRole('region', { name: 'Audible' });
    expect(within(card).getByText('Not signed in')).toBeInTheDocument();
    // The module's toggle is the console's: the page says so.
    expect(within(card).getByText(/Audible module is on/)).toBeInTheDocument();

    await userEvent.click(within(card).getByRole('button', { name: 'Sign in to Audible' }));
    await userEvent.selectOptions(within(card).getByLabelText('Audible store'), 'uk');
    await userEvent.click(within(card).getByRole('button', { name: 'Open Amazon sign-in' }));
    expect(lastCall(calls, 'POST', '/api/accounts/audible/sign-in')?.body).toEqual({
      marketplace: 'uk',
    });
    expect(open).toHaveBeenCalledWith(
      'https://www.amazon.co.uk/ap/signin?x=1',
      '_blank',
      'noopener,noreferrer',
    );
    expect(within(card).getByRole('link', { name: 'open it here' })).toHaveAttribute(
      'href',
      'https://www.amazon.co.uk/ap/signin?x=1',
    );
    expect(within(card).getByText(/15 minutes/)).toBeInTheDocument();

    // A page with no code is caught here, before the Bridge spends the sign-in.
    const address = within(card).getByLabelText('The address Amazon showed');
    await userEvent.type(address, 'https://www.amazon.co.uk/');
    await userEvent.click(within(card).getByRole('button', { name: 'Finish the sign-in' }));
    expect(await within(card).findByRole('alert')).toHaveTextContent(/no sign-in code/);
    expect(lastCall(calls, 'POST', '/api/accounts/audible')).toBeUndefined();

    await userEvent.clear(address);
    await userEvent.type(address, REDIRECT);
    // The only person and library are picked already.
    expect(within(card).getByLabelText('Belongs to')).toHaveValue('3');
    expect(within(card).getByLabelText('Library')).toHaveValue('1');
    // Sync is turned on in the console: no switch here.
    expect(within(card).queryByRole('checkbox')).not.toBeInTheDocument();
    expect(within(card).getByText(/turned on in the server's console/)).toBeInTheDocument();
    await userEvent.selectOptions(within(card).getByLabelText('Back up'), 'series');
    await userEvent.click(within(card).getByRole('button', { name: 'Finish the sign-in' }));
    expect(await within(card).findByText('Signed in to Audible.')).toBeInTheDocument();
    expect(lastCall(calls, 'POST', '/api/accounts/audible')?.body).toEqual({
      login: 'login0001',
      redirect_url: REDIRECT,
      label: '',
      user_id: 3,
      library_id: 1,
      policy: 'series',
      sync: false,
    });
    const row = within(card).getByRole('region', { name: 'Chris (UK)' });
    expect(within(row).getByText('Not checked yet')).toBeInTheDocument();
    expect(within(row).getByText('United States')).toBeInTheDocument();
  });

  it('shows a refused account and signs it in again in its store', async () => {
    vi.spyOn(window, 'open').mockImplementation(() => null);
    const { calls } = mockFetch({
      'GET /api/accounts/audible': {
        body: audibleFixture({
          accounts: [
            audibleAccountFixture({
              marketplace: 'uk',
              refused: true,
              refused_at: '2026-11-03T12:00:00Z',
              last_error: 'Audible refused the sign-in: sign in again',
            }),
          ],
        }),
      },
      'POST /api/accounts/audible/sign-in': {
        body: {
          login: 'l2',
          url: 'https://www.amazon.co.uk/ap/signin',
          marketplace: 'uk',
          expires_at: '',
        },
      },
    });
    render(<AudibleCard client={api()} />);
    const card = await screen.findByRole('region', { name: 'Audible' });
    const row = within(card).getByRole('region', { name: 'Chris (US)' });
    expect(within(row).getByText('Sign in again', { selector: 'span' })).toBeInTheDocument();
    expect(within(row).getByText('Audible refused the sign-in: sign in again')).toBeInTheDocument();
    expect(within(row).getByText('30 titles, 12 already in the library')).toBeInTheDocument();
    await userEvent.click(within(row).getByRole('button', { name: 'Sign in again' }));
    expect(within(card).getByLabelText('Audible store')).toHaveValue('uk');
    await userEvent.click(within(card).getByRole('button', { name: 'Open Amazon sign-in' }));
    expect(lastCall(calls, 'POST', '/api/accounts/audible/sign-in')?.body).toEqual({
      marketplace: 'uk',
    });
  });

  it('asks an account on an older device profile to sign in again', async () => {
    vi.spyOn(window, 'open').mockImplementation(() => null);
    const { calls } = mockFetch({
      'GET /api/accounts/audible': {
        body: audibleFixture({
          accounts: [audibleAccountFixture({ marketplace: 'uk', old_profile: true })],
        }),
      },
      'POST /api/accounts/audible/sign-in': {
        body: {
          login: 'l3',
          url: 'https://www.amazon.co.uk/ap/signin',
          marketplace: 'uk',
          expires_at: '',
        },
      },
    });
    render(<AudibleCard client={api()} />);
    const card = await screen.findByRole('region', { name: 'Audible' });
    const row = within(card).getByRole('region', { name: 'Chris (US)' });
    expect(within(row).getByText('Sign in again', { selector: 'span' })).toBeInTheDocument();
    expect(
      within(row).getByText(/^Sign in again to keep backups working\./, { selector: 'div,p' }),
    ).toBeInTheDocument();
    await userEvent.click(within(row).getByRole('button', { name: 'Sign in again' }));
    expect(within(card).getByLabelText('Audible store')).toHaveValue('uk');
    await userEvent.click(within(card).getByRole('button', { name: 'Open Amazon sign-in' }));
    expect(lastCall(calls, 'POST', '/api/accounts/audible/sign-in')?.body).toEqual({
      marketplace: 'uk',
    });
  });

  it('opens the same Amazon sign-in again until a finish spends it', async () => {
    const open = vi.spyOn(window, 'open').mockImplementation(() => null);
    let n = 0;
    const { calls } = mockFetch({
      'GET /api/accounts/audible': { body: audibleFixture() },
      'POST /api/accounts/audible/sign-in': () => {
        n++;
        return {
          body: {
            login: `login000${n}`,
            url: `https://www.amazon.com/ap/signin?n=${n}`,
            marketplace: 'us',
            expires_at: new Date(Date.now() + 15 * 60000).toISOString(),
          },
        };
      },
      'POST /api/accounts/audible': {
        status: 422,
        body: { error: "the Amazon sign-in wasn't accepted" },
      },
    });
    render(<AudibleCard client={api()} />);
    const card = await screen.findByRole('region', { name: 'Audible' });
    await userEvent.click(within(card).getByRole('button', { name: 'Sign in to Audible' }));
    await userEvent.click(within(card).getByRole('button', { name: 'Open Amazon sign-in' }));
    await userEvent.click(
      within(card).getByRole('button', { name: 'Open the Amazon sign-in again' }),
    );
    // One sign-in: the tab opened first can still finish it.
    expect(calls.filter((c) => c.url.endsWith('/sign-in'))).toHaveLength(1);
    expect(open).toHaveBeenLastCalledWith(
      'https://www.amazon.com/ap/signin?n=1',
      '_blank',
      'noopener,noreferrer',
    );
    await userEvent.type(within(card).getByLabelText('The address Amazon showed'), REDIRECT);
    await userEvent.click(within(card).getByRole('button', { name: 'Finish the sign-in' }));
    expect(await within(card).findByText(/wasn't accepted/)).toBeInTheDocument();
    // The finish spent it: a new one.
    await userEvent.click(
      within(card).getByRole('button', { name: 'Open the Amazon sign-in again' }),
    );
    expect(calls.filter((c) => c.url.endsWith('/sign-in'))).toHaveLength(2);
    expect(open).toHaveBeenLastCalledWith(
      'https://www.amazon.com/ap/signin?n=2',
      '_blank',
      'noopener,noreferrer',
    );
  });

  it("signs another account in again in that account's store", async () => {
    mockFetch({
      'GET /api/accounts/audible': {
        body: audibleFixture({
          accounts: [audibleAccountFixture({ marketplace: 'uk', refused: true })],
        }),
      },
    });
    render(<AudibleCard client={api()} />);
    const card = await screen.findByRole('region', { name: 'Audible' });
    await userEvent.click(
      within(card).getByRole('button', { name: 'Add another Audible account' }),
    );
    expect(within(card).getByLabelText('Audible store')).toHaveValue('us');
    const row = within(card).getByRole('region', { name: 'Chris (US)' });
    await userEvent.click(within(row).getByRole('button', { name: 'Sign in again' }));
    expect(within(card).getByLabelText('Audible store')).toHaveValue('uk');
  });

  it('maps an account again', async () => {
    const two = audibleFixture({
      accounts: [audibleAccountFixture({ other_server: true })],
      libraries: [
        { id: 1, name: 'Books' },
        { id: 2, name: 'Kids' },
      ],
    });
    const { calls } = mockFetch({
      'GET /api/accounts/audible': { body: two },
      'PUT /api/accounts/audible/acct0001': {
        body: {
          ...two,
          accounts: [audibleAccountFixture({ library_id: 2, library_name: 'Kids' })],
        },
      },
    });
    render(<AudibleCard client={api()} />);
    const row = await screen.findByRole('region', { name: 'Chris (US)' });
    expect(within(row).getByText('Mapped on another server')).toBeInTheDocument();
    await userEvent.click(within(row).getByRole('button', { name: 'Change' }));
    // The other server's person and library ids name nothing here: picked again.
    expect(within(row).getByLabelText('Belongs to')).toHaveValue('');
    expect(within(row).getByLabelText('Library')).toHaveValue('');
    await userEvent.selectOptions(within(row).getByLabelText('Belongs to'), '3');
    await userEvent.selectOptions(within(row).getByLabelText('Library'), '2');
    // The policy is the server's once the account exists: no selector here.
    expect(within(row).queryByLabelText('Back up')).not.toBeInTheDocument();
    expect(within(row).getByText(/changed in the server's console/)).toBeInTheDocument();
    await userEvent.click(within(row).getByRole('button', { name: 'Save' }));
    expect(await within(row).findByText('Kids')).toBeInTheDocument();
    expect(lastCall(calls, 'PUT', '/api/accounts/audible/acct0001')?.body).toEqual({
      label: 'Chris (US)',
      user_id: 3,
      library_id: 2,
      policy: 'manual',
      sync: true,
    });
  });

  it('gives each open form its own element ids', async () => {
    mockFetch({
      'GET /api/accounts/audible': {
        body: audibleFixture({
          accounts: [
            audibleAccountFixture(),
            audibleAccountFixture({ id: 'acct0002', label: 'Sam (US)', user_id: 4 }),
          ],
        }),
      },
    });
    render(<AudibleCard client={api()} />);
    const rows = [
      await screen.findByRole('region', { name: 'Chris (US)' }),
      await screen.findByRole('region', { name: 'Sam (US)' }),
    ];
    for (const row of rows) {
      await userEvent.click(within(row).getByRole('button', { name: 'Change' }));
    }
    const ids = Array.from(document.querySelectorAll('[id]'), (el) => el.id);
    expect(new Set(ids).size).toBe(ids.length);
    // Each form's label names its own field.
    for (const row of rows) {
      const library = within(row).getByLabelText('Library');
      expect(row.contains(library)).toBe(true);
    }
  });

  it('removes an account after asking, and says what Amazon did not confirm', async () => {
    const { calls } = mockFetch({
      'GET /api/accounts/audible': {
        body: audibleFixture({ accounts: [audibleAccountFixture()] }),
      },
      'DELETE /api/accounts/audible/acct0001': {
        body: {
          audible: audibleFixture(),
          warning:
            "Amazon didn't confirm removing the AudioSilo device: remove it in Amazon's device list",
        },
      },
    });
    render(<AudibleCard client={api()} />);
    const row = await screen.findByRole('region', { name: 'Chris (US)' });
    await userEvent.click(within(row).getByRole('button', { name: 'Remove' }));
    expect(lastCall(calls, 'DELETE', '/api/accounts/audible/acct0001')).toBeUndefined();
    await userEvent.click(within(row).getByRole('button', { name: 'Remove the account' }));
    expect(
      await screen.findByText(/Removed from the Bridge, but Amazon didn't confirm/),
    ).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'Chris (US)' })).toBeNull();
  });

  it('asks for pairing first, and says when the people cannot be read', async () => {
    mockFetch({
      'GET /api/accounts/audible': {
        body: audibleFixture({
          paired: false,
          users: [],
          users_error: 'pair the Bridge with a server first',
        }),
      },
    });
    render(<AudibleCard client={api()} />);
    const card = await screen.findByRole('region', { name: 'Audible' });
    expect(within(card).getByText('Pair the Bridge with a server first.')).toBeInTheDocument();
    expect(within(card).queryByRole('button', { name: 'Sign in to Audible' })).toBeNull();
  });
});
