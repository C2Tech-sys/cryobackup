/* escrow.js: the device page's repository-key panel, htmx edge (P22-R2b K3).
 *
 * "Verify now" posts with htmx and swaps the panel in place. The console answers every verdict and
 * every refusal it can word with 200 and the panel itself (htmx 2 swaps no 4xx). This file covers
 * what is left: an answer that is not the panel (a 403 from the CSRF or origin gate, a 413, a 5xx, a
 * dropped connection). htmx swaps nothing for those, so without this the administrator would click and
 * see nothing. It shows the message in the panel's own #escrow-fail banner. No request, no storage,
 * nothing from the network; the page works without it (the form posts normally). Scoped to
 * #escrow-panel like restore.js is to #restore-box. */
(function () {
  'use strict';
  function inPanel(el) {
    return !!(el && el.closest && el.closest('#escrow-panel'));
  }
  function show(title, text) {
    var panel = document.getElementById('escrow-panel');
    var fail = panel && panel.querySelector('#escrow-fail');
    if (!fail) { return; }
    fail.querySelector('[data-fail-title]').textContent = title;
    fail.querySelector('[data-fail-text]').textContent = text;
    fail.hidden = false;
    if (fail.scrollIntoView) { fail.scrollIntoView({ block: 'nearest' }); }
  }
  document.addEventListener('htmx:beforeRequest', function (ev) {
    // A new request clears the last error: the banner never says something that is no longer true.
    if (!inPanel(ev.target)) { return; }
    var fail = document.querySelector('#escrow-panel #escrow-fail');
    if (fail) { fail.hidden = true; }
  });
  document.addEventListener('htmx:responseError', function (ev) {
    if (!inPanel(ev.target)) { return; }
    var st = ev.detail && ev.detail.xhr ? ev.detail.xhr.status : 0;
    if (st === 401 || st === 403) {
      show('The console refused the request.', 'Your session may have ended, or the page is out of date. Reload the page, sign in again if asked, and retry. Nothing was checked.');
    } else {
      show('The console could not complete the request.', 'It answered with status ' + st + '. Nothing was checked. Reload the page to see what was recorded, then try again.');
    }
  });
  document.addEventListener('htmx:sendError', function (ev) {
    if (inPanel(ev.target)) { show('The console could not be reached.', 'Check the connection. Nothing was checked. Reload the page to see what was recorded.'); }
  });
  document.addEventListener('htmx:timeout', function (ev) {
    if (inPanel(ev.target)) { show('The console did not answer in time.', 'The check may still have run. Reload the page to see what was recorded.'); }
  });
})();
