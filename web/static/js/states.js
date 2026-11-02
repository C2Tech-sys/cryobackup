/* states.js - first-party loading and error states for htmx result regions
   (components 5.1 and 9.5, M2-W3A). A region marked class="loadable" keeps its
   markup while a request is in flight (CSS draws the skeleton rows), says
   "This is taking longer than usual" after ten seconds, and turns a failed
   request - a network error or an error status htmx will not swap in - into an
   alarm panel with a Retry link, never a list that looks current. Without script
   the lists are plain links and forms and the browser shows its own progress.
   No request is made here; nothing is stored. */
(function () {
  'use strict';
  var SLOW_MS = 10000;

  function region(e) {
    var t = (e.detail && e.detail.target) || e.target;
    return t && t.closest ? t.closest('.loadable') : null;
  }

  function clearFail(r) {
    var old = r.querySelectorAll('.jsfail');
    for (var i = 0; i < old.length; i++) old[i].parentNode.removeChild(old[i]);
  }

  function settle(r) {
    window.clearTimeout(r._slow);
    r.removeAttribute('data-slow');
    r.removeAttribute('aria-busy');
  }

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text) n.textContent = text;
    return n;
  }

  function fail(r, xhr) {
    clearFail(r);
    var status = xhr && xhr.status ? ' (status ' + xhr.status + ')' : '';
    var panel = el('section', 'panel-state alarm jsfail');
    panel.setAttribute('role', 'alert');
    var inner = el('div');
    inner.appendChild(el('h2', null, 'Could not load the list'));
    inner.appendChild(el('p', null, 'The request failed' + status + '. This is an alarm, not an empty list: what is shown below may be out of date.'));
    var retry = el('a', 'btn', 'Retry');
    retry.href = window.location.href;
    inner.appendChild(retry);
    panel.appendChild(inner);
    r.insertBefore(panel, r.firstChild);
  }

  document.addEventListener('htmx:beforeRequest', function (e) {
    var r = region(e);
    if (!r) return;
    clearFail(r);
    r.setAttribute('aria-busy', 'true');
    window.clearTimeout(r._slow);
    r._slow = window.setTimeout(function () { r.setAttribute('data-slow', 'true'); }, SLOW_MS);
  });

  document.addEventListener('htmx:afterRequest', function (e) {
    var r = region(e);
    if (!r) return;
    settle(r);
    if (e.detail && e.detail.failed) fail(r, e.detail.xhr);
  });
})();
