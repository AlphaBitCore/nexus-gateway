/**
 * Client-side JSON Schema validation (draft-07) aligned with gateway hook `configSchema` checks.
 */

import Ajv from 'ajv';
import addFormatsImport from 'ajv-formats';

// ajv-formats ships as CommonJS with `module.exports = formatsPlugin` AND
// `exports.default = formatsPlugin`. Vite's ESM interop sometimes hands us
// the namespace object (with the `default` wrapper) instead of the function
// itself, which then crashes with "Cannot read properties of undefined
// (reading 'code')" the first time it touches `ajv.opts.code`. Unwrap
// defensively so both shapes work.
const addFormats =
  (addFormatsImport as unknown as { default?: typeof addFormatsImport }).default
  ?? addFormatsImport;

const ajv = new Ajv({ allErrors: true, strict: false, allowUnionTypes: true });
addFormats(ajv);

export function validateDataAgainstJsonSchema(schema: Record<string, unknown>, data: unknown): string | null {
  const validate = ajv.compile(schema);
  if (validate(data)) return null;
  return ajv.errorsText(validate.errors, { separator: '; ' });
}
