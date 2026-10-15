import type { ReactNode } from 'react';

const LOOKS = {
  error: 'border-red-800 bg-red-950/60 text-red-200',
  warn: 'border-amber-700 bg-amber-950/60 text-amber-200',
  ok: 'border-green-800 bg-green-950/60 text-green-200',
  info: 'border-edge bg-raised text-body',
};

// Notice is an inline message: an error (role alert), a warning, or a plain note.
export function Notice({ kind, children }: { kind: keyof typeof LOOKS; children: ReactNode }) {
  return (
    <div
      role={kind === 'error' ? 'alert' : 'status'}
      className={`rounded-md border px-3 py-2 text-sm ${LOOKS[kind]}`}
    >
      {children}
    </div>
  );
}
