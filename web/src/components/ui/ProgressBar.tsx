// ProgressBar is a thin bar for a percentage in 0-100 (lib/percent's
// clampPercent makes one), with the progressbar role for screen readers.
export function ProgressBar({
  percent,
  label,
  tone = 'pink',
  className = '',
}: {
  percent: number;
  label: string;
  tone?: 'pink' | 'red';
  className?: string;
}) {
  return (
    <div
      className={`h-1.5 w-full overflow-hidden rounded-full bg-raised ${className}`}
      role="progressbar"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(percent)}
    >
      <div
        className={tone === 'red' ? 'h-full bg-red-600' : 'h-full bg-pink-600'}
        style={{ width: `${percent}%` }}
      />
    </div>
  );
}
