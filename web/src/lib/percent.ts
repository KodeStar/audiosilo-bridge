// clampPercent holds a percentage to 0-100 (NaN is 0), for the page's
// progress bars.
export function clampPercent(n: number): number {
  return Number.isNaN(n) ? 0 : Math.min(100, Math.max(0, n));
}
