import type { Setup, SetupItem, SetupItemId } from '@/api/types';
import type { TabId } from './tabs';

// The setup checklist (the Bridge's internal/setup): what each item is called
// and the tab where it is done. The server's console links to the same tabs
// (agent and github -> accounts, asr and budget -> settings, libraries ->
// libraries); paired is done on the Status tab itself.
export const SETUP_ITEMS: Record<SetupItemId, { label: string; tab: TabId | null }> = {
  paired: { label: 'Pair with a server', tab: null },
  libraries: { label: 'Map the libraries', tab: 'libraries' },
  asr: { label: 'Speech recognition', tab: 'settings' },
  agent: { label: "Set the agent's credential", tab: 'accounts' },
  budget: { label: 'Review the monthly budget', tab: 'settings' },
  github: { label: 'Sign in to GitHub (optional, to share with the community)', tab: 'accounts' },
};

const TAB_NAMES: Record<TabId, string> = {
  status: 'Status',
  libraries: 'Libraries',
  jobs: 'Jobs',
  settings: 'Settings',
  accounts: 'Accounts',
  workers: 'Workers',
  logs: 'Logs',
};

function known(id: string): id is SetupItemId {
  return Object.prototype.hasOwnProperty.call(SETUP_ITEMS, id);
}

// setupItems are the items the page knows, in the Bridge's order (a later
// Bridge's new item is left out rather than shown without words).
export function setupItems(setup: Setup | null | undefined): (SetupItem & { id: SetupItemId })[] {
  if (!setup || !Array.isArray(setup.items)) return [];
  return setup.items.filter((it): it is SetupItem & { id: SetupItemId } => known(it.id));
}

// remainingRequired counts the required items not done yet.
export function remainingRequired(setup: Setup | null | undefined): number {
  return setupItems(setup).filter((it) => it.required && !it.done).length;
}

// setupLink is where an item is done: its tab's hash and name (null: here).
export function setupLink(id: SetupItemId): { hash: string; name: string } | null {
  const tab = SETUP_ITEMS[id].tab;
  return tab ? { hash: `#/${tab}`, name: TAB_NAMES[tab] } : null;
}
