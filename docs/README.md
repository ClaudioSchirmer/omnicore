# OmniCore — Framework Documentation

Static documentation site. No build step, no dependencies. Deploys to GitHub Pages as-is.

Published at **<https://claudioschirmer.github.io/omnicore/>** (GitHub Pages → deploy from branch, folder `/docs`). Section deep-links use the hash route, e.g. `…/omnicore/#integration-events`.

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

## Changelog

Edit `content/sections/changelog.html` — add a new `.release` block at the top and bump the `v…` badge in `index.html` (`#verBadge`).
