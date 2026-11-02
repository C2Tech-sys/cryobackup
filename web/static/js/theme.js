/* theme.js - first-party, runs in <head> before first paint (components 1.9).
   Applies the stored theme choice and marks the page as script-enabled.
   Storage is a per-viewer convenience: every access is guarded and the page
   renders correctly without it (absent data-theme = follow the system). */
(function () {
  var d = document.documentElement;
  d.classList.add('js');
  try {
    var t = localStorage.getItem('cryo.theme');
    if (t === 'dark' || t === 'light') d.setAttribute('data-theme', t);
  } catch (e) { /* storage blocked: follow the system */ }
  window.cryoTheme = {
    set: function (t) {
      if (t === 'dark' || t === 'light') d.setAttribute('data-theme', t); else d.removeAttribute('data-theme');
      try { if (t === 'dark' || t === 'light') localStorage.setItem('cryo.theme', t); else localStorage.removeItem('cryo.theme'); } catch (e) { /* ignore */ }
    },
    get: function () { return d.getAttribute('data-theme') || 'system'; }
  };
})();
