# Wiki page template — every wiki page MUST follow this shape

> Authored by Opus 2026-05-21 as part of the IA v2 expansion. All subagents
> writing wiki pages read this file. Deviations require a `decisions.md`
> entry explaining why.

## Mandatory shape

```markdown
# <Page Title — matches filename with spaces>

<One paragraph what/why — 3-5 sentences. After reading this paragraph the
reader knows whether to keep reading. Tone: declarative, no hype, no marketing
language. Every claim backed by either repo code, repo docs, or production
state recorded in `docs/developers/roadmap.md`. No "we", no "our" — use
project-name or passive voice.>

---

## <Section 1 — most important topic for this page>

<Body. 100-200 lines per section is normal. Use sub-sections (`###`) sparingly.>

## <Section 2>

<...>

## <Section 3>

<...>

<2-5 sections total. More than 5 → split the page. Fewer than 2 → merge with
adjacent page.>

---

## Canonical docs

<For deeper reference, link 1-4 main-repo docs and 3-6 adjacent wiki pages.>

- [`<doc title>`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/<path>) — <one-line what it covers>
- [`<doc title>`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/<path>) — <one-line>
- ...

**Adjacent wiki pages**: [<Page Name>](<Page-Name>) · [<Page Name>](<Page-Name>) · ...
```

## Per-section rules

### H1 — page title
- Exactly one H1 per page; matches the filename (hyphens → spaces).
- No emoji in H1 (sidebar emojis live in `_Sidebar.md` only).
- No YAML frontmatter — GitHub Wiki doesn't parse it (E76-DEC-007).

### Opening paragraph
- 3-5 sentences. Tells reader what this page covers AND why they would read it.
- First sentence is declarative: "<Subject> is <what it is>. <Why it exists>."
- Last sentence (often): "See <one specific section> for <specific detail>."
- No "Welcome to…", no "This page describes…", no "In this guide…".

### Sections
- 2-5 H2 sections per page. More → split. Fewer → merge or expand.
- Section titles are nouns or noun phrases, not questions.
- Each section is 50-200 lines. Big sections use `###` sub-sections sparingly.

### Code, paths, commands
- Wrap inline code in backticks: `packages/ai-gateway/`, `go test ./...`.
- File path references use repo-relative paths in backticks.
- Multi-line code blocks use language hints: ` ```go `, ` ```bash `, ` ```yaml `.
- Mermaid blocks: ` ```mermaid ` (E76-DEC-008 — Mermaid only, no SVG/PNG).

### Links
- **Inter-wiki**: `[AI Gateway Prompt Cache](AI-Gateway-Prompt-Cache)` — page name with hyphens, no `.md`.
- **Repo files**: absolute GitHub URL `https://github.com/AlphaBitCore/nexus-gateway/blob/main/<path>` (E76-DEC-010 binding).
- **External**: full URL with HTTPS.
- **Never** use `../` relative paths — they 404 once the wiki is published.

### Tables
- Use tables for catalogs, capability matrices, comparison grids.
- Keep cells short — multi-paragraph content goes outside the table.

### Diagrams
- Mermaid only (`flowchart LR`, `sequenceDiagram`, `classDiagram`, etc.).
- One diagram per page is plenty; two if they show different perspectives.
- Source citation under the diagram if it derives from a `docs/` doc.

### Lists vs prose
- Use prose by default. Lists for: enumerable items (services, ports, env vars),
  step sequences, capability matrices.
- Avoid bullet salad — a page that is 80% bullets is a sign the prose was skipped.

## Length budget

| Page type | Target lines |
|---|---|
| Overview / Home / Index | 100-200 |
| Single-topic concept page (one mechanism, one model, one boundary) | 100-200 |
| Multi-topic concept page (e.g. Fail-Open-Posture covering all 5 NE invariants + emergency passthrough + kill switch) | 250-400 |
| Subsystem catalog / "this group's group" page | 250-450 |
| Recipe (how-to) | 250-400 |
| Feature card | 200-350 |
| FAQ section / cluster | 300-500 |
| Glossary | 100-300 |

Hard ceiling: 600 lines. Anything bigger should split. Hard floor: ~80 lines.
Anything smaller should expand with examples / cross-references or merge with
adjacent page.

A page that says one focused thing well at 120 lines is better than a padded
page at 300 lines. Dense prose + 1 diagram + table + canonical-docs footer
fills 100-160 lines easily; if your draft is 250+ lines, double-check it
isn't covering 2 topics that should split.

## Voice / terminology

- **English only** (CLAUDE.md binding).
- **Declarative, no hype**. Bad: "Nexus Gateway is the world's most advanced…".
  Good: "Nexus Gateway routes AI traffic through three independent capture
  paths."
- **No "we" / "our"**. Use project name or passive voice. Bad: "We support
  Anthropic." Good: "Nexus supports Anthropic via the `anthropic` provider
  adapter."
- **No "you'll" / "let's"**. Bad: "Let's set up your first VK." Good: "Create
  your first virtual key from the AI Gateway page."
- **IoT terminology boundary** (CLAUDE.md): internal docs use Thing / Shadow /
  desired / reported / drift; user-facing wiki uses node / config sync / target
  config / applied config / out of sync. The wiki sits on the user-facing side
  by default. Concepts pages and contributor-audience pages may use the
  internal vocabulary with a one-line glossary anchor.

## Canonical-docs footer rules

Every page ends with a `## Canonical docs` section containing:

1. **1-4 absolute-URL links** into the main repo. Pick the most authoritative
   doc(s) for the page topic. Prefer architecture docs over feature docs over
   ops docs. Each link gets a one-line "what it covers" gloss.
2. **3-6 adjacent wiki pages** in a single line, separated by middle dots `·`.
   Adjacent = same group, or the page that this page naturally hands off to.

Example:

```markdown
## Canonical docs

- [`provider-adapter-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md) — Rules 1-8 governing every provider adapter
- [`normalization-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/normalization-architecture.md) — Canonical ↔ wire format pipeline
- [`provider-coverage.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-coverage.md) — Current provider/model matrix

**Adjacent wiki pages**: [AI Gateway Overview](AI-Gateway-Overview) · [AI Gateway Streaming](AI-Gateway-Streaming) · [Canonical Vs Wire Format](Canonical-Vs-Wire-Format) · [Recipe Adding A Provider Adapter](Recipe-Adding-A-Provider-Adapter)
```

## Validation checklist (every page, before subagent returns)

Before declaring a page done, the subagent runs through:

- [ ] H1 matches filename
- [ ] No YAML frontmatter
- [ ] Opening paragraph is 3-5 sentences, declarative, no hype words
- [ ] 2-5 H2 sections
- [ ] No relative `../` paths in any link
- [ ] All repo-file links are absolute `https://github.com/AlphaBitCore/nexus-gateway/blob/main/...`
- [ ] Inter-wiki links use `[Title](Hyphenated-Page-Name)` form
- [ ] `## Canonical docs` footer present with 1-4 doc links + adjacent wiki line
- [ ] Page length within budget (see table above)
- [ ] No `TODO` / `FIXME` / `XXX` strings
- [ ] No "we" / "our" / "let's" / "you'll" — English declarative voice
- [ ] No new claim made without a source doc or code path backing it
- [ ] Mermaid blocks (if any) render in standard GitHub flavor
- [ ] If page mentions an IAM action / endpoint / config key, the name exactly
      matches what's in the code (verify via grep)
- [ ] **No archaeology** — `grep -iE '(DEC-[0-9]|supersed|decision log|rejected alternative|as of 2026|in 2026-05-[0-9]|previously this|originally|historically|tracks historical)'` returns no hits in your page (Release-History.md is the only exception — see style guide)
- [ ] **No internal SDD references** — no `E<n>-S<m>` epic-story IDs in the prose unless the page is `Roadmap-Active`, `Roadmap-Queued`, or `Release-History`
