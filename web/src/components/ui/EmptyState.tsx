import type { ReactNode } from 'react';

// EmptyState says why a list has nothing in it yet.
export function EmptyState({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="rounded-xl border border-edge bg-surface p-8">
      <h2 className="mb-2 text-lg font-medium text-hi">{title}</h2>
      <p className="max-w-prose text-sm text-dim">{children}</p>
    </div>
  );
}
