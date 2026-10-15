import type { FileCheck, VerifyResult } from '@/api/types';

export const VERIFY_KIND = 'bridge.verify';

function num(v: unknown): number {
  return typeof v === 'number' && Number.isFinite(v) ? v : 0;
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

// parseVerifyResult reads a bridge.verify result leniently: a missing or
// wrongly typed field takes its zero value, a malformed file row is skipped,
// and anything that isn't an object with a files list is null (the page then
// shows the raw JSON instead).
export function parseVerifyResult(raw: unknown): VerifyResult | null {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return null;
  const r = raw as Record<string, unknown>;
  if (!Array.isArray(r.files)) return null;
  const files: FileCheck[] = [];
  for (const f of r.files) {
    if (typeof f !== 'object' || f === null) continue;
    const row = f as Record<string, unknown>;
    if (typeof row.path !== 'string') continue;
    const check: FileCheck = {
      path: row.path,
      size: num(row.size),
      expected_size: num(row.expected_size),
      ok: row.ok === true,
    };
    const err = str(row.error);
    if (err) check.error = err;
    files.push(check);
  }
  const result: VerifyResult = {
    library_id: num(r.library_id),
    mount_path: str(r.mount_path),
    path: str(r.path),
    files,
    writable: r.writable === true,
    elapsed_ms: num(r.elapsed_ms),
  };
  const werr = str(r.writable_error);
  if (werr) result.writable_error = werr;
  return result;
}

// prettyJSON shows any value as indented JSON.
export function prettyJSON(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2) ?? '';
  } catch {
    return String(v);
  }
}
