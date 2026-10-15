import { Notice } from './Notice';

// Loading stands in for data not fetched yet: the fetch's error, or "Loading...".
export function Loading({ error }: { error: string | null }) {
  return error ? (
    <Notice kind="error">{error}</Notice>
  ) : (
    <p className="text-sm text-dim">Loading...</p>
  );
}
