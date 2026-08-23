import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, act } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { ToastProvider, useToast } from '../../src/context/ToastContext';

function Trigger() {
  const { addToast } = useToast();
  return (
    <div>
      <button onClick={() => addToast('saved ok', 'success')}>ok</button>
      <button onClick={() => addToast('it broke', 'error')}>err</button>
    </div>
  );
}

describe('useToast', () => {
  it('throws when used outside a ToastProvider', () => {
    function Bare() {
      useToast();
      return null;
    }
    // Silence React's error-boundary console noise for the expected throw.
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    expect(() => render(<Bare />)).toThrow('useToast must be used within ToastProvider');
    spy.mockRestore();
  });
});

describe('ToastProvider', () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  // addToast arms a safety-fallback timer. If it outlives the provider, it fires
  // into a torn-down tree: React reaches for `window` during the state update and
  // throws ReferenceError. Under vitest that surfaces as an UNHANDLED ERROR
  // attributed to whichever test file happened to be running — every test passes
  // and the run still fails, which is how this reached CI while 2057 local tests
  // stayed green (CI is ~9x slower, so its timers outlive the environment).
  //
  // The assertion is on pending timers rather than on the crash, because the
  // crash needs the jsdom environment to be gone — something a test inside that
  // environment cannot stage.
  it('leaves no timer running after the provider unmounts', async () => {
    vi.useFakeTimers();
    const { unmount } = render(
      <ToastProvider>
        <Trigger />
      </ToastProvider>,
    );
    act(() => {
      screen.getByText('ok').click();
      screen.getByText('err').click();
    });
    expect(vi.getTimerCount()).toBeGreaterThan(0);

    unmount();

    expect(vi.getTimerCount()).toBe(0);
  });

  it('renders a toast with its message when addToast is called', async () => {
    const user = userEvent.setup();
    render(
      <ToastProvider>
        <Trigger />
      </ToastProvider>,
    );
    await user.click(screen.getByText('ok'));
    expect(screen.getByText('saved ok')).toBeInTheDocument();
  });

  it('stacks multiple toasts', async () => {
    const user = userEvent.setup();
    render(
      <ToastProvider>
        <Trigger />
      </ToastProvider>,
    );
    await user.click(screen.getByText('ok'));
    await user.click(screen.getByText('err'));
    expect(screen.getByText('saved ok')).toBeInTheDocument();
    expect(screen.getByText('it broke')).toBeInTheDocument();
  });

  it('auto-dismisses a success toast after its safety-fallback timeout', () => {
    vi.useFakeTimers();
    render(
      <ToastProvider>
        <Trigger />
      </ToastProvider>,
    );
    // Fire the click via the fake-timer-friendly path.
    act(() => {
      screen.getByText('ok').click();
    });
    expect(screen.getByText('saved ok')).toBeInTheDocument();
    // success = 3000ms + 500ms safety fallback.
    act(() => {
      vi.advanceTimersByTime(3500);
    });
    expect(screen.queryByText('saved ok')).not.toBeInTheDocument();
  });

  it('keeps an error toast longer than a success toast (5000 vs 3000 + fallback)', () => {
    vi.useFakeTimers();
    render(
      <ToastProvider>
        <Trigger />
      </ToastProvider>,
    );
    act(() => {
      screen.getByText('err').click();
    });
    expect(screen.getByText('it broke')).toBeInTheDocument();
    // At 3.5s a success toast would be gone; the error toast (5500ms) survives.
    act(() => {
      vi.advanceTimersByTime(3500);
    });
    expect(screen.getByText('it broke')).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(2000);
    });
    expect(screen.queryByText('it broke')).not.toBeInTheDocument();
  });
});
