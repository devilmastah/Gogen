// Standalone verification of the colorize scheduler in editor.js (copied
// verbatim below — keep in sync):
//
//   - the replay deferral: jobs queue while active, initMonaco's gate waits,
//     and ending the last deferral drains the queue in idle slices,
//   - the viewport gate: a job parks until its element intersects (a shared
//     IntersectionObserver), so offscreen code is never tokenized,
//   - a detached element (cached pane) re-parks instead of running,
//   - releaseColorizeTargets drops the parked work (chat DOM discarded),
//   - the park cap bounds the retained set, and a DETACHED element at the cap
//     is parked rather than bounced through the queue forever,
//   - a stale deferral token (a replay the watchdog already ended) cannot end
//     a newer replay's deferral,
//   - without IntersectionObserver the fallback runs jobs immediately.
//
// IntersectionObserver is a fake whose intersections the checks trigger
// manually, so parking is deterministic.
//
// Run: node scripts/test_colorize_schedule.js
'use strict';

// ── Verbatim copies from editor.js (keep in sync) ──
const COLORIZE_MARGIN_PX = 800;
const COLORIZE_SLICE_MS = 12;
const COLORIZE_PARK_MAX = 400;

let colorizeDeferralTokens = [];
let colorizeDeferralSeq = 0;
let colorizeDeferralWaiters = [];
let colorizeJobs = [];
let colorizeDrainScheduled = false;
const colorizeParked = new Map();
let colorizeObserver = null;

function scheduleColorizeIdle(fn, timeout) {
  if (typeof requestIdleCallback === 'function') requestIdleCallback(fn, { timeout });
  else setTimeout(fn, 0);
}

function colorizeDeferralGate() {
  if (!colorizeDeferralTokens.length) return null;
  return new Promise((resolve) => colorizeDeferralWaiters.push(resolve));
}

function beginColorizeDeferral() {
  const token = ++colorizeDeferralSeq;
  colorizeDeferralTokens.push(token);
  return token;
}

function endColorizeDeferral(tokenEnd) {
  if (!colorizeDeferralTokens.length) return;
  if (tokenEnd === undefined) {
    colorizeDeferralTokens.pop();
  } else {
    const i = colorizeDeferralTokens.lastIndexOf(tokenEnd);
    if (i < 0) return;
    colorizeDeferralTokens.splice(i, 1);
  }
  if (colorizeDeferralTokens.length) return;
  const waiters = colorizeDeferralWaiters;
  colorizeDeferralWaiters = [];
  for (const resolve of waiters) resolve();
  drainColorizeJobs();
}

function releaseColorizeTargets() {
  colorizeParked.clear();
  colorizeJobs = [];
  if (colorizeObserver) {
    colorizeObserver.disconnect();
    colorizeObserver = null;
  }
}

function ensureColorizeObserver() {
  if (!colorizeObserver) {
    colorizeObserver = new IntersectionObserver((entries) => {
      let queued = false;
      for (const entry of entries) {
        if (!entry.isIntersecting) continue;
        const jobs = colorizeParked.get(entry.target);
        if (!jobs) continue;
        colorizeParked.delete(entry.target);
        colorizeObserver.unobserve(entry.target);
        for (const job of jobs) colorizeJobs.push({ el: entry.target, job });
        queued = true;
      }
      if (queued) drainColorizeJobs();
    }, { rootMargin: COLORIZE_MARGIN_PX + 'px 0px' });
  }
  return colorizeObserver;
}

function parkColorizeJob(el, job) {
  let jobs = colorizeParked.get(el);
  if (!jobs) {
    jobs = [];
    colorizeParked.set(el, jobs);
    ensureColorizeObserver().observe(el);
  }
  jobs.push(job);
}

function scheduleColorizeJob(el, job) {
  if (el && typeof IntersectionObserver === 'function') {
    if (colorizeParked.size >= COLORIZE_PARK_MAX && !colorizeParked.has(el)) {
      colorizeJobs.push({ el, job });
      drainColorizeJobs();
      return;
    }
    parkColorizeJob(el, job);
    return;
  }
  colorizeJobs.push({ el: el || null, job });
  drainColorizeJobs();
}

function drainColorizeJobs() {
  if (colorizeDrainScheduled || colorizeDeferralTokens.length || !colorizeJobs.length) return;
  colorizeDrainScheduled = true;
  const runSlice = async () => {
    colorizeDrainScheduled = false;
    const deadline = performance.now() + COLORIZE_SLICE_MS;
    while (colorizeJobs.length && performance.now() < deadline) {
      const { el, job } = colorizeJobs.shift();
      if (el && !el.isConnected) {
        if (typeof IntersectionObserver === 'function') parkColorizeJob(el, job);
        continue;
      }
      try {
        await job();
      } catch (_) {
      }
    }
    if (colorizeJobs.length) {
      colorizeDrainScheduled = true;
      scheduleColorizeIdle(runSlice, 100);
    }
  };
  scheduleColorizeIdle(runSlice, 400);
}
// ── End verbatim copies ──

// ── Fake IntersectionObserver ──
const ioInstances = [];
class FakeIO {
  constructor(cb, opts) {
    this.cb = cb;
    this.opts = opts;
    this.targets = new Set();
    ioInstances.push(this);
  }
  observe(el) { this.targets.add(el); }
  unobserve(el) { this.targets.delete(el); }
  disconnect() { this.targets.clear(); }
  trigger(el, isIntersecting) { this.cb([{ target: el, isIntersecting }], this); }
}
global.IntersectionObserver = FakeIO;

// ── Helpers ──
const fakeEl = () => ({ isConnected: true });
const tick = () => new Promise((r) => setTimeout(r, 0));
async function drainUntil(pred, deadlineMs = 2000) {
  const until = Date.now() + deadlineMs;
  while (!pred() && Date.now() < until) await tick();
  return pred();
}
let failures = 0;
function check(name, cond, detail) {
  if (!cond) { failures++; console.log('FAIL:', name, detail || ''); }
  else console.log('ok  :', name);
}
const lastIO = () => ioInstances[ioInstances.length - 1];

(async () => {
  // 1. Park + intersect.
  const el1 = fakeEl();
  let runs1 = 0;
  scheduleColorizeJob(el1, () => { runs1++; });
  await tick();
  check('park: job does not run before intersection', runs1 === 0);
  check('park: element is observed', lastIO().targets.has(el1));
  lastIO().trigger(el1, false);
  await drainUntil(() => false, 30);
  check('park: a non-intersecting entry does not run the job', runs1 === 0);
  lastIO().trigger(el1, true);
  check('park: intersecting entry runs the job', await drainUntil(() => runs1 === 1));
  check('park: element is unobserved after running', !lastIO().targets.has(el1));

  // 2. Deferral queues even after intersection; the last end drains.
  const el2 = fakeEl();
  let runs2 = 0;
  beginColorizeDeferral();
  scheduleColorizeJob(el2, () => { runs2++; });
  lastIO().trigger(el2, true);
  await drainUntil(() => false, 30);
  check('deferral: job stays queued while active', runs2 === 0);
  endColorizeDeferral();
  check('deferral: last end drains the queue', await drainUntil(() => runs2 === 1));

  // 3. initMonaco gate: waits for the deferral, resolves on end.
  beginColorizeDeferral();
  const gate = colorizeDeferralGate();
  check('gate: active deferral yields a gate promise', !!gate);
  let gateResolved = false;
  gate.then(() => { gateResolved = true; });
  await drainUntil(() => false, 30);
  check('gate: does not resolve while the deferral is active', gateResolved === false);
  endColorizeDeferral();
  await drainUntil(() => gateResolved);
  check('gate: resolves when the deferral ends', gateResolved === true);
  check('gate: no gate without a deferral', colorizeDeferralGate() === null);

  // 4. Nested deferrals: only the last end drains.
  const el3 = fakeEl();
  let runs3 = 0;
  beginColorizeDeferral();
  beginColorizeDeferral();
  scheduleColorizeJob(el3, () => { runs3++; });
  lastIO().trigger(el3, true);
  endColorizeDeferral();
  await drainUntil(() => false, 30);
  check('nested: inner end does not drain', runs3 === 0);
  endColorizeDeferral();
  check('nested: outer end drains', await drainUntil(() => runs3 === 1));

  // 4b. Stale token: a replay the watchdog ended must not let its own finally
  // end the deferral a NEWER replay started.
  const tokenA = beginColorizeDeferral(); // "replay A"
  endColorizeDeferral();                  // watchdog fires: pops A
  const tokenB = beginColorizeDeferral(); // "replay B" starts
  endColorizeDeferral(tokenA);            // A's stale finally: must be a no-op
  check('tokens: stale finally leaves the newer deferral active',
    colorizeDeferralTokens.length === 1, 'active=' + colorizeDeferralTokens.length);
  endColorizeDeferral(tokenB);
  check('tokens: the current token releases', colorizeDeferralTokens.length === 0);

  // 5. A detached element (cached pane) re-parks instead of running.
  const el4 = fakeEl();
  let runs4 = 0;
  scheduleColorizeJob(el4, () => { runs4++; });
  lastIO().trigger(el4, true);
  el4.isConnected = false; // detached between intersect and drain
  await drainUntil(() => false, 50);
  check('detached: job does not run on a detached element', runs4 === 0);
  check('detached: job is parked again', lastIO().targets.has(el4));
  el4.isConnected = true;
  lastIO().trigger(el4, true);
  check('detached: runs after the subtree is restored', await drainUntil(() => runs4 === 1));

  // 6. releaseColorizeTargets drops parked work.
  const el5 = fakeEl();
  let runs5 = 0;
  scheduleColorizeJob(el5, () => { runs5++; });
  releaseColorizeTargets();
  await drainUntil(() => false, 30);
  check('release: parked job dropped', runs5 === 0);
  check('release: observer disconnected and dropped', ioInstances[ioInstances.length - 1].targets.size === 0);

  // 7. Park cap bounds the retained set; the overflow runs eagerly instead of
  // being parked (so no block is left permanently plain).
  let capRuns = 0;
  for (let i = 0; i <= COLORIZE_PARK_MAX; i++) {
    scheduleColorizeJob(fakeEl(), () => { capRuns++; });
  }
  const parkedNow = colorizeParked.size;
  const io = lastIO();
  for (const target of [...io.targets]) io.trigger(target, true);
  await drainUntil(() => capRuns >= COLORIZE_PARK_MAX + 1, 5000);
  check('cap: parked set is bounded', parkedNow <= COLORIZE_PARK_MAX, 'parked=' + parkedNow);
  check('cap: overflow ran eagerly, the parked ones run on intersect',
    capRuns === COLORIZE_PARK_MAX + 1, 'runs=' + capRuns);
  releaseColorizeTargets();

  // 7b. Boundary: a job whose element is detached WHILE the park cap is full
  // must be parked — not bounced through queue → drain → detached → overflow
  // → queue forever. The queue must reach quiescence, and the element must be
  // observed again for when its subtree is restored.
  let bounceRuns = 0;
  for (let i = 0; i < COLORIZE_PARK_MAX; i++) scheduleColorizeJob(fakeEl(), () => {});
  const bounceEl = fakeEl();
  scheduleColorizeJob(bounceEl, () => { bounceRuns++; }); // overflow: queued eagerly
  bounceEl.isConnected = false;                           // detached before the drain
  await drainUntil(() => colorizeJobs.length === 0, 500);
  check('cap+detached: the queue quiesces (no requeue loop)',
    colorizeJobs.length === 0, 'queued=' + colorizeJobs.length);
  check('cap+detached: element is parked on the observer', lastIO().targets.has(bounceEl));
  bounceEl.isConnected = true;
  lastIO().trigger(bounceEl, true);
  check('cap+detached: runs once the subtree is restored', await drainUntil(() => bounceRuns === 1));
  releaseColorizeTargets();

  // 8. No IntersectionObserver support: fallback runs immediately.
  delete global.IntersectionObserver;
  const el6 = fakeEl();
  let runs6 = 0;
  scheduleColorizeJob(el6, () => { runs6++; });
  check('fallback: no-observer job runs through the queue', await drainUntil(() => runs6 === 1));
  global.IntersectionObserver = FakeIO;

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
})().catch((err) => { console.error('TEST ERROR:', err); process.exit(2); });
