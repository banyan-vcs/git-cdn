# git-cdn site

The project website for [git-cdn](https://github.com/banyan-vcs/git-cdn) —
an HTTP Git caching proxy. English at the root, Chinese under `zh-cn/`.

**There is no build step, no generator, and no dependency.** Pages are plain
HTML sharing one stylesheet (`assets/style.css`). What you edit is what is
served — same taste as the banyan site, a different palette (cache steel-blue
instead of banyan green).

## Publish

This is a project site: the content is served from the `gh-pages` branch of
`banyan-vcs/git-cdn` at `https://banyan-vcs.github.io/git-cdn/` (the
organization domain root belongs to the banyan site).

The branch is live — GitHub auto-publishes a pushed `gh-pages` branch, no
Settings change was needed. To update the site, edit here and:

```sh
git add -A && git commit -m "site: ..."
git push
```

If Pages ever needs manual wiring: in the `git-cdn` repository,
Settings → Pages → Deploy from a branch → `gh-pages` / root.

All asset and language links are relative, so the pages work under the
`/git-cdn/` subpath, at a custom domain root, or from a local file preview.
If a custom domain is added later, add a `CNAME` file and update the
canonical URLs in `sitemap.xml` and the `og:url` meta tags.

## Layout

```
index.html            EN home (why / how a fetch is served / features / quick start / architecture)
docs.html             EN documentation index — deep links into the main repo's docs/
404.html              bilingual not-found page
zh-cn/                Chinese mirror of the pages above
assets/style.css      the whole design system (~330 lines, auto dark mode)
assets/avatar.svg     the project mark — favicon and brand glyph
assets/lang.js        language negotiation (relative paths, subpath-safe)
assets/architecture.svg      EN architecture diagram
assets/architecture.zh.svg   ZH copy — update both together when topology changes
robots.txt, sitemap.xml, .nojekyll
```

## Maintenance rules

- **Editing a page**: open the HTML, edit, push. Both languages must be
  updated together (root ↔ `zh-cn/`) — the language switcher links them
  one-to-one.
- **Facts come from the repository** — behavior claims from the
  request/behavior table in `README.md` and from `docs/design.md`, defaults
  from `docs/configuration.md` / `config.example.yaml`, counts (e.g. "208
  test functions across 42 files") from the actual tree. Never invent a
  number; when it changes in the repo, change it here in both languages.
  There are no performance numbers on the site on purpose: single-run
  timings are explicitly not baselines in `docs/testing.md`.
- **Docs index** links to `blob/main/docs/…` and lists the nine documents
  under `docs/`. Add a row in both `docs.html` and `zh-cn/docs.html` when a
  document lands, and move the count in the heading and description.
- **Architecture diagrams**: `assets/architecture.svg` and its `.zh.svg`
  sibling are this site's copies of the pair in the repository's
  `docs/assets/` — the same topology in this site's colors. When the
  repository's diagrams change, carry the change into both copies and keep
  the `alt` texts in the two `index.html` files in sync with them.
- **Voice**: first-party assertions only, no slogan tails. The site renders
  identically offline — no web fonts, no CDN scripts, no analytics. Keep it
  that way.
