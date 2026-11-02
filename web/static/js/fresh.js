/* fresh.js - first-party behaviour for the console shell (components 0.7, 1.5, 1.9, 9.1).
   Owns: the freshness stamp and the stale rule, the SSE stream and its polling
   fallback, the C10 banner, the theme switch, the drawer, collapsible customer
   groups, tooltip dismissal and toasts. No third-party code, no network request
   except same-origin /ui/events and the htmx partials.

   The stale rule: if no event and no successful poll arrived within 2 x the
   refresh interval, or the stream errored, the page says so (pill word "Polling" with
   "last hh:mm:ss", or "No data" with "since hh:mm:ss"; banner C10; "as of" markers on every
   live component). A page never
   freezes looking fresh. */
(function () {
  'use strict';
  var body = document.body;
  var refresh = Math.max(5, parseInt(body.getAttribute('data-refresh') || '30', 10));
  var eventsURL = body.getAttribute('data-events') || '';
  var zone = (document.getElementById('freshness') || body).getAttribute('data-zone') || undefined;
  var last = Date.now(), live = false, forced = false, dropped = false, stale = false, staleSince = '', es = null, retry = null;

  function pad(n) { return (n < 10 ? '0' : '') + n; }
  function clock(d) {
    try {
      return new Intl.DateTimeFormat('en-US', { timeZone: zone, hour12: true, hour: 'numeric', minute: '2-digit', second: '2-digit' }).format(d).replace(/[^0-9A-Za-z:]+/g, ' '); /* some ICU builds put a narrow no-break space before AM/PM: normalise to the server's ASCII space */
    } catch (e) { return (d.getHours() % 12 || 12) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds()) + (d.getHours() < 12 ? ' AM' : ' PM'); }
  }
  function $(s, r) { return (r || document).querySelector(s); }
  function $$(s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); }

  /* ---- freshness stamp ---- */
  /* The pill is four text nodes: .word, .t and the two <b> in .agents. They are updated in place;
     the markup is server-rendered (shared.html "stamp") and carries the same words (views.go stampWords). */
  var WORDS = { fresh: 'Live', partial: 'Partial', stale: 'Polling', unreachable: 'No data', loading: 'Waiting' };
  var lastAt = '--:--:--', curState = 'fresh';
  function stamp() { return document.getElementById('freshness'); }
  function asof() { return lastAt; }
  function stampTime(state) {
    if (state === 'stale') return 'last ' + lastAt;
    if (state === 'unreachable') return 'since ' + staleSince;
    return lastAt;
  }
  function paint(state) {
    var s = stamp(); if (!s) return;
    curState = state;
    s.setAttribute('data-state', state);
    var w = $('.word', s); if (w) w.textContent = WORDS[state] || WORDS.unreachable;
    var t = $('.t', s); if (t) t.textContent = stampTime(state);
  }
  function setAgents(n, m) {
    var s = stamp(); if (!s) return;
    var a = $('.agents', s);
    if (!(m > 0)) { return; }
    if (!a) {
      var sep = document.createElement('i'); sep.className = 'sep'; sep.setAttribute('aria-hidden', 'true');
      a = document.createElement('span'); a.className = 'agents';
      a.appendChild(document.createElement('b')); a.appendChild(document.createTextNode(' of ')); a.appendChild(document.createElement('b')); a.appendChild(document.createTextNode(' agents'));
      s.appendChild(sep); s.appendChild(a);
    }
    var b = $$('b', a); b[0].textContent = String(n); b[1].textContent = String(m);
  }
  /* After the polling fallback swaps the server-rendered stamp in, take the data time from it
     and, if the page is stale, put the stale reading back on it. */
  function syncStamp() {
    var s = stamp(); if (!s) return;
    var t = $('.t', s), at = t && t.getAttribute('data-at');
    if (at && at.charAt(0) !== '-') lastAt = at;
    if (stale) paint(curState); else curState = s.getAttribute('data-state') || 'fresh';
  }
  function setStamp(state) { paint(state); }
  function renderC10() {
    var strip = document.getElementById('banners'); if (!strip) return;
    var old = document.getElementById('c10'); if (old) old.parentNode.removeChild(old);
    if (!stale) return;
    var ul = $('ul', strip);
    if (!ul) { ul = document.createElement('ul'); ul.className = 'banners'; strip.appendChild(ul); }
    var li = document.createElement('li'); li.id = 'c10'; li.className = 'banner warn warn-item'; li.setAttribute('role', 'status');
    li.innerHTML = '<span class="st warn"><i aria-hidden="true"></i>LIVE UPDATES DISCONNECTED</span><div class="b-text"><span class="sr-only">Warning: </span></div>';
    $('.b-text', li).appendChild(document.createTextNode('Live updates disconnected: data as of ' + asof() + '.'));
    ul.appendChild(li);
  }
  function markLive() {
    $$('[data-live]').forEach(function (el) {
      if (stale) {
        el.setAttribute('data-stale', 'since ' + staleSince);
        var a = $('.asof', el); if (a) a.textContent = 'as of ' + asof();
      } else {
        el.removeAttribute('data-stale');
      }
    });
  }
  function setStale(on, why) {
    if (on === stale) { if (on) { renderC10(); markLive(); } return; }
    stale = on;
    if (on) {
      staleSince = clock(new Date()); setStamp(why === 'drop' ? 'unreachable' : 'stale');
      document.documentElement.setAttribute('data-stale', 'since ' + staleSince);
    } else {
      staleSince = ''; setStamp('fresh'); document.documentElement.removeAttribute('data-stale');
    }
    renderC10(); markLive();
  }
  function poll() {
    if (!window.htmx) return;
    $$('[data-poll]').forEach(function (el) { window.htmx.trigger(el, 'cryo:poll'); });
  }
  function mark() {
    last = Date.now();
    if (stale && !forced && !dropped) { poll(); setStale(false); }
  }
  setInterval(function () {
    if (!stamp()) return;
    if (forced || dropped) { setStale(true, dropped ? 'drop' : 'age'); return; }
    if (Date.now() - last > 2 * refresh * 1000) setStale(true, 'age');
  }, 1000);
  setInterval(function () { if (!live && !forced) poll(); }, refresh * 1000);
  document.addEventListener('htmx:afterRequest', function (e) {
    var el = e.detail && e.detail.elt;
    if (el && el.hasAttribute && el.hasAttribute('data-poll') && e.detail.successful) mark();
  });
  document.addEventListener('htmx:afterSwap', function () { syncStamp(); applyGroups(); renderC10(); markLive(); });

  /* ---- SSE ---- */
  function swap(html) {
    var t = document.createElement('template'); t.innerHTML = html;
    Array.prototype.forEach.call(t.content.children, function (n) {
      if (!n.id) return;
      var cur = document.getElementById(n.id);
      if (cur) { cur.replaceWith(n); if (window.htmx) window.htmx.process(n); }
    });
    applyGroups(); renderC10(); markLive();
  }
  function onFresh(data) {
    var s = stamp(); if (!s) return;
    try {
      var j = JSON.parse(data);
      lastAt = j.at;
      var t = $('.t', s); if (t) t.setAttribute('data-at', j.at);
      setAgents(j.n, j.m);
      if (!stale) setStamp(j.n < j.m ? 'partial' : 'fresh'); else paint(curState);
    } catch (e) { /* ignore a malformed stamp */ }
  }
  function connect() {
    if (!eventsURL || !window.EventSource || dropped) return;
    if (es) { es.close(); }
    es = new EventSource(eventsURL);
    es.addEventListener('open', function () { live = true; last = Date.now(); if (stale && !forced) { poll(); setStale(false); } });
    ['fleet.state', 'run.progress', 'banner.update'].forEach(function (n) { es.addEventListener(n, function (e) { mark(); swap(e.data); }); });
    es.addEventListener('freshness', function (e) { mark(); onFresh(e.data); });
    es.addEventListener('ping', function () { mark(); });
    es.addEventListener('error', function () {
      live = false; setStale(true, 'drop');
      if (es && es.readyState === 2 && !dropped) { clearTimeout(retry); retry = setTimeout(connect, 3000); }
    });
  }
  window.cryoFresh = {
    live: function () { return live; },
    isStale: function () { return stale; },
    forceStale: function (on) { forced = !!on; if (!on) { last = Date.now(); setStale(false); } else setStale(true, 'age'); },
    forceDrop: function (on) {
      dropped = !!on;
      if (dropped) { if (es) es.close(); live = false; setStale(true, 'drop'); }
      else { connect(); last = Date.now(); }
    }
  };
  syncStamp();
  connect();

  /* ---- collapsible customer groups ---- */
  var groupState = {};
  try { groupState = JSON.parse(sessionStorage.getItem('cryo.groups') || '{}'); } catch (e) { groupState = {}; }
  function applyGroups() {
    $$('tbody.grp').forEach(function (tb) {
      var id = tb.id;
      var want = Object.prototype.hasOwnProperty.call(groupState, id) ? groupState[id] : tb.getAttribute('data-default-collapsed') === 'true';
      tb.setAttribute('data-collapsed', want ? 'true' : 'false');
      var b = $('.chev', tb); if (b) b.setAttribute('aria-expanded', want ? 'false' : 'true');
    });
  }
  function syncTheme() {
    var cur = window.cryoTheme.get();
    $$('[data-theme-set]').forEach(function (b) { b.setAttribute('aria-pressed', b.getAttribute('data-theme-set') === cur ? 'true' : 'false'); });
  }
  function setDrawer(open) {
    body.setAttribute('data-drawer', open ? 'open' : 'closed');
    var b = $('.menu-btn'); if (b) b.setAttribute('aria-expanded', open ? 'true' : 'false');
  }
  document.addEventListener('click', function (e) {
    var t = e.target;
    var chev = t.closest && t.closest('.chev');
    if (chev) {
      var tb = chev.closest('tbody.grp'); if (!tb) return;
      var collapsed = tb.getAttribute('data-collapsed') !== 'true';
      groupState[tb.id] = collapsed; tb.setAttribute('data-collapsed', collapsed ? 'true' : 'false'); chev.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
      try { sessionStorage.setItem('cryo.groups', JSON.stringify(groupState)); } catch (err) { /* ignore */ }
      return;
    }
    var th = t.closest && t.closest('[data-theme-set]');
    if (th) { window.cryoTheme.set(th.getAttribute('data-theme-set')); syncTheme(); return; }
    if (t.closest && t.closest('.menu-btn')) {
      var open = body.getAttribute('data-drawer') !== 'open';
      setDrawer(open);
      if (open) { var n = $('.sidebar .navlink'); if (n) n.focus(); }
      return;
    }
    if (t.classList && t.classList.contains('scrim')) { setDrawer(false); var mb = $('.menu-btn'); if (mb) mb.focus(); }
  });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    if (body.getAttribute('data-drawer') === 'open') { setDrawer(false); var mb = $('.menu-btn'); if (mb) mb.focus(); return; }
    var tw = document.activeElement && document.activeElement.closest && document.activeElement.closest('.tipwrap');
    if (tw) tw.setAttribute('data-tip-off', '');
  });
  document.addEventListener('mouseout', function (e) { var tw = e.target.closest && e.target.closest('.tipwrap'); if (tw) tw.removeAttribute('data-tip-off'); });
  document.addEventListener('focusout', function (e) { var tw = e.target.closest && e.target.closest('.tipwrap'); if (tw) tw.removeAttribute('data-tip-off'); });

  /* ---- toasts (HX-Trigger: {"toast": {"level": "...", "text": "..."}}) ---- */
  document.body.addEventListener('toast', function (e) {
    var d = e.detail || {}, box = document.getElementById('toasts'); if (!box || !d.text) return;
    var lv = ['ok', 'warn', 'crit', 'info'].indexOf(d.level) >= 0 ? d.level : 'info';
    var t = document.createElement('div'); t.className = 'toast ' + lv; t.setAttribute('role', lv === 'crit' || lv === 'warn' ? 'alert' : 'status');
    t.innerHTML = '<i class="g g-' + lv + '" aria-hidden="true"></i><div></div>'; t.lastChild.textContent = d.text;
    box.appendChild(t);
    if (lv !== 'crit') setTimeout(function () { if (t.parentNode) t.parentNode.removeChild(t); }, lv === 'warn' ? 8000 : 5000);
  });

  /* ---- in-flight refresh (M2-W2C) ----
     An element marked data-fast-poll is waiting on something in flight (a queued command). It is
     refreshed every 5 s, the console's minimum refresh interval, and only while the tab is visible.
     The server stops rendering the attribute once nothing is in flight, so the polling stops itself. */
  setInterval(function () {
    if (document.hidden || !window.htmx) return;
    $$('[data-fast-poll]').forEach(function (el) { window.htmx.trigger(el, 'cryo:poll'); });
  }, 5000);

  applyGroups(); syncTheme(); setDrawer(false);
})();
