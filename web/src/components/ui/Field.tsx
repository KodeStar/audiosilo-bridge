import type { ReactNode } from 'react';

// Field is a stacked label, control and optional note.
export function Field({
  label,
  htmlFor,
  note,
  children,
}: {
  label: string;
  htmlFor: string;
  note?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={htmlFor} className="text-sm font-medium text-hi">
        {label}
      </label>
      {children}
      {note && <p className="text-xs text-dim">{note}</p>}
    </div>
  );
}

export const inputClass =
  'rounded-md border border-edge bg-raised px-3 py-2 text-sm text-body placeholder:text-dim disabled:cursor-not-allowed disabled:opacity-60';
