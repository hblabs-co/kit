// Theme toggle — interactive half of the shared dark-mode switch.
//
// The initial theme is already applied before paint by the inline
// boot snippet (uikit.ThemeBoot / {{ themeBoot }}): localStorage
// 'hb-theme' > prefers-color-scheme > light, written to
// <html data-theme>. This file only wires the .hb-theme-toggle
// button (hook: id="theme-toggle"): flip data-theme, persist the
// choice, and keep aria-pressed in sync for assistive tech. Load it
// with `defer` so the button exists when it runs:
//
//   <script src="{{ asset "uikit/js/public/theme.js" }}" defer></script>
//
// Shared verbatim across every HB Labs service via the uikit
// ExtraStatic mount — no per-service copy.
(function () {
  "use strict";
  var btn = document.getElementById("theme-toggle");
  if (!btn) return;
  var root = document.documentElement;

  function isDark() {
    return root.getAttribute("data-theme") === "dark";
  }

  function sync() {
    btn.setAttribute("aria-pressed", isDark() ? "true" : "false");
  }

  sync();

  btn.addEventListener("click", function () {
    var next = isDark() ? "light" : "dark";
    root.setAttribute("data-theme", next);
    try {
      localStorage.setItem("hb-theme", next);
    } catch (e) {}
    sync();
  });
})();
