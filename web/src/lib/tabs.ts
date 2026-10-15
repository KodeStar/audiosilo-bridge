// The page's tabs; the active one is kept in the URL hash (#jobs).
export const TABS = [
  { id: 'status', label: 'Status' },
  { id: 'libraries', label: 'Libraries' },
  { id: 'jobs', label: 'Jobs' },
  { id: 'settings', label: 'Settings' },
  { id: 'workers', label: 'Workers' },
  { id: 'logs', label: 'Logs' },
] as const;

export type TabId = (typeof TABS)[number]['id'];

export const DEFAULT_TAB: TabId = 'status';

// Route is where the hash points: a tab, and on the Jobs tab one job (the
// server's console links to #/jobs/<server job id>).
export interface Route {
  tab: TabId;
  jobId: number | null;
}

// routeFromHash reads "#jobs", "#/logs" or "#/jobs/12"; unknown or empty is the
// default tab.
export function routeFromHash(hash: string): Route {
  const [head = '', id] = hash.replace(/^#\/?/, '').toLowerCase().split('/');
  const tab = TABS.find((t) => t.id === head)?.id ?? DEFAULT_TAB;
  const jobId =
    tab === 'jobs' && id !== undefined && /^[1-9]\d{0,17}$/.test(id) ? Number(id) : null;
  return { tab, jobId };
}

// tabFromHash reads a tab from a location hash ("#jobs"); unknown or empty is
// the default tab.
export function tabFromHash(hash: string): TabId {
  return routeFromHash(hash).tab;
}

// jobHash is the hash of one job's detail.
export function jobHash(id: number): string {
  return `#/jobs/${id}`;
}
