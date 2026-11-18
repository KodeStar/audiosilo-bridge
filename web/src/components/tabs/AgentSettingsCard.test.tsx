import { describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { AgentSettingsCard } from './AgentSettingsCard';
import { ApiClient } from '@/api/client';
import { lastCall, mockFetch } from '@/test/fetchMock';
import { accountsFixture, agentSettingsFixture } from '@/test/fixtures';

function api() {
  return new ApiClient({ getToken: () => 't', onSignedOut: vi.fn(), onLanBlocked: vi.fn() });
}

describe('AgentSettingsCard', () => {
  it('shows the endpoint fields only for an endpoint, and saves the endpoint with its models', async () => {
    const { calls } = mockFetch({
      'GET /api/settings/agent': { body: agentSettingsFixture() },
      'PUT /api/settings/agent': ({ body }) => ({
        body: agentSettingsFixture({ ...(body as object), running_backend: 'claude' }),
      }),
    });
    render(<AgentSettingsCard client={api()} />);
    const select = await screen.findByLabelText('The agent runs');
    expect(screen.queryByLabelText('Endpoint address')).not.toBeInTheDocument();

    await userEvent.selectOptions(select, 'openai-compatible');
    const url = screen.getByLabelText('Endpoint address');
    await userEvent.click(screen.getByRole('button', { name: 'Save agent settings' }));
    expect(screen.getByRole('alert')).toHaveTextContent("Enter the endpoint's address");
    await userEvent.type(url, 'http://user:pw@192.168.1.20:11434/v1');
    await userEvent.click(screen.getByRole('button', { name: 'Save agent settings' }));
    expect(screen.getByRole('alert')).toHaveTextContent('Leave the user name and password out');
    await userEvent.clear(url);
    await userEvent.type(url, 'http://192.168.1.20:11434/v1/');
    await userEvent.click(screen.getByRole('button', { name: 'Save agent settings' }));
    expect(screen.getByRole('alert')).toHaveTextContent('an endpoint has no default model');
    expect(lastCall(calls, 'PUT', '/api/settings/agent')).toBeUndefined();

    await userEvent.type(screen.getByLabelText('Model'), 'qwen3:32b');
    await userEvent.type(screen.getByLabelText('Fact notes'), ' qwen3:8b ');
    await userEvent.click(screen.getByRole('button', { name: 'Save agent settings' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    expect(lastCall(calls, 'PUT', '/api/settings/agent')?.body).toEqual({
      backend: 'openai-compatible',
      openai_compatible: {
        base_url: 'http://192.168.1.20:11434/v1',
        model: 'qwen3:32b',
        models: { fact_pass: 'qwen3:8b' },
      },
    });
    // The backend applies when the Bridge restarts, and the page says so.
    expect(screen.getByText(/until the Bridge restarts/)).toBeInTheDocument();
  });

  it('tests the connection and warns about the chosen models, never blocking a save', async () => {
    const { calls } = mockFetch({
      'GET /api/settings/agent': {
        body: agentSettingsFixture({
          backend: 'openai-compatible',
          running_backend: 'openai-compatible',
          openai_compatible: {
            base_url: 'http://192.168.1.20:11434/v1',
            model: 'tiny',
            models: { synthesizing: 'gone' },
          },
        }),
      },
      'POST /api/settings/agent/test': {
        body: {
          models: [
            { id: 'qwen3:32b', context_tokens: 40960, tools: true },
            { id: 'tiny', context_tokens: 4096, tools: false },
          ],
          responses: 'no',
          responses_detail: 'the endpoint has no /responses (404)',
          error: '',
        },
      },
      'PUT /api/settings/agent': ({ body }) => ({ body: agentSettingsFixture(body as object) }),
    });
    render(<AgentSettingsCard client={api()} />);
    await userEvent.click(await screen.findByRole('button', { name: 'Test connection' }));
    expect(await screen.findByText(/answered with 2 models/)).toBeInTheDocument();
    expect(lastCall(calls, 'POST', '/api/settings/agent/test')?.body).toEqual({
      base_url: 'http://192.168.1.20:11434/v1',
    });
    const list = screen.getByRole('list', { name: "The endpoint's models" });
    expect(within(list).getByText('qwen3:32b')).toBeInTheDocument();
    expect(within(list).getByText('4,096 tokens, no tools')).toBeInTheDocument();
    expect(screen.getByText(/tiny doesn't call tools/)).toBeInTheDocument();
    expect(screen.getByText(/tiny takes 4,096 tokens of context/)).toBeInTheDocument();
    expect(screen.getByText("gone isn't among the endpoint's models.")).toBeInTheDocument();
    expect(screen.getByText('the endpoint has no /responses (404)')).toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Save agent settings' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
  });

  it("shows the endpoint's failure, and keeps the key write-only", async () => {
    const { calls } = mockFetch({
      'GET /api/settings/agent': {
        body: agentSettingsFixture({
          backend: 'openai-compatible',
          running_backend: 'openai-compatible',
          openai_compatible: {
            base_url: 'https://api.openai.com/v1',
            model: 'gpt-5.1',
            models: {},
          },
        }),
      },
      'POST /api/settings/agent/test': {
        body: {
          models: [],
          responses: '',
          responses_detail: '',
          error: 'the endpoint refused the API key (401 Unauthorized): check the key',
        },
      },
      'PUT /api/accounts/agent': {
        body: accountsFixture({
          agent: {
            ...accountsFixture().agent,
            set: { endpoint_api_key: true },
            set_at_by_kind: { endpoint_api_key: '2026-11-18T09:00:00Z' },
          },
        }),
      },
    });
    render(<AgentSettingsCard client={api()} />);
    await userEvent.click(await screen.findByRole('button', { name: 'Test connection' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('refused the API key');

    const key = screen.getByLabelText('Endpoint API key');
    expect(key).toHaveAttribute('type', 'password');
    expect(screen.getByText(/Not set/)).toBeInTheDocument();
    await userEvent.type(key, 'sk-proj-new');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(lastCall(calls, 'PUT', '/api/accounts/agent')?.body).toEqual({
      kind: 'endpoint_api_key',
      value: 'sk-proj-new',
    });
    expect(await screen.findByRole('button', { name: 'Replace' })).toBeInTheDocument();
    expect(key).toHaveValue('');
    // The model field kept what it held.
    expect(screen.getByLabelText('Model')).toHaveValue('gpt-5.1');
  });

  it('keeps settings the environment sets read-only', async () => {
    mockFetch({
      'GET /api/settings/agent': {
        body: agentSettingsFixture({
          backend: 'openai-compatible',
          running_backend: 'openai-compatible',
          openai_compatible: { base_url: 'http://10.0.0.5:1234/v1', model: 'm', models: {} },
          locked: ['agent.backend', 'agent.openai_compatible.base_url'],
        }),
      },
    });
    render(<AgentSettingsCard client={api()} />);
    expect(await screen.findByLabelText('The agent runs')).toBeDisabled();
    expect(screen.getByLabelText('Endpoint address')).toBeDisabled();
    expect(screen.getByLabelText('Model')).toBeEnabled();
  });
});
