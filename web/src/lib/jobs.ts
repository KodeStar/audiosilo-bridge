import type { Progress, ServerJob } from '@/api/types';
import type { Tone } from '@/components/ui/Badge';
import { effectiveState } from './generate';
import { clampPercent } from './percent';

const STATES: Record<string, { label: string; tone: Tone; hint?: string }> = {
  running: { label: 'Running', tone: 'busy' },
  done: { label: 'Done', tone: 'good' },
  failed: { label: 'Failed', tone: 'bad' },
  needs_attention: { label: 'Needs attention', tone: 'warn' },
  cancelled: { label: 'Cancelled', tone: 'muted' },
  lost: { label: 'Lost', tone: 'muted', hint: 'The server took it back' },
};

// stateLabel words a job state; an unknown state is shown as it is.
export function stateLabel(state: string): string {
  return STATES[state]?.label ?? state;
}

export function stateTone(state: string): Tone {
  return STATES[state]?.tone ?? 'muted';
}

// stateHint explains a state that needs it ("" otherwise).
export function stateHint(state: string): string {
  return STATES[state]?.hint ?? '';
}

export const JOBS_POLL_BUSY_MS = 3000;
export const JOBS_POLL_IDLE_MS = 15000;

// jobsPollInterval polls quickly while a job runs, slowly otherwise (a parked
// job may wait for days).
export function jobsPollInterval(jobs: ServerJob[]): number {
  return jobs.some((j) => effectiveState(j) === 'running') ? JOBS_POLL_BUSY_MS : JOBS_POLL_IDLE_MS;
}

// reportText says whether the server has heard a finished job's outcome ("" for
// a running job, or a lost one, which is never reported).
export function reportText(job: ServerJob): string {
  if (job.state === 'running' || job.state === 'lost') return '';
  return job.reported ? 'Reported' : 'Report pending';
}

// progressText is "stat: Comparing the files (3/12)"; "" with no progress.
export function progressText(p: Progress | null | undefined): string {
  if (!p || typeof p !== 'object') return '';
  const parts: string[] = [];
  const head = [p.step, p.message].filter((x) => typeof x === 'string' && x !== '').join(': ');
  if (head) parts.push(head);
  if (typeof p.total === 'number' && p.total > 0) parts.push(`(${p.done ?? 0}/${p.total})`);
  return parts.join(' ');
}

// progressPercent is done/total in 0-100, or null when there is no count.
export function progressPercent(p: Progress | null | undefined): number | null {
  if (!p || typeof p.total !== 'number' || p.total <= 0) return null;
  return clampPercent(((p.done ?? 0) / p.total) * 100);
}

// jobTarget is "library 2: Author/Book", "library 2" or "".
export function jobTarget(job: ServerJob): string {
  const lib = job.library_id != null ? `library ${job.library_id}` : '';
  if (job.path) return lib ? `${lib}: ${job.path}` : job.path;
  return lib;
}

// mergeJobs folds a fresh first page into the jobs already shown (which may
// include older pages): the fresh rows win, older rows beyond the fresh page are
// kept, newest first.
export function mergeJobs(fresh: ServerJob[], shown: ServerJob[]): ServerJob[] {
  if (fresh.length === 0) return [];
  const oldest = Math.min(...fresh.map((j) => j.id));
  const older = shown.filter((j) => j.id < oldest);
  return [...fresh, ...older].sort((a, b) => b.id - a.id);
}

// appendOlder adds an older page below the jobs shown, skipping any repeats.
export function appendOlder(shown: ServerJob[], older: ServerJob[]): ServerJob[] {
  const seen = new Set(shown.map((j) => j.id));
  return [...shown, ...older.filter((j) => !seen.has(j.id))];
}

// hasResult reports whether a job carries a non-empty result object.
export function hasResult(job: ServerJob): boolean {
  const r = job.result;
  if (r == null) return false;
  if (typeof r === 'object') return Object.keys(r as object).length > 0;
  return true;
}
