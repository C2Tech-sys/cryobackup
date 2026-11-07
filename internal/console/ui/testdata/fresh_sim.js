// fresh_sim.js - drives web/static/js/fresh.js against a fake DOM, a fake clock and a fake
// EventSource (M2-FIX-C, F13). Run by TestFixCFreshStreamGrace with: node fresh_sim.js <path to fresh.js>.
// Prints one "ok <name>" line per scenario and exits 1 on the first failure. No dependencies.
'use strict';
const fs = require('fs');
const vm = require('vm');

const src = fs.readFileSync(process.argv[2], 'utf8');

function run(name, steps, opts) {
  opts = opts || {};
  let now = 1000000000000;
  const timers = [];
  let nextId = 1;
  class FakeDate extends Date {
    constructor(...a) { if (a.length) super(...a); else super(now); }
    static now() { return now; }
  }
  const stampAttrs = { 'data-state': 'fresh', 'data-zone': 'UTC' };
  const stampKids = { '.word': { textContent: 'Live' }, '.t': { textContent: '1:00:00 PM', attrs: {}, getAttribute(k) { return this.attrs[k] || null; }, setAttribute(k, v) { this.attrs[k] = v; } } };
  const stampEl = {
    getAttribute: (k) => (k in stampAttrs ? stampAttrs[k] : null),
    setAttribute: (k, v) => { stampAttrs[k] = v; },
    querySelector: (sel) => stampKids[sel] || null,
    querySelectorAll: () => [],
    appendChild() {},
  };
  const bodyAttrs = { 'data-refresh': String(opts.refresh || 30), 'data-events': '/ui/events' };
  const body = {
    getAttribute: (k) => (k in bodyAttrs ? bodyAttrs[k] : null),
    setAttribute: (k, v) => { bodyAttrs[k] = v; },
    appendChild() {},
    closest: () => null,
  };
  // The banner strip: renderC10 puts <li id="c10"> in a <ul> in #banners; c10() reports whether it is there.
  const mkEl = (tag) => {
    const el = { tag, id: '', className: '', kids: [], parentNode: null, attrs: {}, style: {}, classList: { contains: () => false },
      setAttribute(k, v) { this.attrs[k] = v; }, getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
      appendChild(c) { c.parentNode = this; this.kids.push(c); return c; },
      removeChild(c) { this.kids.splice(this.kids.indexOf(c), 1); c.parentNode = null; },
      querySelector(sel) { return sel === '.b-text' ? (this._bt = this._bt || mkEl('div')) : null; }, querySelectorAll: () => [] };
    return el;
  };
  const strip = mkEl('section');
  strip.querySelector = (sel) => (sel === 'ul' ? strip.kids.find((k) => k.tag === 'ul') || null : null);
  const pollEl = { hasAttribute: (k) => k === 'data-poll' };
  const docAttrs = {};
  const docListeners = {};
  const document = {
    body,
    hidden: false,
    documentElement: { clientWidth: 1360, clientHeight: 800, setAttribute: (k, v) => { docAttrs[k] = v; }, removeAttribute: (k) => { delete docAttrs[k]; } },
    getElementById: (id) => {
      if (id === 'freshness') return stampEl;
      if (id === 'banners') return strip;
      if (id === 'c10') { const ul = strip.kids.find((k) => k.tag === 'ul'); return (ul && ul.kids.find((k) => k.id === 'c10')) || null; }
      return null;
    },
    querySelector: () => null,
    querySelectorAll: (sel) => (sel === '[data-poll]' ? [pollEl] : []),
    createElement: (t) => mkEl(t),
    createTextNode: (t) => ({ text: t }),
    addEventListener: (n, f) => { (docListeners[n] = docListeners[n] || []).push(f); },
    activeElement: null,
  };
  body.addEventListener = document.addEventListener;
  const winListeners = {};
  const streams = [];
  class FakeES {
    constructor(url) { this.url = url; this.readyState = 0; this.l = {}; this.closed = false; streams.push(this); }
    addEventListener(n, f) { (this.l[n] = this.l[n] || []).push(f); }
    close() { this.closed = true; this.readyState = 2; }
    emit(n, data) { (this.l[n] || []).forEach((f) => f({ data })); }
  }
  const ctx = {
    document,
    Date: FakeDate,
    Intl,
    JSON,
    Math,
    parseInt,
    console,
    Array,
    Object,
    sessionStorage: { getItem: () => null, setItem() {} },
    EventSource: FakeES,
    setInterval: (f, ms) => { timers.push({ id: nextId, f, every: ms, due: now + ms }); return nextId++; },
    setTimeout: (f, ms) => { timers.push({ id: nextId, f, every: 0, due: now + ms }); return nextId++; },
    clearTimeout: (id) => { const i = timers.findIndex((t) => t.id === id); if (i >= 0) timers.splice(i, 1); },
    clearInterval: (id) => { const i = timers.findIndex((t) => t.id === id); if (i >= 0) timers.splice(i, 1); },
  };
  ctx.window = ctx;
  ctx.location = { href: '' };
  // What htmx 2 really fires for a poll that swaps outerHTML (the board): by then the polled element is out of the
  // page, so the event that reaches the document comes from the nearest ancestor still in it. detail.elt is that
  // ancestor (<main>); only requestConfig.elt is still the polled element.
  const mainEl = { hasAttribute: () => false };
  const firePoll = () => (docListeners['htmx:afterRequest'] || []).forEach((f) => f({ detail: { elt: mainEl, requestConfig: { elt: pollEl }, successful: true } }));
  // htmx stand-in: a poll succeeds (fires htmx:afterRequest) only while sim.pollsOK is set.
  const pollLog = [];
  ctx.htmx = { trigger: () => { pollLog.push(now); if (sim.pollsOK) firePoll(); }, process() {} };
  ctx.cryoTheme = { get: () => 'dark', set() {} };
  ctx.addEventListener = (n, f) => { (winListeners[n] = winListeners[n] || []).push(f); };
  vm.createContext(ctx);
  vm.runInContext(src, ctx, { filename: 'fresh.js' });

  const sim = {
    pollsOK: false,
    polls: () => pollLog.length,
    c10: () => !!ctx.document.getElementById('c10'),
    pollNow: () => firePoll(),
    configRequest: (el) => { const d = { elt: el, headers: {} }; (docListeners['htmx:configRequest'] || []).forEach((f) => f({ detail: d })); return d.headers; },
    pollEl,
    fastPollEl: { hasAttribute: (k) => k === 'data-fast-poll' },
    plainEl: { hasAttribute: () => false },
    location: () => ctx.location.href,
    es: () => streams[streams.length - 1],
    streams: () => streams.length,
    stale: () => ctx.cryoFresh.isStale(),
    tipPlace: (...a) => ctx.cryoFresh.tipPlace(...a),
    word: () => stampKids['.word'].textContent,
    state: () => stampAttrs['data-state'],
    fireWin: (n, ev) => (winListeners[n] || []).forEach((f) => f(ev || {})),
    fireDoc: (n, ev) => (docListeners[n] || []).forEach((f) => f(ev)),
    freeze(ms) { now += ms; }, // a page in the back-forward cache: the clock moves, no timer runs
    advance(ms) { // 100 ms steps keep timer order faithful
      const end = now + ms;
      while (now < end) {
        now += 100;
        for (const t of timers.slice()) {
          if (t.due <= now) {
            if (t.every) t.due += t.every; else timers.splice(timers.indexOf(t), 1);
            t.f();
          }
        }
      }
    },
    fail(msg) { console.error('FAIL ' + name + ': ' + msg); process.exit(1); },
    expect(cond, msg) { if (!cond) this.fail(msg); },
  };
  steps(sim);
  console.log('ok ' + name);
}

const open = (s) => { s.es().readyState = 1; s.es().emit('open'); };
const drop = (s) => { s.es().readyState = 0; s.es().emit('error'); }; // the browser keeps retrying: CONNECTING

run('a healthy stream is live', (s) => {
  open(s);
  s.advance(5000);
  s.es().emit('ping');
  s.expect(!s.stale(), 'stale on a healthy stream');
});

run('an error that reconnects within seconds shows nothing', (s) => {
  open(s);
  s.advance(5000);
  drop(s);
  s.advance(4000);
  s.expect(!s.stale(), 'stale 4 s into a blip');
  open(s);
  s.advance(60000);
  s.es().emit('ping');
  s.expect(!s.stale(), 'stale after the blip recovered');
  s.expect(s.state() === 'fresh', 'stamp is not fresh: ' + s.state());
});

run('an error during navigation is ignored', (s) => {
  open(s);
  s.advance(5000);
  s.fireWin('pagehide', {});
  s.expect(s.es().closed, 'pagehide must close the stream');
  s.es().emit('error');
  s.advance(50000); // past the grace, short of the 2 x refresh age rule
  s.expect(!s.stale(), 'stale because the page was being left');
});

run('a real outage shows after two ping intervals, and only then', (s) => {
  open(s);
  s.advance(5000);
  drop(s);
  s.advance(25000);
  s.expect(!s.stale(), 'stale before two ping intervals (25 s)');
  s.advance(10000); // 35 s down
  s.expect(s.stale(), 'not stale after 35 s down');
  s.expect(s.word() === 'No data' && s.state() === 'unreachable', 'stamp says ' + s.word() + '/' + s.state());
});

run('repeated errors do not restart the clock', (s) => {
  open(s);
  drop(s);
  for (let i = 0; i < 8; i++) { s.advance(4000); drop(s); } // EventSource retry errors every few seconds
  s.expect(s.stale(), 'retry errors kept resetting the down clock');
});

run('recovery clears the warning by itself', (s) => {
  open(s);
  drop(s);
  s.advance(40000);
  s.expect(s.stale(), 'precondition: stale');
  open(s);
  s.expect(!s.stale(), 'still stale after the stream reopened');
  s.advance(10000);
  s.expect(!s.stale() && s.state() === 'fresh', 'did not stay clear: ' + s.state());
});

run('a closed stream is reopened after 3 s', (s) => {
  open(s);
  const n = s.streams();
  s.es().readyState = 2;
  s.es().emit('error');
  s.advance(3500);
  s.expect(s.streams() === n + 1, 'no reconnect attempt');
});

run('a page restored from the back-forward cache reconnects', (s) => {
  open(s);
  s.fireWin('pagehide', {});
  const n = s.streams();
  s.fireWin('pageshow', { persisted: true });
  s.expect(s.streams() === n + 1, 'no new stream after pageshow');
  open(s);
  s.es().emit('error');
  s.advance(35000);
  s.expect(s.stale(), 'a real outage after a restore must still show');
});

// F10: tooltip placement. The chip's rectangle is in viewport pixels; the tip is placed with position:fixed.
run('a tooltip opens below its chip, left-aligned', (s) => {
  const p = s.tipPlace({ left: 100, right: 180, top: 200, bottom: 224 }, 200, 48, 1360, 800, false);
  s.expect(p.left === 100 && p.top === 230, 'got ' + JSON.stringify(p));
});

run('a tooltip near the bottom opens above its chip', (s) => {
  const p = s.tipPlace({ left: 100, right: 180, top: 760, bottom: 784 }, 200, 48, 1360, 800, false);
  s.expect(p.top === 760 - 6 - 48, 'got ' + JSON.stringify(p));
});

run('a tooltip stays inside the viewport at phone width', (s) => {
  const wide = s.tipPlace({ left: 300, right: 380, top: 100, bottom: 124 }, 320, 40, 390, 800, false);
  s.expect(wide.left === 390 - 320 - 8, 'right edge not clamped: ' + JSON.stringify(wide));
  const edge = s.tipPlace({ left: 2, right: 40, top: 100, bottom: 124 }, 320, 40, 390, 800, true);
  s.expect(edge.left === 8, 'left edge not clamped: ' + JSON.stringify(edge));
});

run('a right-aligned tooltip ends at the right edge of its chip', (s) => {
  const p = s.tipPlace({ left: 1000, right: 1060, top: 100, bottom: 120 }, 200, 30, 1360, 800, true);
  s.expect(p.left === 860, 'got ' + JSON.stringify(p));
});

// ---- delta round: the grace holds at short refresh intervals, restores do not flash, polls are honoured ----
run('refresh 5: a 20 s blip shows nothing (the grace beats 2 x refresh)', (s) => {
  open(s);
  drop(s);
  s.advance(20000);
  s.expect(!s.stale() && s.state() === 'fresh', 'warned during a 20 s blip: ' + s.state());
  open(s);
  s.advance(5000);
  s.expect(!s.stale() && s.state() === 'fresh', 'did not stay clear');
}, { refresh: 5 });

run('refresh 10: a 20 s blip shows nothing', (s) => {
  open(s);
  drop(s);
  s.advance(20000);
  s.expect(!s.stale() && s.state() === 'fresh', 'warned during a 20 s blip: ' + s.state());
}, { refresh: 10 });

run('refresh 5: a quiet healthy stream is not stale between pings', (s) => {
  open(s);
  for (let i = 0; i < 6; i++) { s.advance(15000); s.es().emit('ping'); s.expect(!s.stale(), 'stale between pings at refresh 5'); }
}, { refresh: 5 });

run('refresh 5: a real 60 s outage reads NO DATA with the banner, and clears itself', (s) => {
  open(s);
  drop(s);
  s.advance(25000);
  s.expect(!s.stale() && !s.c10(), 'warned before the grace');
  s.advance(35000);
  s.expect(s.stale() && s.c10(), 'no warning after 60 s down');
  s.expect(s.word() === 'No data' && s.state() === 'unreachable', 'pill says ' + s.word() + '/' + s.state());
  open(s);
  s.expect(!s.stale() && !s.c10() && s.state() === 'fresh', 'did not clear on recovery: ' + s.state());
}, { refresh: 5 });

run('a stale reason of "age" is upgraded to NO DATA when the stream is the cause', (s) => {
  open(s);
  s.advance(70000); // no events at all for 70 s: stale by age
  s.expect(s.stale() && s.state() === 'stale', 'precondition: stale by age: ' + s.state());
  drop(s);
  s.advance(40000);
  s.expect(s.state() === 'unreachable' && s.word() === 'No data', 'still reads ' + s.word());
});

run('a restore from the back-forward cache does not flash the warning', (s) => {
  open(s);
  s.advance(5000);
  s.fireWin('pagehide', {});
  s.freeze(200000); // frozen for far longer than 2 x refresh
  s.fireWin('pageshow', { persisted: true });
  s.advance(3000);
  s.expect(!s.stale(), 'flashed the warning after a restore');
  open(s);
  s.advance(5000);
  s.expect(!s.stale() && s.state() === 'fresh', 'not fresh after the new stream opened');
});

run('stream down but polls succeed: no NO DATA, and the pill does not say Live', (s) => {
  s.pollsOK = true;
  open(s);
  s.advance(5000);
  drop(s);
  s.expect(s.polls() >= 1, 'the first error must ask for a poll right away');
  s.advance(20000);
  s.expect(!s.stale() && s.state() === 'fresh', 'inside the grace nothing changes: ' + s.state());
  for (let i = 0; i < 6; i++) { s.advance(10000); s.pollNow(); } // the 30 s poll timer plus manual ones keep it current
  s.expect(!s.stale() && !s.c10(), 'NO DATA or the banner appeared although polls succeed');
  s.expect(s.state() === 'stale' && s.word() === 'Polling', 'pill says ' + s.word() + '/' + s.state() + ', want Polling');
  open(s);
  s.expect(s.state() === 'fresh', 'the pill must return to Live when the stream is back: ' + s.state());
});

run('a refresh the page makes on its own is marked; what the operator does is not', (s) => {
  s.expect(s.configRequest(s.pollEl)['X-Cryo-Background'] === '1', 'a data-poll refresh is not marked');
  s.expect(s.configRequest(s.fastPollEl)['X-Cryo-Background'] === '1', 'a data-fast-poll refresh is not marked');
  s.expect(!('X-Cryo-Background' in s.configRequest(s.plainEl)), 'a request the operator made is marked');
});

run('a session the console ended moves the page to the sign-in page, and the stream stays down', (s) => {
  open(s);
  s.es().emit('session.ended');
  s.expect(s.location() === '/login?ended=1', 'the page stayed: location ' + s.location());
  const n = s.streams();
  s.expect(s.es().closed, 'the stream was left open');
  s.advance(60000);
  s.expect(s.streams() === n, 'the page reopened the stream after its session ended');
});

run('stream down, polls worked once then stopped: NO DATA', (s) => {
  s.pollsOK = true;
  open(s);
  drop(s);
  s.advance(35000);
  s.pollNow();
  s.pollsOK = false;
  s.advance(5000);
  s.expect(s.state() === 'stale', 'precondition: polling');
  s.advance(100000);
  s.expect(s.stale() && s.state() === 'unreachable', 'polls stopped but the page still says ' + s.state());
});

// M3-T5: a flapping stream (up for 2 s, down, up, down...) polled on every drop, about 20 requests a minute.
run('a flapping stream backs off: few polls, few reconnects', (s) => {
  open(s);
  const n0 = s.polls();
  for (let t = 0; t < 300000; t += 1000) { // five minutes
    if (!s.es().closed) { open(s); s.advance(2000); t += 2000; drop(s); }
    s.advance(1000);
  }
  const perMin = (s.polls() - n0) / 5;
  s.expect(perMin <= 4, 'flapping made ' + perMin + ' polls a minute (limit 4)');
  s.expect(s.streams() <= 14, 'flapping reopened the stream ' + s.streams() + ' times in five minutes');
  // the delay resets once a stream stays up for 30 s: the next drop reconnects after 3 s again
  while (s.es().closed) s.advance(1000);
  open(s);
  s.advance(40000); s.es().emit('ping');
  const n = s.streams();
  drop(s);
  s.advance(3500);
  s.expect(s.streams() === n + 1, 'a stable stream did not reset the reconnect delay');
});

// M3-T5 delta (L1): backing off the polling and the flapping must not slow recovery. A console that was down for
// minutes came back and the page took up to a minute to notice (the retry delay had grown to its cap).
run('a dead stream is retried every 3 s, reaches NO DATA at about 30 s, and recovers on the first retry after the console is back', (s) => {
  open(s);
  s.advance(20000); s.es().emit('ping');
  drop(s);
  const seen = new Set([s.es()]);
  let attempts = 0, t = 0, at25, at35;
  while (t < 240000) { // four minutes with nothing listening: every reconnect attempt fails at once
    s.advance(100); t += 100;
    const cur = s.es();
    if (!seen.has(cur)) { seen.add(cur); attempts++; cur.readyState = 0; cur.emit('error'); }
    if (t === 25000) at25 = s.stale();
    if (t === 35000) at35 = s.stale();
  }
  s.expect(at25 === false, 'NO DATA before the 30 s grace');
  s.expect(at35 === true, 'no NO DATA at 35 s of a dead stream');
  s.expect(attempts >= 70, 'only ' + attempts + ' reconnect attempts in four minutes of a dead console (a retry every 3 s makes about 80): the delay grew');
  // the console is back: the next attempt comes within one 3 s retry, and the warning clears with it
  const before = s.streams();
  let waited = 0;
  while (s.streams() === before) { s.advance(100); waited += 100; }
  s.expect(waited <= 3100, 'the next attempt came after ' + waited + ' ms');
  open(s);
  s.expect(!s.stale() && s.state() === 'fresh', 'still warning after the console came back: ' + s.state());
});

run('a blip after a good run recovers within one 3 s retry and shows nothing', (s) => {
  open(s);
  s.advance(40000); s.es().emit('ping');
  const n = s.streams();
  drop(s);
  s.advance(3100);
  s.expect(s.streams() === n + 1, 'the first reconnect did not come after 3 s');
  open(s);
  s.advance(1000);
  s.expect(!s.stale() && s.state() === 'fresh', 'a blip showed a warning: ' + s.state());
});

// M3-T5: the device-count tooltip (class "right" on the .tip, hidden until :hover applies) was placed from a 0-wide
// box and from the chip's left edge, so it sat too far right. It is measured as shown and right-aligned to its chip.
run('a right-aligned .tip is measured as shown and ends at its chip', (s) => {
  const tip = { style: { display: '' }, classList: { contains: (c) => c === 'right' },
    getBoundingClientRect() { return { width: tip.style.display === 'block' ? 117 : 0, height: 40 }; } };
  const tw = { isConnected: true, classList: { contains: () => false }, querySelector: () => tip, removeAttribute() {},
    getBoundingClientRect: () => ({ left: 1230, right: 1267, top: 300, bottom: 320 }) };
  s.fireDoc('mouseover', { target: { closest: () => tw } });
  s.expect(tip.style.left === '1150px', 'tip left is ' + tip.style.left + ', want 1150px (its right edge on the chip\'s)');
  s.expect(tip.style.display === '', 'the measuring left display:block behind');
});

run('a closed tooltip is no longer tracked: scrolling does no layout work', (s) => {
  let rects = 0;
  const tip = { style: {}, getBoundingClientRect: () => ({ width: 100, height: 20 }) };
  const tw = { isConnected: true, classList: { contains: () => false }, querySelector: () => tip, removeAttribute() {},
    getBoundingClientRect: () => { rects++; return { left: 10, right: 50, top: 10, bottom: 30 }; } };
  const target = { closest: () => tw };
  s.fireDoc('mouseover', { target });
  s.expect(tip.style.position === 'fixed', 'the open tip is not placed with position:fixed');
  const open = rects;
  s.fireWin('scroll', {});
  s.expect(rects > open, 'an open tip must follow a scroll');
  s.fireDoc('mouseout', { target });
  const closed = rects;
  s.fireWin('scroll', {}); s.fireWin('scroll', {});
  s.expect(rects === closed, 'scrolling still measures a closed tooltip');
});
