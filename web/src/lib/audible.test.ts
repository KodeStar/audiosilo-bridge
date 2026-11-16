import { describe, expect, it } from 'vitest';
import {
  accountState,
  needsSignIn,
  defaultMapping,
  mappingOf,
  mappingProblem,
  marketplaceName,
  pollSummary,
  redirectProblem,
} from './audible';
import { audibleAccountFixture } from '@/test/fixtures';

describe('audible', () => {
  it('names the stores', () => {
    expect(marketplaceName('uk')).toBe('United Kingdom');
    expect(marketplaceName('xx')).toBe('XX');
  });

  it('takes only an address that holds a sign-in code', () => {
    expect(redirectProblem('')).toMatch(/Paste the address/);
    expect(redirectProblem('not a url')).toMatch(/isn't a web address/);
    expect(redirectProblem('https://www.amazon.com/ap/maplanding?x=1')).toMatch(/no sign-in code/);
    expect(
      redirectProblem(
        ' https://www.amazon.com/ap/maplanding?openid.oa2.authorization_code=ANcode&x=1 ',
      ),
    ).toBeNull();
  });

  it('asks for a person and a library', () => {
    expect(mappingProblem({ user_id: 0, library_id: 1 })).toMatch(/person/);
    expect(mappingProblem({ user_id: 3, library_id: 0 })).toMatch(/library/);
    expect(mappingProblem({ user_id: 3, library_id: 1 })).toBeNull();
  });

  it('picks the only person and library for a new account', () => {
    expect(
      defaultMapping({
        users: [{ id: 3, name: 'Chris', admin: true }],
        libraries: [{ id: 1, name: 'Books' }],
      }),
    ).toEqual({ label: '', user_id: 3, library_id: 1, policy: 'manual' });
    const two = defaultMapping({
      users: [
        { id: 3, name: 'Chris', admin: true },
        { id: 4, name: 'Sam', admin: false },
      ],
      libraries: [],
    });
    expect(two.user_id).toBe(0);
    expect(two.library_id).toBe(0);
  });

  it("says an account's state and its last check", () => {
    const a = audibleAccountFixture();
    expect(accountState(a)).toBe('checked');
    expect(pollSummary(a)).toBe('30 titles, 12 already in the library');
    expect(accountState({ ...a, last_poll_at: '' })).toBe('waiting');
    expect(pollSummary({ ...a, last_poll_at: '' })).toBe('');
    expect(pollSummary({ ...a, last_titles: 1 })).toBe('1 title, 12 already in the library');
    expect(accountState({ ...a, other_server: true })).toBe('other_server');
    expect(accountState({ ...a, refused: true, other_server: true })).toBe('refused');
    expect(accountState({ ...a, old_profile: true, other_server: true })).toBe('old_profile');
    expect(accountState({ ...a, refused: true, old_profile: true })).toBe('refused');
    expect(needsSignIn(a)).toBe(false);
    expect(needsSignIn({ ...a, old_profile: true })).toBe(true);
    expect(needsSignIn({ ...a, refused: true })).toBe(true);
    expect(mappingOf(a)).toEqual({
      label: 'Chris (US)',
      user_id: 3,
      library_id: 1,
      policy: 'manual',
    });
    // Another server's person and library ids name nothing here: picked again.
    expect(mappingOf({ ...a, other_server: true })).toEqual({
      label: 'Chris (US)',
      user_id: 0,
      library_id: 0,
      policy: 'manual',
    });
  });
});
