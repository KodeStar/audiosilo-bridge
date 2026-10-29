import { useState } from 'react';
import { ApiError, errorText, type ApiClient } from '@/api/client';
import type {
  ChaptersResult,
  ReadalongResult,
  DescribeResult,
  GenerateResult,
  ServerJob,
  VerifyResult,
} from '@/api/types';
import { useLoad } from '@/lib/useLoad';
import { bytes, elapsed } from '@/lib/time';
import {
  appendOlder,
  hasResult,
  jobTarget,
  jobsPollInterval,
  mergeJobs,
  progressPercent,
  progressText,
  reportText,
  stateHint,
  stateLabel,
  stateTone,
} from '@/lib/jobs';
import { VERIFY_KIND, parseVerifyResult, prettyJSON } from '@/lib/verify';
import {
  canRetry,
  descriptionText,
  chaptersSummary,
  isReadalong,
  readalongSummary,
  describeSummary,
  effectiveState,
  generateFacts,
  isChapters,
  isDescribe,
  isGenerate,
  isSidecars,
  kindLabel,
  parkOf,
  parkText,
  parseChaptersResult,
  parseReadalongResult,
  parseDescribeResult,
  parseGenerateResult,
  resultSummary,
} from '@/lib/generate';
import { jobHash } from '@/lib/tabs';
import { Ago } from '../ui/Ago';
import { Badge } from '../ui/Badge';
import { EmptyState } from '../ui/EmptyState';
import { Loading } from '../ui/Loading';
import { Notice } from '../ui/Notice';
import { ProgressBar } from '../ui/ProgressBar';
import { dangerButton, primaryButton, secondaryButton } from '../ui/buttons';

export const JOBS_PAGE = 50;

// JobsTab lists the jobs; with focusId (the console's #/jobs/<id> link) it shows
// that one job.
export function JobsTab({
  client,
  focusId = null,
}: {
  client: ApiClient;
  focusId?: number | null;
}) {
  if (focusId !== null) return <FocusedJob client={client} id={focusId} />;
  return <JobList client={client} />;
}

// FocusedJob is one job's detail, polled like the list.
function FocusedJob({ client, id }: { client: ApiClient; id: number }) {
  const {
    data: job,
    error,
    reload,
  } = useLoad(
    () => client.job(id),
    (shown) => jobsPollInterval(shown ? [shown] : []),
    { fallback: `This Bridge has no job ${id}.` },
  );
  return (
    <div className="flex flex-col gap-4">
      <p className="text-sm">
        <a href="#jobs" className="text-pink-500 underline">
          All jobs
        </a>
      </p>
      {job === null ? (
        <Loading error={error} />
      ) : (
        <>
          {error && <Notice kind="error">{error}</Notice>}
          <JobCard client={client} job={job} onChanged={reload} open />
        </>
      )}
    </div>
  );
}

function JobList({ client }: { client: ApiClient }) {
  // The cursor for the next older page (undefined: no more).
  const [nextBefore, setNextBefore] = useState<number | undefined>(undefined);
  // Older pages were loaded: polling then keeps them and their cursor.
  const [pagedBack, setPagedBack] = useState(false);
  const [loadingOlder, setLoadingOlder] = useState(false);

  const {
    data: jobs,
    setData: setJobs,
    error,
    setError,
    reload,
  } = useLoad(
    async () => {
      const res = await client.jobs(undefined, JOBS_PAGE);
      if (!pagedBack) setNextBefore(res.next_before);
      return res.jobs;
    },
    (shown) => jobsPollInterval(shown ?? []),
    { merge: (fresh, shown) => mergeJobs(fresh, shown ?? []) },
  );

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

  if (jobs === null) return <Loading error={error} />;

  return (
    <div className="flex flex-col gap-4">
      <p className="max-w-prose text-sm text-dim">
        What this Bridge was asked to do, newest first: by an admin in the server&apos;s console, by
        a listener in the player, or by the server&apos;s schedule.
      </p>
      {error && <Notice kind="error">{error}</Notice>}
      {jobs.length === 0 ? (
        <EmptyState title="No jobs yet">
          When the server queues a job, this Bridge takes it and it shows here.
        </EmptyState>
      ) : (
        <ul className="flex flex-col gap-3">
          {jobs.map((job) => (
            <li key={job.id}>
              <JobCard client={client} job={job} onChanged={reload} />
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
  open = false,
}: {
  client: ApiClient;
  job: ServerJob;
  onChanged: () => Promise<void>;
  // Show the details at once (the job's own page).
  open?: boolean;
}) {
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState<{ kind: 'error' | 'info'; text: string } | null>(null);
  const sidecars = isSidecars(job);
  const label = kindLabel(job.kind);
  const shown = effectiveState(job);
  const progress = sidecars ? '' : progressText(job.progress);
  const percent = progressPercent(job.progress);
  const report = reportText(job);
  const hint = stateHint(shown);
  const target = jobTarget(job);

  async function retry() {
    setBusy(true);
    setNote(null);
    try {
      await client.retryJob(job.id);
      setNote({ kind: 'info', text: 'Retrying. It goes on in a few seconds.' });
      await onChanged();
    } catch (err) {
      setNote({ kind: 'error', text: errorText(err, "Couldn't retry the job.") });
    }
    setBusy(false);
  }

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
          <a href={jobHash(job.id)} className="text-sm font-medium text-hi hover:underline">
            #{job.id}
          </a>
          {label && <span className="text-sm text-body">{label}</span>}
          <code className="text-xs text-body">{job.kind}</code>
          <Badge tone={stateTone(shown)} title={hint || undefined}>
            {stateLabel(shown)}
          </Badge>
          {hint && <span className="text-xs text-dim">{hint}</span>}
          {report && (
            <span className={report === 'Reported' ? 'text-xs text-dim' : 'text-xs text-amber-300'}>
              {report}
            </span>
          )}
        </div>
        {job.state === 'running' && (
          <div className="flex gap-2">
            {canRetry(job) && (
              <button type="button" onClick={retry} disabled={busy} className={primaryButton}>
                Retry
              </button>
            )}
            <button type="button" onClick={cancel} disabled={busy} className={dangerButton}>
              {busy ? 'Working...' : 'Cancel'}
            </button>
          </div>
        )}
      </div>

      {target && <p className="mt-2 text-sm break-all text-body">{target}</p>}

      {sidecars && job.state === 'running' && <GenerateProgress job={job} />}

      {job.state === 'running' && progress && (
        <div className="mt-2 flex flex-col gap-1">
          <p className="text-xs text-dim">{progress}</p>
          {percent !== null && <ProgressBar percent={percent} label={`Job ${job.id} progress`} />}
        </div>
      )}

      {job.error && !parkOf(job) && <p className="mt-2 text-sm text-red-300">{job.error}</p>}

      <p className="mt-2 text-xs text-dim">
        Attempt {job.attempt}
        {' - '}claimed <Ago at={job.claimed_at} />
        {' - '}updated <Ago at={job.updated_at} />
        {job.finished_at && (
          <>
            {' - '}finished <Ago at={job.finished_at} />
          </>
        )}
      </p>

      {note && (
        <div className="mt-2">
          <Notice kind={note.kind}>{note.text}</Notice>
        </div>
      )}

      {hasResult(job) && (
        <details className="mt-3" open={open || undefined}>
          <summary className="cursor-pointer text-sm text-pink-500">Details</summary>
          <div className="mt-2">
            <ResultView job={job} />
          </div>
        </details>
      )}
    </article>
  );
}

// GenerateProgress is a running sidecars job: the Bridge's message for the step,
// its count, the ETA, the spend and the source; while parked, the park and its
// advice.
function GenerateProgress({ job }: { job: ServerJob }) {
  const p = job.progress;
  const park = parkOf(job);
  const parkMsg = park ? parkText(job, park) : '';
  const percent = progressPercent(p);
  const facts = generateFacts(p, park !== null);
  return (
    <div className="mt-2 flex flex-col gap-1.5">
      {p?.message && <p className="text-sm text-body">{p.message}</p>}
      {percent !== null && !park && (
        <ProgressBar percent={percent} label={`Job ${job.id} progress`} />
      )}
      {facts.length > 0 && <p className="text-xs text-dim">{facts.join(' - ')}</p>}
      {park && (
        <Notice kind="warn">
          {parkMsg && <p>{parkMsg}</p>}
          {park.hint && <p className={parkMsg ? 'mt-1' : undefined}>{park.hint}</p>}
          {!parkMsg && !park.hint && <p>The job needs attention: see the Bridge&apos;s log.</p>}
          {park.retry_at && (
            <p className="mt-1 text-xs">
              Resumes by itself <Ago at={park.retry_at} />.
            </p>
          )}
        </Notice>
      )}
    </div>
  );
}

function ResultView({ job }: { job: ServerJob }) {
  const verify = job.kind === VERIFY_KIND ? parseVerifyResult(job.result) : null;
  if (verify) return <VerifyView result={verify} />;
  const generated = isGenerate(job) ? parseGenerateResult(job.result) : null;
  if (generated) return <GenerateView result={generated} />;
  const described = isDescribe(job) ? parseDescribeResult(job.result) : null;
  if (described) return <DescribeView result={described} />;
  const chapters = isChapters(job) ? parseChaptersResult(job.result) : null;
  if (chapters) return <ChaptersView result={chapters} />;
  const aligned = isReadalong(job) ? parseReadalongResult(job.result) : null;
  if (aligned) return <ReadalongView result={aligned} />;
  return (
    <pre className="overflow-x-auto rounded-md bg-deep p-3 text-xs text-body">
      {prettyJSON(job.result)}
    </pre>
  );
}

function GenerateView({ result }: { result: GenerateResult }) {
  const links = Object.entries(result.contributed ?? {});
  const description = descriptionText(result);
  return (
    <div className="flex flex-col gap-2 text-xs text-dim">
      <p className="text-sm text-body">{resultSummary(result)}</p>
      {description && <p>{description}</p>}
      <SavedWith path={result.path} files={result.files} />
      {result.work_id && (
        <p>
          Work <code className="text-body">{result.work_id}</code>
        </p>
      )}
      {links.length > 0 && (
        <ul className="flex flex-col gap-1">
          {links.map(([kind, url]) => (
            <li key={kind}>
              Contributed {kind}:{' '}
              <a href={url} target="_blank" rel="noreferrer" className="text-pink-500 underline">
                {url}
              </a>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function DescribeView({ result }: { result: DescribeResult }) {
  return (
    <div className="flex flex-col gap-2 text-xs text-dim">
      <p className="text-sm text-body">{describeSummary(result)}</p>
      <SavedWith path={result.path} files={result.files} />
    </div>
  );
}

function ChaptersView({ result }: { result: ChaptersResult }) {
  return (
    <div className="flex flex-col gap-2 text-xs text-dim">
      <p className="text-sm text-body">{chaptersSummary(result)}</p>
      {result.files.length > 0 && <SavedWith path={result.path} files={result.files} />}
    </div>
  );
}

function ReadalongView({ result }: { result: ReadalongResult }) {
  return (
    <div className="flex flex-col gap-2 text-xs text-dim">
      <p className="text-sm text-body">{readalongSummary(result)}</p>
      <SavedWith path={result.path} files={[result.file]} />
    </div>
  );
}

// SavedWith says where a sidecars job saved its files.
function SavedWith({ path, files }: { path: string; files: string[] }) {
  return (
    <p>
      Saved with <code className="break-all text-body">{path}</code> in its <code>.audiosilo</code>{' '}
      folder: {files.join(', ')}
    </p>
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
