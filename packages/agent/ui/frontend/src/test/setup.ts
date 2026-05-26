// Vitest setup — runs once before any test file.
//
// 1. Pull in jest-dom matchers (toBeInTheDocument, toHaveTextContent, …).
// 2. Import the app's i18n bundle so useTranslation() resolves keys in
//    every test without each test bootstrapping i18next manually.
// 3. Run @testing-library/react cleanup between tests so rendered DOM
//    from a previous test doesn't bleed into the next one (Vitest with
//    globals=false does not auto-wire this).
import '@testing-library/jest-dom/vitest';
import '@/i18n';

import { afterEach } from 'vitest';
import { cleanup } from '@testing-library/react';

afterEach(() => {
  cleanup();
});
