// Vitest setup — runs once before any test file.
//
// 1. Register jest-dom matchers (toBeInTheDocument, toHaveTextContent, …).
//    Vitest 4 scopes the setup-file expect instance per file, so the
//    side-effect `import '@testing-library/jest-dom/vitest'` no longer
//    reliably registers matchers — explicit expect.extend works under v4.
// 2. Import the app's i18n bundle so useTranslation() resolves keys in
//    every test without each test bootstrapping i18next manually.
// 3. Run @testing-library/react cleanup between tests so rendered DOM
//    from a previous test doesn't bleed into the next one (Vitest with
//    globals=false does not auto-wire this).
import { afterEach, expect } from 'vitest';
import * as jestDomMatchers from '@testing-library/jest-dom/matchers';

expect.extend(jestDomMatchers);

import '@/i18n';
import { cleanup } from '@testing-library/react';

afterEach(() => {
  cleanup();
});
