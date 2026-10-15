import { useEffect, useState } from 'react';
import type { ApiClient } from '@/api/client';
import { tabFromHash, type TabId } from '@/lib/tabs';
import { Wordmark } from './Wordmark';
import { TabBar } from './TabBar';
import { secondaryButton } from './ui/buttons';
import { StatusTab } from './tabs/StatusTab';
import { LibrariesTab } from './tabs/LibrariesTab';
import { JobsTab } from './tabs/JobsTab';
import { SettingsTab } from './tabs/SettingsTab';
import { WorkersTab } from './tabs/WorkersTab';
import { LogsTab } from './tabs/LogsTab';

interface ShellProps {
  client: ApiClient;
  onSignedOut: () => void;
}

export function Shell({ client, onSignedOut }: ShellProps) {
  const [active, setActive] = useState<TabId>(() => tabFromHash(window.location.hash));
  const [signingOut, setSigningOut] = useState(false);

  useEffect(() => {
    const onHash = () => setActive(tabFromHash(window.location.hash));
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  function select(id: TabId) {
    setActive(id);
    if (window.location.hash !== `#${id}`) window.location.hash = id;
  }

  async function signOut() {
    setSigningOut(true);
    try {
      await client.logout();
    } catch {
      // Signed out here whatever the Bridge answered.
    }
    onSignedOut();
  }

  return (
    <div className="flex min-h-screen flex-col">
      <header className="flex items-center justify-between border-b border-edge bg-surface px-6 py-3">
        <Wordmark />
        <button type="button" onClick={signOut} disabled={signingOut} className={secondaryButton}>
          {signingOut ? 'Signing out...' : 'Sign out'}
        </button>
      </header>
      <TabBar active={active} onSelect={select} />
      <main
        id="panel"
        role="tabpanel"
        aria-labelledby={`tab-${active}`}
        className="mx-auto w-full max-w-4xl flex-1 p-4 sm:p-6"
      >
        {active === 'status' && <StatusTab client={client} />}
        {active === 'libraries' && <LibrariesTab client={client} />}
        {active === 'jobs' && <JobsTab client={client} />}
        {active === 'settings' && (
          <SettingsTab client={client} onSignOut={signOut} signingOut={signingOut} />
        )}
        {active === 'workers' && <WorkersTab client={client} />}
        {active === 'logs' && <LogsTab client={client} />}
      </main>
    </div>
  );
}
