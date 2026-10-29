import type {
  ChaptersResult,
  CheckedAgainst,
  DescribeResult,
  GenerateResult,
  Park,
  ReadalongResult,
  Progress,
  ServerJob,
} from '@/api/types';

// The sidecars.generate job (internal/generate): its progress carries the
// engine's stage, ETA, spend, source and park beside the step and the count.
export const GENERATE_KIND = 'sidecars.generate';
// The description-only job: the same progress, without an ETA.
export const DESCRIBE_KIND = 'sidecars.describe';
// The chapters-only job (Phase 1d): speech recognition and chapter detection for a
// book with no chapter markers; the same progress, without a spend.
export const CHAPTERS_KIND = 'chapters.detect';
// The read-along job (Phase 3c): the ebook lined up with the narration, no agent;
// the same progress, without a spend.
export const READALONG_KIND = 'readalong.align';

export function isGenerate(job: ServerJob): boolean {
  return job.kind === GENERATE_KIND;
}

export function isDescribe(job: ServerJob): boolean {
  return job.kind === DESCRIBE_KIND;
}

export function isChapters(job: ServerJob): boolean {
  return job.kind === CHAPTERS_KIND;
}

export function isReadalong(job: ServerJob): boolean {
  return job.kind === READALONG_KIND;
}

// isSidecars: a job that follows the engine (its progress and parks look alike).
export function isSidecars(job: ServerJob): boolean {
  return isGenerate(job) || isDescribe(job) || isChapters(job) || isReadalong(job);
}

// The console's names for the same kinds (its bridge.kind.* strings): one set of
// names across the two pages.
const KIND_LABELS: Record<string, string> = {
  [GENERATE_KIND]: 'Characters and recaps',
  [DESCRIBE_KIND]: 'Description',
  [CHAPTERS_KIND]: 'Chapters',
  [READALONG_KIND]: 'Read-along',
  'sidecars.contribute': 'Community share',
  'meta.correct': 'Corrections',
  'bridge.verify': 'Verify',
  'podcast.subscribe': 'Following a show',
  'podcast.poll': 'Checking a feed',
  'podcast.fetch': 'Fetching an episode',
  'podcast.retain': 'Checking retained episodes',
};

// kindLabel words a job kind ("" for one with no label: the kind shows as it is).
// Own keys only: a kind like "constructor" must not find Object's.
export function kindLabel(kind: string): string {
  return Object.hasOwn(KIND_LABELS, kind) ? KIND_LABELS[kind] : '';
}

// parkOf is a running job's park (null when it isn't parked). The Bridge keeps
// a parked job running under its claim, so the park lives in its progress.
export function parkOf(job: ServerJob): Park | null {
  if (job.state !== 'running') return null;
  const park = job.progress?.park;
  return park && typeof park.code === 'string' && park.code !== '' ? park : null;
}

// parkText is a parked job's message as prose: the engine's message, left out
// when it is only the park's code (a park with no message) or repeats the hint.
// The code itself is for the log, never the page.
export function parkText(job: ServerJob, park: Park): string {
  let msg = (job.error ?? '').trim();
  const prefix = `${park.code}: `;
  if (msg.startsWith(prefix)) msg = msg.slice(prefix.length).trim();
  return msg === '' || msg === park.code || msg === park.hint ? '' : msg;
}

// effectiveState is the state to show: a running job that is parked shows as
// needs attention, as the server shows it.
export function effectiveState(job: ServerJob): string {
  return parkOf(job) ? 'needs_attention' : job.state;
}

// canRetry: a parked sidecars job can be retried from this page.
export function canRetry(job: ServerJob): boolean {
  return isSidecars(job) && parkOf(job) !== null;
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

// generateFacts are the short facts under a running sidecars job's message: the
// ETA, the spend and the source ("" entries left out).
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

// SavedFields are what every sidecars job's result carries: the library, the
// book's path, the names written in its .audiosilo folder and the spend.
type SavedFields = Pick<GenerateResult, 'library_id' | 'path' | 'files' | 'cost_usd'>;

// parseSavedFields reads a done sidecars job's shared fields, or null when the
// result isn't one.
function parseSavedFields(r: unknown): SavedFields | null {
  if (!r || typeof r !== 'object') return null;
  const o = r as Partial<SavedFields>;
  if (typeof o.path !== 'string' || !Array.isArray(o.files)) return null;
  return {
    library_id: typeof o.library_id === 'number' ? o.library_id : 0,
    path: o.path,
    files: o.files.filter((f): f is string => typeof f === 'string'),
    cost_usd: typeof o.cost_usd === 'number' ? o.cost_usd : 0,
  };
}

// parseGenerateResult reads a done sidecars job's result, or null when it isn't
// one.
export function parseGenerateResult(r: unknown): GenerateResult | null {
  const saved = parseSavedFields(r);
  if (!saved) return null;
  const o = r as Partial<GenerateResult>;
  return {
    ...saved,
    source: o.source === 'ebook' ? 'ebook' : 'audio',
    characters: typeof o.characters === 'number' ? o.characters : 0,
    recaps: typeof o.recaps === 'number' ? o.recaps : 0,
    work_id: typeof o.work_id === 'string' ? o.work_id : '',
    contributed:
      o.contributed && typeof o.contributed === 'object'
        ? Object.fromEntries(
            Object.entries(o.contributed).filter(
              (e): e is [string, string] => typeof e[1] === 'string' && /^https:\/\//.test(e[1]),
            ),
          )
        : undefined,
    description: typeof o.description === 'boolean' ? o.description : undefined,
    description_error: typeof o.description_error === 'string' ? o.description_error : undefined,
  };
}

// resultSummary is "41 characters and 38 recaps from the audio, $12.50".
export function resultSummary(r: GenerateResult): string {
  const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;
  const what = `${plural(r.characters, 'character', 'characters')} and ${plural(r.recaps, 'recap', 'recaps')}`;
  return `${what} from ${sourceText(r.source)}, ${usd(r.cost_usd)}`;
}

// descriptionText words a generate result's description outcome ("" from a
// Bridge before descriptions).
export function descriptionText(r: GenerateResult): string {
  if (r.description === true) return 'With a description';
  if (r.description === false) {
    return r.description_error ? `No description: ${r.description_error}` : 'No description';
  }
  return '';
}

// checkedText words what a description's no-verbatim check measured against.
export function checkedText(c: CheckedAgainst): string {
  if (c === 'transcripts') return 'checked against the transcripts';
  if (c === 'book text') return 'checked against the book text';
  return 'not checked: the source text was already removed';
}

// parseDescribeResult reads a done description-only job's result, or null when
// it isn't one.
export function parseDescribeResult(r: unknown): DescribeResult | null {
  const saved = parseSavedFields(r);
  if (!saved) return null;
  const o = r as Partial<DescribeResult>;
  const checked: CheckedAgainst =
    o.checked_against === 'transcripts' || o.checked_against === 'book text'
      ? o.checked_against
      : 'none';
  return { ...saved, description: o.description === true, checked_against: checked };
}

// describeSummary is "A description, $0.25, checked against the transcripts".
export function describeSummary(r: DescribeResult): string {
  return `A description, ${usd(r.cost_usd)}, ${checkedText(r.checked_against)}`;
}

// parseChaptersResult reads a done chapters job's result, or null when it isn't
// one.
export function parseChaptersResult(r: unknown): ChaptersResult | null {
  const saved = parseSavedFields(r);
  const o = r as Partial<ChaptersResult>;
  if (!saved || typeof o.chapters !== 'number') return null;
  // A chapters job spends nothing, so its result carries no cost.
  const { library_id, path, files } = saved;
  return { library_id, path, files, chapters: o.chapters, source: 'audio' };
}

// chaptersSummary is "18 chapters found in the narration" ("One chapter: ..." for a
// book with no headings, which gets no chapters file; one written with no file says
// so too: more chapters than the server reads).
export function chaptersSummary(r: ChaptersResult): string {
  if (r.chapters <= 1) return 'One chapter: no chapter file was written';
  if (r.files.length === 0)
    return `${r.chapters} chapters found in the narration, but no chapter file was written`;
  return `${r.chapters} chapters found in the narration`;
}

// parseReadalongResult reads a done read-along job's result, or null when it isn't
// one.
export function parseReadalongResult(r: unknown): ReadalongResult | null {
  if (!r || typeof r !== 'object') return null;
  const o = r as Partial<ReadalongResult>;
  const cov = o.coverage;
  if (
    typeof o.path !== 'string' ||
    typeof o.file !== 'string' ||
    !cov ||
    typeof cov.text !== 'number' ||
    typeof cov.audio !== 'number' ||
    typeof o.spans !== 'number'
  )
    return null;
  return {
    library_id: typeof o.library_id === 'number' ? o.library_id : 0,
    path: o.path,
    file: o.file,
    coverage: { text: cov.text, audio: cov.audio },
    sections: typeof o.sections === 'number' ? o.sections : 0,
    spans: o.spans,
    audio_gaps: typeof o.audio_gaps === 'number' ? o.audio_gaps : 0,
    heal_windows: typeof o.heal_windows === 'number' ? o.heal_windows : 0,
    transcribed: o.transcribed === true,
    ebook_sha256: typeof o.ebook_sha256 === 'string' ? o.ebook_sha256 : '',
  };
}

// readalongSummary is "96% of the text lined up (1,234 sentences), transcribed
// for it" ("from the transcript kept" when an earlier run's was reused).
export function readalongSummary(r: ReadalongResult): string {
  const pct = Math.round(r.coverage.text * 100);
  const sentences = r.spans === 1 ? '1 sentence' : `${r.spans.toLocaleString('en-US')} sentences`;
  const how = r.transcribed ? 'transcribed for it' : 'from the transcript kept';
  return `${pct}% of the text lined up (${sentences}), ${how}`;
}
