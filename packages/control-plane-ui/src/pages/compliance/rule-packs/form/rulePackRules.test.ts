import { describe, it, expect } from 'vitest';

import { isValidPackName, isValidPackVersion } from './rulePackRules';

// These mirror the backend contract in packages/shared/policy/rulepack/yaml.go
// (packNameRE / semverRE). If the backend regex changes, both must move
// together — the whole point is that the form rejects exactly what the API
// would reject, so an operator never sees the raw 400 detail.
describe('isValidPackName', () => {
  it('accepts <namespace>/<short-name> with lowercase, digits and hyphens', () => {
    expect(isValidPackName('acme/pii-rules')).toBe(true);
    expect(isValidPackName('acme/rules')).toBe(true);
    expect(isValidPackName('a1/b2')).toBe(true);
    expect(isValidPackName('team-security/pci-dss-3')).toBe(true);
  });

  it('rejects a bare name with no namespace slash (the "test" case)', () => {
    expect(isValidPackName('test')).toBe(false);
  });

  it('rejects uppercase, spaces, leading digits/hyphens, and empty segments', () => {
    expect(isValidPackName('Acme/rules')).toBe(false);
    expect(isValidPackName('acme/Rules')).toBe(false);
    expect(isValidPackName('acme rules/x')).toBe(false);
    expect(isValidPackName('1acme/rules')).toBe(false);
    expect(isValidPackName('-acme/rules')).toBe(false);
    expect(isValidPackName('acme/')).toBe(false);
    expect(isValidPackName('/rules')).toBe(false);
    expect(isValidPackName('acme/rules/extra')).toBe(false);
    expect(isValidPackName('')).toBe(false);
  });
});

describe('isValidPackVersion', () => {
  it('accepts v-prefixed semver with optional pre-release/build', () => {
    expect(isValidPackVersion('v1.0.0')).toBe(true);
    expect(isValidPackVersion('v10.20.30')).toBe(true);
    expect(isValidPackVersion('v1.2.3-rc1')).toBe(true);
    expect(isValidPackVersion('v1.2.3+build.5')).toBe(true);
  });

  it('rejects a missing v prefix, partial versions, and non-numeric parts', () => {
    expect(isValidPackVersion('1.0.0')).toBe(false);
    expect(isValidPackVersion('v1.0')).toBe(false);
    expect(isValidPackVersion('v1')).toBe(false);
    expect(isValidPackVersion('vx.y.z')).toBe(false);
    expect(isValidPackVersion('')).toBe(false);
  });
});
