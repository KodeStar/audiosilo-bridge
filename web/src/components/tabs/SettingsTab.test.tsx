import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { SettingsTab } from './SettingsTab';
import { ApiClient } from '@/api/client';
import { lastCall, mockFetch } from '@/test/fetchMock';
import { settingsFixture } from '@/test/fixtures';

function api(onSignedOut = vi.fn(), onLanBlocked = vi.fn()) {
  return new ApiClient({ getToken: () => 't', onSignedOut, onLanBlocked });
}

describe('SettingsTab', () => {
  it('keeps a locked field read-only and saves the rest', async () => {
    const initial = settingsFixture({ public_url: 'http://10.0.0.5:8091', locked: ['public_url'] });
    const { calls } = mockFetch({
      'GET /api/settings': { body: initial },
      'PUT /api/settings': ({ body }) => ({
        body: { ...initial, ...(body as object) },
      }),
    });
    render(<SettingsTab client={api()} />);

    const url = await screen.findByLabelText('Public address');
    expect(url).toHaveValue('http://10.0.0.5:8091');
    expect(url).toBeDisabled();
    expect(screen.getByText('Set by the environment')).toBeInTheDocument();

    const minutes = screen.getByLabelText('Check the libraries every (minutes)');
    expect(minutes).toBeEnabled();
    await userEvent.clear(minutes);
    await userEvent.type(minutes, '5000');
    await userEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    expect(screen.getByRole('alert')).toHaveTextContent('Enter between 1 and 1440 minutes.');
    expect(lastCall(calls, 'PUT', '/api/settings')).toBeUndefined();

    await userEvent.clear(minutes);
    await userEvent.type(minutes, '30');
    await userEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    expect(lastCall(calls, 'PUT', '/api/settings')?.body).toEqual({
      public_url: 'http://10.0.0.5:8091',
      mapping_check_minutes: 30,
      asr_worker_fallback: 'local',
      monthly_budget_usd: 100,
    });
  });

  it('saves the monthly budget, which reviews it, and keeps a locked one out', async () => {
    const { calls } = mockFetch({
      'GET /api/settings': { body: settingsFixture({ budget_reviewed: false }) },
      'PUT /api/settings': { body: settingsFixture({ monthly_budget_usd: 25 }) },
    });
    render(<SettingsTab client={api()} />);
    const field = await screen.findByLabelText('Monthly agent budget (dollars)');
    expect(screen.getByText(/Save the settings to confirm it/)).toBeInTheDocument();
    await userEvent.clear(field);
    await userEvent.type(field, 'lots');
    await userEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    expect(screen.getByRole('alert')).toHaveTextContent('Enter the budget in dollars');
    await userEvent.clear(field);
    await userEvent.type(field, '25');
    await userEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    expect(lastCall(calls, 'PUT', '/api/settings')?.body).toMatchObject({ monthly_budget_usd: 25 });
    expect(screen.queryByText(/Save the settings to confirm it/)).not.toBeInTheDocument();
  });

  it('saves an untouched budget as loaded, even one the field would refuse', async () => {
    const { calls } = mockFetch({
      'GET /api/settings': { body: settingsFixture({ monthly_budget_usd: 12.345 }) },
      'PUT /api/settings': { body: settingsFixture({ monthly_budget_usd: 12.345 }) },
    });
    render(<SettingsTab client={api()} />);
    expect(await screen.findByLabelText('Monthly agent budget (dollars)')).toHaveValue('12.345');
    await userEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    expect(lastCall(calls, 'PUT', '/api/settings')?.body).toMatchObject({
      monthly_budget_usd: 12.345,
    });
  });

  it('sends no budget the environment sets', async () => {
    const { calls } = mockFetch({
      'GET /api/settings': {
        body: settingsFixture({ monthly_budget_usd: 40, locked: ['agent.monthly_budget_usd'] }),
      },
      'PUT /api/settings': { body: settingsFixture() },
    });
    render(<SettingsTab client={api()} />);
    expect(await screen.findByLabelText('Monthly agent budget (dollars)')).toBeDisabled();
    await userEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    expect(lastCall(calls, 'PUT', '/api/settings')?.body).not.toHaveProperty('monthly_budget_usd');
  });

  it('saves the ASR fallback, and keeps a locked one', async () => {
    const { calls } = mockFetch({
      'GET /api/settings': { body: settingsFixture() },
      'PUT /api/settings': { body: settingsFixture({ asr_worker_fallback: 'wait' }) },
    });
    render(<SettingsTab client={api()} />);
    const select = await screen.findByLabelText('When no ASR worker is connected');
    await userEvent.selectOptions(select, 'wait');
    await userEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    expect(lastCall(calls, 'PUT', '/api/settings')?.body).toMatchObject({
      asr_worker_fallback: 'wait',
    });
  });

  it('shows a locked ASR fallback as locked', async () => {
    mockFetch({
      'GET /api/settings': {
        body: settingsFixture({ asr_worker_fallback: 'wait', locked: ['asr.worker_fallback'] }),
      },
    });
    render(<SettingsTab client={api()} />);
    expect(await screen.findByLabelText('When no ASR worker is connected')).toBeDisabled();
  });

  it('shows a 409 from the Bridge verbatim', async () => {
    mockFetch({
      'GET /api/settings': { body: settingsFixture() },
      'PUT /api/settings': {
        status: 409,
        body: { error: 'AUDIOSILO_BRIDGE_PUBLIC_URL sets public_url', code: 'locked' },
      },
    });
    render(<SettingsTab client={api()} />);
    await userEvent.type(await screen.findByLabelText('Public address'), 'http://bridge.lan');
    await userEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'AUDIOSILO_BRIDGE_PUBLIC_URL sets public_url',
    );
  });

  it('changes the password and shows a wrong current password without signing out', async () => {
    const onSignedOut = vi.fn();
    const onLanBlocked = vi.fn();
    let first = true;
    const { calls } = mockFetch({
      'GET /api/settings': { body: settingsFixture() },
      'POST /api/password': () => {
        if (first) {
          first = false;
          return { status: 403, body: { error: 'the current password is wrong' } };
        }
        return { status: 204 };
      },
    });
    render(<SettingsTab client={api(onSignedOut, onLanBlocked)} />);
    await userEvent.type(screen.getByLabelText('Current password'), 'old');
    await userEvent.type(screen.getByLabelText('New password'), 'a-long-new-one');
    await userEvent.click(screen.getByRole('button', { name: 'Change password' }));
    expect(await screen.findByText('the current password is wrong')).toBeInTheDocument();
    expect(onSignedOut).not.toHaveBeenCalled();
    expect(onLanBlocked).not.toHaveBeenCalled();

    await userEvent.click(screen.getByRole('button', { name: 'Change password' }));
    expect(await screen.findByText('The password is changed.')).toBeInTheDocument();
    expect(lastCall(calls, 'POST', '/api/password')?.body).toEqual({
      current: 'old',
      new: 'a-long-new-one',
    });
  });
});
