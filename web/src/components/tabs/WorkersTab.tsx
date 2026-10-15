import { useEffect, useState } from 'react';
import { errorText, type ApiClient } from '@/api/client';
import type { Worker } from '@/api/types';
import { relativeTime } from '@/lib/time';
import { Notice } from '../ui/Notice';

export function WorkersTab({ client }: { client: ApiClient }) {
  const [workers, setWorkers] = useState<Worker[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    client
      .workers()
      .then((r) => {
        if (!cancelled) setWorkers(r.workers);
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(errorText(err, "Can't reach the Bridge."));
      });
    return () => {
      cancelled = true;
    };
  }, [client]);

  if (error) return <Notice kind="error">{error}</Notice>;
  if (workers === null) return <p className="text-sm text-dim">Loading...</p>;
  if (workers.length === 0) {
    return (
      <div className="rounded-xl border border-edge bg-surface p-8">
        <h2 className="mb-2 text-lg font-medium text-hi">No workers</h2>
        <p className="max-w-prose text-sm text-dim">ASR workers connect here in a later version.</p>
      </div>
    );
  }
  return (
    <ul className="flex flex-col gap-3">
      {workers.map((w) => (
        <li key={w.name} className="rounded-xl border border-edge bg-surface p-4 text-sm">
          <span className="font-medium text-hi">{w.name}</span>
          <span className="ml-2 text-dim">
            {[w.version, w.platform].filter(Boolean).join(', ')}
            {w.last_seen && ` - seen ${relativeTime(w.last_seen)}`}
          </span>
        </li>
      ))}
    </ul>
  );
}
