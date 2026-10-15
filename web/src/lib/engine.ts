import type { Engine, EngineAgent } from '@/api/types';
import { usd } from './generate';

// credentialText words the kind of credential an agent CLI has.
export function credentialText(credential: string): string {
  switch (credential) {
    case 'oauth_token':
    case 'login':
      return 'subscription login';
    case 'api_key':
      return 'API key';
    case '':
      return 'no credential';
    default:
      return credential;
  }
}

// agentText words the agent container: ready, or why not.
export function agentText(a: EngineAgent): string {
  if (a.available) {
    const cli = a.clis[a.backend];
    const version = cli?.version ? ` ${cli.version}` : '';
    const cred = cli ? `, ${credentialText(cli.credential)}` : '';
    return `Ready: ${a.backend}${version}${cred}`;
  }
  return `Not ready${a.detail ? `: ${a.detail}` : ''}`;
}

// budgetText words the monthly agent budget and this month's spend.
export function budgetText(e: Engine): string {
  if (e.monthly_budget_usd <= 0)
    return `No monthly cap (${usd(e.month_spent_usd)} spent this month)`;
  return `${usd(e.month_spent_usd)} of ${usd(e.monthly_budget_usd)} spent this month`;
}

// budgetFraction is this month's spend against the cap, 0-1 (null with no cap).
export function budgetFraction(e: Engine): number | null {
  if (e.monthly_budget_usd <= 0) return null;
  return Math.min(1, Math.max(0, e.month_spent_usd / e.monthly_budget_usd));
}

// retentionText words the transcript retention.
export function retentionText(days: number): string {
  if (days <= 0) return 'Deleted as soon as the job ends';
  return `Kept ${days === 1 ? '1 day' : `${days} days`} after the job ends, then deleted`;
}
