import type { GenerateResult, Park, Progress, ServerJob } from '@/api/types';

// The sidecars.generate job (internal/generate): its progress carries the
// engine's stage, ETA, spend, source and park beside the step and the count.
export const GENERATE_KIND = 'sidecars.generate';

export function isGenerate(job: ServerJob): boolean {
  return job.kind === GENERATE_KIND;
}

// parkOf is a running job's park (null when it isn't parked). The Bridge keeps
// a parked job running under its claim, so the park lives in its progress.
export function parkOf(job: ServerJob): Park | null {
  if (job.state !== 'running') return null;
  const park = job.progress?.park;
  return park && typeof park.code === 'string' && park.code !== '' ? park : null;
}

// effectiveState is the state to show: a running job that is parked shows as
// needs attention, as the server shows it.
export function effectiveState(job: ServerJob): string {
  return parkOf(job) ? 'needs_attention' : job.state;
}

// canRetry: a parked sidecars job can be retried from this page.
export function canRetry(job: ServerJob): boolean {
  return isGenerate(job) && parkOf(job) !== null;
}

// The engine stages in words, for a stage the message doesn't already name.
const STAGES: Record<string, string> = {
  queued: 'Waiting for its turn',
  inspecting: 'Reading the audio files',
  markers_normalizing: 'Working out the chapters',
  splitting: 'Splitting the audio into chapters',
  asr: 'Transcribing',
  sanitizing: 'Tidying the transcripts',
  qa_sweep: 'Checking the transcripts',
  qa_adjudicating: 'Reviewing transcript problems',
  retranscribing: 'Transcribing chapters again',
  spelling_research: 'Researching the names',
  correcting: 'Correcting the names',
  extracting: 'Reading the ebook',
  chapter_mapping: "Working out the ebook's chapters",
  fact_pass: 'Reading for the facts',
  synthesizing: 'Writing the characters and recaps',
  validating: 'Checking the characters and recaps',
  auditing: 'Checking for spoilers',
  fixing: 'Fixing what the check found',
  ready: 'Ready to save',
  contributing: 'Saving the characters and recaps',
  done: 'Finished',
};

// stageLabel words an engine stage; an unknown one is shown as it is.
export function stageLabel(stage: string | undefined): string {
  if (!stage) return '';
  return STAGES[stage] ?? stage;
}

// etaText words the engine's ETA: "under a minute", "about 25 min",
// "about 3 h 10 min", "about 2 days 4 h". "" with none.
export function etaText(seconds: number | undefined): string {
  if (typeof seconds !== 'number' || !Number.isFinite(seconds) || seconds < 0) return '';
  if (seconds < 60) return 'under a minute';
  const mins = Math.round(seconds / 60);
  if (mins < 60) return `about ${mins} min`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) {
    const rest = mins - hours * 60;
    return rest === 0 ? `about ${hours} h` : `about ${hours} h ${rest} min`;
  }
  const days = Math.floor(hours / 24);
  const restH = hours - days * 24;
  const d = days === 1 ? '1 day' : `${days} days`;
  return restH === 0 ? `about ${d}` : `about ${d} ${restH} h`;
}

// usd words a dollar figure: "$3.42". "" for anything that isn't a number.
export function usd(n: number | undefined): string {
  if (typeof n !== 'number' || !Number.isFinite(n)) return '';
  return `$${n.toFixed(2)}`;
}

// sourceText words what a run reads.
export function sourceText(source: string | undefined): string {
  if (source === 'ebook') return 'the ebook';
  if (source === 'audio') return 'the audio';
  return '';
}

// generateFacts are the short facts under a running sidecars job: the stage,
// the ETA, the spend and the source ("" entries left out).
export function generateFacts(p: Progress | null | undefined, parked: boolean): string[] {
  if (!p) return [];
  const facts: string[] = [];
  const eta = parked ? '' : etaText(p.eta_seconds);
  if (eta) facts.push(`${eta} left`);
  const cost = usd(p.cost_usd);
  if (cost) facts.push(`${cost} spent`);
  const source = sourceText(p.source);
  if (source) facts.push(`from ${source}`);
  return facts;
}

// parseGenerateResult reads a done sidecars job's result, or null when it isn't
// one.
export function parseGenerateResult(r: unknown): GenerateResult | null {
  if (!r || typeof r !== 'object') return null;
  const o = r as Partial<GenerateResult>;
  if (typeof o.path !== 'string' || !Array.isArray(o.files)) return null;
  return {
    library_id: typeof o.library_id === 'number' ? o.library_id : 0,
    path: o.path,
    source: o.source === 'ebook' ? 'ebook' : 'audio',
    files: o.files.filter((f): f is string => typeof f === 'string'),
    characters: typeof o.characters === 'number' ? o.characters : 0,
    recaps: typeof o.recaps === 'number' ? o.recaps : 0,
    cost_usd: typeof o.cost_usd === 'number' ? o.cost_usd : 0,
    work_id: typeof o.work_id === 'string' ? o.work_id : '',
    contributed:
      o.contributed && typeof o.contributed === 'object'
        ? Object.fromEntries(
            Object.entries(o.contributed).filter(
              (e): e is [string, string] => typeof e[1] === 'string' && /^https:\/\//.test(e[1]),
            ),
          )
        : undefined,
  };
}

// resultSummary is "41 characters and 38 recaps from the audio, $12.50".
export function resultSummary(r: GenerateResult): string {
  const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;
  const what = `${plural(r.characters, 'character', 'characters')} and ${plural(r.recaps, 'recap', 'recaps')}`;
  return `${what} from ${sourceText(r.source)}, ${usd(r.cost_usd)}`;
}
