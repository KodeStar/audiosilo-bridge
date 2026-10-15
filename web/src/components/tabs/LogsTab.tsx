import { useCallback, useEffect, useRef, useState } from 'react';
import { errorText, type ApiClient } from '@/api/client';
import type { LogEntry, LogLevel } from '@/api/types';
import { usePoll } from '@/lib/usePoll';
import { dateTime } from '@/lib/time';
import {
  LOGS_POLL_MS,
  LOG_LEVELS,
  appendLogs,
  formatAttrs,
  lastLogId,
  levelClass,
} from '@/lib/logs';
import { inputClass } from '../ui/Field';
import { Notice } from '../ui/Notice';

export function LogsTab({ client }: { client: ApiClient }) {
  const [level, setLevel] = useState<LogLevel>('INFO');
  // null: nothing fetched yet at this level.
  const [entries, setEntries] = useState<LogEntry[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [follow, setFollow] = useState(true);
  // The newest id shown, read by the poll without restarting it.
  const lastId = useRef(0);
  // Bumped on a level change, so an answer for the old level is dropped.
  const generation = useRef(0);
  const box = useRef<HTMLDivElement>(null);

  const load = useCallback(async () => {
    const gen = generation.current;
    try {
      const res = await client.logs(lastId.current, level);
      if (gen !== generation.current) return;
      setEntries((shown) => {
        const next = appendLogs(shown ?? [], res.entries);
        lastId.current = lastLogId(next);
        return next;
      });
      setError(null);
    } catch (err) {
      if (gen === generation.current) setError(errorText(err, "Can't reach the Bridge."));
    }
  }, [client, level]);

  // A level change starts the list again and restarts the poll at once.
  usePoll(load, LOGS_POLL_MS, level);

  function changeLevel(next: LogLevel) {
    generation.current++;
    lastId.current = 0;
    setEntries(null);
    setLevel(next);
  }

  // Follow: keep the newest line in view.
  useEffect(() => {
    const el = box.current;
    if (follow && el) el.scrollTop = el.scrollHeight;
  }, [entries, follow]);

  function onScroll() {
    const el = box.current;
    if (!el) return;
    setFollow(el.scrollHeight - el.scrollTop - el.clientHeight < 24);
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <label htmlFor="log-level" className="text-sm text-hi">
            Level
          </label>
          <select
            id="log-level"
            value={level}
            onChange={(e) => changeLevel(e.target.value as LogLevel)}
            className={inputClass}
          >
            {LOG_LEVELS.map((l) => (
              <option key={l.level} value={l.level}>
                {l.label}
              </option>
            ))}
          </select>
        </div>
        <span className="text-xs text-dim">
          {follow ? 'Following new lines' : 'Scroll to the bottom to follow'}
        </span>
      </div>
      {error && <Notice kind="error">{error}</Notice>}
      <div
        ref={box}
        onScroll={onScroll}
        tabIndex={0}
        role="log"
        aria-label="Bridge log"
        className="h-[60vh] overflow-y-auto rounded-xl border border-edge bg-deep p-3 font-mono text-xs"
      >
        {entries === null ? (
          <p className="text-dim">Loading...</p>
        ) : entries.length === 0 ? (
          <p className="text-dim">No log lines at this level yet.</p>
        ) : (
          <ol className="flex flex-col gap-0.5">
            {entries.map((e) => {
              const attrs = formatAttrs(e.attrs);
              return (
                <li key={e.id} className="break-words whitespace-pre-wrap">
                  <span className="text-dim">{dateTime(e.time)}</span>{' '}
                  <span className={levelClass(e.level)}>{e.level}</span>{' '}
                  <span className="text-hi">{e.message}</span>
                  {attrs && <span className="text-dim"> {attrs}</span>}
                </li>
              );
            })}
          </ol>
        )}
      </div>
    </div>
  );
}
