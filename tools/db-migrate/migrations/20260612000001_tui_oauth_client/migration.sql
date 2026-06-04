-- tui OAuth client — the Nexus operator toolkit (CLI/TUI) signs in via the
-- RFC 8252 loopback Authorization-Code + PKCE flow: it binds 127.0.0.1 on a
-- random high port and redirects to http://127.0.0.1:<port>/callback. A
-- dedicated public client (rather than reusing cp-ui) keeps the CLI's loopback
-- redirect surface separate from the web console's fixed redirects.
--
-- The redirect pattern uses the ":*" port wildcard the auth server's
-- matchLoopback (RFC 8252 §7.3) honors, so every random-port login matches
-- without registering each port. Mirrors the existing agent-desktop client,
-- which uses the same loopback pattern. Scopes mirror cp-ui so the toolkit has
-- the same reach as the web console (actual admin authority is still the
-- signed-in user's IAM, not the OAuth scope).
--
-- Idempotent: ON CONFLICT DO NOTHING so re-applying (or overlap with the
-- baseline seed, which carries the same row) is a no-op.
INSERT INTO public."OAuthClient"
  (id, name, type, "redirectUris", "allowedScopes", "requirePkce",
   "accessTtlSeconds", "refreshTtlSeconds", "clientSecretHash", "createdAt", "updatedAt")
VALUES
  ('tui', 'Nexus Operator Toolkit (CLI/TUI)', 'public',
   '{http://127.0.0.1:*/callback}', '{admin,openid,profile,email}', true,
   3600, 86400, NULL, NOW(), NOW())
ON CONFLICT (id) DO NOTHING;
