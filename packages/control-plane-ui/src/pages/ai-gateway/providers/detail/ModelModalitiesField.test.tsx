/**
 * Unit tests for ModelModalitiesField — the required-modality floor.
 *
 * The floor is the protective half of this control: it is what stops a request
 * being routed to a model that cannot serve it, and until it had an editing
 * surface an admin could set the ceiling and not the floor. These tests cover
 * the three ways that surface can be wrong:
 *
 *  (a) the floor is offered only over what the model accepts — requiring
 *      something outside the ceiling is a row that can serve no request at all
 *  (b) narrowing the ceiling drops the floor entries it orphans
 *  (c) the summary states a floor only when there is one
 */

import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { ModelModalitiesField } from './ModelModalitiesField';

vi.mock('react-i18next', () => ({
  useTranslation: () => ({
    t: (k: string, vars?: Record<string, string>) =>
      vars ? `${k}(${Object.values(vars).join('|')})` : k,
  }),
}));

function open(props: Partial<React.ComponentProps<typeof ModelModalitiesField>> = {}) {
  const onChange = vi.fn();
  render(
    <ModelModalitiesField
      input={props.input ?? ['text', 'image']}
      output={props.output ?? ['text']}
      required={props.required}
      onChange={props.onChange ?? onChange}
    />,
  );
  fireEvent.click(screen.getByText('pages:providers.capabilities.modalitiesEdit'));
  return onChange;
}

/** Chips are rendered per row; the floor row is the third. */
function chipsAfter(label: string): HTMLElement[] {
  const heading = screen.getByText(label);
  const row = heading.nextElementSibling as HTMLElement;
  return Array.from(row.querySelectorAll('button'));
}

describe('ModelModalitiesField — the required floor', () => {
  it('offers as a floor only what the model accepts', () => {
    open({ input: ['text', 'image'] });
    const offered = chipsAfter('pages:providers.capabilities.modalitiesRequired').map(
      (b) => b.textContent,
    );
    expect(offered).toEqual(['text', 'image']);
    // audio is a legal modality but this model does not accept it, so requiring
    // it would produce a row that refuses every request.
    expect(offered).not.toContain('audio');
  });

  it('says what to do first when nothing is accepted yet', () => {
    open({ input: [] });
    expect(chipsAfter('pages:providers.capabilities.modalitiesRequired')).toHaveLength(0);
    expect(
      screen.getByText('pages:providers.capabilities.modalitiesRequiredNeedsAccepts'),
    ).toBeTruthy();
  });

  it('reports a floor selection without disturbing the ceiling', () => {
    const onChange = vi.fn();
    open({ input: ['text', 'image'], output: ['text'], required: [], onChange });
    fireEvent.click(
      chipsAfter('pages:providers.capabilities.modalitiesRequired').find(
        (b) => b.textContent === 'image',
      )!,
    );
    expect(onChange).toHaveBeenCalledWith({
      input: ['text', 'image'],
      output: ['text'],
      required: ['image'],
    });
  });

  it('drops a floor entry the ceiling no longer accepts', () => {
    const onChange = vi.fn();
    open({ input: ['text', 'image'], output: ['text'], required: ['image'], onChange });
    // Un-accepting image must not leave the model requiring it.
    fireEvent.click(
      chipsAfter('pages:providers.capabilities.modalitiesInput').find(
        (b) => b.textContent === 'image',
      )!,
    );
    expect(onChange).toHaveBeenCalledWith({
      input: ['text'],
      output: ['text'],
      required: [],
    });
  });

  it('states the floor in the summary only when there is one', () => {
    const { unmount } = render(
      <ModelModalitiesField input={['text']} output={['text']} required={[]} onChange={vi.fn()} />,
    );
    expect(screen.queryByText(/modalitiesRequiredSummary/)).toBeNull();
    unmount();

    render(
      <ModelModalitiesField
        input={['text', 'audio']}
        output={['text']}
        required={['audio']}
        onChange={vi.fn()}
      />,
    );
    expect(screen.getByText(/modalitiesRequiredSummary\(audio\)/)).toBeTruthy();
  });
});
