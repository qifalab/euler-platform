// FOUC guard: resolve the theme before first paint so the --eu-* token
// set (mesh background, glass surfaces) never flashes light/dark.
// Mirrors the logic in App.vue: localStorage['eu:theme'] wins,
// otherwise follow prefers-color-scheme.
// Kept as a same-origin file (not inline) so the console's
// Content-Security-Policy can use script-src 'self' without 'unsafe-inline'.
(function () {
  var theme;
  try {
    theme = localStorage.getItem("eu:theme");
  } catch (e) {
    theme = null;
  }
  if (theme !== "dark" && theme !== "light") {
    theme =
      window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches
        ? "dark"
        : "light";
  }
  document.documentElement.dataset.theme = theme;
})();
