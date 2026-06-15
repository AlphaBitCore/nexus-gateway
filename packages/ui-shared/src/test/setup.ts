// Global Vitest setup for @nexus-gateway/ui-shared. Registers jest-dom's
// matchers so any `*.test.tsx` in this package can use
// `toBeInTheDocument`, `toBeDisabled`, etc. without restating the
// import. Vitest 4 scopes the setup-file expect instance per file, so the
// side-effect `import '@testing-library/jest-dom/vitest'` no longer
// reliably registers matchers — explicit expect.extend works under v4.
import { expect } from 'vitest';
import * as jestDomMatchers from '@testing-library/jest-dom/matchers';

expect.extend(jestDomMatchers);
