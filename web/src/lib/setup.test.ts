import { describe, expect, it } from 'vitest';
import { ASR_WAITING, remainingRequired, setupItems, setupLink, setupLinks } from './setup';
import { setupFixture } from '@/test/fixtures';

describe('setup', () => {
  it('links each item to the tab the console links to', () => {
    expect(setupLink('agent')?.hash).toBe('#/accounts');
    expect(setupLink('github')?.hash).toBe('#/accounts');
    expect(setupLink('asr')?.hash).toBe('#/settings');
    expect(setupLink('budget')?.hash).toBe('#/settings');
    expect(setupLink('libraries')).toEqual({ hash: '#/libraries', name: 'Libraries' });
    expect(setupLink('paired')).toBeNull();
  });

  it('links Workers first while speech recognition waits for a worker', () => {
    expect(setupLinks({ id: 'asr', detail: ASR_WAITING }).map((l) => l.name)).toEqual([
      'Workers',
      'Settings',
    ]);
    expect(setupLinks({ id: 'asr', detail: 'whisper is missing' }).map((l) => l.name)).toEqual([
      'Settings',
    ]);
    // The words only mean a wait on the asr item.
    expect(setupLinks({ id: 'budget', detail: ASR_WAITING }).map((l) => l.name)).toEqual([
      'Settings',
    ]);
    expect(setupLinks({ id: 'paired' })).toEqual([]);
  });

  it('counts the required items left, never the optional one', () => {
    expect(remainingRequired(setupFixture())).toBe(0);
    expect(remainingRequired(setupFixture({ github: { done: false } }))).toBe(0);
    expect(
      remainingRequired(setupFixture({ agent: { done: false }, budget: { done: false } })),
    ).toBe(2);
    expect(remainingRequired(null)).toBe(0);
  });

  it('leaves out an item a later Bridge adds', () => {
    const s = setupFixture();
    s.items.push({ id: 'telepathy', done: false, required: true });
    expect(setupItems(s).map((it) => it.id)).toEqual([
      'paired',
      'libraries',
      'asr',
      'agent',
      'budget',
      'github',
    ]);
  });
});
