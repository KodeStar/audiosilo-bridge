import { describe, expect, it } from 'vitest';
import { agentText, budgetFraction, budgetText, credentialText, retentionText } from './engine';
import { engineFixture } from '@/test/fixtures';

describe('engine wording', () => {
  it('words the agent container', () => {
    expect(agentText(engineFixture().agent)).toBe('Ready: claude 2.1.300, subscription login');
    const down = engineFixture().agent;
    down.available = false;
    down.detail = 'no heartbeat from the agent container';
    expect(agentText(down)).toBe('Not ready: no heartbeat from the agent container');
    expect(credentialText('api_key')).toBe('API key');
    expect(credentialText('')).toBe('no credential');
  });
  it('words the budget and the retention', () => {
    expect(budgetText(engineFixture())).toBe('$12.30 of $100.00 spent this month');
    expect(budgetFraction(engineFixture())).toBeCloseTo(0.123);
    expect(budgetText(engineFixture({ monthly_budget_usd: 0 }))).toBe(
      'No monthly cap ($12.30 spent this month)',
    );
    expect(budgetFraction(engineFixture({ monthly_budget_usd: 0 }))).toBeNull();
    expect(budgetFraction(engineFixture({ month_spent_usd: 500 }))).toBe(1);
    expect(retentionText(30)).toBe('Kept 30 days after the job ends, then deleted');
    expect(retentionText(1)).toBe('Kept 1 day after the job ends, then deleted');
    expect(retentionText(0)).toBe('Deleted as soon as the job ends');
  });
});
