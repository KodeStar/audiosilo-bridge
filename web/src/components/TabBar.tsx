import { TABS, type TabId } from '@/lib/tabs';

interface TabBarProps {
  active: TabId;
  onSelect: (id: TabId) => void;
}

// The tabs wrap onto a second row at phone width rather than scroll sideways,
// where Workers and Logs sat off-screen with nothing to say the row scrolled.
export function TabBar({ active, onSelect }: TabBarProps) {
  return (
    <nav
      className="flex flex-wrap gap-x-1 border-b border-edge bg-surface px-2 sm:px-4"
      role="tablist"
      aria-label="Sections"
    >
      {TABS.map((tab) => {
        const selected = tab.id === active;
        return (
          <button
            key={tab.id}
            type="button"
            role="tab"
            id={`tab-${tab.id}`}
            aria-selected={selected}
            aria-controls="panel"
            onClick={() => onSelect(tab.id)}
            className={
              'border-b-2 px-3 py-3 text-sm sm:px-4 whitespace-nowrap transition-colors ' +
              (selected ? 'border-pink-600 text-hi' : 'border-transparent text-dim hover:text-body')
            }
          >
            {tab.label}
          </button>
        );
      })}
    </nav>
  );
}
