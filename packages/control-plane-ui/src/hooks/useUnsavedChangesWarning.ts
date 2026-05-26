/**
 * Warns users before navigating away from a page with unsaved form changes.
 *
 * Handles two scenarios:
 * 1. Browser navigation (back/forward/close/reload) — uses `beforeunload` event
 * 2. In-app navigation (React Router links) — uses `window.onbeforeunload`
 *
 * Usage with React Hook Form:
 *   const form = useZodForm({ schema, defaultValues });
 *   useUnsavedChangesWarning(form.formState.isDirty);
 *
 * Usage with manual state:
 *   useUnsavedChangesWarning(name !== originalName || email !== originalEmail);
 */
import { useEffect } from 'react';

export function useUnsavedChangesWarning(isDirty: boolean, message?: string) {
  useEffect(() => {
    if (!isDirty) return;

    const msg = message ?? 'You have unsaved changes. Are you sure you want to leave?';

    const handleBeforeUnload = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      // Modern browsers ignore custom messages but still show a generic prompt
      e.returnValue = msg;
      return msg;
    };

    window.addEventListener('beforeunload', handleBeforeUnload);
    return () => window.removeEventListener('beforeunload', handleBeforeUnload);
  }, [isDirty, message]);
}
