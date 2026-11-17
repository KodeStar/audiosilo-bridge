import type { AudibleAccount, AudibleAccounts, AudibleMapping, AudiblePolicy } from '@/api/types';

// The Audible storefronts, as the page names them (audible.MarketplaceCodes).
export const MARKETPLACE_NAMES: Record<string, string> = {
  us: 'United States',
  uk: 'United Kingdom',
  ca: 'Canada',
  au: 'Australia',
  de: 'Germany',
  fr: 'France',
  es: 'Spain',
  it: 'Italy',
  jp: 'Japan',
  in: 'India',
  br: 'Brazil',
};

export function marketplaceName(code: string): string {
  return MARKETPLACE_NAMES[code] ?? code.toUpperCase();
}

// What each policy backs up on its own: the server queues a new purchase's
// backup after a check when the account's policy covers it.
export const POLICY_LABELS: Record<AudiblePolicy, string> = {
  all: 'Every new title',
  series: 'New titles in a series they already have',
  manual: 'Only the titles picked in the console',
};

// The marker the page Amazon lands on carries (audible.ParseAuthCode).
const CODE_PARAM = 'openid.oa2.authorization_code';

// redirectProblem says what is wrong with a pasted address, or null when it
// holds a sign-in code.
export function redirectProblem(text: string): string | null {
  const v = text.trim();
  if (v === '') return 'Paste the address of the page Amazon showed after you signed in.';
  let url: URL;
  try {
    url = new URL(v);
  } catch {
    return "That isn't a web address: copy the whole address from the browser's address bar.";
  }
  if (!url.searchParams.get(CODE_PARAM)) {
    return 'That address has no sign-in code: copy the address of the page Amazon showed after you signed in (it starts with the Amazon site and holds "authorization_code").';
  }
  return null;
}

// mappingProblem says what the mapping still needs, or null.
export function mappingProblem(m: Pick<AudibleMapping, 'user_id' | 'library_id'>): string | null {
  if (!m.user_id) return 'Pick the person the account belongs to.';
  if (!m.library_id) return 'Pick the books library its titles go to.';
  return null;
}

// defaultMapping is a new account's mapping: the only person and the only
// library when there is one of each, the manual policy. Whether its places
// sync is the server console's (Phase 4c), so a mapping carries none.
export function defaultMapping(data: Pick<AudibleAccounts, 'users' | 'libraries'>): AudibleMapping {
  return {
    label: '',
    user_id: data.users.length === 1 ? data.users[0].id : 0,
    library_id: data.libraries.length === 1 ? data.libraries[0].id : 0,
    policy: 'manual',
  };
}

// mappingOf is an account's mapping as the form edits it. An account mapped on
// another server keeps its name and policy, but its person and library
// are that server's ids (here they name nothing, or someone else): they are
// picked again.
export function mappingOf(a: AudibleAccount): AudibleMapping {
  const here = !a.other_server;
  return {
    label: a.label,
    user_id: here ? a.user_id : 0,
    library_id: here ? a.library_id : 0,
    policy: a.policy,
  };
}

export type AudibleState = 'refused' | 'old_profile' | 'other_server' | 'checked' | 'waiting';

// accountState is what an account's badge says.
export function accountState(a: AudibleAccount): AudibleState {
  if (a.refused) return 'refused';
  if (a.old_profile) return 'old_profile';
  if (a.other_server) return 'other_server';
  return a.last_poll_at ? 'checked' : 'waiting';
}

// needsSignIn reports whether an account must sign in again: Audible refused
// it, or its device is on an older profile that gets no downloads.
export function needsSignIn(a: AudibleAccount): boolean {
  return a.refused || a.old_profile;
}

// OLD_PROFILE_TEXT explains an account on an older device profile.
export const OLD_PROFILE_TEXT =
  'Sign in again to keep backups working. This AudioSilo device was registered by an earlier version of the Bridge, and Audible no longer allows downloads to it. Signing in again replaces it with a new device.';

// pollSummary words the last complete poll ("" before the first).
export function pollSummary(a: AudibleAccount): string {
  if (!a.last_poll_at) return '';
  const titles = a.last_titles === 1 ? '1 title' : `${a.last_titles} titles`;
  return `${titles}, ${a.last_matched} already in the library`;
}
