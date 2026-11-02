/* copy.js - first-party copy buttons (components 9.6 "Code block", M2-W3A).
   A button with data-copy-from="<id>" copies the text of that element with the
   Clipboard API and shows "Copied" for two seconds; where the API is missing or
   refused (an http lab console is not a secure context) it falls back to
   execCommand, and if that fails it selects the text and says so, so Ctrl+C
   works. The button is hidden until this script marks the page (theme.js adds
   the js class); without script the text is still select-on-click. Nothing is
   sent anywhere: no request, no storage, no third-party code. */
(function () {
  'use strict';
  var HOLD = 2000;
  var live = null;

  function announce(msg) {
    if (!live) {
      live = document.createElement('div');
      live.className = 'sr-only';
      live.setAttribute('role', 'status');
      live.setAttribute('aria-live', 'polite');
      document.body.appendChild(live);
    }
    live.textContent = '';
    window.setTimeout(function () { live.textContent = msg; }, 30);
  }

  /* The text to copy: a list copies one item per line (the numbering is CSS, not text),
     anything else copies exactly what is shown. */
  function textOf(el) {
    if (el.tagName === 'OL' || el.tagName === 'UL') {
      return Array.prototype.map.call(el.querySelectorAll('li'), function (li) { return li.textContent.trim(); }).join('\n');
    }
    return el.textContent;
  }

  function selectText(el) {
    try {
      var r = document.createRange();
      r.selectNodeContents(el);
      var s = window.getSelection();
      s.removeAllRanges();
      s.addRange(r);
    } catch (e) { /* nothing to select with */ }
  }

  function execCopy(text) {
    var ta = null;
    try {
      ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.top = '-1000px';
      document.body.appendChild(ta);
      ta.select();
      return document.execCommand('copy');
    } catch (e) {
      return false;
    } finally {
      if (ta && ta.parentNode) ta.parentNode.removeChild(ta);
    }
  }

  function write(text) {
    if (navigator.clipboard && window.isSecureContext) {
      return navigator.clipboard.writeText(text).then(function () { return true; }, function () { return execCopy(text); });
    }
    return Promise.resolve(execCopy(text));
  }

  function show(btn, state, label, said) {
    var l = btn.querySelector('.copylabel');
    if (!btn.hasAttribute('data-idle')) btn.setAttribute('data-idle', l ? l.textContent : 'Copy');
    if (l) l.textContent = label;
    btn.setAttribute('data-state', state);
    announce(said);
    window.clearTimeout(btn._t);
    btn._t = window.setTimeout(function () {
      if (l) l.textContent = btn.getAttribute('data-idle');
      btn.removeAttribute('data-state');
    }, state === 'failed' ? HOLD * 3 : HOLD);
  }

  document.addEventListener('click', function (ev) {
    var btn = ev.target.closest ? ev.target.closest('[data-copy-from]') : null;
    if (!btn) return;
    var src = document.getElementById(btn.getAttribute('data-copy-from'));
    if (!src) { show(btn, 'failed', 'Copy failed', 'Copy failed. The text is not on this page.'); return; }
    var text = textOf(src);
    write(text).then(function (ok) {
      if (ok) {
        show(btn, 'copied', 'Copied', 'Copied to the clipboard.');
      } else {
        selectText(src);
        show(btn, 'failed', 'Copy failed', 'Copy failed. The text is selected: press Ctrl+C.');
      }
    });
  });
})();
