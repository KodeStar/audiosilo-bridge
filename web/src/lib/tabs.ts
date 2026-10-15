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

// tabFromHash reads a tab from a location hash ("#jobs"); unknown or empty is
// the default tab.
export function tabFromHash(hash: string): TabId {
  const id = hash.replace(/^#\/?/, '').toLowerCase();
  return TABS.find((t) => t.id === id)?.id ?? DEFAULT_TAB;
}
