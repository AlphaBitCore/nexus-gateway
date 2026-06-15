// slashRouter.ts — the web chat's local "/" command grammar (the TUI parity
// surface, trimmed to what web genuinely supports). Pure routing over injected
// effects so the chat shell stays a thin composition root.

export interface SlashDeps {
  /** Post a local assistant-style notice (an i18n key + interpolation vars). */
  notice: (key: string, vars?: Record<string, string>) => void;
  /** Reset the conversation (/clear, /new). */
  newChat: () => void;
}

// routeSlashCommand intercepts a "/" input as a LOCAL command. Returns true
// when the text was a command — nothing is sent to the assistant.
export function routeSlashCommand(text: string, deps: SlashDeps): boolean {
  if (!text.startsWith('/')) return false;
  const [cmd] = text.slice(1).split(/\s+/);
  switch (cmd.toLowerCase()) {
    case 'help':
      deps.notice('common:assistant.slash.help');
      return true;
    case 'clear':
    case 'new':
      deps.newChat();
      return true;
    default:
      // NOT a known command -> not intercepted: the message goes to the
      // assistant. An operator's "/v1/messages returns 404" must never be
      // eaten by the router (the TUI shows a live palette while typing, so
      // interception there is visible; the web has no such warning).
      return false;
  }
}
