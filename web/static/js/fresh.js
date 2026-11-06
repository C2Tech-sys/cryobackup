/* fresh.js - first-party behaviour for the console shell (components 0.7, 1.5, 1.9, 9.1).
   Owns: the freshness stamp and the stale rule, the SSE stream and its polling
   fallback, the C10 banner, the theme switch, the drawer, collapsible customer
   groups, tooltip dismissal and toasts. No third-party code, no network request
   except same-origin /ui/events and the htmx partials.

   The stale rule: if no event and no successful poll arrived within 2 x the
   refresh interval, or the stream errored, the page says so (pill word "Polling" with
   "last hh:mm:ss", or "No data" with "since hh:mm:ss"; banner C10; "as of" markers on every
   live component). A page never
   freezes looking fresh.

   A stream error alone is not an outage (M2-FIX-C, F13): the browser fires one when the page
   navigates away, and a dropped connection often comes back within seconds. The stream must
   stay down longer than DOWN_GRACE (two ping intervals; the server pings every 15 s) before the
   page says "Live updates disconnected" / "No data", and an error caused by leaving the page
   never counts. The grace governs while the stream is down whatever the refresh setting (the
   2 x refresh rule never fires faster than two pings either). After the grace, a successful poll since the
   drop means the data is current: the pill reads "Polling" (never "Live"), with no NO DATA and no banner;
   with no poll, the page shows NO DATA and the banner. A stream event or reconnect clears the clock, and the
   warning clears itself on recovery. */
(function () {
  'use strict';
  var body = document.body;
  var refresh = Math.max(5, parseInt(body.getAttribute('data-refresh') || '30', 10));
  var eventsURL = body.getAttribute('data-events') || '';
  var zone = (document.getElementById('freshness') || body).getAttribute('data-zone') || undefined;
  var PING = 15000, DOWN_GRACE = 2 * PING;
  var last = Date.now(), live = false, forced = false, dropped = false, stale = false, staleSince = '', es = null, retry = null;
  var tipOpen = null; /* the .tipwrap whose tip is open (placed); null when none */
  var downSince = 0, unloading = false; /* downSince: when the stream first errored and has not been heard from since (0 = up) */
  var pollAt = 0, polling = false; /* pollAt: last successful poll; polling: stream down, but polls prove the data is current */
  var STALE_AFTER = Math.max(2 * refresh * 1000, DOWN_GRACE); /* the stream's heartbeat is a ping every 15 s: never judge a quiet stream faster than two of them, whatever the refresh setting */

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
    if (stale || polling) paint(curState); else curState = s.getAttribute('data-state') || 'fresh';
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
    if (on === stale) {
      if (on) {
        if (why === 'drop' && curState !== 'unreachable') { setStamp('unreachable'); } /* already stale for another reason: a real outage reads NO DATA */
        renderC10(); markLive();
      }
      return;
    }
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
  /* pollSoon: a poll asked for by a stream event (drop, reopen). At most one per POLL_MIN, so a flapping stream cannot
     turn into a request storm; the refresh timer and recovery after a real outage still poll as before. */
  var RETRY_MIN = 3000, FLAP_MAX = 30000, STABLE_UP = 30000, POLL_MIN = 15000;
  var flaps = 0, upSince = 0, lastPollTry = -POLL_MIN;
  function pollSoon() {
    var n = Date.now();
    if (n - lastPollTry < POLL_MIN) return;
    lastPollTry = n; poll();
  }
  /* Polling state: the stream is down but successful polls keep the data current. The pill must not claim
     "Live" (it says "Polling" with the time of the last data) and no NO DATA stamp or banner is raised. */
  function setPolling(on) {
    if (on === polling) return;
    polling = on;
    if (on) paint('stale'); else if (!stale) setStamp('fresh');
  }
  /* mark: the stream spoke (event, ping, open). markPoll: a poll succeeded; that proves the data is current
     but says nothing about the stream, so it does not clear the stream's down clock. */
  function mark() {
    last = Date.now(); downSince = 0; setPolling(false);
    if (stale && !forced && !dropped) { poll(); setStale(false); }
  }
  function markPoll() {
    last = pollAt = Date.now();
    if (stale && !forced && !dropped) { poll(); setStale(false); }
  }
  setInterval(function () {
    if (!stamp()) return;
    if (forced || dropped) { setStale(true, dropped ? 'drop' : 'age'); return; }
    var now = Date.now();
    if (downSince && !unloading) {
      /* While the stream is down the grace governs, whatever the refresh interval: nothing is said until it has
         been down DOWN_GRACE. After that, a recent successful poll means "Polling"; none means NO DATA. */
      if (now - downSince <= DOWN_GRACE) return;
      if (pollAt > downSince && now - pollAt <= STALE_AFTER) { if (stale) setStale(false); setPolling(true); return; }
      setPolling(false); setStale(true, 'drop'); return;
    }
    if (now - last > STALE_AFTER) setStale(true, 'age');
  }, 1000);
  setInterval(function () { if (!live && !forced) poll(); }, refresh * 1000);
  document.addEventListener('htmx:afterRequest', function (e) {
    var el = e.detail && e.detail.elt;
    if (el && el.hasAttribute && el.hasAttribute('data-poll') && e.detail.successful) markPoll();
  });
  document.addEventListener('htmx:afterSwap', function () { syncStamp(); applyGroups(); renderC10(); markLive(); if (tipOpen && tipOpen.isConnected) placeTip(tipOpen); /* the explain tip changed size */ });

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
      if (!stale && !polling) setStamp(j.n < j.m ? 'partial' : 'fresh'); else paint(curState);
    } catch (e) { /* ignore a malformed stamp */ }
  }
  function connect() {
    if (!eventsURL || !window.EventSource || dropped) return;
    if (es) { es.close(); }
    es = new EventSource(eventsURL);
    es.addEventListener('open', function () { live = true; upSince = last = Date.now(); downSince = 0; setPolling(false); if (stale && !forced) { pollSoon(); setStale(false); } });
    ['fleet.state', 'run.progress', 'banner.update'].forEach(function (n) { es.addEventListener(n, function (e) { mark(); swap(e.data); }); });
    es.addEventListener('freshness', function (e) { mark(); onFresh(e.data); });
    es.addEventListener('ping', function () { mark(); });
    es.addEventListener('error', function () {
      if (unloading) return; /* the browser errors every open stream when the page is left */
      var now = Date.now();
      live = false; if (!downSince) { downSince = now; pollSoon(); } /* ask once now: a poll that works means the console is up and only the stream broke */
      /* The page owns the reconnect. A stream that never opens (the console is down) or that dropped once after a good run is retried
         after RETRY_MIN, as the browser itself did, so a blip or a restart recovers in about that time and NO DATA clears at once.
         Only a FLAPPING stream (it opened and dropped again inside STABLE_UP, over and over) is backed off, 3 s, 6 s, 12 s up to
         FLAP_MAX, and a poll asked for by a drop is limited to one per POLL_MIN: that was about 20 requests a minute. */
      if (upSince && now - upSince < STABLE_UP) flaps++; else flaps = 0;
      upSince = 0;
      if (es && !dropped) { es.close(); clearTimeout(retry); retry = setTimeout(connect, flaps > 1 ? Math.min(RETRY_MIN * Math.pow(2, flaps - 1), FLAP_MAX) : RETRY_MIN); }
    });
  }
  window.cryoFresh = {
    tipPlace: function () { return tipPlace.apply(null, arguments); },
    live: function () { return live; },
    isStale: function () { return stale; },
    forceStale: function (on) { forced = !!on; if (!on) { last = Date.now(); setStale(false); } else setStale(true, 'age'); },
    forceDrop: function (on) {
      dropped = !!on;
      if (dropped) { if (es) es.close(); live = false; setStale(true, 'drop'); }
      else { connect(); last = Date.now(); }
    }
  };
  /* Leaving the page: stop the stream quietly. A page restored from the back/forward cache starts it again. */
  window.addEventListener('pagehide', function () { unloading = true; clearTimeout(retry); if (es) es.close(); });
  /* No beforeunload hook: a download response fires it and the page stays, which would silence real outages. */
  window.addEventListener('pageshow', function (e) { if (e && e.persisted) { unloading = false; downSince = 0; last = Date.now(); setPolling(false); connect(); } });
  syncStamp();
  connect();

  /* ---- collapsible customer groups ---- */
  var groupState = {};
  try { groupState = JSON.parse(sessionStorage.getItem('cryo.groups') || '{}'); } catch (e) { groupState = {}; }
  /* While a board filter is active (#board[data-filtered]) every matching group is open, on load and after each live update,
     whatever was collapsed before: the rows on screen are then the rows the "Showing N of M" line counts (M3-T5, M2). */
  function filtered() { var b = document.getElementById('board'); return !!(b && b.getAttribute('data-filtered') === 'true'); }
  function applyGroups() {
    var flt = filtered();
    $$('tbody.grp').forEach(function (tb) {
      var id = tb.id;
      var want = flt ? false : (Object.prototype.hasOwnProperty.call(groupState, id) ? groupState[id] : tb.getAttribute('data-default-collapsed') === 'true');
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
      tb.setAttribute('data-collapsed', collapsed ? 'true' : 'false'); chev.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
      if (!filtered()) { /* a collapse under a filter lasts until the next update and is not remembered for the unfiltered board */
        groupState[tb.id] = collapsed;
        try { sessionStorage.setItem('cryo.groups', JSON.stringify(groupState)); } catch (err) { /* ignore */ }
      }
      return;
    }
    var cl = t.closest && t.closest('[data-close]'); /* a "Close" button inside a <details> popover (the phone saved-views sheet) */
    if (cl) { var dt = cl.closest('details'); if (dt) { dt.removeAttribute('open'); var sm = dt.querySelector('summary'); if (sm) sm.focus(); } return; }
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
    var ov = document.querySelector && document.querySelector('details.viewmenu[open]');
    if (ov) { ov.removeAttribute('open'); var os = ov.querySelector('summary'); if (os) os.focus(); return; }
    if (body.getAttribute('data-drawer') === 'open') { setDrawer(false); var mb = $('.menu-btn'); if (mb) mb.focus(); return; }
    var tw = document.activeElement && document.activeElement.closest && document.activeElement.closest('.tipwrap');
    if (tw) { tw.setAttribute('data-tip-off', ''); if (tipOpen === tw) tipOpen = null; }
  });
  function tipClosed(tw) { tw.removeAttribute('data-tip-off'); if (tipOpen === tw) tipOpen = null; } /* a closed tip is not tracked: scrolling costs nothing again */
  document.addEventListener('mouseout', function (e) { var tw = e.target.closest && e.target.closest('.tipwrap'); if (tw) tipClosed(tw); });
  document.addEventListener('focusout', function (e) { var tw = e.target.closest && e.target.closest('.tipwrap'); if (tw) tipClosed(tw); });

  /* ---- tooltips above their container (M2-FIX-C, F10) ----
     A .tip is position:absolute inside its .tipwrap, so inside a table's overflow box it was clipped and made
     the box scroll. On hover or focus the tip is placed with position:fixed from the chip's on-screen
     rectangle: fixed boxes are not clipped by an overflow ancestor and add nothing to its scroll size. Without
     script the tip keeps the absolute placement. tipPlace is pure so the simulation can test it. */
  function tipPlace(a, w, h, vw, vh, right) {
    var m = 8, gap = 6;
    var left = right ? a.right - w : a.left;
    left = Math.max(m, Math.min(left, vw - w - m));
    var top = a.bottom + gap;
    if (top + h > vh - m && a.top - gap - h >= m) top = a.top - gap - h; /* no room below: open above */
    return { left: left, top: top };
  }
  function placeTip(tw) {
    var tip = tw && tw.querySelector ? tw.querySelector('.tip') : null; if (!tip) return;
    /* M3-T5: measure the tip as it will be shown. On mouseover it is still display:none (the :hover rule has not applied yet), so
       its width read as 0 and a right-aligned tip was placed from the chip's right edge and ran off to the right. The class
       "right" is on the .tip itself (shared.html "roll"), not on the wrapper. */
    var was = tip.style.display; tip.style.display = 'block';
    var a = tw.getBoundingClientRect(), r = tip.getBoundingClientRect();
    tip.style.display = was;
    var right = tw.classList.contains('right') || !!(tip.classList && tip.classList.contains('right'));
    var p = tipPlace(a, r.width, r.height, document.documentElement.clientWidth, document.documentElement.clientHeight, right);
    tip.style.position = 'fixed'; tip.style.right = 'auto'; tip.style.left = p.left + 'px'; tip.style.top = p.top + 'px';
    tipOpen = tw;
  }
  document.addEventListener('mouseover', function (e) { var tw = e.target.closest && e.target.closest('.tipwrap'); if (tw) placeTip(tw); });
  document.addEventListener('focusin', function (e) { var tw = e.target.closest && e.target.closest('.tipwrap'); if (tw) placeTip(tw); });
  window.addEventListener('scroll', function () { if (tipOpen && tipOpen.isConnected) placeTip(tipOpen); }, true);
  window.addEventListener('resize', function () { if (tipOpen && tipOpen.isConnected) placeTip(tipOpen); });

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
