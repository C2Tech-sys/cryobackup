/* restore.js: the restore wizard's htmx edge (P22-G fix G1).
 *
 * The wizard answers every refusal as a banner inside its own box, and answers an htmx post with 200 so
 * htmx swaps it (htmx 2 swaps no 4xx). This file covers what is left: an answer that is not the box
 * (a 413 from the body cap, a 5xx, a dropped connection). htmx swaps nothing for those, so without
 * this the operator would click and see nothing. It shows the message in the box's #restore-fail
 * banner. No request, no storage, nothing from the network; the page works without it. */
(function () {
  'use strict';
  function inBox(el) {
    return !!(el && el.closest && el.closest('#restore-box'));
  }
  function show(title, text) {
    var box = document.getElementById('restore-box');
    var fail = box && box.querySelector('#restore-fail');
    if (!fail) { return; }
    fail.querySelector('[data-fail-title]').textContent = title;
    fail.querySelector('[data-fail-text]').textContent = text;
    fail.hidden = false;
    if (fail.scrollIntoView) { fail.scrollIntoView({ block: 'nearest' }); }
  }
  document.addEventListener('htmx:responseError', function (ev) {
    if (!inBox(ev.target)) { return; }
    var st = ev.detail && ev.detail.xhr ? ev.detail.xhr.status : 0;
    if (st === 413) {
      show('The selection is too large.', 'It is more than the 64 KiB one restore form can carry. Select the folders that hold the files instead of the files one by one. Nothing was started.');
    } else if (st === 401 || st === 403) {
      show('The console refused the request.', 'Your session may have ended or needs a new check. Reload the page, sign in again if asked, and retry. Nothing was started.');
    } else {
      show('The console could not complete the request.', 'It answered with status ' + st + '. Nothing was started. Reload the page and try again.');
    }
  });
  document.addEventListener('htmx:sendError', function (ev) {
    if (inBox(ev.target)) { show('The console could not be reached.', 'Check the connection and try again. Nothing was started.'); }
  });
  document.addEventListener('htmx:timeout', function (ev) {
    if (inBox(ev.target)) { show('The console did not answer in time.', 'Nothing was started. Try again.'); }
  });
})();
