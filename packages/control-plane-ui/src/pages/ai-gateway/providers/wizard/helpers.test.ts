import { describe, it, expect } from 'vitest';
import { initials } from './helpers';

describe('initials', () => {
  it('returns two-letter initials from two-word name', () => {
    expect(initials('Open AI')).toBe('OA');
  });

  it('returns first two chars for single word', () => {
    expect(initials('Anthropic')).toBe('AN');
  });

  it('returns "AI" for undefined', () => {
    expect(initials(undefined)).toBe('AI');
  });

  it('returns "AI" for null', () => {
    expect(initials(null)).toBe('AI');
  });

  it('returns "AI" for empty string', () => {
    expect(initials('')).toBe('AI');
  });

  it('handles special characters', () => {
    expect(initials('---')).toBe('AI');
  });
});
