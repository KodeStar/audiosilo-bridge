import { describe, expect, it } from 'vitest';
import { clampPercent } from './percent';

describe('clampPercent', () => {
  it('keeps 0-100 and holds the rest to the ends', () => {
    expect(clampPercent(42.5)).toBe(42.5);
    expect(clampPercent(250)).toBe(100);
    expect(clampPercent(-3)).toBe(0);
    expect(clampPercent(Infinity)).toBe(100);
    expect(clampPercent(NaN)).toBe(0);
  });
});
