# OmniCore — Framework Documentation

Static documentation site. No build step, no dependencies. Deploys to GitHub Pages as-is.

Published at **<https://claudioschirmer.github.io/omnicore/>** via the GitHub Actions workflow `.github/workflows/pages.yml`, which uploads this `docs/` folder as the single Pages artifact. Publishing is **manual** — Actions tab → *Deploy docs to GitHub Pages* → *Run workflow* — so a push/merge to `main` never republishes the live site on its own; you choose when it goes live. Section deep-links use the hash route, e.g. `…/omnicore/#integration-events`.

> **Pages source setting (one-time):** Settings → Pages → Source must be **"GitHub Actions"** (not "Deploy from a branch"). Leaving the branch source on alongside this workflow produces two `github-pages` artifacts in one deployment and fails with *"Multiple artifacts named github-pages … Artifact count is 2"* — the workflow is the single source of truth.

## Run locally

`fetch()` is blocked on `file://`, so serve over http:

```bash
python3 -m http.server      # then open http://localhost:8000
```

(or the VS Code "Live Server" extension). On GitHub Pages it just works.

## Structure

```
index.html              Shell: layout, sidebar, topbar, search box. Rarely changes.
assets/styles.css        All styling. Theme tokens (light/dark) at the top under :root / [data-theme="dark"].
assets/app.js            All behavior: routing, section loading, search, theme, print, etc.
content/nav.json         Sidebar groups + order. The source of truth for which sections exist.
content/sections/<id>.html   One file per section — pure content HTML (h2, p, tables, pre/code, .box, .card, .grid-2 ...).
```

## Common edits

**Edit a section's text** — open `content/sections/<id>.html`. It's only that section's content; nothing else can be affected.

**Add a new section**
1. Create `content/sections/<my-id>.html` with the content (start with an `<h2>`).
2. Add `{ "id": "my-id", "label": "My Title" }` to the right group in `content/nav.json`.
   (Add a new group object if needed — `{ "group": "...", "items": [ ... ] }`.)
That's it — nav, routing, prev/next, search, and "On this page" pick it up automatically.

**Reorder / regroup** — reorder entries/groups in `content/nav.json`. Order there = order everywhere.

**Change colors** — edit the CSS variables in `assets/styles.css` (`:root` for light, `[data-theme="dark"]` for dark).

## Conventions used inside section files

- `<h2>` = section title (one per file). `<h3>` = subsection (auto-gets an id, shows in "On this page", gets a copy-link icon). When a section has 2+ `<h3>`, they become collapsible automatically.
- Code: ```<pre><code> ... </code></pre>``` — gets syntax highlight + a copy button.
- Callouts: `<div class="box">`, `.box.good`, `.box.warn`, `.box.bad`, or `<div class="note">`.
- Cards: `<div class="grid-2"><div class="card">...</div>...</div>`.
- Status pills: `<span class="tag auto">automatic</span>` / `<span class="tag manual">manual</span>`.

## Releasing — version-bump checklist

Every place that must change when cutting `vX.Y.Z`. Miss one and the site/spec drifts
from the tag.

1. **`../CHANGELOG.md`** — rename `## [Unreleased]` → `## [X.Y.Z] - YYYY-MM-DD`; add a
   fresh empty `## [Unreleased]` above it. (The `[X.Y.Z]: …/releases/tag/vX.Y.Z` link
   reference at the bottom is optional — the convention lapsed after 0.12.0.)
2. **`index.html`** — bump the `#verBadge` span (`v…`).
3. **`content/sections/changelog.html`** — add a new `.release` block at the top
   (`<span class="release-tag">vX.Y.Z</span><span class="release-date">Month D, YYYY</span>`),
   mirroring the CHANGELOG.md entry. **Breaking entries** (here or in the standing
   `[Unreleased]` block) MUST carry a standalone `<strong>breaking</strong>` right
   after `<strong>Changed</strong> —`: `assets/app.js` auto-derives each release's
   severity — the ▲ icon and the release-tag colour — by scanning entries for a
   `<strong>` whose text is exactly `breaking`. Prose like "(breaking)" does NOT
   trigger it (the root `CHANGELOG.md` is free prose, not parsed).
4. **`content/sections/features.html`** — refresh the coverage figure in the stat strip
   (the `94.7%` `.stat`). Recompute with this pinned command (so it never drifts on method):
   - coverage: `go test ./... -coverpkg=./... -coverprofile=/tmp/c.out && go tool cover -func=/tmp/c.out | tail -1` (pure unit, all packages in the denominator)
5. **Tag (maintainer only):** `git commit` → `git tag vX.Y.Z` → push. The Go module
   version IS the tag — there is no version constant in code.
6. **`../../omnicore-example-users/go.mod`** — bump `require …/omnicore vX.Y.Z` once the
   tag is published, so the consumer's clean-clone / CI builds against the release
   (locally `go.work` overlays the in-tree checkout and masks a stale `require`).
