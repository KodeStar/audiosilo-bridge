import { TABS, type TabId } from '@/lib/tabs';

interface TabBarProps {
  active: TabId;
  onSelect: (id: TabId) => void;
}

export function TabBar({ active, onSelect }: TabBarProps) {
  return (
    <nav
      className="flex gap-1 overflow-x-auto border-b border-edge bg-surface px-4"
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
              'border-b-2 px-4 py-3 text-sm whitespace-nowrap transition-colors ' +
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
