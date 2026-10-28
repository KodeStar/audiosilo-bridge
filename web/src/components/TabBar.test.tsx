import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { TabBar } from './TabBar';
import { TABS } from '@/lib/tabs';

describe('TabBar', () => {
  it('shows every tab, wrapping rather than scrolling at phone width', async () => {
    const onSelect = vi.fn();
    render(<TabBar active="status" onSelect={onSelect} />);
    const bar = screen.getByRole('tablist', { name: 'Sections' });
    expect(bar.className).toContain('flex-wrap');
    expect(bar.className).not.toContain('overflow-x');
    expect(screen.getAllByRole('tab').map((t) => t.textContent)).toEqual(TABS.map((t) => t.label));
    expect(screen.getByRole('tab', { name: 'Status' })).toHaveAttribute('aria-selected', 'true');
    await userEvent.click(screen.getByRole('tab', { name: 'Logs' }));
    expect(onSelect).toHaveBeenCalledWith('logs');
  });
});
