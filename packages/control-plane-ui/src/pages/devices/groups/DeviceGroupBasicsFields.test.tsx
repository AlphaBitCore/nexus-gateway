import { describe, it, expect, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithProviders } from '@/test/test-utils';
import { DeviceGroupBasicsFields } from './DeviceGroupBasicsFields';

describe('DeviceGroupBasicsFields', () => {
  it('renders name and description fields', () => {
    renderWithProviders(
      <DeviceGroupBasicsFields
        name="Alpha"
        description="Test group"
        onNameChange={vi.fn()}
        onDescriptionChange={vi.fn()}
      />,
    );
    expect(screen.getByDisplayValue('Alpha')).toBeDefined();
    expect(screen.getByDisplayValue('Test group')).toBeDefined();
  });

  it('propagates name changes', async () => {
    const user = userEvent.setup();
    const onNameChange = vi.fn();
    renderWithProviders(
      <DeviceGroupBasicsFields
        name=""
        description=""
        onNameChange={onNameChange}
        onDescriptionChange={vi.fn()}
      />,
    );
    const nameInput = screen.getByRole('textbox', { name: /name/i });
    await user.type(nameInput, 'x');
    expect(onNameChange).toHaveBeenCalled();
  });
});
