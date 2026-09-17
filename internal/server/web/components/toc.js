// Conversation TOC rail for the GoGen web UI (ChatGPT-style): one dot
// per user prompt between the sidebar and the chat. Dots mirror the
// transcript: every user bubble is created by appendMessageAtTime, so
// a dot is appended there (O(1) per prompt) and the rail is rebuilt
// from scratch on clearChat (pane switch, /new, compaction re-fetch,
// rewind). The active dot follows the prompt currently in view;
// clicking a dot jumps to its prompt; hovering shows a preview
// tooltip (fixed-position so the rail's overflow never clips it). On
// mobile the rail is hidden by CSS.
//
// Wiring: app.js calls initToc(deps) once at startup.
//   deps.getMessages()           — the #messages element
//   deps.getDistanceFromBottom() — px between the chat bottom and the
//                                  viewport bottom
//   deps.isPinned()              — stick-to-bottom state
//   deps.getNearBottomPx()       — the re-pin threshold
//   deps.isReplaying()           — history replay in progress
//   deps.unpin()                 — unpin from the bottom (dot click and
//                                  wheel-up at the rail's top boundary)

let deps = null;

let tocRail = null;
let tocDots = null;
let tocTooltip = null;
let tocTooltipLabel = null;
let tocTooltipBody = null;

let lastTocActive = -1;
// Cache of the .message.user elements in document order. Dots are 1:1
// with user messages (appendTocDot/rebuildToc), so the list is
// refreshed lazily when its length no longer matches the dot count —
// updateTocActive runs on every pin/repin/scroll frame and must not
// pay for a querySelectorAll (or a forced layout) each time.
let cachedUserMsgs = null;
let tocHideTimer = null;
const TOC_TOOLTIP_HIDE_MS = 120; // debounce: sliding across dots must not flicker
const TOC_PREVIEW_MAX_CHARS = 300;

export function appendTocDot(msgDiv) {
    if (!tocRail || !tocDots || !msgDiv) return;
    const i = tocDots.children.length;
    const dot = document.createElement('button');
    dot.type = 'button';
    dot.className = 'toc-dot';
    dot.dataset.tocItemIndex = String(i);
    dot.setAttribute('aria-label', `Prompt ${i + 1}`);
    dot.addEventListener('mouseenter', () => showTocTooltip(dot, msgDiv, i));
    dot.addEventListener('mouseleave', scheduleHideTocTooltip);
    dot.addEventListener('focus', () => showTocTooltip(dot, msgDiv, i));
    dot.addEventListener('blur', scheduleHideTocTooltip);
    dot.addEventListener('click', () => {
        deps.unpin();
        hideTocTooltip();
        msgDiv.scrollIntoView({ behavior: 'smooth', block: 'start' });
    });
    tocDots.appendChild(dot);
    tocRail.classList.add('has-dots');
    if (!deps.isReplaying()) updateTocActive();
}

export function rebuildToc() {
    if (!tocRail || !tocDots) return;
    tocDots.innerHTML = '';
    lastTocActive = -1;
    // clearChat re-creates the message elements, so the cached list is
    // stale even when its length still matches the dot count.
    cachedUserMsgs = null;
    const messagesDiv = deps.getMessages();
    for (const m of messagesDiv.querySelectorAll('.message.user')) appendTocDot(m);
    const hasDots = tocDots.children.length > 0;
    tocRail.classList.toggle('has-dots', hasDots);
    if (!hasDots) hideTocRail();
    updateTocActive();
}

// The cached .message.user list, refreshed lazily: dots are 1:1 with
// user messages, so a length mismatch with the dot column means the
// transcript changed (new prompt appended or rail rebuilt).
function getUserMsgs(messagesDiv) {
    const dotCount = tocDots ? tocDots.children.length : 0;
    if (!cachedUserMsgs || cachedUserMsgs.length !== dotCount) {
        cachedUserMsgs = Array.from(messagesDiv.querySelectorAll('.message.user'));
    }
    return cachedUserMsgs;
}

// Highlight the dot of the prompt currently in view: the last user
// message whose top is above the upper-third line of the chat
// viewport; while pinned at the bottom (or within the re-pin
// threshold) the last prompt wins. Also keeps the active dot visible
// inside the scrollable rail (minimal scroll, only when the active
// dot changes, so streaming never churns the rail).
export function updateTocActive(d) {
    const messagesDiv = deps.getMessages();
    const dots = tocDots ? tocDots.children : null;
    if (!dots || !dots.length) return;
    if (deps.isPinned() && d === undefined) {
        // Pinned at the bottom: the active dot is unconditionally the last
        // prompt, and dots are 1:1 with the user messages — the dot count
        // answers without getUserMsgs (a transcript-wide querySelectorAll
        // that a history rebuild would otherwise pay once per chunk) and
        // without getDistanceFromBottom (a forced layout).
        const active = dots.length - 1;
        if (active !== lastTocActive) setActiveDot(dots, active);
        return;
    }
    const users = getUserMsgs(messagesDiv);
    if (!users.length) return;
    if (d === undefined) d = deps.getDistanceFromBottom();
    let active = users.length - 1;
    if (!deps.isPinned() && d > deps.getNearBottomPx()) {
        const viewTop = messagesDiv.getBoundingClientRect().top;
        const probe = viewTop + messagesDiv.clientHeight * 0.35;
        active = 0;
        // Messages are in document order: the first one below the
        // probe line means everything after it is too — stop early.
        for (let i = 0; i < users.length; i++) {
            if (users[i].getBoundingClientRect().top <= probe) active = i;
            else break;
        }
    }
    // Nothing changed — skip the class pass and rail auto-scroll so
    // streaming never churns the DOM (active stays put while pinned).
    if (active === lastTocActive) return;
    setActiveDot(dots, active);
}

// Move the active dot: only the PREVIOUS and the NEW dot change. The old
// pass re-toggled every dot on every call, which made a growing session
// O(prompts) per pin — and pins are frequent (the history rebuild pins once
// per chunk, streaming pins on every colorize/resize event). When the target
// index is past the rail's dots (dots lag the messages), the previous dot is
// still cleared and lastTocActive is left alone, exactly like the old pass.
function setActiveDot(dots, active) {
    const prev = lastTocActive;
    if (prev >= 0 && prev < dots.length && prev !== active) {
        dots[prev].classList.remove('active');
        dots[prev].removeAttribute('data-toc-active');
    }
    if (active < 0 || active >= dots.length) return;
    dots[active].classList.add('active');
    dots[active].setAttribute('data-toc-active', '');
    lastTocActive = active;
    // offsetTop is relative to the nearest positioned ancestor; dot and
    // column share it, so the difference is the dot's offset within the
    // column. Minimal scroll: only when the active dot would fall outside
    // the rail's viewport.
    const dot = dots[active];
    const relTop = dot.offsetTop - tocDots.offsetTop;
    if (relTop < tocDots.scrollTop) {
        tocDots.scrollTop = relTop;
    } else if (relTop + dot.offsetHeight > tocDots.scrollTop + tocDots.clientHeight) {
        tocDots.scrollTop = relTop + dot.offsetHeight - tocDots.clientHeight;
    }
}

function showTocTooltip(dot, msgDiv, i) {
    if (!tocTooltip) return;
    clearTimeout(tocHideTimer);
    tocHideTimer = null;
    const raw = (msgDiv && msgDiv.dataset.rawContent) || '';
    tocTooltipLabel.textContent = `Prompt ${i + 1}`;
    tocTooltipBody.textContent = raw.length > TOC_PREVIEW_MAX_CHARS
        ? raw.slice(0, TOC_PREVIEW_MAX_CHARS) + '…'
        : (raw || '(image attachment)');
    tocTooltip.style.display = 'block';
    // Position after un-hiding so offsetWidth/offsetHeight measure
    // the real size. Fixed coordinates from the dot's viewport rect
    // — immune to the rail's overflow clipping. The rail sits on
    // the right edge of the chat pane, so the preview opens to the
    // LEFT of the dot and flips to the right when the left side has
    // no room (clamped to the viewport in both directions).
    const r = dot.getBoundingClientRect();
    const w = tocTooltip.offsetWidth;
    const h = tocTooltip.offsetHeight;
    let left = r.left - w - 8;
    if (left < 8) left = r.right + 8;
    left = Math.max(8, Math.min(left, window.innerWidth - w - 8));
    const top = Math.max(8, Math.min(r.top + r.height / 2 - h / 2, window.innerHeight - h - 8));
    tocTooltip.style.left = left + 'px';
    tocTooltip.style.top = top + 'px';
}

export function hideTocTooltip() {
    clearTimeout(tocHideTimer);
    tocHideTimer = null;
    if (tocTooltip) tocTooltip.style.display = 'none';
}

function scheduleHideTocTooltip() {
    if (!tocTooltip) return;
    clearTimeout(tocHideTimer);
    tocHideTimer = setTimeout(hideTocTooltip, TOC_TOOLTIP_HIDE_MS);
}

// The rail overlays the chat, so it must stay aligned with the
// #messages box: it must never cover the composer toolbar, the
// input row, or the scroll-to-bottom button (whose positions flex
// with the terminal strip and window size). Re-synced on every
// #messages box change (ResizeObserver) and at init.
export function syncTocRailBox() {
    const container = tocRail ? tocRail.parentElement : null;
    if (!container || !tocDots) return;
    const messagesDiv = deps.getMessages();
    const cRect = container.getBoundingClientRect();
    const mRect = messagesDiv.getBoundingClientRect();
    // Keep the strip clear of the #messages scrollbar. Classic
    // scrollbars take layout space (offsetWidth - clientWidth is the
    // scrollbar width); overlay-scrollbar platforms (macOS default,
    // etc.) report 0 but still float a scrollbar over the right
    // edge. Reserve a minimum 16px gutter either way so the rail
    // (and its hover zone) can never cover the scrollbar strip and
    // swallow its clicks.
    const gutter = Math.max(16, messagesDiv.offsetWidth - messagesDiv.clientWidth);
    tocRail.style.top = (mRect.top - cRect.top) + 'px';
    tocRail.style.height = mRect.height + 'px';
    tocRail.style.right = gutter + 'px';
    // Viewport-space hover zone (clientX/clientY are viewport
    // absolute, so no container math needed here). The trigger is a
    // small box around the dot column, NOT the full-height right
    // strip: the strip would reveal the rail while the pointer is
    // over the bottom-right resend/edit buttons on user messages.
    // The dots stay laid out while hidden (visibility:hidden), so
    // their rect is measurable at any time.
    const dRect = tocDots.getBoundingClientRect();
    tocZone = {
        left: dRect.left - TOC_ZONE_MARGIN,
        right: dRect.right + TOC_ZONE_MARGIN,
        top: dRect.top - TOC_ZONE_MARGIN,
        bottom: dRect.bottom + TOC_ZONE_MARGIN,
    };
}

let tocRailHideTimer = null;
let tocZone = null;
const TOC_RAIL_HIDE_MS = 150; // debounce: leaving the dot area hides the rail
const TOC_ZONE_MARGIN = 8; // padding around the dot column for the hover trigger

function showTocRail() {
    if (!tocRail.classList.contains('has-dots')) return;
    clearTimeout(tocRailHideTimer);
    tocRailHideTimer = null;
    tocRail.classList.add('toc-hover');
}

function hideTocRail() {
    clearTimeout(tocRailHideTimer);
    tocRailHideTimer = null;
    tocRail.classList.remove('toc-hover');
    // The pointer left the strip — its fixed-position preview would
    // point at nothing.
    hideTocTooltip();
}

function scheduleHideTocRail() {
    if (!tocRail) return;
    clearTimeout(tocRailHideTimer);
    tocRailHideTimer = setTimeout(hideTocRail, TOC_RAIL_HIDE_MS);
}

export function initToc(d) {
    deps = d;
    tocRail = document.getElementById('toc-rail');
    tocDots = document.getElementById('toc-dots');
    tocTooltip = document.getElementById('toc-tooltip');
    tocTooltipLabel = document.getElementById('toc-tooltip-label');
    tocTooltipBody = document.getElementById('toc-tooltip-body');
    if (!tocRail || !tocDots || !tocTooltip) return;
    const container = tocRail.parentElement;
    // Wheel over the rail scrolls the dot list; at the boundaries
    // the wheel is chained to the chat below (manual: the rail is
    // an overlay, not a sibling, so native chaining would not reach
    // #messages).
    tocRail.addEventListener('wheel', (e) => {
        const overflows = tocDots.scrollHeight > tocDots.clientHeight;
        if (!overflows) return;
        const delta = e.deltaMode === 1 ? e.deltaY * 16
            : e.deltaMode === 2 ? e.deltaY * tocDots.clientHeight
            : e.deltaY;
        if ((delta < 0 && tocDots.scrollTop <= 0) ||
            (delta > 0 && tocDots.scrollTop + tocDots.clientHeight >= tocDots.scrollHeight - 1)) {
            // At a boundary: chain the wheel to the chat below (the
            // rail overlays #messages now) instead of swallowing it.
            // Wheel-up also leaves the bottom, matching the chat's
            // own unpin behavior.
            if (delta < 0) deps.unpin();
            const messagesDiv = deps.getMessages();
            messagesDiv.scrollTop += delta;
            return;
        }
        e.preventDefault();
        tocDots.scrollTop += delta;
    }, { passive: false });
    // Hover reveal via coordinates, NOT pointer events: the rail is
    // always pointer-events:none (only the dot column captures
    // events while shown), so a mouseenter on the rail itself can
    // never fire. Watch mousemove over the chat container instead:
    // the pointer is in the small box around the dots → show;
    // elsewhere → schedule hide. The dot column's own mouseleave
    // covers the pointer leaving the window.
    if (container) {
        container.addEventListener('mousemove', (e) => {
            if (!tocRail.classList.contains('has-dots') || !tocZone) return;
            const inZone = e.clientX >= tocZone.left && e.clientX < tocZone.right
                && e.clientY >= tocZone.top && e.clientY < tocZone.bottom;
            if (inZone) showTocRail();
            else scheduleHideTocRail();
        }, { passive: true });
    }
    // The rail is pointer-events:none, so these listeners live on
    // the dot column (the only part that captures events).
    tocDots.addEventListener('mouseleave', scheduleHideTocRail);
    // Keyboard: Tab-focusing a dot (visibility:hidden removes dots
    // from the tab order, so this only fires while visible) keeps
    // the rail shown while focus is inside it.
    tocDots.addEventListener('focusin', showTocRail);
    tocDots.addEventListener('focusout', scheduleHideTocRail);
    // The hovered dot moves when the list scrolls; a fixed tooltip
    // anchored to the old spot would mislead.
    tocDots.addEventListener('scroll', () => hideTocTooltip());
    window.addEventListener('resize', () => hideTocTooltip());
    syncTocRailBox();
}
