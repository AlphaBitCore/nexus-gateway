import { createContext, useContext, useState, useCallback } from 'react';
import { useTimeouts } from '@/hooks/useTimeouts';
import { ToastContainer } from '../components/ui/ToastContainer';
import type { ToastItem } from '../components/ui/ToastContainer';

interface ToastContextValue {
  addToast: (message: string, type?: 'success' | 'error' | 'warning' | 'info') => void;
}

const ToastContext = createContext<ToastContextValue | null>(null);

let toastIdCounter = 0;

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<ToastItem[]>([]);
  // The safety-fallback timers below must not outlive the provider: one that
  // fires into an unmounted tree updates state on it, and with the surrounding
  // environment already gone React reaches for `window` and throws.
  const armTimeout = useTimeouts();

  const dismiss = useCallback((id: number) => {
    setToasts((prev) => prev.filter((t) => t.id !== id));
  }, []);

  const addToast = useCallback((message: string, type: 'success' | 'error' | 'warning' | 'info' = 'info') => {
    const id = ++toastIdCounter;
    setToasts((prev) => [...prev, { id, message, type }]);
    const dismissMs = type === 'success' ? 3000 : type === 'info' ? 4000 : 5000;
    // Safety fallback after ToastContainer auto-dismiss.
    armTimeout(() => dismiss(id), dismissMs + 500);
  }, [dismiss, armTimeout]);

  return (
    <ToastContext.Provider value={{ addToast }}>
      {children}
      <ToastContainer toasts={toasts} onDismiss={dismiss} />
    </ToastContext.Provider>
  );
}

export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext);
  if (!ctx) throw new Error('useToast must be used within ToastProvider');
  return ctx;
}
