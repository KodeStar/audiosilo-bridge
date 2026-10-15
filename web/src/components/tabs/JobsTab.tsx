import { useCallback, useState } from 'react';
import { ApiError, errorText, type ApiClient } from '@/api/client';
import type { ServerJob, VerifyResult } from '@/api/types';
import { usePoll } from '@/lib/usePoll';
import { bytes, dateTime, elapsed, relativeTime } from '@/lib/time';
import {
  appendOlder,
  hasResult,
  isCancellable,
  jobTarget,
  jobsPollInterval,
  mergeJobs,
  progressFraction,
  progressText,
  reportText,
  stateHint,
  stateLabel,
  stateTone,
} from '@/lib/jobs';
import { VERIFY_KIND, parseVerifyResult, prettyJSON } from '@/lib/verify';
import { Badge } from '../ui/Badge';
import { Notice } from '../ui/Notice';
import { dangerButton, secondaryButton } from '../ui/buttons';

export const JOBS_PAGE = 50;

export function JobsTab({ client }: { client: ApiClient }) {
  const [jobs, setJobs] = useState<ServerJob[] | null>(null);
  // The cursor for the next older page (undefined: no more).
  const [nextBefore, setNextBefore] = useState<number | undefined>(undefined);
  // Older pages were loaded: polling then keeps them and their cursor.
  const [pagedBack, setPagedBack] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loadingOlder, setLoadingOlder] = useState(false);

  const load = useCallback(async () => {
    try {
      const res = await client.jobs(undefined, JOBS_PAGE);
      setJobs((shown) => mergeJobs(res.jobs, shown ?? []));
      if (!pagedBack) setNextBefore(res.next_before);
      setError(null);
    } catch (err) {
      setError(errorText(err, "Can't reach the Bridge."));
    }
  }, [client, pagedBack]);
  usePoll(load, jobs === null ? jobsPollInterval([]) : jobsPollInterval(jobs));

  async function older() {
    if (nextBefore === undefined) return;
    setLoadingOlder(true);
    try {
      const res = await client.jobs(nextBefore, JOBS_PAGE);
      setJobs((shown) => appendOlder(shown ?? [], res.jobs));
      setNextBefore(res.next_before);
      setPagedBack(true);
    } catch (err) {
      setError(errorText(err, "Couldn't load older jobs."));
    }
    setLoadingOlder(false);
  }

  if (jobs === null) {
    return error ? (
      <Notice kind="error">{error}</Notice>
    ) : (
      <p className="text-sm text-dim">Loading...</p>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      <p className="max-w-prose text-sm text-dim">
        The jobs this Bridge took from the server, newest first. Jobs are started from the
        server&apos;s console.
      </p>
      {error && <Notice kind="error">{error}</Notice>}
      {jobs.length === 0 ? (
        <div className="rounded-xl border border-edge bg-surface p-8">
          <h2 className="mb-2 text-lg font-medium text-hi">No jobs yet</h2>
          <p className="max-w-prose text-sm text-dim">
            When the server&apos;s console queues a job, this Bridge takes it and it shows here.
          </p>
        </div>
      ) : (
        <ul className="flex flex-col gap-3">
          {jobs.map((job) => (
            <li key={job.id}>
              <JobCard client={client} job={job} onChanged={load} />
            </li>
          ))}
        </ul>
      )}
      {nextBefore !== undefined && (
        <div>
          <button type="button" onClick={older} disabled={loadingOlder} className={secondaryButton}>
            {loadingOlder ? 'Loading...' : 'Older'}
          </button>
        </div>
      )}
    </div>
  );
}

function JobCard({
  client,
  job,
  onChanged,
}: {
  client: ApiClient;
  job: ServerJob;
  onChanged: () => Promise<void>;
}) {
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<{ kind: 'error' | 'info'; text: string } | null>(null);
  const progress = progressText(job.progress);
  const fraction = progressFraction(job.progress);
  const report = reportText(job);
  const hint = stateHint(job.state);
  const target = jobTarget(job);

  async function cancel() {
    setBusy(true);
    setNote(null);
    try {
      await client.cancelJob(job.id);
      setNote({ kind: 'info', text: 'Cancelling. The server hears about it next.' });
      await onChanged();
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) {
        setNote({ kind: 'info', text: "That job isn't running here any more." });
        await onChanged();
      } else {
        setNote({ kind: 'error', text: errorText(err, "Couldn't cancel the job.") });
      }
    }
    setBusy(false);
  }

  return (
    <article className="rounded-xl border border-edge bg-surface p-4" aria-label={`Job ${job.id}`}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-sm font-medium text-hi">#{job.id}</span>
          <code className="text-xs text-body">{job.kind}</code>
          <Badge tone={stateTone(job.state)} title={hint || undefined}>
            {stateLabel(job.state)}
          </Badge>
          {hint && <span className="text-xs text-dim">{hint}</span>}
          {report && (
            <span className={report === 'Reported' ? 'text-xs text-dim' : 'text-xs text-amber-300'}>
              {report}
            </span>
          )}
        </div>
        {isCancellable(job) && (
          <button type="button" onClick={cancel} disabled={busy} className={dangerButton}>
            {busy ? 'Cancelling...' : 'Cancel'}
          </button>
        )}
      </div>

      {target && <p className="mt-2 text-sm break-all text-body">{target}</p>}

      {job.state === 'running' && progress && (
        <div className="mt-2 flex flex-col gap-1">
          <p className="text-xs text-dim">{progress}</p>
          {fraction !== null && (
            <div
              className="h-1.5 w-full overflow-hidden rounded-full bg-raised"
              role="progressbar"
              aria-label={`Job ${job.id} progress`}
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuenow={Math.round(fraction * 100)}
            >
              <div className="h-full bg-pink-600" style={{ width: `${fraction * 100}%` }} />
            </div>
          )}
        </div>
      )}

      {job.error && <p className="mt-2 text-sm text-red-300">{job.error}</p>}

      <p className="mt-2 text-xs text-dim">
        Attempt {job.attempt}
        {' - '}claimed <span title={dateTime(job.claimed_at)}>{relativeTime(job.claimed_at)}</span>
        {' - '}updated <span title={dateTime(job.updated_at)}>{relativeTime(job.updated_at)}</span>
        {job.finished_at && (
          <>
            {' - '}finished{' '}
            <span title={dateTime(job.finished_at)}>{relativeTime(job.finished_at)}</span>
          </>
        )}
      </p>

      {note && (
        <div className="mt-2">
          <Notice kind={note.kind}>{note.text}</Notice>
        </div>
      )}

      {hasResult(job) && (
        <details className="mt-3">
          <summary className="cursor-pointer text-sm text-pink-500">Details</summary>
          <div className="mt-2">
            <ResultView job={job} />
          </div>
        </details>
      )}
    </article>
  );
}

function ResultView({ job }: { job: ServerJob }) {
  const verify = job.kind === VERIFY_KIND ? parseVerifyResult(job.result) : null;
  if (verify) return <VerifyView result={verify} />;
  return (
    <pre className="overflow-x-auto rounded-md bg-deep p-3 text-xs text-body">
      {prettyJSON(job.result)}
    </pre>
  );
}

function VerifyView({ result }: { result: VerifyResult }) {
  return (
    <div className="flex flex-col gap-2">
      {result.path && (
        <p className="text-xs text-dim">
          Book <code className="break-all text-body">{result.path}</code>
        </p>
      )}
      <p className="text-xs text-dim">
        {result.mount_path && (
          <>
            Through <code>{result.mount_path}</code>
            {' - '}
          </>
        )}
        {result.writable
          ? 'writable'
          : `not writable${result.writable_error ? `: ${result.writable_error}` : ''}`}
        {' - '}took {elapsed(result.elapsed_ms)}
      </p>
      {result.files.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-xs">
            <thead className="text-dim">
              <tr>
                <th scope="col" className="py-1 pr-3 font-medium">
                  File
                </th>
                <th scope="col" className="py-1 pr-3 font-medium">
                  Size here
                </th>
                <th scope="col" className="py-1 pr-3 font-medium">
                  Expected
                </th>
                <th scope="col" className="py-1 font-medium">
                  OK
                </th>
              </tr>
            </thead>
            <tbody className="divide-y divide-edge">
              {result.files.map((f) => (
                <tr key={f.path}>
                  <td className="py-1 pr-3 break-all text-body">
                    {f.path}
                    {f.error && <span className="block text-red-300">{f.error}</span>}
                  </td>
                  <td className="py-1 pr-3 whitespace-nowrap">{bytes(f.size)}</td>
                  <td className="py-1 pr-3 whitespace-nowrap">{bytes(f.expected_size)}</td>
                  <td className="py-1">
                    {f.ok ? (
                      <span className="text-green-300">Yes</span>
                    ) : (
                      <span className="text-red-300">No</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
