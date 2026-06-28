import { describe, it, expect, vi } from 'vitest';
import { screen, fireEvent } from '@testing-library/react';
import { renderWithProviders } from '@/test/test-utils';
import { JsonSchemaHookConfigForm, buildDefaultsFromSchema } from '@/components/config/JsonSchemaHookConfigForm';

describe('buildDefaultsFromSchema', () => {
  it('returns {} when there are no properties', () => {
    expect(buildDefaultsFromSchema({})).toEqual({});
    expect(buildDefaultsFromSchema({ properties: 'nope' })).toEqual({});
  });

  it('derives a default per property type (default > enum > type)', () => {
    const out = buildDefaultsFromSchema({
      properties: {
        explicit: { type: 'string', default: 'X' },
        choice: { type: 'string', enum: ['a', 'b'] },
        flag: { type: 'boolean' },
        items: { type: 'array' },
        obj: { type: 'object' },
        text: { type: 'string' },
        num: { type: 'number' },
      },
    });
    expect(out).toEqual({ explicit: 'X', choice: 'a', flag: false, items: [], obj: {}, text: '' });
    expect('num' in out).toBe(false);
  });
});

describe('JsonSchemaHookConfigForm', () => {
  it('falls back to a raw JSON textarea when the schema has no properties', () => {
    const onChange = vi.fn();
    renderWithProviders(<JsonSchemaHookConfigForm schema={{}} value={{ a: 1 }} onChange={onChange} />);
    const ta = screen.getByRole('textbox') as HTMLTextAreaElement;
    expect(ta.value).toContain('"a": 1');
    fireEvent.change(ta, { target: { value: '{"b": 2}' } });
    expect(onChange).toHaveBeenCalledWith({ b: 2 });
  });

  it('ignores invalid JSON in the fallback textarea', () => {
    const onChange = vi.fn();
    renderWithProviders(<JsonSchemaHookConfigForm schema={{}} value={{}} onChange={onChange} />);
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '{ not json' } });
    expect(onChange).not.toHaveBeenCalled();
  });

  it('renders schema-driven fields for an onMatch property', () => {
    const onChange = vi.fn();
    const schema = { properties: { onMatch: { type: 'object', description: 'what to do' } }, required: ['onMatch'] };
    renderWithProviders(
      <JsonSchemaHookConfigForm schema={schema} value={{ onMatch: { inflightAction: 'redact' } }} onChange={onChange} />,
    );
    expect(screen.getAllByRole('combobox').length).toBeGreaterThan(0);
  });

  it('boolean property → a Switch that patches true on toggle', () => {
    const onChange = vi.fn();
    renderWithProviders(<JsonSchemaHookConfigForm schema={{ properties: { flag: { type: 'boolean' } } }} value={{}} onChange={onChange} />);
    fireEvent.click(screen.getByRole('switch'));
    expect(onChange).toHaveBeenCalledWith({ flag: true });
  });

  it('number property → numeric coercion', () => {
    const onChange = vi.fn();
    renderWithProviders(<JsonSchemaHookConfigForm schema={{ properties: { n: { type: 'number' } } }} value={{}} onChange={onChange} />);
    fireEvent.change(screen.getByRole('spinbutton'), { target: { value: '5' } });
    expect(onChange).toHaveBeenCalledWith({ n: 5 });
  });

  it('number property → undefined when cleared (controlled value populated)', () => {
    const onChange = vi.fn();
    renderWithProviders(<JsonSchemaHookConfigForm schema={{ properties: { n: { type: 'number' } } }} value={{ n: 5 }} onChange={onChange} />);
    fireEvent.change(screen.getByRole('spinbutton'), { target: { value: '' } });
    expect(onChange).toHaveBeenCalledWith({ n: undefined });
  });

  it('string-array property → one-value-per-line textarea', () => {
    const onChange = vi.fn();
    renderWithProviders(<JsonSchemaHookConfigForm schema={{ properties: { tags: { type: 'array', items: { type: 'string' } } } }} value={{}} onChange={onChange} />);
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'a\n b \n\nc' } });
    expect(onChange).toHaveBeenCalledWith({ tags: ['a', 'b', 'c'] });
  });

  it('generic-array property → JSON textarea parsing', () => {
    const onChange = vi.fn();
    renderWithProviders(<JsonSchemaHookConfigForm schema={{ properties: { data: { type: 'array' } } }} value={{}} onChange={onChange} />);
    fireEvent.change(screen.getByRole('textbox'), { target: { value: '[1, 2]' } });
    expect(onChange).toHaveBeenCalledWith({ data: [1, 2] });
  });

  it('onMatch with a redact action reveals + patches the replacement field', () => {
    const onChange = vi.fn();
    const schema = { properties: { onMatch: { type: 'object' } } };
    renderWithProviders(<JsonSchemaHookConfigForm schema={schema} value={{ onMatch: { inflightAction: 'redact' } }} onChange={onChange} />);
    const repl = document.querySelector('input[name="onMatch-replacement"]') as HTMLInputElement;
    expect(repl).toBeTruthy();
    fireEvent.change(repl, { target: { value: '[REDACTED]' } });
    // The editor canonicalizes onMatch to the single-action shape, dropping the
    // legacy inflightAction/storageAction pair (mirrors backend ActionFromLegacy).
    expect(onChange).toHaveBeenCalledWith({ onMatch: { action: 'redact', replacement: '[REDACTED]' } });
  });
});
