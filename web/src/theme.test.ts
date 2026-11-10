import { describe, expect, it } from 'vitest';
import css from './index.css?raw';

// Every source file of the page, as text: the classes it uses.
const sources = import.meta.glob(['./**/*.tsx', './**/*.ts', '!./**/*.test.*'], {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>;

// The light theme's block (BRIDGE-17): the tokens it redefines.
function lightTokens(): Set<string> {
  const at = css.indexOf('@media (prefers-color-scheme: light)');
  expect(at).toBeGreaterThan(-1);
  return new Set([...css.slice(at).matchAll(/(--color-[a-z]+(?:-\d+)?):/g)].map((m) => m[1]));
}

describe('the light theme', () => {
  it('redefines every brand token the dark theme defines', () => {
    const theme = css.slice(css.indexOf('@theme'), css.indexOf('}', css.indexOf('@theme')));
    const dark = [...theme.matchAll(/(--color-[a-z]+(?:-\d+)?):/g)].map((m) => m[1]);
    expect(dark.length).toBeGreaterThan(0);
    const light = lightTokens();
    expect(dark.filter((t) => !light.has(t))).toEqual([]);
  });

  it('redefines every stock state shade the page uses (they are dark-theme shades)', () => {
    const used = new Set<string>();
    for (const text of Object.values(sources)) {
      for (const m of text.matchAll(/\b(?:[a-z]+:)*[a-z]+-(red|amber|green|pink)-(\d+)\b/g)) {
        used.add(`--color-${m[1]}-${m[2]}`);
      }
    }
    expect(used.size).toBeGreaterThan(0);
    const light = lightTokens();
    expect([...used].filter((t) => !light.has(t)).sort()).toEqual([]);
  });
});
