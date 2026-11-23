// No-FOUC theme bootstrap (ships inline in <head>; keep it terse —
// it is on every page). Rationale + contract: see uikit/theme.go.
(function () {
  try {
    var saved = localStorage.getItem('hb-theme');
    var sys = window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
    var theme = saved === 'light' || saved === 'dark' ? saved : sys;
    document.documentElement.setAttribute('data-theme', theme);
  } catch (e) {}
})();
