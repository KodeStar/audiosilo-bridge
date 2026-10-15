import type { ReactNode } from 'react';

// Row is one label and value line of a definition list.
export function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid grid-cols-1 gap-1 py-2 sm:grid-cols-[12rem_1fr] sm:gap-4">
      <dt className="text-sm text-dim">{label}</dt>
      <dd className="text-sm break-words text-body">{children}</dd>
    </div>
  );
}
