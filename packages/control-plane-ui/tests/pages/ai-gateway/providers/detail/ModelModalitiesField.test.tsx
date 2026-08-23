import { describe, expect, it, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { ModelModalitiesField } from '@/pages/ai-gateway/providers/detail/ModelModalitiesField';

// The gap this field closes: an administrator could state that a model "has
// vision" and could not state that it accepts audio at all, while the arrays
// were already in the database and already served to every SDK caller.
describe('ModelModalitiesField', () => {
  it('summarises without opening an editor, because most models never need one', () => {
    render(<ModelModalitiesField input={['text', 'image']} output={['text']} onChange={vi.fn()} />);
    // The chips are the editor; a collapsed field must not render them.
    // Chips ARE the editor. Collapsed means none of them is rendered — the
    // modality values are their own labels, so this holds without translations.
    expect(screen.queryByRole('button', { name: 'image' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'audio' })).toBeNull();
  });

  it('lets an admin declare audio input — the fact that had nowhere to live', () => {
    const onChange = vi.fn();
    render(<ModelModalitiesField input={['text']} output={['text']} onChange={onChange} />);
    fireEvent.click(screen.getByRole('button', { name: /modalitiesEdit/ }));
    // Both directions render an 'audio' chip; [0] is the Accepts row.
    fireEvent.click(screen.getAllByRole('button', { name: 'audio' })[0]);
    expect(onChange).toHaveBeenCalledWith({
      input: ['text', 'audio'],
      output: ['text'],
      required: [],
    });
  });

  it('removes a modality without disturbing the other direction', () => {
    const onChange = vi.fn();
    render(<ModelModalitiesField input={['text', 'image']} output={['text']} onChange={onChange} />);
    fireEvent.click(screen.getByRole('button', { name: /modalitiesEdit/ }));
    fireEvent.click(screen.getAllByRole('button', { name: 'image' })[0]);
    expect(onChange).toHaveBeenCalledWith({
      input: ['text'],
      output: ['text'],
      required: [],
    });
  });

  it('renders a row saved before these columns existed instead of crashing', () => {
    // Absent is a real state, and it reached this component from a form whose
    // defaults predate the field — which is exactly how it was found.
    render(<ModelModalitiesField onChange={vi.fn()} />);
    expect(document.body.textContent).toBeTruthy();
  });

  it('offers no editor to an admin who cannot update the model', () => {
    render(<ModelModalitiesField input={['text']} output={['text']} onChange={vi.fn()} disabled />);
    expect(screen.queryByRole('button', { name: /modalitiesEdit/ })).toBeNull();
  });
});
