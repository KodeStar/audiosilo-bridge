import type { LogEntry } from '@/api/types';
import type { Tone } from './jobs';

export const LOG_LEVELS = ['INFO', 'WARN', 'ERROR'] as const;

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

export function levelTone(level: string): Tone {
  switch (level.toUpperCase()) {
    case 'ERROR':
      return 'bad';
    case 'WARN':
      return 'warn';
    default:
      return 'muted';
  }
}
