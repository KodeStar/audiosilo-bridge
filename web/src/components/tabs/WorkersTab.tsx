import type { ApiClient } from '@/api/client';
import { useLoad } from '@/lib/useLoad';
import { relativeTime } from '@/lib/time';
import { EmptyState } from '../ui/EmptyState';
import { Loading } from '../ui/Loading';

export function WorkersTab({ client }: { client: ApiClient }) {
  const { data: workers, error } = useLoad(async () => (await client.workers()).workers, null);

  if (workers === null) return <Loading error={error} />;
  if (workers.length === 0) {
    return <EmptyState title="No workers">ASR workers connect here in a later version.</EmptyState>;
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
