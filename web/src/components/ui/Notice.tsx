import type { ReactNode } from 'react';

// Notice is an inline message: an error (role alert) or a plain note.
export function Notice({ kind, children }: { kind: 'error' | 'info' | 'ok'; children: ReactNode }) {
  const look =
    kind === 'error'
      ? 'border-red-800 bg-red-950/60 text-red-200'
      : kind === 'ok'
        ? 'border-green-800 bg-green-950/60 text-green-200'
        : 'border-edge bg-raised text-body';
  return (
    <div
      role={kind === 'error' ? 'alert' : 'status'}
      className={`rounded-md border px-3 py-2 text-sm ${look}`}
    >
      {children}
    </div>
  );
}
