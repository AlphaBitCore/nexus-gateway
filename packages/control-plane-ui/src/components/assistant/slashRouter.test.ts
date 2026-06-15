import { it, expect, vi } from 'vitest';
import { routeSlashCommand } from './slashRouter';

it('routes /help and /clear locally and leaves everything else to the assistant', () => {
  const notice = vi.fn();
  const newChat = vi.fn();

  expect(routeSlashCommand('/help', { notice, newChat })).toBe(true);
  expect(notice).toHaveBeenCalledWith('common:assistant.slash.help');

  expect(routeSlashCommand('/clear', { notice, newChat })).toBe(true);
  expect(routeSlashCommand('/new', { notice, newChat })).toBe(true);
  expect(newChat).toHaveBeenCalledTimes(2);

  // An unknown /-prefixed message is NOT intercepted — an operator's
  // "/v1/messages returns 404" must reach the assistant.
  expect(routeSlashCommand('/v1/messages returns 404', { notice, newChat })).toBe(false);
  // Plain text is never intercepted.
  expect(routeSlashCommand('hello', { notice, newChat })).toBe(false);
});
