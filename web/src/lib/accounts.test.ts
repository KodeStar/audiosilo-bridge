import { describe, expect, it } from 'vitest';
import {
  checkCredential,
  elsewhereBadge,
  fromText,
  githubState,
  minutesLeft,
  savedText,
} from './accounts';
import { accountsFixture } from '@/test/fixtures';

describe('accounts', () => {
  it('reads the GitHub state', () => {
    const g = accountsFixture().github;
    expect(githubState(g)).toBe('signed_out');
    expect(githubState({ ...g, available: false })).toBe('unavailable');
    expect(githubState({ ...g, signed_in: true, login: 'octocat' })).toBe('signed_in');
    expect(githubState({ ...g, signed_in: true, refused: true })).toBe('refused');
    expect(
      githubState({
        ...g,
        signed_in: true,
        pending: { user_code: 'AB', verification_uri: 'u', expires_at: '' },
      }),
    ).toBe('pending');
    // Signed in through gh or the environment, not on this page.
    expect(githubState({ ...g, from: 'gh', can_contribute: true })).toBe('elsewhere');
    expect(githubState({ ...g, from: 'env', can_contribute: true })).toBe('elsewhere');
    expect(githubState({ ...g, available: false, from: 'gh' })).toBe('elsewhere');
    expect(elsewhereBadge('gh')).toBe('Signed in (gh)');
    expect(elsewhereBadge('env')).toBe('Signed in (environment)');
  });

  it("says where a contribution's token comes from", () => {
    expect(fromText('env')).toContain('AUDIOSILO_BRIDGE_GITHUB_TOKEN');
    expect(fromText('device')).toBe('This sign-in');
    expect(fromText('')).toContain('None');
  });

  it('counts the minutes a code has left', () => {
    const now = Date.parse('2026-10-19T12:00:00Z');
    expect(minutesLeft('2026-10-19T12:14:01Z', now)).toBe(15);
    expect(minutesLeft('2026-10-19T11:00:00Z', now)).toBe(0);
    expect(minutesLeft('nonsense', now)).toBe(0);
  });

  it('says which credential the runs use once one is saved', () => {
    const a = accountsFixture().agent;
    expect(savedText({ ...a, set: { anthropic_api_key: true } }, 'anthropic_api_key')).toBe(
      'Saved. The next agent run uses it.',
    );
    // A subscription token comes first for claude: a key saved beside it waits.
    expect(
      savedText(
        { ...a, set: { claude_oauth_token: true, anthropic_api_key: true } },
        'anthropic_api_key',
      ),
    ).toMatch(/Claude subscription token is used while it is set: remove it/);
    expect(
      savedText(
        { ...a, set: { claude_oauth_token: true, anthropic_api_key: true } },
        'claude_oauth_token',
      ),
    ).toBe('Saved. The next agent run uses it.');
    expect(
      savedText(
        { ...a, set: { claude_oauth_token: true }, container_credential: 'api_key' },
        'claude_oauth_token',
      ),
    ).toMatch(/container's own credential/);
  });

  it('checks a pasted credential', () => {
    expect(checkCredential('  ')).toMatch(/Paste/);
    expect(checkCredential('sk-ant oat')).toMatch(/one line/);
    expect(checkCredential('x'.repeat(2049))).toMatch(/longer/);
    expect(checkCredential(' sk-ant-oat01-abc\n')).toBeNull();
  });
});
