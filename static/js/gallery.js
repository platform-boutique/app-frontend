// Product gallery: clicking a thumbnail shows it as the main image.
// Without JS the thumbnails are plain links to the full-size images.
(function () {
  document.querySelectorAll('[data-gallery]').forEach(function (gallery) {
    var main = gallery.querySelector('[data-gallery-main]');
    if (!main) { return; }
    gallery.addEventListener('click', function (e) {
      var link = e.target.closest('[data-gallery-src]');
      if (!link || e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) { return; }
      e.preventDefault();
      main.src = link.getAttribute('data-gallery-src');
      gallery.querySelectorAll('[data-gallery-src]').forEach(function (l) { l.removeAttribute('aria-current'); });
      link.setAttribute('aria-current', 'true');
    });
  });
})();
