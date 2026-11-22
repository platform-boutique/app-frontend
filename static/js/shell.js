// Page shell behaviour (header and footer): the theme toggle and the currency select.
// A deferred file instead of inline code and inline event handlers: the Content-Security-Policy needs no
// 'unsafe-inline' for scripts.
(function () {
  // Currency: choosing a value sends the form (without JS the <noscript> button does it).
  var currency = document.getElementById('currency_code');
  var currencyForm = document.getElementById('currency_form');
  if (currency && currencyForm) {
    currency.addEventListener('change', function () { currencyForm.submit(); });
  }

  // Theme toggle: light / dark, remembered in localStorage (theme-init.js applies it on the next page).
  var btn = document.getElementById('theme-toggle');
  if (!btn) { return; }
  var root = document.documentElement;
  var media = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;
  function current() {
    var t = root.getAttribute('data-theme');
    if (t === 'light' || t === 'dark') { return t; }
    return media && media.matches ? 'dark' : 'light';
  }
  function label() {
    var next = current() === 'dark' ? 'light' : 'dark';
    btn.setAttribute('aria-label', 'Switch to ' + next + ' theme');
    btn.setAttribute('title', 'Switch to ' + next + ' theme');
  }
  btn.addEventListener('click', function () {
    var next = current() === 'dark' ? 'light' : 'dark';
    root.setAttribute('data-theme', next);
    try { window.localStorage.setItem('theme', next); } catch (e) { /* not remembered */ }
    label();
  });
  if (media && media.addEventListener) { media.addEventListener('change', label); }
  label();
})();
