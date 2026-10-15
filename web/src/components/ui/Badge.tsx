export type Tone = 'good' | 'bad' | 'warn' | 'busy' | 'muted';

const TONES: Record<Tone, string> = {
  good: 'border-green-700 bg-green-950 text-green-300',
  bad: 'border-red-800 bg-red-950 text-red-300',
  warn: 'border-amber-700 bg-amber-950 text-amber-300',
  busy: 'border-pink-700 bg-pink-950 text-pink-300',
  muted: 'border-edge bg-raised text-dim',
};

export function Badge({ tone, children, title }: { tone: Tone; children: string; title?: string }) {
  return (
    <span
      title={title}
      className={`inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium ${TONES[tone]}`}
    >
      {children}
    </span>
  );
}
