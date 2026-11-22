// Runs in the head, before the stylesheets, so the chosen theme is applied on first paint (no flash).
// A blocking file instead of inline code: the Content-Security-Policy needs no 'unsafe-inline' for scripts.
(function () {
  var d = document.documentElement;
  d.className += ' js';
  try {
    var t = window.localStorage.getItem('theme');
    if (t === 'light' || t === 'dark') { d.setAttribute('data-theme', t); }
  } catch (e) { /* storage blocked: follow the OS setting */ }
})();
