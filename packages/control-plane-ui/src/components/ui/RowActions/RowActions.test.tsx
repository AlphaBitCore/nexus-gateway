import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import '@testing-library/jest-dom/vitest';

import {
  DeleteActionIcon,
  OpenActionIcon,
  RowActionIconButton,
  RowActions,
} from './RowActions';

describe('RowActions', () => {
  it('shows a tooltip for icon actions', async () => {
    const user = userEvent.setup();

    render(
      <RowActionIconButton label="Delete provider" onAction={vi.fn()}>
        <DeleteActionIcon />
      </RowActionIconButton>,
    );

    const button = screen.getByRole('button', { name: 'Delete provider' });
    expect(button).toHaveAttribute('title', 'Delete provider');

    await user.hover(button);

    expect(await screen.findByRole('tooltip')).toHaveTextContent('Delete provider');
  });

  it('keeps tooltips available for disabled icon actions', async () => {
    const user = userEvent.setup();

    render(
      <RowActionIconButton label="Delete provider" disabled onAction={vi.fn()}>
        <DeleteActionIcon />
      </RowActionIconButton>,
    );

    const button = screen.getByRole('button', { name: 'Delete provider' });
    const trigger = button.parentElement;
    expect(button).toBeDisabled();
    expect(trigger).toBeInstanceOf(HTMLElement);

    await user.hover(trigger as HTMLElement);

    expect(await screen.findByRole('tooltip')).toHaveTextContent('Delete provider');
  });

  it('keeps icon action clicks from opening the row', () => {
    const rowHandler = vi.fn();
    const actionHandler = vi.fn();

    render(
      <div onClick={rowHandler}>
        <RowActions>
          <RowActionIconButton label="Open detail" onAction={actionHandler}>
            <OpenActionIcon />
          </RowActionIconButton>
        </RowActions>
      </div>,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Open detail' }));

    expect(actionHandler).toHaveBeenCalledTimes(1);
    expect(rowHandler).not.toHaveBeenCalled();
  });
});
