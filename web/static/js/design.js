/* design.js - toggles of the /design check page (components 12.1): theme, reduced
   motion, force stale, force SSE drop, viewport width presets and the hit-target
   overlay. First-party, loaded only on /design. */
(function () {
  'use strict';
  var d = document.documentElement;
  function $$(s) { return Array.prototype.slice.call(document.querySelectorAll(s)); }
  function press(group, value) {
    $$('[data-toggle="' + group + '"]').forEach(function (b) { b.setAttribute('aria-pressed', b.getAttribute('data-value') === value ? 'true' : 'false'); });
  }
  function hitOverlay(on) {
    $$('.hit-small').forEach(function (e) { e.classList.remove('hit-small'); });
    d.classList.toggle('show-hit', on);
    if (!on) return;
    $$('a[href], button, input, select, textarea, summary').forEach(function (e) {
      var r = e.getBoundingClientRect();
      if (r.width && r.height && (r.width < 32 || r.height < 32)) e.classList.add('hit-small');
    });
  }
  document.addEventListener('click', function (ev) {
    var b = ev.target.closest && ev.target.closest('[data-toggle]'); if (!b) return;
    var g = b.getAttribute('data-toggle'), v = b.getAttribute('data-value');
    switch (g) {
      case 'theme': window.cryoTheme.set(v === 'system' ? '' : v); press('theme', v); break;
      case 'motion': d.classList.toggle('reduce-motion', v === 'reduce'); press('motion', v); break;
      case 'stale': if (window.cryoFresh) window.cryoFresh.forceStale(v === 'on'); press('stale', v); break;
      case 'drop': if (window.cryoFresh) window.cryoFresh.forceDrop(v === 'on'); press('drop', v); break;
      case 'width': if (v === 'full') d.removeAttribute('data-design-width'); else d.setAttribute('data-design-width', v); press('width', v); break;
      case 'hit': hitOverlay(v === 'on'); press('hit', v); break;
    }
  });
  press('theme', window.cryoTheme ? window.cryoTheme.get() : 'system');
})();
