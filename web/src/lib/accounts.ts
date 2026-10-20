import type { AgentAccount, AgentKind, GitHubAccount, TokenFrom } from '@/api/types';
import { parseTime } from './time';

// How often the Accounts tab looks again while a GitHub sign-in waits for its
// code to be entered (the Bridge polls GitHub; the page polls the Bridge).
export const PENDING_POLL_MS = 3000;

// The agent credential kinds, as the page names them.
export const KIND_LABELS: Record<AgentKind, { label: string; note: string }> = {
  claude_oauth_token: {
    label: 'Claude subscription token',
    note: 'Run claude setup-token on any computer signed in to your Claude plan, and paste the token here.',
  },
  anthropic_api_key: {
    label: 'Anthropic API key',
    note: 'An API key from console.anthropic.com. Its use is billed to the key.',
  },
  openai_api_key: {
    label: 'OpenAI API key',
    note: 'An API key from platform.openai.com. Its use is billed to the key.',
  },
};

// fromText says where a contribution's GitHub token comes from now.
export function fromText(from: TokenFrom): string {
  switch (from) {
    case 'env':
      return 'The AUDIOSILO_BRIDGE_GITHUB_TOKEN environment variable';
    case 'device':
      return 'This sign-in';
    case 'gh':
      return "The gh command's sign-in on the Bridge's computer";
    default:
      return "None: the Bridge can't share with the community";
  }
}

// GitHubState is what the GitHub card shows.
export type GitHubState = 'unavailable' | 'pending' | 'refused' | 'signed_in' | 'signed_out';

export function githubState(g: GitHubAccount): GitHubState {
  if (g.pending) return 'pending';
  if (g.signed_in) return g.refused ? 'refused' : 'signed_in';
  return g.available ? 'signed_out' : 'unavailable';
}

// minutesLeft is how long a sign-in code stays good, in whole minutes (0 when
// it has run out).
export function minutesLeft(expiresAt: string, now: number = Date.now()): number {
  const t = parseTime(expiresAt);
  if (!t) return 0;
  return Math.max(0, Math.ceil((t.getTime() - now) / 60000));
}

// savedText says which credential the agent's runs use once kind is saved: the
// agent container's own wins, then the first kind set in the CLI's order (a
// subscription token before an API key for claude).
export function savedText(a: AgentAccount, kind: AgentKind): string {
  if (a.container_credential !== '') {
    return "Saved. The agent container's own credential is used while it has one.";
  }
  const used = a.kinds.find((k) => a.set[k]);
  if (used && used !== kind) {
    return `Saved. The ${KIND_LABELS[used].label} is used while it is set: remove it to use this one.`;
  }
  return 'Saved. The next agent run uses it.';
}

// checkCredential is the page's first look at a pasted credential: an error
// sentence, or null (the Bridge checks it again).
export function checkCredential(value: string): string | null {
  const v = value.trim();
  if (v === '') return 'Paste the credential first.';
  if (v.length > 2048) return 'That is longer than any credential (2048 characters at most).';
  if (/[^\x21-\x7e]/.test(v)) return 'Paste it as one line, with no spaces.';
  return null;
}
