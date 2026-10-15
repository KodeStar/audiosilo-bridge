// Time and size formatting for the page. Pure functions; "now" is passed in.

// parseTime reads an RFC 3339 time (the store writes nine fractional digits,
// which some engines refuse, so the fraction is cut to milliseconds). null for
// "", null or anything unreadable.
export function parseTime(s: string | null | undefined): Date | null {
  if (!s) return null;
  const trimmed = s.replace(/(\.\d{3})\d+/, '$1');
  const ms = Date.parse(trimmed);
  return Number.isNaN(ms) ? null : new Date(ms);
}

// relativeTime words how long ago (or, for a future time, how soon) t was:
// "just now", "40 s ago", "5 min ago", "3 h ago", "2 days ago".
export function relativeTime(s: string | null | undefined, now: Date = new Date()): string {
  const t = parseTime(s);
  if (!t) return 'never';
  const diff = Math.round((now.getTime() - t.getTime()) / 1000);
  const future = diff < 0;
  const secs = Math.abs(diff);
  let text: string;
  if (secs < 5) return 'just now';
  if (secs < 60) text = `${secs} s`;
  else if (secs < 3600) text = `${Math.floor(secs / 60)} min`;
  else if (secs < 86400) text = `${Math.floor(secs / 3600)} h`;
  else {
    const days = Math.floor(secs / 86400);
    text = days === 1 ? '1 day' : `${days} days`;
  }
  return future ? `in ${text}` : `${text} ago`;
}

const DATE_TIME = new Intl.DateTimeFormat(undefined, {
  year: 'numeric',
  month: 'short',
  day: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
});

// dateTime is a short local date and time, or "" for no time.
export function dateTime(s: string | null | undefined): string {
  const t = parseTime(s);
  return t ? DATE_TIME.format(t) : '';
}

// bytes words a size: "512 B", "1.5 KB", "3.2 MB", "1.1 GB".
export function bytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return '';
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

// elapsed words a duration in milliseconds: "850 ms", "2.4 s", "3 min 5 s".
export function elapsed(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '';
  if (ms < 1000) return `${Math.round(ms)} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(1)} s`;
  const m = Math.floor(s / 60);
  return `${m} min ${Math.round(s - m * 60)} s`;
}
