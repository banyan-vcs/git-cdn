/* git-cdn site language negotiation — shared by every page, no dependencies.
 *
 * The site is served from a subpath (project pages, e.g. /git-cdn/), so no
 * path is hardcoded: zh pages live in a zh-cn/ directory next to the EN
 * pages, and redirects are always relative.
 *
 * Precedence for the initial language:
 *   1. ?lang=zh|en — explicit link intent; remembered, redirected if needed
 *   2. a remembered manual switch (localStorage) — never overridden again
 *   3. browser preferred language — the default (EN) pages redirect to
 *      zh-cn/ when the browser prefers Chinese; localized pages never
 *      bounce a visitor back, so shared zh-cn links keep their intent.
 */
(function () {
  'use strict';

  var KEY = 'git-cdn-lang';
  var PAGES = ['', 'index.html', 'docs.html'];

  function remember(value) {
    try { localStorage.setItem(KEY, value); } catch (e) { /* private mode */ }
  }
  function choice() {
    try { return localStorage.getItem(KEY); } catch (e) { return null; }
  }

  // Manual switches (language links carry data-lang) are remembered wherever
  // they happen, including pages this script does not negotiate.
  document.addEventListener('click', function (event) {
    var target = event.target;
    var link = target && target.closest ? target.closest('a[data-lang]') : null;
    if (link) remember(link.getAttribute('data-lang'));
  }, false);

  var inZh = /\/zh-cn\/[^/]*$/.test(location.pathname);
  var leaf = location.pathname.replace(/^.*\//, ''); // '' for directory URLs
  var page = PAGES.indexOf(leaf) !== -1 ? leaf : '';

  function redirect(toZh) {
    var params = location.search.replace(/[?&]lang=(?:zh|en)(?=&|$)/gi, '');
    if (params === '?' || params === '&') params = '';
    else if (params.charAt(0) === '&') params = '?' + params.slice(1);
    location.replace((toZh ? 'zh-cn/' : '../') + page + params + location.hash);
  }

  var explicit = /[?&]lang=(zh|en)(?=&|$)/i.exec(location.search);
  if (explicit) {
    var want = explicit[1].toLowerCase();
    remember(want);
    if (want === 'zh' && !inZh) redirect(true);
    else if (want === 'en' && inZh) redirect(false);
    return;
  }

  if (choice()) return;             // the visitor already picked a language
  if (inZh) return;                 // localized pages never bounce back

  var langs = navigator.languages && navigator.languages.length
    ? navigator.languages : [navigator.language || 'en'];
  for (var i = 0; i < langs.length; i++) {
    var tag = String(langs[i]).toLowerCase();
    if (/^zh(-|$)/.test(tag)) { redirect(true); return; }
    if (/^en(-|$)/.test(tag)) return;   // first supported preference wins
  }
})();
