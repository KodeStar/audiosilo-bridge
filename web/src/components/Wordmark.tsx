interface WordmarkProps {
  size?: 'sm' | 'lg';
}

// The logo glyph and the product wordmark. The glyph is the favicon
// (public/favicon.svg), the logo's one source on the page.
export function Wordmark({ size = 'sm' }: WordmarkProps) {
  const glyph = size === 'lg' ? 40 : 28;
  return (
    <div className="flex items-center gap-3">
      <img src={`${import.meta.env.BASE_URL}favicon.svg`} width={glyph} height={glyph} alt="" />
      <span
        className={size === 'lg' ? 'text-xl font-medium text-hi' : 'text-base font-medium text-hi'}
      >
        AudioSilo <span className="text-pink-500">Bridge</span>
      </span>
    </div>
  );
}
