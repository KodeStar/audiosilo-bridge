import type { LogEntry, LogLevel } from '@/api/types';

// The level filter's choices: each shows its level and those above it.
export const LOG_LEVELS: { level: LogLevel; label: string }[] = [
  { level: 'INFO', label: 'INFO and up' },
  { level: 'WARN', label: 'WARN and up' },
  { level: 'ERROR', label: 'ERROR' },
];

// The most lines the page keeps.
export const MAX_LOG_LINES = 1000;

export const LOGS_POLL_MS = 3000;

// appendLogs adds new lines (oldest first) below the shown ones, skipping
// repeats, and keeps the newest max.
export function appendLogs(
  shown: LogEntry[],
  incoming: LogEntry[],
  max = MAX_LOG_LINES,
): LogEntry[] {
  if (incoming.length === 0) return shown;
  const last = lastLogId(shown);
  const fresh = incoming.filter((e) => e.id > last);
  if (fresh.length === 0) return shown;
  const all = [...shown, ...fresh];
  return all.length > max ? all.slice(all.length - max) : all;
}

// lastLogId is the newest id shown (0 for none): the next poll's after.
export function lastLogId(entries: LogEntry[]): number {
  return entries.length === 0 ? 0 : entries[entries.length - 1].id;
}

// formatAttrs is "key=value key2=value2", keys sorted; a value with a space or a
// quote is quoted.
export function formatAttrs(attrs: Record<string, string> | undefined): string {
  if (!attrs) return '';
  return Object.keys(attrs)
    .sort()
    .map((k) => {
      const v = attrs[k];
      return `${k}=${/[\s"=]/.test(v) || v === '' ? JSON.stringify(v) : v}`;
    })
    .join(' ');
}

// levelClass is the text colour of a line's level: errors red, warnings amber.
export function levelClass(level: string): string {
  switch (level.toUpperCase()) {
    case 'ERROR':
      return 'text-red-300';
    case 'WARN':
      return 'text-amber-300';
    default:
      return 'text-dim';
  }
}
