interface WordmarkProps {
  size?: 'sm' | 'lg';
}

// The logo glyph and the product wordmark, inline (no external asset).
export function Wordmark({ size = 'sm' }: WordmarkProps) {
  const glyph = size === 'lg' ? 40 : 28;
  return (
    <div className="flex items-center gap-3">
      <svg width={glyph} height={glyph} viewBox="0 0 32 32" aria-hidden="true" fill="none">
        <rect width="32" height="32" rx="8" fill="var(--color-pink-600)" />
        <g stroke="#ffffff" strokeWidth="2.4" strokeLinecap="round" opacity="0.95">
          <path d="M7 21c3-7 15-7 18 0" />
          <line x1="11" y1="17" x2="11" y2="22" />
          <line x1="16" y1="15.5" x2="16" y2="22" />
          <line x1="21" y1="17" x2="21" y2="22" />
        </g>
      </svg>
      <span
        className={size === 'lg' ? 'text-xl font-medium text-hi' : 'text-base font-medium text-hi'}
      >
        AudioSilo <span className="text-pink-600">Bridge</span>
      </span>
    </div>
  );
}
