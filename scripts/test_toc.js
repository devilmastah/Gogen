'use strict';
// Regression check for the conversation TOC rail (one dot per user prompt,
// ChatGPT-style) and its hover preview. Loads the REAL index.html + app.js
// into jsdom (imports stripped, editor/marked/dompurify stubbed, no server
// needed) and asserts:
//   - a dot appears per user message (live append AND clearChat rebuild),
//   - the active dot follows scroll position (with the last dot winning
//     while pinned at the bottom),
//   - the active dot is kept in view inside the scrollable rail,
//   - hovering shows a fixed-position preview tooltip; Esc / mouseleave /
//     scroll hide it,
//   - clicking a dot scrolls to its prompt (unpins from the bottom),
//   - wheel over the rail scrolls the dot list and chains at the boundaries,
//   - the rail is hidden while the transcript has no user messages.
// Run: node scripts/test_toc.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');

// jsdom has no layout: fake the scroll metrics and per-element rects the
// chat scroll machinery reads. Data properties on the instance shadow the
// prototype accessors.
function fake(el, props) {
  for (const [k, v] of Object.entries(props)) {
    Object.defineProperty(el, k, { value: v, configurable: true, writable: true });
  }
  return el;
}

async function main() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  const { document } = window;

  window.matchMedia = window.matchMedia || (() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  window.Notification = { permission: 'denied', requestPermission: () => Promise.resolve('denied') };
  window.navigator.clipboard = { writeText: async () => {} };
  class FakeTerminal {
    constructor() { this._cbs = {}; }
    open() {} write() {} fit() {}
    onData(cb) { this._cbs.data = cb; }
    onResize() { return { dispose() {} }; }
    dispose() {} reset() {} focus() {}
  }
  window.Terminal = FakeTerminal;
  window.FitAddon = class { fit() {} };
  window.WebSocket = class {
    constructor() { this.readyState = 0; }
    static OPEN = 1;
    send() {} close() {}
  };
  // jsdom does not implement scrollIntoView; app.js's dot click calls it.
  window.HTMLElement.prototype.scrollIntoView = function () {};

  installEditorStubs(window);
  // Synchronous string mapper (app.js imports it from /editor.js); the
  // generic stub above would hand back a Promise.
  window.escapeHtml = (s) => String(s == null ? '' : s);
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  const messagesDiv = document.getElementById('messages');
  const tocRail = document.getElementById('toc-rail');
  const tocDots = document.getElementById('toc-dots');
  const tocTooltip = document.getElementById('toc-tooltip');
  const tocTooltipLabel = document.getElementById('toc-tooltip-label');
  const tocTooltipBody = document.getElementById('toc-tooltip-body');

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };
  const raf = () => new Promise((r) => window.requestAnimationFrame(() => r()));
  const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
  const dots = () => Array.from(tocDots.children);
  const appendUser = (text, i) => window.appendMessageAtTime('user', text, new Date(), i);
  // User scroll: set the faked position and dispatch a scroll event; the
  // rAF-throttled handler classifies it and updates the active dot.
  async function scrollMessages(top) {
    // Flush pending smartScroll suppression rAFs first: appendMessageAtTime
    // leaves ignoreScrollEvent=true until its clear-rAF runs, and a scroll
    // dispatched in that window is silently dropped by the handler.
    await raf();
    await sleep(20);
    messagesDiv.scrollTop = top;
    messagesDiv.dispatchEvent(new window.Event('scroll'));
    await raf();
    await sleep(20);
  }

  // ── Phase A: dots mirror user messages ──
  console.log('— Phase A: rail + dots —');
  check('rail hidden with no user messages', !tocRail.classList.contains('has-dots'));
  appendUser('first prompt', 0);
  appendUser('second prompt', 1);
  appendUser('third prompt', 2);
  check('rail visible after first user message', tocRail.classList.contains('has-dots'));
  check('one dot per user message', dots().length === 3);
  check('dot labels are Prompt N', dots().every((d, i) => d.getAttribute('aria-label') === `Prompt ${i + 1}`));
  check('dots carry data-toc-item-index', dots().every((d, i) => d.dataset.tocItemIndex === String(i)));
  check('active dot is the last while pinned at bottom',
    dots()[2].classList.contains('active') && dots()[2].hasAttribute('data-toc-active')
    && !dots()[0].classList.contains('active') && !dots()[1].classList.contains('active'));
  check('rail is not hover-visible initially', !tocRail.classList.contains('toc-hover'));

  // ── Phase A2: hover reveal (overlay above the chat) ──
  console.log('— Phase A2: hover reveal —');
  // Fake the messages box and the dot column so syncTocRailBox can build
  // the hover zone (jsdom has no layout). The reveal is coordinate-driven:
  // the rail is pointer-events:none (so the chat below stays interactive),
  // which means a mouseenter on the rail itself can never fire — the app
  // watches mousemove over the chat container instead. The trigger is a
  // small box around the dots, not the full-height right strip.
  messagesDiv.getBoundingClientRect = () => ({ top: 0, right: 800, bottom: 500, left: 0, height: 500, width: 800 });
  // Dots at x 756..780, y 150..250 → zone (margin 8) x 748..788, y 142..258.
  tocDots.getBoundingClientRect = () => ({ top: 150, left: 756, right: 780, bottom: 250, height: 100, width: 24 });
  window.syncTocRailBox();
  const chatContainer = document.getElementById('chat-container');
  chatContainer.dispatchEvent(new window.MouseEvent('mousemove', { clientX: 770, clientY: 200, bubbles: true }));
  check('hovering near the dots reveals the rail', tocRail.classList.contains('toc-hover'));
  // Regression: the trigger must NOT be the full-height right strip — the
  // resend/edit buttons live at the bottom-right of user messages and the
  // old strip-based zone revealed the rail over them (blocking clicks).
  chatContainer.dispatchEvent(new window.MouseEvent('mousemove', { clientX: 770, clientY: 450, bubbles: true }));
  await sleep(200); // > TOC_RAIL_HIDE_MS debounce
  check('hovering the right edge away from the dots does not reveal the rail',
    !tocRail.classList.contains('toc-hover'));
  chatContainer.dispatchEvent(new window.MouseEvent('mousemove', { clientX: 700, clientY: 200, bubbles: true }));
  await sleep(200);
  check('moving away hides the rail again', !tocRail.classList.contains('toc-hover'));
  // Keyboard: focusing a dot reveals the rail and keeps it visible.
  tocDots.dispatchEvent(new window.FocusEvent('focusin'));
  check('keyboard focus on the dots reveals the rail', tocRail.classList.contains('toc-hover'));
  tocDots.dispatchEvent(new window.FocusEvent('focusout'));
  await sleep(200);
  check('focus leaving the dots hides the rail', !tocRail.classList.contains('toc-hover'));
  // The rail must clear the #messages scrollbar: with a classic scrollbar
  // (offsetWidth > clientWidth) the rail shifts left by the scrollbar width
  // and the dots (inside the rail) sit left of the scrollbar, so the hover
  // zone excludes the scrollbar strip and dragging it stays possible.
  fake(messagesDiv, { offsetWidth: 820, clientWidth: 800 });
  // Dots now at x 740..764 (rail shifted 20px) → zone x 732..772.
  tocDots.getBoundingClientRect = () => ({ top: 150, left: 740, right: 764, bottom: 250, height: 100, width: 24 });
  window.syncTocRailBox();
  check('strip shifts left of the messages scrollbar', tocRail.style.right === '20px');
  chatContainer.dispatchEvent(new window.MouseEvent('mousemove', { clientX: 790, clientY: 200, bubbles: true }));
  await sleep(200);
  check('hovering the scrollbar does not reveal the rail', !tocRail.classList.contains('toc-hover'));
  chatContainer.dispatchEvent(new window.MouseEvent('mousemove', { clientX: 750, clientY: 200, bubbles: true }));
  check('hovering just left of the scrollbar reveals the rail', tocRail.classList.contains('toc-hover'));
  chatContainer.dispatchEvent(new window.MouseEvent('mousemove', { clientX: 700, clientY: 200, bubbles: true }));
  await sleep(200);
  check('moving away hides the rail again', !tocRail.classList.contains('toc-hover'));
  // Overlay-scrollbar platforms measure 0 (the scrollbar takes no layout
  // space): the 16px minimum gutter must still keep the rail off the
  // scrollbar strip. Restore the zero-scrollbar state for the phases below.
  fake(messagesDiv, { offsetWidth: 0, clientWidth: 0 });
  window.syncTocRailBox();
  check('minimum gutter reserved on overlay-scrollbar platforms', tocRail.style.right === '16px');

  // ── Phase B: hover preview tooltip ──
  console.log('— Phase B: tooltip —');
  dots()[0].dispatchEvent(new window.MouseEvent('mouseenter'));
  check('tooltip visible on hover', tocTooltip.style.display === 'block');
  check('tooltip label shows prompt number', tocTooltipLabel.textContent === 'Prompt 1');
  check('tooltip body shows the raw prompt', tocTooltipBody.textContent === 'first prompt');
  const bodyRect = { top: 100, left: 40, right: 58, bottom: 100, height: 0, width: 0 };
  dots()[0].getBoundingClientRect = () => bodyRect;
  dots()[0].dispatchEvent(new window.MouseEvent('mouseenter'));
  check('tooltip positioned to the left of the dot (rail on the right)',
    tocTooltip.style.left === String(40 - 8) + 'px' && tocTooltip.style.top === '100px');
  // Near the left edge there is no room to the left → flip to the right.
  const edgeRect = { top: 100, left: 4, right: 22, bottom: 100, height: 0, width: 0 };
  dots()[0].getBoundingClientRect = () => edgeRect;
  dots()[0].dispatchEvent(new window.MouseEvent('mouseenter'));
  check('tooltip flips to the right when the left side has no room',
    tocTooltip.style.left === String(22 + 8) + 'px');
  // Sliding to the next dot must not flicker: the hide timer is cancelled.
  dots()[0].dispatchEvent(new window.MouseEvent('mouseleave'));
  dots()[1].dispatchEvent(new window.MouseEvent('mouseenter'));
  await sleep(150); // > TOC_TOOLTIP_HIDE_MS debounce
  check('tooltip stays open when moving between dots', tocTooltip.style.display === 'block');
  dots()[1].dispatchEvent(new window.MouseEvent('mouseleave'));
  await sleep(150);
  check('tooltip hides after mouseleave debounce', tocTooltip.style.display === 'none');
  dots()[2].dispatchEvent(new window.MouseEvent('mouseenter'));
  document.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
  check('Escape hides the tooltip', tocTooltip.style.display === 'none');
  check('empty prompt preview falls back to image label', (() => {
    appendUser('', 3);
    dots()[3].dispatchEvent(new window.MouseEvent('mouseenter'));
    const ok = tocTooltipBody.textContent === '(image attachment)';
    document.dispatchEvent(new window.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    return ok;
  })());

  // ── Phase C: active dot follows scroll position ──
  console.log('— Phase C: active dot vs scroll —');
  fake(messagesDiv, { scrollHeight: 1000, clientHeight: 500, scrollTop: 0 });
  const users = Array.from(messagesDiv.querySelectorAll('.message.user'));
  users.forEach((el, i) => {
    el.getBoundingClientRect = () => ({ top: 100 + i * 100, left: 0, right: 0, bottom: 0, height: 20, width: 0 });
  });
  // scrollTop=100 → distanceFromBottom = 1000-100-500 = 400 > 32 → unpinned.
  // Probe line = 0 + 500*0.35 = 175 → messages at top 100, 200, 300, 400:
  // only the first is above it → active dot 0.
  await scrollMessages(100);
  check('unpinned scroll highlights the prompt in view', dots()[0].classList.contains('active'));
  check('only one active dot', dots().filter((d) => d.classList.contains('active')).length === 1);
  check('data-toc-active mirrors the active class',
    dots()[0].hasAttribute('data-toc-active') && !dots()[1].hasAttribute('data-toc-active'));
  // Back at the bottom → the last prompt wins.
  // The scroll handler's re-pin branch is gated by the 400ms unpin grace
  // window (UNPIN_REPIN_GRACE_MS) since the last gesture unpin; wait it out.
  await sleep(450);
  await scrollMessages(500);
  check('bottom scroll re-pins and highlights the last prompt', dots()[3].classList.contains('active'));

  // ── Phase D: active dot kept in view inside the scrollable rail ──
  console.log('— Phase D: rail auto-scroll —');
  for (let i = 0; i < 6; i++) appendUser(`prompt ${i + 4}`, i + 4); // 10 dots total
  const allDots = dots();
  allDots.forEach((d, i) => fake(d, { offsetTop: i * 10, offsetHeight: 2 }));
  fake(tocDots, { scrollHeight: 500, clientHeight: 30, scrollTop: 0 });
  const users10 = Array.from(messagesDiv.querySelectorAll('.message.user'));
  users10.forEach((el, i) => {
    el.getBoundingClientRect = () => ({ top: 100 + i * 50, left: 0, right: 0, bottom: 0, height: 20, width: 0 });
  });
  fake(messagesDiv, { scrollHeight: 2000, clientHeight: 500, scrollTop: 800 });
  // viewTop = -25 → probe = -25 + 175 = 150 → only prompts 0 and 1 above it.
  messagesDiv.getBoundingClientRect = () => ({ top: -25, left: 0, right: 0, bottom: 0, height: 500, width: 0 });
  await scrollMessages(800); // d = 2000-800-500 = 700 > 32 → unpinned
  check('active follows the probe line with 10 prompts', dots()[1].classList.contains('active'));
  // Active dot 1 (offsetTop 10) fits the 30px viewport — no auto-scroll yet.
  check('rail not scrolled while the active dot is visible', tocDots.scrollTop === 0);
  // Move the view down: viewTop = 425 → probe = 600 → all 10 prompts above
  // it → active = 9 (offsetTop 90, outside the 30px viewport) → the rail
  // scrolls to reveal the active dot.
  messagesDiv.getBoundingClientRect = () => ({ top: 425, left: 0, right: 0, bottom: 0, height: 500, width: 0 });
  await scrollMessages(800);
  check('active jumps to the in-view prompt', dots()[9].classList.contains('active'));
  check('rail auto-scrolled to keep the active dot visible', tocDots.scrollTop === 90 + 2 - 30);

  // ── Phase E: click jumps to the prompt ──
  console.log('— Phase E: click —');
  let jumpedTo = null;
  const userEls = Array.from(messagesDiv.querySelectorAll('.message.user'));
  userEls[2].scrollIntoView = (opts) => { jumpedTo = opts; };
  dots()[2].dispatchEvent(new window.MouseEvent('mouseenter'));
  dots()[2].click();
  check('click hides the tooltip', tocTooltip.style.display === 'none');
  check('click scrolls to the prompt', jumpedTo && jumpedTo.block === 'start');
  check('click does not throw', true);

  // ── Phase F: wheel scrolls the rail, chains at boundaries ──
  console.log('— Phase F: wheel —');
  fake(tocDots, { scrollHeight: 500, clientHeight: 30, scrollTop: 0 });
  const wheelDown = new window.Event('wheel', { cancelable: true });
  wheelDown.deltaY = 50; wheelDown.deltaMode = 0;
  tocRail.dispatchEvent(wheelDown);
  check('wheel scrolls the dot list', wheelDown.defaultPrevented && tocDots.scrollTop === 50);
  const wheelUpAtTop = new window.Event('wheel', { cancelable: true });
  wheelUpAtTop.deltaY = -50; wheelUpAtTop.deltaMode = 0;
  tocDots.scrollTop = 0;
  tocRail.dispatchEvent(wheelUpAtTop);
  check('wheel at the top boundary is not swallowed', !wheelUpAtTop.defaultPrevented);
  const wheelNoOverflow = new window.Event('wheel', { cancelable: true });
  wheelNoOverflow.deltaY = 50; wheelNoOverflow.deltaMode = 0;
  fake(tocDots, { scrollHeight: 30, clientHeight: 30, scrollTop: 0 });
  tocRail.dispatchEvent(wheelNoOverflow);
  check('wheel is untouched when the dots do not overflow', !wheelNoOverflow.defaultPrevented);

  // ── Phase G: clearChat rebuilds (pane switch / /new / compaction) ──
  console.log('— Phase G: clearChat —');
  window.clearChat();
  check('clearChat empties the rail', dots().length === 0);
  check('clearChat hides the rail', !tocRail.classList.contains('has-dots'));
  appendUser('after clear', 100);
  check('rail rebuilds on the next user message', dots().length === 1 && tocRail.classList.contains('has-dots'));
  check('rebuilt dot is active (pinned after append)', dots()[0].classList.contains('active'));

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
