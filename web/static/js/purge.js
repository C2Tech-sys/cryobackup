/* purge.js - ADR 0019: behaviour of the purge and delete dialogs (partials/purge.html). First-party, no dependency.
   Without it every dialog still works: a plain form the server checks in full (owner, session, fresh MFA, the exact
   name, the reason). With it:
   - the destructive button is aria-disabled, with its reason shown, until the typed name matches exactly;
   - a submit shows the submitting state (busy button, fields read-only) and cannot be sent twice;
   - opening a dialog focuses its first empty field (the code when one is asked for), a refused one focuses its error;
   - Tab stays inside an open dialog, and Esc closes it (not while submitting) and returns focus to its button. */
(function () {
  'use strict';
  var OPEN = 'details.act-purge[open]';

  function sync(form) {
    var inp = form.querySelector('[data-purge-name]'), btn = form.querySelector('[data-purge-submit]');
    if (!inp || !btn || form.getAttribute('data-busy') === '1') return;
    var ok = inp.value === inp.getAttribute('data-purge-name');
    if (ok) btn.removeAttribute('aria-disabled'); else btn.setAttribute('aria-disabled', 'true');
    var why = form.querySelector('[data-purge-why]'); if (why) why.hidden = ok;
  }
  function all(sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); }
  function focusables(d) {
    return all('summary, input:not([type=hidden]), button, a[href], select, textarea', d).filter(function (el) {
      return !el.disabled && el.offsetParent !== null;
    });
  }
  function firstField(d) {
    var err = d.querySelector('.purge-err'); if (err) return err;
    var f = all('input:not([type=hidden])', d).filter(function (i) { return !i.value && !i.readOnly; });
    return f[0] || d.querySelector('[data-purge-submit]');
  }

  document.addEventListener('input', function (e) {
    var f = e.target.closest && e.target.closest('form[data-purge]'); if (f) sync(f);
  });
  document.addEventListener('submit', function (e) {
    var f = e.target; if (!f.matches || !f.matches('form[data-purge]')) return;
    var btn = f.querySelector('[data-purge-submit]');
    if (f.getAttribute('data-busy') === '1') { e.preventDefault(); return; }
    if (btn && btn.getAttribute('aria-disabled') === 'true') {
      e.preventDefault(); var inp = f.querySelector('[data-purge-name]'); if (inp) inp.focus(); return;
    }
    f.setAttribute('data-busy', '1');
    all('input:not([type=hidden])', f).forEach(function (i) { i.readOnly = true; });
    if (btn) { btn.setAttribute('aria-busy', 'true'); btn.textContent = btn.getAttribute('data-busy-label') || btn.textContent; }
  });
  /* toggle does not bubble: listen in the capture phase. */
  document.addEventListener('toggle', function (e) {
    var d = e.target; if (!d.matches || !d.matches('details.act-purge') || !d.open) return;
    var el = firstField(d); if (el) el.focus();
  }, true);
  document.addEventListener('keydown', function (e) {
    var d = document.querySelector(OPEN); if (!d) return;
    if (e.key === 'Escape') {
      if (d.querySelector('form[data-busy="1"]')) return;
      d.removeAttribute('open'); var s = d.querySelector('summary'); if (s) s.focus(); e.stopPropagation(); return;
    }
    if (e.key !== 'Tab' || !d.contains(document.activeElement)) return;
    var els = focusables(d); if (!els.length) return;
    var first = els[0], last = els[els.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  });
  /* back from the browser cache after a submit: the form is usable again */
  window.addEventListener('pageshow', function () {
    all('form[data-purge][data-busy="1"]').forEach(function (f) {
      if (f.closest('.d-static')) return; /* the /design page's submitting sample stays as it is */
      f.removeAttribute('data-busy');
      all('input:not([type=hidden])', f).forEach(function (i) { i.readOnly = false; });
      var b = f.querySelector('[data-purge-submit]'); if (b) b.removeAttribute('aria-busy');
    });
    all('form[data-purge]').forEach(sync);
  });
  all('form[data-purge]').forEach(sync);
  var open = document.querySelector(OPEN); if (open) { var el = firstField(open); if (el) el.focus(); }
})();
