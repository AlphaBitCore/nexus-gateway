# Publishing the site — why the Pages deploy failed, and the options

## What happened
`pages` workflow failed at `configure-pages`:
> Your current plan does not support GitHub Pages for this repository.

`Kaushik985/nexus-benchmark-site` is **private** on a **free plan**. GitHub
Pages is not available for private repos on free, so an auto-deploy on push
fails every time. The push trigger is therefore disabled (manual dispatch only)
until a publish path is chosen. Not a code bug — a plan/visibility limit.

## Important: this repo holds INTERNAL material
Beyond the site (`index.html`, `data.json`, `history/`) the repo contains
`obs-backend/`, `rig/`, `HANDOFF.md`, `INTEGRATION.md` — internal planning +
methodology-candid docs. So making **this** repo public would expose all of
that in the source tree, regardless of the site itself. (The Pages workflow was
also hardened to publish only the site files via a staged `_site/`, but that
doesn't protect the repo *source* if the repo goes public.)

## Options
1. **Split — recommended.** Create a **public** site-only repo (e.g.
   `AlphaBitCore/nexus-benchmark-site` or `Kaushik985/nexus-benchmark`) holding
   ONLY `index.html`, `data.json`, `history/`, `CNAME`, `pages.yml`, a minimal
   README. Public repo → free Pages works. Internal repo stays private with the
   backend + planning. This is the right separation (public display layer vs
   internal) and unblocks the custom domain.
2. **Upgrade the plan.** GitHub Pro/Team enables Pages on private repos; keep
   everything in one repo. Costs money; still serves the site publicly.
3. **Make this repo public.** Cheapest for Pages, but **leaks the internal
   docs/code** above. Not recommended.

## Gate (unchanged)
Publishing the benchmark numbers is competitive/public and the data is marked
internal-draft (indicative, n=2, rig operated by us). James's methodology
sign-off should land before the site is truly public, whichever option is used.

## Local preview (no publishing)
`python3 -m http.server 8080` → http://localhost:8080 — identical to what Pages
would serve.
