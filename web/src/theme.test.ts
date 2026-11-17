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

// A theme's colour tokens: the dark theme's @theme block, or the light one's.
function tokenValues(theme: 'dark' | 'light'): Map<string, string> {
  const at =
    theme === 'dark' ? css.indexOf('@theme') : css.indexOf('@media (prefers-color-scheme: light)');
  const block = css.slice(at, css.indexOf('}', at));
  return new Map(
    [...block.matchAll(/(--color-[a-z]+(?:-\d+)?):\s*(#[0-9a-f]{6});/g)].map((m) => [m[1], m[2]]),
  );
}

// WCAG relative luminance and contrast ratio of two #rrggbb colours.
function luminance(hex: string): number {
  const [r, g, b] = [1, 3, 5].map((i) => {
    const c = parseInt(hex.slice(i, i + 2), 16) / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}
function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

describe('the wordmark', () => {
  // bridge#65: "Bridge" in pink-600 was 3.61:1 on the dark header.
  it('keeps "Bridge" at 4.5:1 or better on the surface in both themes', () => {
    const shade = /text-(pink-\d+)">Bridge</.exec(sources['./components/Wordmark.tsx'])?.[1];
    expect(shade).toBeDefined();
    for (const theme of ['dark', 'light'] as const) {
      const tokens = tokenValues(theme);
      const ink = tokens.get(`--color-${shade}`);
      const surface = tokens.get('--color-surface');
      expect(ink && surface, `${theme}: --color-${shade} and --color-surface`).toBeTruthy();
      expect(contrast(ink!, surface!), theme).toBeGreaterThanOrEqual(4.5);
    }
  });
});
