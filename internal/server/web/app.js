        import {
            connectEditorSocket,
            setupEditorUI,
            refreshExplorer,
            disposeChatEditors,
            mountDiffEditor,
            updateDiffEditor,
            updateDiffFallback,
            chatDiffWheelEdge,
            extractDiffValue,
            scanDiffValueIncremental,
            initMonaco,
            colorizeNode,
            colorizeElement,
            beginColorizeDeferral,
            endColorizeDeferral,
            languageFromPath,
            setToastHandler,
            focusFindInFiles,
            editorUndo,
            editorRedo,
            saveAll,
            saveActive,
            openFileAtLine,
            openModal,
            closeModal,
            escapeHtml,
        } from '/editor.js';
        // Shared UI components (see web/components/): the generic anchored
        // popover shell and the model + reasoning-effort picker rendering
        // used by the toolbar, the settings subagent picker and the board
        // "Start agent" popover.
        import { createPopover } from '/components/popover.js';
        import { icon } from '/components/icons.js';
        // Safe localStorage wrappers (see storage.js): sidebar widths are
        // persisted per target; a storage-blocked browser must not throw.
        import { storageGet, storageSet } from '/components/storage.js';
        import {
            createModelThinkingPicker,
            createThinkingChips,
            formatTokenCount,
            renderModelList,
        } from '/components/model-picker.js';
        import {
            boardTabVisible,
            handleBoardState,
            initBoard,
            refreshBoardStartPicker,
            renderBoard,
            requestBoardState,
        } from '/components/board.js';
        import {
            automationsPaneVisible,
            handleAutomationsRuns,
            handleAutomationsState,
            initAutomations,
            renderAutomations,
            requestAutomationsState,
        } from '/components/automations.js';
        import {
            appendTocDot,
            hideTocTooltip,
            initToc,
            rebuildToc,
            updateTocActive,
        } from '/components/toc.js';
        // Chat auto-scroll / follow system (components/scroll.js): sticky
        // stick-to-bottom state, the jump button, unpin gestures, re-pin
        // machinery and smartScroll. Owns the stickToBottom /
        // ignoreScrollEvent state; app.js reaches it through the exports.
        import {
            disableFollow,
            distanceFromBottom,
            enableFollow,
            initScroll,
            isPinned,
            measureChatAnchor,
            nearBottomPx,
            pinToBottom,
            restoreChatAnchor,
            scheduleRepinIfPinned,
            sidebarDragEnd,
            sidebarDragStart,
            smartScroll,
            unpinFromBottom,
            updateScrollBottomBtn,
        } from '/components/scroll.js';
        // Off-screen paint skip that preserves the real scroll geometry
        // (components/paint-skip.js): measures a finalized transcript item's
        // real height once, then lets content-visibility: auto skip it.
        import { enablePaintSkip } from '/components/paint-skip.js';
        import {
            USER_TERM_ID,
            initTerminal,
            terminalDismissMobile,
            terminalEnsureUserTab,
            terminalExit,
            terminalFitSoon,
            terminalHideMoreMenu,
            terminalInterruptAll,
            terminalOpen,
            terminalToggleMobile,
            terminalTogglePanel,
            terminalUserEnsure,
            terminalUserExited,
            terminalUserOpened,
            terminalWrite,
        } from '/components/terminal.js';
        // Sidebar session list (components/sessions.js): unified open-pane +
        // saved-session rows, the delete confirm modal, resume/export.
        import {
            deleteSession,
            exportChat,
            getLastSessions,
            initSessions,
            refreshSidebarSessions,
            renderSessionList,
            requestSessionList,
            resumeSession,
            setLastSessions,
            updateCurrentSessionLabel,
        } from '/components/sessions.js';
        // Settings modal + all persisted preferences (components/settings.js):
        // tabbed overlay, server-backed feature/runtime config, MCP test
        // flow, providers list, theme, editor preferences, notifications,
        // accent color. The ws dispatch stays here and forwards to the
        // module's apply*Settings functions.
        import {
            applyFeatureSettings,
            applyMCPSettings,
            applyProviderSettings,
            applyRuntimeSettings,
            closeSettings,
            diffViewerMode,
            getNotificationPref,
            getShowReplyModelPref,
            initSettings,
            isSettingsOpen,
            noteMCPOpFailed,
            openSettings,
            sendNotification,
            sendRuntimeConfig,
            setNotificationPref,
            showMCPTest,
            showProviderTest,
        } from '/components/settings.js';
        // Status-display relay (components/status-display.js): in
        // `status_display: browser` mode the server builds the rows and
        // sends them over the chat socket; this browser POSTs them to the
        // display, which may be reachable from the browser's network when
        // the server's is not.
        import {
            applyStatusDisplayConfig,
            handleStatusDisplayFrame,
        } from '/components/status-display.js';
        // Command palette (components/palette.js): the Ctrl+K overlay,
        // fuzzy filtering and keyboard navigation. app.js keeps the
        // command catalog and the global shortcuts, and hands the
        // catalog to initPalette.
        import {
            closePalette,
            initPalette,
            isPaletteOpen,
            openPalette,
        } from '/components/palette.js';
        // Delete-approval queue + confirm modal (components/delete-approval.js):
        // owns the pendingDeleteApprovals queue; app.js keeps the ws dispatch
        // (delete_approval) and forwards payloads to showDeleteApproval.
        import {
            initDeleteApproval,
            showDeleteApproval,
            showCommandApproval,
        } from '/components/delete-approval.js';
        // Composer input helpers (components/composer.js): the slash-command
        // suggest box and the attachment flow (file picker, paste,
        // drag-and-drop). The send path (sendMessage) stays here; app.js
        // reads attachments through getPendingAttachments /
        // clearAttachments, inlines text-file attachments into the outgoing
        // content via composeMessageContent, and routes keydown through
        // slashKeydown.
        import {
            clearAttachments,
            composeMessageContent,
            getPendingAttachments,
            getSlashCommands,
            hideSlashSuggest,
            initComposer,
            slashKeydown,
            updateSlashSuggest,
        } from '/components/composer.js';
        // Markdown rendering pipeline (components/markdown.js): the
        // text→sanitized-HTML renderer, code-block copy buttons, the
        // @path:line linkifier, the single-shot full render and the
        // incremental streaming renderer. The streaming STATE stays here.
        import {
            initMarkdown,
            msgBody,
            setMessageMarkdown,
            renderStreamMarkdown,
        } from '/components/markdown.js';
        // Tool-result rendering (components/tool-result.js): the expandable
        // result body, the deferred-during-replay colorize queue, and the
        // compact tool-args formatters. The tool-card DOM + streaming state
        // stay here.
        import {
            appendExpandableResult,
            bigArgChars,
            flushDeferredResultColorize,
            formatArgsCompact,
            initToolResult,
            readFilePathFromArgs,
        } from '/components/tool-result.js';
        import { marked } from '/vendor/marked.esm.js';

        marked.use({
            gfm: true,
            breaks: true,
            renderer: {
                // Model/user text is not trusted HTML: a quoted <div id="…">
                // must display as the literal text <div id="…"> (escaped in
                // the DOM, decoded on screen and in copy), never become an
                // element — the same policy user messages already get
                // (escapeHtml in appendMessage). Raw HTML that passes
                // through would otherwise render as real elements (and, when
                // unclosed, nest and compound the 0.9em code font-size in
                // reasoning cards). Marked-generated tags (fences, emphasis,
                // links) don't pass through this renderer, so markdown is
                // unaffected. escapeHtml is imported from /editor.js.
                html(token) { return escapeHtml(token.text); },
            },
        });

        const messagesDiv = document.getElementById('messages');
        const inputArea = document.getElementById('message-input');
        const sendBtn = document.getElementById('send-btn');
        const cancelBtn = document.getElementById('cancel-btn');
        const dirInput = document.getElementById('working-dir-input');
        const setDirBtn = document.getElementById('set-dir-btn');
        const workingDirDisplay = document.getElementById('working-dir-display');
        const workingDirPath = document.getElementById('working-dir-path');
        const workingDirConfig = document.getElementById('working-dir-config');
        const connIndicator = document.getElementById('conn-indicator');
        const sessionInfoDiv = document.getElementById('session-info');
        const sessionListDiv = document.getElementById('session-list');
        let currentMode = 'act';
        let isGlobalMode = false;
        const inputProgress = document.getElementById('input-progress');
        // Composer container (#input-wrap): carries .progress-active in
        // lockstep with #input-progress.active so the textarea can hand its
        // reserved strip-row height to the strip without changing the input
        // bar's height (see --progress-row-h in styles.css).
        const inputWrap = document.getElementById('input-wrap');
        const globalModeBadge = document.getElementById('global-mode-badge');
        const toastHost = document.getElementById('toast-host');
        // The conversation TOC rail (one dot per user prompt) and its
        // hover preview tooltip live in components/toc.js (initToc
        // below); the rail is hidden until the first user message
        // renders (.has-dots).
        // Toolbar elements
        const tbModelBtn = document.getElementById('tb-model-btn');
        const tbModelPopover = document.getElementById('tb-model-popover');
        const tbModelList = document.getElementById('tb-model-list');
        const tbModelFilter = document.getElementById('tb-model-filter');
        const tbThinkingGrid = document.getElementById('tb-thinking-grid');
        const tbThinkingSection = document.getElementById('tb-thinking-section');
        const nearCompactBanner = document.getElementById('near-compact-banner');
        const ncbCompactBtn = document.getElementById('ncb-compact-btn');
        const ncbDismissBtn = document.getElementById('ncb-dismiss-btn');
        const condensedBanner = document.getElementById('condensed-banner');
        const condensedBannerText = document.getElementById('condensed-banner-text');
        const condensedBannerDismiss = document.getElementById('condensed-banner-dismiss');
        const tbModeBtn = document.getElementById('tb-mode-btn');
        const tbContextBadge = document.getElementById('tb-context-badge');
        let messageRawStore = new WeakMap();
        // User-attached images per user bubble, keyed like messageRawStore.
        // dataset/rawContent only carries text — edit/resend must re-send
        // the attachments too, and history replay repopulates this from the
        // history entry's images so replayed bubbles stay resendable.
        let messageImageStore = new WeakMap();

        // Bounded toast stack: a burst of events (copy feedback, connection
        // flips) must not pile an unbounded column over the composer. When
        // the cap is hit the oldest toast is removed first.
        const MAX_TOASTS = 8;
        // Errors need more reading time than info pings.
        const TOAST_TTL = { error: 8000, info: 3000, success: 3000 };

        // Dismiss one toast with a short slide-out. `immediate` skips the
        // exit animation and always removes the node (bounded-stack
        // eviction: the loop must be able to shed a node even if it is
        // already mid-exit). Animated dismissal is idempotent — the
        // animationend handler and the fallback timer may both fire.
        function dismissToast(el, immediate) {
            if (!el) return;
            if (immediate) {
                el.dataset.dismissing = '1';
                clearTimeout(el._toastTimer);
                el.remove();
                return;
            }
            if (el.dataset.dismissing) return;
            el.dataset.dismissing = '1';
            clearTimeout(el._toastTimer);
            el.classList.add('toast-out');
            const done = () => el.remove();
            el.addEventListener('animationend', done, { once: true });
            // Fallback in case the animation never fires.
            setTimeout(done, 400);
        }

        // (Re)start a toast's auto-dismiss countdown, remembering the
        // remaining time so a hover/focus pause resumes where it left off.
        function scheduleToastDismiss(el, remaining) {
            clearTimeout(el._toastTimer);
            el._toastRemaining = remaining;
            el._toastDeadline = performance.now() + remaining;
            el._toastTimer = setTimeout(() => dismissToast(el), remaining);
        }

        function showToast(message, kind = 'info') {
            if (!toastHost || !message) return;
            const ttl = TOAST_TTL[kind] || 3000;
            // De-dupe: a repeated identical toast (copy feedback, a burst of
            // connection flips) refreshes the visible one's timer instead of
            // stacking another copy.
            for (const existing of toastHost.children) {
                if (existing.dataset.message === message && existing.dataset.kind === kind
                    && !existing.dataset.dismissing) {
                    scheduleToastDismiss(existing, ttl);
                    return;
                }
            }
            while (toastHost.childElementCount >= MAX_TOASTS) {
                dismissToast(toastHost.firstElementChild, true);
            }
            const el = document.createElement('div');
            el.className = `toast ${kind}`;
            el.dataset.message = message;
            el.dataset.kind = kind;
            const text = document.createElement('span');
            text.className = 'toast-text';
            text.textContent = message;
            el.appendChild(text);
            const close = document.createElement('button');
            close.type = 'button';
            close.className = 'toast-close';
            close.setAttribute('aria-label', 'Dismiss');
            close.innerHTML = icon('x');
            close.addEventListener('click', (e) => { e.stopPropagation(); dismissToast(el); });
            el.appendChild(close);
            // Click anywhere on the toast still dismisses (unchanged affordance).
            el.addEventListener('click', () => dismissToast(el));
            // Pause the countdown while the user is reading or interacting
            // (hover, or keyboard focus inside), so a toast cannot vanish
            // mid-read or mid-tab; resume with the remaining time.
            const pause = () => {
                if (el.dataset.dismissing) return;
                clearTimeout(el._toastTimer);
                el._toastRemaining = Math.max(0, el._toastDeadline - performance.now());
            };
            const resume = () => {
                if (el.dataset.dismissing) return;
                scheduleToastDismiss(el, el._toastRemaining || ttl);
            };
            el.addEventListener('mouseenter', pause);
            el.addEventListener('mouseleave', resume);
            el.addEventListener('focusin', pause);
            el.addEventListener('focusout', resume);
            toastHost.appendChild(el);
            scheduleToastDismiss(el, ttl);
        }
        setToastHandler(showToast);

        // Legacy copy path for non-secure contexts: the Clipboard API is
        // only available in secure contexts (https / localhost), but the UI
        // is also served over plain http on the LAN (the phone QR pairing
        // flow), where navigator.clipboard is undefined. A temporary
        // textarea + execCommand('copy') works there (and in the remaining
        // browsers that lack the async API).
        function copyTextLegacy(text) {
            const ta = document.createElement('textarea');
            ta.value = text;
            ta.setAttribute('readonly', '');
            // Off-screen but rendered: unrendered (display:none) nodes
            // refuse selection in some mobile browsers.
            ta.style.cssText = 'position:fixed;top:0;left:0;opacity:0;';
            document.body.appendChild(ta);
            ta.select();
            ta.setSelectionRange(0, ta.value.length);
            let ok = false;
            try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
            ta.remove();
            return ok;
        }

        // Copy text to the clipboard, preferring the async Clipboard API and
        // falling back to execCommand in non-secure contexts. Resolves true
        // on success so callers can toast the real outcome instead of
        // assuming (an unguarded navigator.clipboard access would throw an
        // uncaught TypeError on plain-HTTP LAN access).
        async function copyTextToClipboard(text) {
            if (navigator.clipboard && window.isSecureContext) {
                try {
                    await navigator.clipboard.writeText(text);
                    return true;
                } catch (_) { /* fall through to the legacy path */ }
            }
            return copyTextLegacy(text);
        }

        function setConnState(state) {
            if (!connIndicator) return;
            connIndicator.classList.remove('connected', 'disconnected', 'reconnecting');
            connIndicator.classList.add(state);
            const label = state === 'connected' ? 'Connected'
                : state === 'reconnecting' ? 'Reconnecting…'
                : 'Disconnected';
            connIndicator.title = label;
            connIndicator.setAttribute('aria-label', label);
        }

        // True when the chat socket is usable; otherwise toasts the standard
        // "Not connected" error and returns false so callers can bail out.
        function ensureConnected() {
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                showToast('Not connected', 'error');
                return false;
            }
            return true;
        }

        document.querySelectorAll('.main-tab').forEach((btn) => {
            btn.addEventListener('click', () => {
                // The terminal pane toggles the mobile full-screen overlay
                // instead of switching panes; every other pane switch (tab
                // click, palette, settings, board/automations links) goes
                // through switchMainPane.
                if (btn.dataset.pane === 'terminal') {
                    terminalToggleMobile();
                    return;
                }
                switchMainPane(btn.dataset.pane);
            });
        });

        setupEditorUI();

        // ── Toolbar: model picker popover ──
        let modelFilterQuery = '';
        // Shared popover shell (components/popover.js): outside-click +
        // Escape dismissal; the stylesheet keeps the absolute placement
        // above the button (fixed: false).
        const tbModelPopoverCtl = createPopover({
            el: tbModelPopover,
            getAnchor: () => tbModelBtn,
        });

        function toggleModelPopover() {
            tbModelPopoverCtl.toggle();
            if (tbModelPopoverCtl.isOpen()) {
                // Start from a clean slate each time the popover opens.
                if (tbModelFilter) tbModelFilter.value = '';
                modelFilterQuery = '';
                renderToolbarModelList(availableModels, availableModels.find((m) => m.current)?.id || '');
            }
        }

        function closeModelPopover() {
            tbModelPopoverCtl.close();
        }

        tbModelFilter?.addEventListener('input', () => {
            modelFilterQuery = tbModelFilter.value.trim().toLowerCase();
            renderToolbarModelList(availableModels, availableModels.find((m) => m.current)?.id || '');
        });

        tbModelBtn.addEventListener('click', (e) => {
            e.stopPropagation();
            if (availableModels.length <= 1) {
                // Fetch models if we don't have a list yet
                modelsRequested = false;
                ensureModelsLoaded();
            }
            toggleModelPopover();
        });

        // The model-list rendering (grouped rows, filter, empty state)
        // lives in components/model-picker.js — shared with the settings
        // subagent picker and the board "Start agent" popover.

        // Canonical key of the selected model ("" = none): the model flagged
        // `current`, else the caller's `current` selector resolved against the
        // catalog (payloads without the per-row flag, and callers that pass a
        // bare id). Both the render sites and the config frame derive the key
        // the same way, so the "did the selection move?" check below is not
        // fooled by the bare-vs-qualified form of the selection.
        function toolbarSelectionKey(models, current) {
            const list = models || [];
            const flagged = list.find((m) => m.current);
            if (flagged) return (flagged.provider || 'default') + '/' + flagged.id;
            if (!current) return '';
            const owner = list.find((m) => {
                const prov = m.provider || 'default';
                return (prov === 'default' ? m.id : prov + '/' + m.id) === current || m.id === current;
            });
            return owner ? (owner.provider || 'default') + '/' + owner.id : '';
        }

        // The selection the toolbar list was last RENDERED with. A config
        // frame re-derives every row's `current` flag, but when the resulting
        // selection is unchanged the rendered rows are identical — and a
        // config frame arrives on every attach, mode/thinking change and
        // feature-toggle broadcast, so rebuilding a several-hundred-row list
        // (fresh nodes + listeners) for an unchanged selection is pure waste.
        // Open (toggleModelPopover) and filter (input) re-render regardless,
        // and a catalog reply (updateModelSelect) always renders once.
        let toolbarListSelection = null;

        // Toolbar wrapper: behavior unchanged (per-session set_model +
        // popover close), just routed through the shared renderer.
        function renderToolbarModelList(models, current) {
            if (!tbModelList) return;
            toolbarListSelection = toolbarSelectionKey(models, current);
            renderModelList(tbModelList, modelFilterQuery, tbModelFilter ? tbModelFilter.value : modelFilterQuery, models, current, (id, active) => {
                if (active) { closeModelPopover(); return; }
                if (!ws || ws.readyState !== WebSocket.OPEN) return;
                // set_model is per-session: the sessionId scopes it
                // to the requesting pane's provider only.
                ws.send(JSON.stringify({ type: 'set_model', model: id, sessionId: activePane().id }));
                closeModelPopover();
            });
        }

        // ── Subagent model + reasoning-effort picker (Agent settings tab) ──
        // The shared ModelThinkingPicker (components/model-picker.js),
        // configured for the settings tab:
        // the model list reuses the toolbar's model-list rendering (same
        // grouping, filter, rows) with a leading "Inherit" row (empty
        // value = subagents use the parent's model); the effort chips are
        // model-aware (the selected subagent model's accepted values from
        // the catalog, the active pane's model values while it is
        // "Inherit" (the child then runs the parent's model), the default
        // set as a last resort). Selections round-trip through the
        // runtime-config channel (configFields: ["subagentModel",
        // "subagentThinkingLevel"]) so the server persists and
        // re-broadcasts the values to every tab.
        const subagentPickerState = { model: '', thinkingLevel: '' }; // server-pushed current values ('' = inherit)
        const subagentModelFilter = document.getElementById('subagent-model-filter');
        const subagentPicker = createModelThinkingPicker({
            listEl: document.getElementById('subagent-model-list'),
            filterEl: subagentModelFilter,
            chipsEl: document.getElementById('subagent-thinking-options'),
            getState: () => subagentPickerState,
            getModels: () => availableModels,
            getPane: () => activePane(),
            defaultRow: { label: 'Inherit (parent\u2019s model)' },
            inheritChipTitle: "Inherit the parent session's level",
            stripPaneCurrent: true,
            onModelChange: (id) => sendRuntimeConfig({ subagentModel: { prop: 'subagentModel', value: id } }),
            onThinkingChange: (value) => sendRuntimeConfig({ subagentThinkingLevel: { prop: 'subagentThinkingLevel', value } }),
        });
        subagentModelFilter?.addEventListener('input', () => subagentPicker.render());

        // ── Review agent model + reasoning-effort picker (Agent settings
        // tab) ── The shared ModelThinkingPicker, configured for the
        // board auto-review agent: the model list's default row is the
        // CASCADE value ("" = the ticket's own review model override,
        // else the workspace default model); the effort chips are
        // model-aware (the selected review model's accepted values, the
        // pane's model values while the cascade row is selected, the
        // default set as a last resort — the server re-validates against
        // the reviewer's final model at review start). Selections
        // round-trip through the runtime-config channel (configFields:
        // ["reviewAgentModel", "reviewAgentThinkingLevel"]).
        const reviewAgentPickerState = { model: '', thinkingLevel: '' }; // server-pushed current values ('' = cascade/inherit)
        const reviewAgentModelFilter = document.getElementById('review-agent-model-filter');
        const reviewAgentPicker = createModelThinkingPicker({
            listEl: document.getElementById('review-agent-model-list'),
            filterEl: reviewAgentModelFilter,
            chipsEl: document.getElementById('review-agent-thinking-options'),
            getState: () => reviewAgentPickerState,
            getModels: () => availableModels,
            getPane: () => activePane(),
            defaultRow: { label: 'Cascade (ticket override → workspace default)', title: 'Use the ticket override, else the workspace default model' },
            inheritChipTitle: 'Inherit the workspace level',
            stripPaneCurrent: true,
            onModelChange: (id) => sendRuntimeConfig({ reviewAgentModel: { prop: 'reviewAgentModel', value: id } }),
            onThinkingChange: (value) => sendRuntimeConfig({ reviewAgentThinkingLevel: { prop: 'reviewAgentThinkingLevel', value } }),
        });
        reviewAgentModelFilter?.addEventListener('input', () => reviewAgentPicker.render());

        // ── Toolbar: thinking level chips ──
        // The chip rendering (Off chip + the model's accepted values,
        // L/M/H short labels) lives in components/model-picker.js; this
        // instance sends set_thinking_level on the chat socket.
        const renderThinkingChips = createThinkingChips({
            gridEl: tbThinkingGrid,
            sectionEl: tbThinkingSection,
            onSelect: (value) => {
                if (!ws || ws.readyState !== WebSocket.OPEN) return;
                ws.send(JSON.stringify({ type: 'set_thinking_level', thinkingLevel: value, sessionId: activePane().id }));
            },
        });

        // ── Toolbar: mode selector popover ──
        const tbModePopover = document.getElementById('tb-mode-popover');
        const tbModePicker = document.getElementById('tb-mode-picker');
        // Shared popover shell (components/popover.js); the anchor is the
        // whole .tb-group so clicks on the button or the list don't close.
        const tbModePopoverCtl = createPopover({
            el: tbModePopover,
            getAnchor: () => tbModePicker,
        });

        function toggleModePopover() {
            tbModePopoverCtl.toggle();
        }

        function closeModePopover() {
            tbModePopoverCtl.close();
        }

        tbModeBtn.addEventListener('click', (e) => {
            e.stopPropagation();
            toggleModePopover();
        });

        document.querySelectorAll('#tb-mode-list .tb-model-row').forEach((row) => {
            row.addEventListener('click', () => {
                const mode = row.dataset.mode;
                if (mode === currentMode) { closeModePopover(); return; }
                if (!ws || ws.readyState !== WebSocket.OPEN) return;
                ws.send(JSON.stringify({ type: 'set_mode', mode, sessionId: activePane().id }));
                closeModePopover();
            });
        });

        // ── Toolbar: context badge ──
        // The badge DOM is built once and mutated in place. Per-token context
        // estimate updates (up to ~60/s while streaming) must not recreate
        // nodes each time.
        let tbBadgeRing = null;
        let tbBadgeLabel = null;
        let tbBadgeAria = null;
        let tbBadgeTone = null;
        function ensureToolbarBadgeNodes() {
            if (tbBadgeRing) return true;
            if (!tbContextBadge) return false;
            tbContextBadge.innerHTML = '';
            tbBadgeRing = document.createElement('span');
            tbBadgeRing.className = 'ctx-ring';
            tbBadgeLabel = document.createTextNode('');
            tbContextBadge.appendChild(tbBadgeRing);
            tbContextBadge.appendChild(tbBadgeLabel);
            return true;
        }
        function updateToolbarContext(data) {
            if (!tbContextBadge) return;
            if (!ensureToolbarBadgeNodes()) return;
            const used = (data && data.usedTokens) || 0;
            const limit = (data && data.contextLimit) || 0;
            if (limit <= 0) {
                // No window size known at all — nothing to render.
                tbBadgeRing.style.setProperty('--ctx-pct', '0%');
                if (tbBadgeLabel.nodeValue !== ' —') tbBadgeLabel.nodeValue = ' —';
                // aria-label carries the descriptive text for assistive tech.
                // The native `title` bubble is intentionally left unset: the
                // custom #context-tooltip already renders on hover/focus, and
                // a title would pop a second, overlapping bubble ~1s later.
                const aria = 'Context usage unknown';
                if (tbBadgeAria !== aria) { tbContextBadge.setAttribute('aria-label', aria); tbBadgeAria = aria; }
                const tone = '';
                if (tbBadgeTone !== tone) {
                    tbContextBadge.removeAttribute('data-tone');
                    tbBadgeTone = tone;
                }
                return;
            }
            if (used <= 0) {
                // Window size known but nothing counted yet (fresh session):
                // show "—/limit" instead of claiming 0% of a window the
                // system prompt alone already occupies.
                tbBadgeRing.style.setProperty('--ctx-pct', '0%');
                const label = ` —/${formatTokenCount(limit)}`;
                if (tbBadgeLabel.nodeValue !== label) tbBadgeLabel.nodeValue = label;
                const aria = `Context usage: nothing counted yet, limit ${limit.toLocaleString()} tokens`;
                if (tbBadgeAria !== aria) { tbContextBadge.setAttribute('aria-label', aria); tbBadgeAria = aria; }
                const tone = '';
                if (tbBadgeTone !== tone) {
                    tbContextBadge.removeAttribute('data-tone');
                    tbBadgeTone = tone;
                }
                return;
            }
            const pct = Math.min(100, Math.round((used / limit) * 100));
            tbBadgeRing.style.setProperty('--ctx-pct', pct + '%');
            const label = ` ${pct}% ${formatTokenCount(used)}/${formatTokenCount(limit)}`;
            if (tbBadgeLabel.nodeValue !== label) tbBadgeLabel.nodeValue = label;
            const aria = `Context usage: ${pct}% of ${limit.toLocaleString()} tokens (${used.toLocaleString()} used)`;
            if (tbBadgeAria !== aria) { tbContextBadge.setAttribute('aria-label', aria); tbBadgeAria = aria; }
            const tone = pct >= 90 ? 'danger' : pct >= 75 ? 'warning' : '';
            if (tbBadgeTone !== tone) {
                if (tone) tbContextBadge.setAttribute('data-tone', tone);
                else tbContextBadge.removeAttribute('data-tone');
                tbBadgeTone = tone;
            }
        }

        // ── Context tooltip on toolbar badge (reuses sidebar tooltip element) ──
        // Shown on hover AND focus: the badge is tabbable, so keyboard and
        // touch users reach the same detailed breakdown. The native `title`
        // bubble is never set on the badge (updateToolbarContext writes
        // aria-label instead), so nothing overlaps this custom tooltip.
        function showContextTooltip() {
            const d = lastContextData || {};
            if (!d.contextLimit && !d.usedTokens) return;
            const lines = [];
            lines.push('── Context ──');
            if (d.usedTokens) lines.push(`Used: ${formatTokenCount(d.usedTokens)}`);
            else if (d.contextLimit) lines.push('Used: — (nothing counted yet)');
            if (d.contextLimit) lines.push(`Limit: ${formatTokenCount(d.contextLimit)}`);
            if (d.promptTokens) lines.push(`Prompt: ${formatTokenCount(d.promptTokens)}`);
            if (d.completionTokens) lines.push(`Completion: ${formatTokenCount(d.completionTokens)}`);
            if (d.cachedTokens) lines.push(`Cached: ${formatTokenCount(d.cachedTokens)}`);
            if (d.cachedTokens && d.promptTokens > 0) lines.push(`Cache hit: ${Math.round((d.cachedTokens / d.promptTokens) * 100)}% of prompt`);
            if (d.compactAt) lines.push(`Compact at: ${formatTokenCount(d.compactAt)}`);
            if (d.messageCount > 0) lines.push(`Messages: ${d.messageCount}`);
            if (d.usedSource) lines.push(`Source: ${d.usedSource}`);
            const prompt = d.totalPromptTokens || 0;
            const completion = d.totalCompletionTokens || 0;
            const cached = d.totalCachedTokens || 0;
            const turns = d.totalTurns || 0;
            if (turns > 0 || prompt > 0 || currentModelPricing) {
                lines.push('── Session ──');
                if (turns > 0 || prompt > 0) {
                    lines.push(`Total: ${formatTokenCount(prompt + completion, '0')} · ${turns} turns`);
                    lines.push(`Prompt: ${formatTokenCount(prompt, '0')}`);
                    lines.push(`Completion: ${formatTokenCount(completion, '0')}`);
                    lines.push(`Cached: ${formatTokenCount(cached, '0')}`);
                    if (cached > 0 && prompt > 0) lines.push(`Cache hit: ${Math.round((cached / prompt) * 100)}% of session prompt`);
                }
                if (currentModelPricing) {
                    const billablePrompt = Math.max(0, prompt - cached);
                    const cost = (billablePrompt / 1e6) * currentModelPricing.input
                               + (cached / 1e6) * currentModelPricing.cached
                               + (completion / 1e6) * currentModelPricing.output;
                    lines.push(`Pricing: in $${currentModelPricing.input}/1M · out $${currentModelPricing.output}/1M`);
                    if (turns > 0 || prompt > 0) {
                        lines.push(`Est. cost: ${fmtCost(cost)}`);
                    }
                }
            }
            contextTooltip.textContent = lines.join('\n');
            contextTooltip.style.display = 'block';
        }
        function hideContextTooltip() {
            contextTooltip.style.display = 'none';
        }
        tbContextBadge.addEventListener('mouseenter', showContextTooltip);
        tbContextBadge.addEventListener('focus', showContextTooltip);
        tbContextBadge.addEventListener('mouseleave', hideContextTooltip);
        tbContextBadge.addEventListener('blur', hideContextTooltip);

        // ── Command mode indicator ──
        // inputAreaWrap is declared here; the command-mode class toggle runs
        // in the single shared input listener registered next to
        // updateSlashSuggest (one listener for all per-keystroke composer
        // updates instead of two).
        const inputAreaWrap = document.getElementById('input-area');

        function updateModeInfo(mode) {
            // A config frame without a mode (the field is omitempty) must not
            // repaint the picker: the fallback used to be 'act', which showed
            // Act for a session the server still runs in plan mode — and the
            // picker's "already active" short-circuit then made clicking Act a
            // silent no-op. keep the last known mode instead.
            if (!mode) return;
            currentMode = mode.toLowerCase();
            if (!tbModeBtn) return;
            const label = currentMode === 'plan' ? 'Plan' : 'Act';
            tbModeBtn.innerHTML = `${icon('pen')} <span class="tb-mode-label">${label}</span> <span class="tb-arrow">${icon('chevron-down')}</span>`;
            // Highlight active option in popover
            document.querySelectorAll('#tb-mode-list .tb-model-row').forEach((r) => {
                r.classList.toggle('active', r.dataset.mode === currentMode);
            });
        }

        let availableModels = [];
        let currentModelPricing = null; // { input, output, cached } or null

        function updateModelInfo(model, description) {
            // Update toolbar model button. The server's config.Model is
            // authoritative (it may be empty after validation cleared a
            // stale restored model); do NOT fall back to the last-known
            // catalog entry, which can still carry the cleared model.
            if (tbModelBtn) {
                const name = model || '—';
                // Model ids come from the provider catalog / user config:
                // build the label with textContent (never innerHTML) so a
                // crafted id cannot inject markup into the toolbar.
                tbModelBtn.textContent = '';
                tbModelBtn.appendChild(document.createTextNode(name + ' '));
                const arrow = document.createElement('span');
                arrow.className = 'tb-arrow';
                arrow.innerHTML = icon('chevron-down');
                tbModelBtn.appendChild(arrow);
                // Hover tooltip: models.dev description of the current model.
                if (description) {
                    tbModelBtn.title = description;
                } else {
                    tbModelBtn.removeAttribute('title');
                }
            }
        }

        function updateModelSelect(models, current, provider) {
            availableModels = Array.isArray(models) ? models : [];
            // The server's `current` is authoritative (empty after
            // validation cleared a stale restored model); do not fall back
            // to the last-known catalog entry.
            const modelId = current || '';
            // (provider, id) is the routing key: the bare id is not unique
            // across registered providers.
            const owner = provider || 'default';
            const active = modelId
                ? availableModels.find((m) => m.id === modelId && (m.provider || 'default') === owner)
                : availableModels.find((m) => m.current);
            updateModelInfo(modelId, active && active.description);
            // Populate toolbar model list (the qualified selector keeps the
            // renderer's bare-flag fallback comparable for non-default rows).
            renderToolbarModelList(availableModels, modelId ? (owner === 'default' ? modelId : owner + '/' + modelId) : '');
            // The board "Start agent" popover renders from the same
            // catalog — refresh it while open so a late list_models reply
            // is not stuck on "No models loaded" (same pattern as the
            // settings subagent picker). The picker's refresh re-runs the
            // shared model-aware effort guard: a stored effort the model
            // does not accept is now knowable, so it resets to "Inherit"
            // before re-rendering (the server re-validates at start).
            refreshBoardStartPicker();
            // Extract pricing for the active model
            currentModelPricing = modelPricing(
                active && active.inputPricePer1M,
                active && active.outputPricePer1M,
                active && active.cachedPricePer1M
            );
        }

        // Build the toolbar's per-model pricing snapshot (used for the
        // context-tooltip cost estimate). Null when the model has no
        // published input price.
        function modelPricing(input, output, cached) {
            if (!input) return null;
            return {
                input,
                output: output || 0,
                cached: cached || 0,
            };
        }

        // Fetch the model catalog lazily so it never blocks initial connect.
        let modelsRequested = false;
        function ensureModelsLoaded() {
            if (modelsRequested) return;
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            modelsRequested = true;
            ws.send(JSON.stringify({ type: 'list_models' }));
        }

        // ── Near-compact banner ──
        // The server flags warnNearCompact once usage reaches 75% of the
        // window — before the auto-compaction trigger (nearCompact) — so the
        // user gets lead time to compact manually. The dismissed flag is a
        // per-pane working copy mirrored to/from the pane object in
        // save/loadActivePaneState, so dismissing it on one pane does not
        // suppress the banner on another. Fresh panes start undismissed.
        let nearCompactDismissed = false;

        // True while a compact command is in flight (server-side summarization
        // can take a while); drives the persistent "Compacting context…"
        // indicator and gates duplicate clicks. Cleared when the compact
        // response arrives or on disconnect.
        let compacting = false;

        function startCompact() {
            if (!ensureConnected()) return;
            if (compacting) return;
            compacting = true;
            ws.send(JSON.stringify({ type: 'compact', sessionId: activePane().id }));
            // Persistent progress, not a 3s toast: it stays until the server
            // reports the compact result (see the 'response' handler).
            setInputProgress('compacting', 'Compacting context…');
        }

        ncbCompactBtn?.addEventListener('click', () => {
            startCompact();
        });

        ncbDismissBtn?.addEventListener('click', () => {
            nearCompactDismissed = true;
            nearCompactBanner.hidden = true;
        });

        // Phase 0e condensation banner: dismissible; a new condensation
        // re-shows it (the server sends a fresh "condensed" message each
        // time a message is condensed).
        condensedBannerDismiss?.addEventListener('click', () => {
            condensedBanner.hidden = true;
        });

        // The server omits contextLimit until the session's window is
        // resolved (first turn, model switch, or session restore). Until
        // then the badge seeds from the model list's per-model
        // contextLimit — the best known estimate; the first resolved
        // context message carries the authoritative value and overwrites
        // it. Returns 0 when the catalog has no entry for the model.
        function catalogContextLimit(model) {
            if (!model || !availableModels.length) return 0;
            const m = availableModels.find((x) => x.id === model);
            return (m && m.contextLimit) || 0;
        }

        function updateContextInfo(data) {
            lastContextData = data || lastContextData;
            // warnNearCompact drives the banner (early warning); the server
            // always sends it explicitly, false included, so a post-compaction
            // message reliably hides a banner that was previously shown.
            if (typeof data.warnNearCompact === 'boolean' && nearCompactBanner) {
                nearCompactBanner.hidden = nearCompactDismissed || !data.warnNearCompact;
            }
            const used = data.usedTokens || 0;
            // A server-provided limit is authoritative; an absent one means
            // the window is unresolved yet — seed from the model list and
            // stamp it onto data so the badge and its tooltip agree.
            contextLimitResolved = !!data.contextLimit;
            let limit = data.contextLimit || 0;
            if (!limit) {
                limit = catalogContextLimit(activePane() && activePane().model);
                if (limit) data.contextLimit = limit;
            }
            if (data.usedSource !== 'estimated') {
                contextBaseUsed = used;
                // Server value only (0 until resolved) — the display seed
                // must not leak into the live-estimation state.
                contextLimit = contextLimitResolved ? data.contextLimit : 0;
                contextEstAdded = 0;
            }

            if (used <= 0 && limit <= 0) {
                updateToolbarContext(null);
                return;
            }
            // Update toolbar context badge
            updateToolbarContext(data);
        }

        // Last working directory a config frame carried. The explorer is
        // rebuilt only when this actually changes: a config frame arrives on
        // every attach, model switch, mode/thinking change, feature toggle
        // broadcast and provider save, and each refresh is a directory walk
        // plus a git-status round trip (plus the tree DOM rebuild) for an
        // event that never touched the working dir. A pane switch to the
        // editor and the editor socket's reconnect refresh it explicitly.
        let lastWorkingDir = '';

        function applyServerConfig(data) {
            const workingDirChanged = !!data.workingDir && data.workingDir !== lastWorkingDir;
            if (data.workingDir) {
                lastWorkingDir = data.workingDir;
                dirInput.value = data.workingDir;
                if (workingDirPath) workingDirPath.textContent = data.workingDir;
            }
            // Keep the popover model list in sync with the active model after a switch.
            // Match (provider, id), not the bare id: model IDs are not unique
            // across registered providers, so a bare-id match would mark EVERY
            // catalog row that lists the same id (the deepseek copy AND the
            // opencode copy of deepseek-flash) as current — the server's
            // modelProvider names the profile the pane actually runs on.
            if (data.model && availableModels.length > 0) {
                const provider = data.modelProvider || 'default';
                for (const m of availableModels) {
                    m.current = (m.id === data.model) && ((m.provider || 'default') === provider);
                }
                const selector = provider === 'default' ? data.model : provider + '/' + data.model;
                // The flags above are what every consumer reads; the DOM only
                // needs the rebuild when the rendered selection moves.
                if (toolbarSelectionKey(availableModels, selector) !== toolbarListSelection) {
                    renderToolbarModelList(availableModels, selector);
                }
            } else if (!data.model && availableModels.length > 0) {
                // Model cleared (validation dropped a stale restored model):
                // nothing is current — a leftover checkmark would keep
                // pointing at a model the pane no longer runs.
                for (const m of availableModels) {
                    m.current = false;
                }
                if (toolbarSelectionKey(availableModels, '') !== toolbarListSelection) renderToolbarModelList(availableModels, '');
            }
            // Remember the active model's accepted efforts + description so
            // pane switches and focus restores re-render the correct chips
            // and tooltip.
            const cfgPane = activePane();
            if (cfgPane) {
                cfgPane.reasoningEfforts = data.reasoningEfforts || [];
                cfgPane.modelDescription = data.modelDescription || '';
            }
            renderThinkingChips(data.thinkingLevel, data.reasoningEfforts, data.reasoningEffortsUnsupported);
            updateModelInfo(data.model, data.modelDescription);
            updateModeInfo(data.mode);
            // The server omits globalMode when false (JSON omitempty), so
            // absence always means project mode.
            isGlobalMode = !!data.globalMode;
            if (globalModeBadge) globalModeBadge.classList.toggle('visible', isGlobalMode);
            // The working directory can only be changed in global mode:
            // project mode shows a read-only path, global mode shows the
            // editable control (the server also rejects the change).
            if (workingDirDisplay) workingDirDisplay.style.display = isGlobalMode ? 'none' : '';
            if (workingDirConfig) workingDirConfig.style.display = isGlobalMode ? '' : 'none';
            // Sync pricing from server-provided fields (always present on config from cached lookups)
            if (data.inputPricePer1M) {
                currentModelPricing = modelPricing(data.inputPricePer1M, data.outputPricePer1M, data.cachedPricePer1M);
            } else if (data.model && availableModels.length > 0) {
                // Fallback: look up from locally cached model list —
                // (provider, id), since the bare id is not unique across
                // registered providers.
                const owner = data.modelProvider || 'default';
                const active = availableModels.find((m) => m.id === data.model && (m.provider || 'default') === owner);
                if (active && active.inputPricePer1M) {
                    currentModelPricing = modelPricing(active.inputPricePer1M, active.outputPricePer1M, active.cachedPricePer1M);
                }
            }
            updateContextInfo(data);
            applyFeatureSettings(data);
            applyRuntimeSettings(data);
            applyStatusDisplayConfig(data);
            applyProviderSettings(data);
            applyMCPSettings(data);

            if (data.sessionId) {
                sessionInfoDiv.textContent = data.sessionId;
                updateCurrentSessionLabel(data.sessionLabel);
            }
            // Refresh the explorer when the working dir changed (only then —
            // see lastWorkingDir above).
            if (workingDirChanged && document.getElementById('editor-pane').classList.contains('active')) {
                refreshExplorer().catch(() => {});
            }
        }

        // Reset the module-level chat state that belongs to the ACTIVE pane
        // (stream references, pending acks/cancels, follow). clearChat runs
        // it after wiping the DOM; the pane-switch cache path runs it after
        // moving the outgoing pane's transcript into its offscreen cache —
        // the state is the outgoing pane's either way.
        function resetChatTranscriptState() {
            hideTocTooltip();
            rebuildToc();
            enableFollow();
            // The transcript is gone: pending-ack entries point at
            // detached bubbles, and the re-attach re-derives every
            // histIdx from server history, so the tracking starts fresh.
            pendingAcks.length = 0;
            pendingCancels.length = 0;
            cancelTarget.clear();
            lastCancelTarget = null;

            streamRafPending = false;
            streamLastRender = 0;
            endStream();
            currentThinkingDiv = null;
            currentThinkingSpan = null;
            currentThinkingRaw = '';
            thinkingRafPending = false;
            streamingToolCards = {};
            pendingToolCards = {};
            bgTermCards = {};
            historyToolCallArgs = {};
            toolsStartedThisTurn = false;
            streamContentPos = 0;
            thinkingContentPos = 0;
            lastFinalizedThinking = null;
            // noMirror: this also runs mid-pane-switch (cache → load),
            // where the new pane's turnActive must survive the reset.
            setTurnActive(false, { silent: true, noMirror: true });
            setInputProgress(null);
            // Transcript is empty: pinned and at the bottom, so this hides
            // the jump button (same as the old direct classList.remove).
            updateScrollBottomBtn();
        }

        // O(1) empty-state probes: while the placeholder is shown it is the
        // transcript's only (first) child, and messages are appended after it
        // only once it has been removed. The previous
        // messagesDiv.querySelector('.empty-state') — run for every appended
        // message — scanned every node in the transcript, which made a
        // history replay O(n²) (measured: ~60% of the replay time for a
        // 400-message session). Checking the live DOM per pane also keeps the
        // per-pane transcript cache correct: a tracked element reference
        // would dangle across pane switches.
        function hasEmptyState() {
            const first = messagesDiv.firstElementChild;
            return !!first && first.classList.contains('empty-state');
        }

        // Show the empty-state placeholder only while the transcript is
        // truly empty.
        function ensureEmptyState() {
            if (hasEmptyState()) return;
            if (messagesDiv.querySelector('.message, .thought-card, .tool-card')) return;
            messagesDiv.appendChild(buildEmptyState());
        }

        function clearChat() {
            disposeChatEditors();
            messagesDiv.innerHTML = '';
            // The DOM no longer reflects any history payload: void the active
            // pane's rendered-index claim with it. Otherwise a wipe whose
            // rebuild snapshot never arrives (a reconnect whose attach
            // snapshot write hit the server's 5s send-queue timeout and was
            // dropped) would let later hist_index frames re-advance the mark
            // to "current" — and the turn-end convergence attach would then
            // skip the very rebuild that restores the missing transcript.
            const wiped = activePane();
            if (wiped) {
                wiped.histEpoch = undefined;
                wiped.renderedHistIdx = -1;
            }
            resetChatTranscriptState();
            ensureEmptyState();
        }

        // ── Per-pane transcript cache ──
        // Switching panes used to clear the transcript and re-derive it from
        // a full server history snapshot on every focus (re-clone, re-marshal,
        // re-ship, re-parse, re-render — seconds on a long session). A SETTLED
        // pane instead moves its rendered transcript into an offscreen
        // container here, and focusPane moves it back before re-attaching:
        // nothing is re-rendered, and the attach tells the server the pane's
        // history fingerprint so it can skip the snapshot entirely (and even
        // the wire cost) when the session is unchanged. Panes that were live
        // mid-turn at switch time are NOT cached — background stream events
        // never reach the DOM, so their transcript would be stale by the
        // in-flight content; those keep the clear-and-rederive path.
        function paneSettledForCache() {
            return !turnActive
                && !hasLiveInFlight()
                // Live agent terminal cards stream tool output by node
                // reference; a pane holding any would miss updates while
                // cached. Same conservative rule as the live-render check.
                && Object.keys(bgTermCards).length === 0
                && pendingAcks.length === 0
                && pendingCancels.length === 0
                && cancelTarget.size === 0
                && !pendingSessionResponse
                && !resendAwaitingHistory
                && !compacting;
        }

        // Capture the ACTIVE pane's rendered transcript into an offscreen
        // cache so a later focus can restore it without a server round-trip.
        // Returns true when cached; the caller then runs
        // resetChatTranscriptState() instead of clearChat().
        function cachePaneTranscript(pane) {
            if (!pane || !pane.id) return false;
            if (!paneSettledForCache()) return false;
            const cache = {
                container: document.createElement('div'),
                sessionId: pane.id,
                scrollTop: messagesDiv.scrollTop,
                pinned: isPinned(),
                historyToolCallArgs,
            };
            while (messagesDiv.firstChild) cache.container.appendChild(messagesDiv.firstChild);
            pane.domCache = cache;
            return true;
        }

        // Move a pane's cached transcript back into the live container and
        // restore the per-pane state captured with it. The cache is consumed.
        // Returns true when a cache was restored (the caller then re-attaches
        // with the pane's known history fingerprint); false leaves the
        // container empty for the caller's empty-state.
        function restorePaneTranscript(pane) {
            const cache = pane && pane.domCache;
            pane.domCache = null;
            if (!cache || cache.sessionId !== pane.id) return false;
            // Stray content (should not happen — the outgoing pane was moved
            // out or wiped) is dropped, not merged into the restored cache.
            messagesDiv.replaceChildren(...cache.container.childNodes);
            historyToolCallArgs = cache.historyToolCallArgs;
            // resetChatTranscriptState emptied the TOC rail after the cache
            // was captured; rebuild it from the restored DOM (same nodes, so
            // the dot ↔ message wiring is preserved).
            rebuildToc();
            // Re-derive the code colorization for the restored subtree. While
            // this pane sat in its offscreen cache, its parked colorize jobs
            // could be dropped: clearing ANOTHER pane's transcript runs
            // disposeChatEditors → releaseColorizeTargets, which clears the
            // shared park map and queue (only a restored subtree can be woken
            // by the IntersectionObserver again). colorizeNode is idempotent
            // now — blocks that are already colored are skipped by their
            // stamp — so this costs one traversal, and the rest hit the cache
            // or park on the viewport observer as on a first render.
            colorizeNode(messagesDiv);
            if (cache.pinned) {
                pinToBottom();
            } else {
                disableFollow();
                messagesDiv.scrollTop = cache.scrollTop;
            }
            return true;
        }

        // ── Per-session scroll anchor ──
        // The settled-pane cache above restores the EXACT scroll offset
        // (raw scrollTop against the same DOM). Every other path that
        // brings a session's transcript back — a mid-turn switch-away (no
        // cache), a stale cache rebuilt from a fresh snapshot, a pane
        // closed and reopened — rebuilds the DOM and lands pin-to-bottom,
        // losing the part of the transcript the user was reading. The
        // anchor records WHICH message crossed the viewport top (history
        // index + px offset, see components/scroll.js) so a rebuild can
        // put the viewport back on it. Keyed by session id, not by pane:
        // panes close and reopen, and domCache is consumed on restore.
        // Invalidated when the history epoch reshapes (compaction /
        // rollback renumber the indexes) or the session is removed; when
        // the user was following the bottom there is nothing to remember
        // (a rebuild lands at the bottom anyway).
        const paneScrollAnchors = new Map();

        // Capture the outgoing pane's reading position. Runs in
        // transitionToPane BEFORE the transcript is cached or wiped — the
        // measurement reads live layout, which is gone once the DOM moves
        // offscreen or is dropped.
        function capturePaneScrollAnchor(pane) {
            if (!pane || !pane.id) return;
            if (isPinned()) {
                // Following the bottom: a rebuild lands at the bottom
                // anyway — drop any stale anchor.
                paneScrollAnchors.delete(pane.id);
                return;
            }
            const a = measureChatAnchor();
            if (a) {
                paneScrollAnchors.set(pane.id, {
                    histIdx: a.histIdx,
                    subPx: a.subPx,
                    // The epoch under which the anchor's index is valid is
                    // the pane's last-applied history epoch. undefined (the
                    // transcript was never built from a snapshot) is stored
                    // as null so it can never match an incoming snapshot
                    // epoch — a conservative drop to the old behavior.
                    histEpoch: typeof pane.histEpoch === 'number' ? pane.histEpoch : null,
                });
            } else {
                paneScrollAnchors.delete(pane.id);
            }
        }

        // Build the scroll settlement for a history rebuild: a callback
        // that puts the viewport back on the reader's anchor instead of
        // the replay's default pin-to-bottom, or null when the anchor
        // must not apply. Guards:
        //   - pane.id must already match the payload's session: a session
        //     change (/new, /resume, /fork, edit-resend) delivers the NEW
        //     session's history while the pane still holds the OLD id,
        //     and the old session's anchor must never position a
        //     different session's transcript;
        //   - no in-flight session change / resend fork (same reason —
        //     belt and braces with the per-pane flag mirrors);
        //   - the anchor's epoch must equal the snapshot's: a reshaped
        //     history renumbered the indexes and the anchor is garbage;
        //   - an anchor must exist at all.
        // The anchor is kept on use: repeated rebuilds for the same
        // session (turn_end convergence refetch) restore again, and the
        // next switch-away re-captures a fresher one anyway.
        function paneScrollRestoreSettle(pane, sessionId, incomingEpoch) {
            if (!pane || pane.id !== sessionId) return null;
            if (pendingSessionResponse || pane.pendingSessionResponse) return null;
            if (resendAwaitingHistory || pane.resendAwaitingHistory) return null;
            const anchor = paneScrollAnchors.get(sessionId);
            if (!anchor || anchor.histEpoch !== incomingEpoch) return null;
            return () => {
                if (!restoreChatAnchor(anchor)) pinToBottom();
            };
        }

        // ── Empty chat state (fresh session) ──
        // Shown only while the transcript is truly empty; any message render
        // (appendMessageAtTime / startStream / showThinking) removes it.
        const EMPTY_STATE_STARTERS = [
            'Explain this codebase',
            'Run the tests and fix any failures',
            'Plan a new feature',
        ];

        function sendStarterPrompt(text) {
            if (!ensureConnected()) return;
            inputArea.value = text;
            sendMessage();
        }

        function buildEmptyState() {
            const wrap = document.createElement('div');
            wrap.className = 'empty-state';
            const emoji = document.createElement('div');
            emoji.className = 'empty-state-emoji';
            emoji.textContent = '🤖';
            const title = document.createElement('div');
            title.className = 'empty-state-title';
            title.textContent = 'GoGen is ready';
            const sub = document.createElement('div');
            sub.className = 'empty-state-sub';
            sub.textContent = 'Ask me to explore, fix, or build something in this repository.';
            const prompts = document.createElement('div');
            prompts.className = 'empty-state-prompts';
            for (const p of EMPTY_STATE_STARTERS) {
                const btn = document.createElement('button');
                btn.type = 'button';
                btn.className = 'empty-state-btn';
                btn.textContent = p;
                btn.addEventListener('click', () => sendStarterPrompt(p));
                prompts.appendChild(btn);
            }
            const hint = document.createElement('div');
            hint.className = 'empty-state-hint';
            hint.textContent = 'Type / for commands · Ctrl+K for the command palette';
            wrap.append(emoji, title, sub, prompts, hint);
            return wrap;
        }

        function removeEmptyState() {
            const first = messagesDiv.firstElementChild;
            if (first && first.classList.contains('empty-state')) first.remove();
        }

        // ── History-rebuild blur ──
        // See the #messages.restoring CSS rule: the chunked replay builds the
        // transcript while it is softly blurred, so the 12-entries-per-frame
        // motion never flies past legibly — but nothing is hidden, so the pane
        // never looks blank (the blank veil read as a crash). The composer
        // strip's "Restoring session…" label is delayed a moment so a fast
        // rebuild (the common case now) cannot flash it for a single frame,
        // and it only claims the strip when no turn phase already owns it.
        const RESTORING_LABEL_DELAY_MS = 120;
        let restoringLabelTimer = null;
        let restoringLabelShown = false;
        const RESTORING_LABEL = 'Restoring session\u2026';

        function beginRestoring() {
            messagesDiv.classList.add('restoring');
            if (restoringLabelTimer) return;
            restoringLabelTimer = setTimeout(() => {
                restoringLabelTimer = null;
                if (!messagesDiv.classList.contains('restoring')) return;
                if (currentProgressPhase) return; // a turn phase owns the strip
                setInputProgress('thinking', RESTORING_LABEL);
                restoringLabelShown = true;
            }, RESTORING_LABEL_DELAY_MS);
        }

        function endRestoring() {
            if (restoringLabelTimer) {
                clearTimeout(restoringLabelTimer);
                restoringLabelTimer = null;
            }
            messagesDiv.classList.remove('restoring');
            // Clear only the label we set: a live event (or afterHistory's
            // turn-active branch) may have replaced it meanwhile.
            if (restoringLabelShown) {
                restoringLabelShown = false;
                if (progressLabelText === RESTORING_LABEL) setInputProgress(null);
            }
        }

        let pendingSessionResponse = false;
        // After session_new / session_fork for edit-resend, send this as a
        // normal message once history has been replayed.
        let pendingResendContent = null;
        // The images (data URLs) riding along with pendingResendContent —
        // mirrored per pane exactly like the content it belongs to.
        let pendingResendImages = null;
        // True only while a beginResend is waiting for its history snapshot.
        // Prevents unrelated history (reconnect/resume) from flushing a stale send.
        let resendAwaitingHistory = false;
        // True from beginResend until the session change's rebuild has begun
        // (its session_state, or the reply's history frame). The fork's
        // background model validation pushes its own config BEFORE the reply
        // (sessionFork → validateModelAsync → pushConfigForAgent wins the
        // race on a warm catalog cache), and that config already carries the
        // NEW session id: flushing from it would append the optimistic edited
        // bubble only for the reply's clear_chat to wipe it — the rebuild
        // then replays the truncated prefix without it, and flush already
        // cleared needsFreshHistory, so no turn-end refetch ever restores it.
        let resendRebuildPending = false;
        // Count of cancel→turn_end pairs to ignore so a cancelled turn's turn_end
        // cannot clobber an in-flight resend/send (pendingSessionResponse / turnActive).
        let suppressTurnEnds = 0;
        // Watchdog for a resend waiting on its session-change reply (see
        // armResendWatchdog): connection-scoped like the flags it clears.
        const RESEND_FLUSH_TIMEOUT_MS = 15000;
        let resendWatchdog = 0;
        // ── pending-ack tracking ─────────────────────────────────────────
        // user_acked carries only the server-side index, so the client
        // must remember WHICH bubble the ack belongs to. pendingAcks is a
        // FIFO of user bubbles awaiting their ack, in send order. Acks
        // arrive in send order (same-socket FIFO), and the server writes
        // a turn's terminal frame (cancelled / error response) before the
        // next turn's ack, so the oldest still-pending bubble is the next
        // ack's target. Sends that never ack — a turn cancelled before the
        // server emitted the frame, a busy/error rejection, a typed
        // command that starts no turn — are dropped when their terminal
        // frame arrives, so a stale flag can never steal a later ack and
        // stamp the wrong bubble.
        const pendingAcks = [];
        // One record per local cancel (cancelActiveTurn), consumed 1:1 by
        // the "cancelled" frame it produces: the pendingAcks entry of the
        // cancelled turn, or null when its ack had already arrived. The
        // record's entry is stale exactly when it is still queued at
        // frame time — an in-flight ack always arrives before the frame
        // (same-socket FIFO) and leaves the queue first.
        const pendingCancels = [];
        // Which pending entry each send cancelled (the turn that was
        // active when it was sent), for the terminal-frame drop rule in
        // dropPendingForTerminal.
        const cancelTarget = new Map();
        let lastCancelTarget = null;

        /** Mark a freshly sent user bubble as awaiting its user_acked frame. */
        function markPendingAck(el) {
            if (!el) return;
            el.dataset.pendingAck = '1';
            cancelTarget.set(el, lastCancelTarget);
            lastCancelTarget = null;
            pendingAcks.push(el);
        }

        // ── queued (steering) message tracking ───────────────────────────
        // A send while the pane's turn is active is QUEUED server-side and
        // delivered as the next user turn when the in-flight one ends. The
        // bubble is marked optimistically at send time and reconciled
        // against server truth: queued → running (user_acked at the drain
        // clears the chip; user_acked semantics are unchanged — it still
        // fires at turn start and carries the real history index), or
        // queued → cancelled (the item was removed before running). Other
        // tabs render the queued item from the queue_update frame itself.

        // Monotonic local correlation id for queued sends (crypto.randomUUID
        // is unavailable on non-secure origins). The per-PAGE nonce keeps two
        // tabs from minting the same id: the seq counter starts at 1 in every
        // tab and Date.now() has millisecond resolution, so without it two
        // tabs sending in the same millisecond would collide — and the id is
        // the correlation key in the server's queue and in every client's
        // per-id bubble map (the server replaces a duplicate defensively, but
        // the sender's own bubble could then not reconcile).
        let queueIdSeq = 0;
        const queueIdNonce = Math.random().toString(36).slice(2, 8);

        function nextQueueId() {
            return 'q-' + queueIdNonce + '-' + (++queueIdSeq) + '-' + Date.now().toString(36);
        }

        function queuedBubblesFor(pane) {
            if (!pane.queuedBubbles) pane.queuedBubbles = new Map();
            return pane.queuedBubbles;
        }

        /** Stamp a user bubble as queued: chip + hover ✕ (remove from queue). */
        function markQueuedBubble(el, qid) {
            if (!el) return;
            el.dataset.queueId = qid;
            el.classList.add('queued');
            const chip = document.createElement('span');
            chip.className = 'queued-chip';
            chip.textContent = 'queued';
            el.appendChild(chip);
            const now = document.createElement('button');
            now.className = 'queue-send-btn';
            now.type = 'button';
            now.textContent = 'Send now';
            now.title = 'Stop at the next round and send the queue';
            now.addEventListener('click', (ev) => {
                ev.stopPropagation();
                sendQueuedNow(qid);
            });
            el.appendChild(now);
            const rm = document.createElement('button');
            rm.className = 'queue-remove-btn';
            rm.type = 'button';
            rm.innerHTML = '✕';
            rm.title = 'Remove from queue';
            rm.addEventListener('click', (ev) => {
                ev.stopPropagation();
                removeQueuedItem(qid);
            });
            el.appendChild(rm);
        }

        // Send now is on the last queued bubble only. The pane's map is in
        // queue order (anchorQueuedBubbles keeps the DOM in that order), so
        // this walks at most the queue cap instead of the transcript. Called
        // once after each change to the queued set.
        function refreshQueuedSendNow(pane) {
            const bubbles = pane && pane.queuedBubbles;
            if (!bubbles) return;
            let last = null;
            for (const el of bubbles.values()) {
                const btn = el.querySelector(':scope > .queue-send-btn');
                if (!btn) continue;
                btn.hidden = true;
                last = btn;
            }
            if (last) last.hidden = false;
        }

        /** Clear the queued mark (chip + ✕) — the item's turn has started. */
        function clearQueuedMark(el) {
            if (!el || el.dataset.queueId === undefined) return;
            el.classList.remove('queued');
            delete el.dataset.queueId;
            const chip = el.querySelector(':scope > .queued-chip');
            if (chip) chip.remove();
            const now = el.querySelector(':scope > .queue-send-btn');
            if (now) now.remove();
            const rm = el.querySelector(':scope > .queue-remove-btn');
            if (rm) rm.remove();
        }

        // sendQueuedNow asks the running turn to finish its current round
        // and then deliver the queue, in its order. qid is the bubble the
        // button is on: the server ignores the request once that item has
        // left the queue, so a late click cannot stop the turn it started.
        function sendQueuedNow(qid) {
            const pane = activePane();
            if (!ws || ws.readyState !== WebSocket.OPEN || !pane || !pane.id) return;
            ws.send(JSON.stringify({ type: 'queue_send_now', queueId: qid, sessionId: pane.id }));
        }

        /**
         * A queued item left the queue WITHOUT running (removed or cleared
         * by an interrupt): resolve its pending-ack entry so it cannot steal
         * a later ack, and restyle the bubble as cancelled-before-running.
         */
        function resolveCancelledQueued(el) {
            if (!el || !el.isConnected || el.dataset.queueId === undefined) return;
            dropPendingAck(el);
            clearQueuedMark(el);
            el.classList.add('queue-cancelled');
        }

        function removeQueuedItem(qid) {
            const pane = activePane();
            if (!ws || ws.readyState !== WebSocket.OPEN || !pane || !pane.id) return;
            ws.send(JSON.stringify({ type: 'queue_remove', queueId: qid, sessionId: pane.id }));
        }

        /** Re-register a RE-RENDERED bubble as awaiting its user_acked (the
         * re-render of an item this tab sent that is still queued). Unlike
         * markPendingAck it consumes no cancel target: a re-render is not a
         * send, and a pending resend's lastCancelTarget belongs to its own
         * element. Keeps the ack FIFO resolving through pendingAcks instead
         * of the DOM fallback. */
        function rearmPendingAck(el) {
            if (!el) return;
            el.dataset.pendingAck = '1';
            pendingAcks.push(el);
        }

        /** Remove a bubble from pending-ack tracking and clear its flag. */
        function dropPendingAck(el) {
            const i = pendingAcks.indexOf(el);
            if (i !== -1) pendingAcks.splice(i, 1);
            cancelTarget.delete(el);
            if (el) delete el.dataset.pendingAck;
        }

        /** Drop queue entries whose bubble left the DOM (pane switch / re-render). */
        function prunePendingAcks() {
            while (pendingAcks.length && !pendingAcks[0].isConnected) pendingAcks.shift();
        }

        /**
         * The next ack's target: the oldest still-pending bubble. Entries
         * whose bubble left the DOM are unresolvable here and are
         * dropped; the DOM fallback covers re-rendered transcripts, where
         * every user bubble already carries its server index.
         */
        function resolveAckTarget() {
            prunePendingAcks();
            const el = pendingAcks[0];
            if (el && el.dataset.histIdx === undefined) return el;
            // The DOM fallback covers re-rendered transcripts and frames
            // whose local pending entry is gone (an item another tab sent).
            // Still-queued bubbles are excluded: their turn has NOT started,
            // so stamping one with the ack's history index (used by
            // resend/fork) would target a message that never ran. The ack of
            // an item that just drained is unaffected — the server's
            // queue_update(left) clears the mark before the user_acked
            // (FIFO on the same socket).
            return messagesDiv.querySelector('.message.user[data-pending-ack="1"]')
                || [...messagesDiv.querySelectorAll('.message.user:not([data-hist-idx]):not(.queued)')].at(-1);
        }

        /**
         * Drop the pending entries a `response` / `models` frame
         * terminates. The frame replies to the newest send processed no
         * later than its write: every entry at or before that send is
         * stale (an older entry's ack, if ever written, arrived before
         * the frame — same-socket FIFO — and left the queue). The cut is
         * the newest entry that cancelled a still-queued turn — its
         * terminal is this frame or the pending "cancelled" frame — or,
         * when no entry cancelled anything live, the newest entry
         * itself (rejection, or a command sent with no active turn).
         * Entries newer than the cut are live (their turns start after
         * the frame's write) and are kept.
         */
        function dropPendingForTerminal() {
            prunePendingAcks();
            let cut = pendingAcks.length - 1;
            for (let i = pendingAcks.length - 1; i >= 0; i--) {
                const target = cancelTarget.get(pendingAcks[i]);
                if (target && pendingAcks.includes(target)) { cut = i; break; }
            }
            for (let i = 0; i <= cut; i++) dropPendingAck(pendingAcks[0]);
        }
        // Error text of the most recent turn's failed model reply (a `response`
        // message whose content starts with "Error:"). Consumed by setTurnActive
        // to fire an error-specific turn-end notification instead of the generic
        // "Agent finished responding." one, and by handleResponse to notify
        // directly for failures that never became an active turn; cleared when
        // a new turn starts.
        let lastTurnError = null;
        let ws;
        let reconnectTimer;
        let wasDisconnected = false;
        // Reconnect delay doubles per failed attempt and resets on a
        // successful open, so a down server is probed at 3s, 6s, 12s, …
        // up to the 30s cap instead of hammered every 3s.
        const RECONNECT_BASE_MS = 3000;
        const RECONNECT_MAX_MS = 30000;
        let reconnectDelay = RECONNECT_BASE_MS;
        // (The sticky stick-to-bottom state and enableFollow live in
        // components/scroll.js; imported above.)
        let currentStreamDiv = null;
        let currentStreamRaw = '';
        // Last assistant bubble created by startStream in the current round.
        // Fallback for model_used when currentStreamDiv is null (the round's
        // stream already ended); reset at each round start so a round that
        // never produced a bubble cannot stamp a previous turn's reply.
        let lastStreamDiv = null;
        // Waiting placeholder: a visible transcript bubble for the silent
        // pre-first-token window (long prefill, muted streaming retry,
        // non-streaming fallback) so the user is not left with a blank
        // screen. Shown at round start, re-labelled on stream_retry,
        // removed when the first real token (thinking or content) lands.
        let waitingPlaceholder = null;
        let waitingPlaceholderTimer = null;
        let waitingPlaceholderBase = '';
        let waitingPlaceholderStart = 0;
        const STREAM_RENDER_INTERVAL = 32; // ms between renders (~2 frames at 60fps)
        let streamRafPending = false;
        let streamLastRender = 0;
        let currentThinkingDiv = null;
        let currentThinkingSpan = null;
        let currentThinkingRaw = '';
        let thinkingRafPending = false;
        let thinkingLastRender = 0;
        let pendingToolCards = {};
        // Cards of background jobs (execute_command background=true): their
        // output streams AFTER the tool result (the job outlives the turn),
        // so the card must stay reachable by term id once it leaves
        // pendingToolCards. Cleared with pendingToolCards at every reset.
        let bgTermCards = {};
        let streamingToolCards = {};
        let toolCallCounter = 0;
        let turnActive = false;
        let toolsStartedThisTurn = false;
        // Cumulative character offsets (server-stamped contentPos/thinkingPos)
        // of the content/thinking the client has rendered in the current
        // round. The rewind merge trims incoming chunks against these so a
        // chunk already inside the attach snapshot is never re-appended
        // (duplicate) and a chunk beyond it is never dropped (gap).
        let streamContentPos = 0;
        let thinkingContentPos = 0;
        // Raw + position of the most recently finalized thinking block, kept
        // across finalizeThinking so a rewind merge can still reach it.
        let lastFinalizedThinking = null;
        let contextBaseUsed = 0;
        let contextLimit = 0;
        let contextEstAdded = 0;
        // True once the server has resolved the session's context window
        // (a context echo carried contextLimit). While false the badge may
        // show the model-list seed (catalogContextLimit) instead.
        let contextLimitResolved = false;
        let lastContextData = null;
        // Coalesces per-token context badge updates into one rAF pass.
        let ctxEstimateRafPending = false;
        // Map toolCallId → args for history replay of patch_file diffs
        let historyToolCallArgs = {};
        // True while replayHistory() rebuilds the pane; suppresses per-message
        // smartScroll so the rebuild doesn't force O(n) layout passes.
        let replayInProgress = false;
        // Active-pane WS events that arrive while a chunked history replay is
        // in flight. The replay yields to rAF between batches, so live
        // stream/thinking/tool events could interleave mid-rebuild and garble
        // the transcript. They are buffered here and flushed in arrival order
        // after the replay (and its rewind merge) completes — the same
        // ordering the old single-task synchronous replay had, when no event
        // could run until the whole rebuild finished.
        let replayEventBuffer = [];
        // Connection-scoped frames whose handlers never touch the transcript
        // (the sidebar's session list, toasts, the board/automations panes,
        // settings-modal probe results). They must NOT wait for the replay:
        // the sidebar payload for a refresh arrives while the restored
        // transcript is still replaying, and buffering it left the sidebar
        // empty for the whole rebuild (the "sidebar loads slow on refresh"
        // symptom). Everything transcript-bearing (stream/thinking/tool/
        // history/response/clear_chat/config/context/session_state/…) stays
        // buffered.
        const REPLAY_IMMEDIATE_TYPES = new Set([
            'sessions', 'notice',
            'board_state', 'automations_state', 'automations_runs',
            'provider_test', 'mcp_test',
        ]);
        // A catalog reply (list_models, no content) is toolbar state for the
        // model picker and may render immediately; a /models list reply
        // WITH content is a transcript line and stays buffered.
        function replayImmediate(data) {
            if (REPLAY_IMMEDIATE_TYPES.has(data.type)) return true;
            return data.type === 'models' && !data.content;
        }
        // Screen-reader live region: announces turn lifecycle without
        // reading the streaming transcript token-by-token.
        const srLive = document.getElementById('sr-live');
        function announceLive(text) {
            if (srLive && text) srLive.textContent = text;
        }

        let _prevTurnActive = false;
        // Composer placeholder per state: while a turn is active, Enter
        // QUEUES the message — the placeholder says so before the keypress.
        const COMPOSER_PLACEHOLDER = 'Ask the agent\u2026 (type / for commands, Enter to send)';
        const QUEUE_PLACEHOLDER = 'Queue a message\u2026 (runs after the current turn)';
        function setTurnActive(active, opts) {
            const silent = !!(opts && opts.silent);
            const noMirror = !!(opts && opts.noMirror);
            const wasActive = _prevTurnActive;
            turnActive = !!active;
            if (turnActive && !wasActive) announceLive('Agent started responding.');
            if (cancelBtn) cancelBtn.disabled = !turnActive;
            // Send is always enabled: while a turn is active it QUEUES the
            // message (steering — runs after the current turn) instead of
            // cancelling. Interrupt is the Cancel button / Esc. The old
            // Send/Cancel CSS swap (which made send-while-busy mean
            // cancel-then-send) is gone; the .turn-active class now only
            // styles the busy state.
            if (sendBtn) {
                sendBtn.disabled = false;
                sendBtn.title = turnActive ? 'Queue message (runs after the current turn)' : 'Send';
            }
            inputArea.placeholder = turnActive ? QUEUE_PLACEHOLDER : COMPOSER_PLACEHOLDER;
            // Toggle class so the busy styling applies
            document.getElementById('input-area').classList.toggle('turn-active', turnActive);
            if (!active) {
                toolsStartedThisTurn = false;
            } else {
                // A new turn (or round) must not inherit the previous turn's
                // error: the turn-end notification reflects only the current
                // turn's outcome.
                lastTurnError = null;
            }
            // Keep the active pane object's mirror current (badges,
            // afterHistory's "resuming…" restore). clearChat passes noMirror
            // so its reset does not clobber the pane's state mid-switch.
            if (!noMirror) {
                const p = activePane();
                if (p) p.turnActive = turnActive;
            }
            // Live-refresh the sidebar session row when the active pane's
            // turn state flips, so the amber responding indicator appears the
            // moment a turn starts (previously the row only re-rendered on a
            // pane switch, a turn end, or a background-pane message — the
            // active pane's row went stale until the user switched sessions).
            if (!noMirror && wasActive !== turnActive) {
                refreshSidebarSessions();
            }
            // Turn-end notification on any active→inactive transition, except
            // user-initiated stops that immediately start a new turn
            // (send-while-busy / resend are tracked via suppressTurnEnds) and
            // silent state restores (pane focus, session_state replay).
            _prevTurnActive = turnActive;
            if (wasActive && !turnActive && suppressTurnEnds === 0 && !silent) {
                if (lastTurnError) {
                    announceLive('Agent stopped with an error.');
                    // The model stopped on an error: say so in the notification
                    // instead of the misleading "Agent finished responding.".
                    sendNotification('GoGen — Error', lastTurnError, 'gogen-turn-error');
                    lastTurnError = null;
                } else {
                    announceLive('Agent finished responding.');
                    sendNotification('GoGen', 'Agent finished responding.', 'gogen-turn-end');
                }
            }
        }

        function estimateTokenCount(text) {
            if (!text) return 0;
            // Rough char/4 estimate matching TUI streaming estimate spirit.
            return Math.max(1, Math.ceil(String(text).length / 4));
        }

        function bumpContextEstimate(delta) {
            if (!delta || (contextBaseUsed <= 0 && contextLimit <= 0)) return;
            contextEstAdded += estimateTokenCount(delta);
            // Batch the badge update to one rAF pass instead of a DOM write
            // per token. The server sends an authoritative context message at
            // turn end, which supersedes any pending estimate.
            if (ctxEstimateRafPending) return;
            ctxEstimateRafPending = true;
            requestAnimationFrame(() => {
                ctxEstimateRafPending = false;
                if (contextEstAdded === 0) return;
                const used = contextBaseUsed + contextEstAdded;
                const data = Object.assign({}, lastContextData || {}, {
                    usedTokens: used,
                    // Module state only — never lastContextData.contextLimit,
                    // which may hold the catalog seed (stamped by
                    // updateContextInfo for display). Feeding the seed back
                    // here would leak it into the live-estimation state and
                    // flip contextLimitResolved true (updateContextInfo reads
                    // it from this object) although the server never resolved
                    // the window. Display is unaffected: updateContextInfo
                    // re-derives the seed for the badge/tooltip from the
                    // catalog when contextLimit is absent.
                    contextLimit: contextLimit,
                    usedSource: 'estimated',
                    usedPercent: contextLimit > 0 ? used / contextLimit : 0,
                    toolTruncated: false,
                });
                updateContextInfo(data);
            });
        }

        // ── Multi-pane sessions ──
        // The UI renders one transcript at a time — the ACTIVE pane. Every
        // open pane keeps its control state (turnActive, pending flags,
        // context data) in `panes`; the module-level state variables above
        // are mirrors of the active pane. Switching panes saves the mirrors,
        // swaps which pane is active, then re-attaches so the server resends
        // session_state/history/config/context for the focused session — the
        // transcript is re-derived from the server (the accepted §4 caveat:
        // a mid-turn pane shows the completed history, then live events).
        const panes = new Map(); // paneKey -> pane
        let nextPaneKey = 0;
        let activePaneKey = 0;

        // initialActivity seeds the output-recency stamp. Pass a saved
        // session's updatedAt (epoch ms) when opening EXISTING history as a
        // pane: activation must not fabricate recency, so the row keeps its
        // earned position in the sidebar and moves again only on new output.
        // Omit for genuinely new sessions (/new, sidebar New, /fork) —
        // creation is their first recency event and they appear at the top
        // once.
        function makePane(initialActivity) {
            const pane = {
                key: nextPaneKey++,
                id: null, // session id (null until known)
                label: '',
                turnActive: false,
                // Latched when this pane is attached to a session whose turn
                // is RUNNING (mid-turn focus / reconnect): the attach history
                // snapshot lacks the in-flight reply, so on turn_end we
                // re-attach once to converge the transcript.
                needsFreshHistory: false,
                // The DOM's data-hist-idx stamps are stale for this pane
                // (a compaction reshaped the server's indexes and no
                // rebuild has landed since): edit/resend/fork are gated
                // until the post-compact convergence attach re-stamps them.
                // Cleared by the full history rebuild; a missed compacting
                // frame self-heals on focus via the server's bumped epoch.
                indexesDirty: false,
                // historyEpoch of the last history payload applied to this
                // pane's transcript (0 = the session was never reshaped;
                // undefined = no payload applied yet). Compared against an
                // attach snapshot's historyEpoch to tell a stale snapshot
                // from a reshaped history, and sent back on session_attach
                // (with the DOM's newest rendered index) so the server can
                // skip the history snapshot entirely when the pane is
                // already current (conditional attach).
                histEpoch: undefined,
                // Highest server message index this pane has APPLIED to its
                // transcript (-1 = none yet): advanced by history replays,
                // user_acked, and the hist_index frames the server sends for
                // each appended assistant/tool message. It is the pane's own
                // lower bound of what is rendered, so the turn-end
                // convergence attach can prove the transcript is current and
                // skip the history snapshot. Only the pane that is actively
                // rendering the turn advances it (a background pane's DOM is
                // stale until focused), and a mismatch still falls back to
                // the full snapshot.
                renderedHistIdx: -1,
                // Offscreen transcript cache captured when this pane was
                // switched away from while settled: { container, sessionId,
                // scrollTop, pinned, historyToolCallArgs }.
                // Focusing the pane restores the DOM directly (no server
                // round-trip, no replay) and the attach re-verifies the
                // fingerprint server-side; null = no cache.
                domCache: null,
                // Session id whose turn_end must be ignored: set when the
                // user resumes another session while this pane's session has
                // a running turn. The old session keeps running headless
                // (resume does not cancel it), so its turn_end must not clear
                // the pending flag or block the resumed session's
                // convergence. Cleared when the reply re-keys the pane.
                ignoreTurnEndsFor: null,
                suppressTurnEnds: 0,
                pendingSessionResponse: false,
                pendingResendContent: null,
                pendingResendImages: null,
                resendAwaitingHistory: false,
                compacting: false,
                lastContextData: null,
                contextBaseUsed: 0,
                contextLimit: 0,
                contextEstAdded: 0,
                contextLimitResolved: false,
                nearCompactDismissed: false,
                mode: 'act',
                thinkingLevel: 'off',
                reasoningEffortsUnsupported: false,
                model: '',
                _notifiedBusy: false,
                // Epoch ms of this pane's latest OUTPUT event (stream/tool/
                // turn markers). Drives sidebar session ordering (newest
                // output first); focusing or activating a saved session
                // does NOT reorder.
                lastActivity: initialActivity || Date.now(),
            };
            panes.set(pane.key, pane);
            refreshSidebarSessions();
            return pane;
        }

        function activePane() {
            return panes.get(activePaneKey);
        }

        function findPaneBySession(id) {
            if (!id) return null;
            for (const pane of panes.values()) {
                if (pane.id === id) return pane;
            }
            return null;
        }

        // The open pane with the newest output stamp, or null. The panes
        // map holds creation order (the sidebar sorts by output recency —
        // focusing no longer reorders), so close/detach fallbacks that used
        // to rely on map-front == MRU scan stamps instead.
        function mostRecentlyActivePane() {
            let best = null;
            for (const p of panes.values()) {
                if (!best || (p.lastActivity || 0) > (best.lastActivity || 0)) best = p;
            }
            return best;
        }

        // Route a session-scoped server message to the pane it belongs to.
        // Returns null when the message is for a session the client does not
        // have open (the connection is only attached to open panes, so this
        // is rare — e.g. the server's default-session handshake after a
        // reconnect where the default session is not an open pane).
        function paneForMessage(data) {
            const sid = data.sessionId;
            if (sid) {
                const act = activePane();
                const activeInFlight = !!(act && pendingSessionResponse);
                // A session command's reply (response with a sessionAction,
                // e.g. clear_chat for /new, /resume, /fork) carries the NEW
                // session id before the pane has adopted it — the response
                // handler re-keys the pane so the follow-up clear_chat/
                // history/config route correctly.
                // Route it to the pane that initiated the change: normally
                // the active pane (its mirror lives in the module-level
                // pendingSessionResponse), but the user may have switched
                // panes while the reply was in flight — the initiator's flag
                // lives on the pane object, which the module mirror no longer
                // reflects after a switch. (Two simultaneous in-flight
                // changes across panes can still interleave ambiguously —
                // each change is one round-trip, so this is a sub-second
                // race.)
                // The initiator is checked BEFORE a pane that already holds
                // the reply's session id: the latter is a different pane
                // (e.g. "resume latest" resolving to a session open
                // elsewhere) and must not steal the reply from the pane that
                // initiated the change, or that pane's in-flight flag would
                // stay stuck forever.
                if (data.type === 'response' && data.sessionAction) {
                    if (activeInFlight) return act;
                    for (const pane of panes.values()) {
                        if (pane !== act && pane.pendingSessionResponse) return pane;
                    }
                    return act;
                }
                const p = findPaneBySession(sid);
                if (p) return p;
                // The server sends history before config in a session change,
                // so the pane's id is stale until config adopts the new one.
                // While a session change is in flight, unknown ids belong to
                // the pane with the pending change (the follow-ups also
                // include the session_state resume now sends before its
                // reply).
                if (activeInFlight) return act;
                for (const pane of panes.values()) {
                    if (pane !== act && pane.pendingSessionResponse) return pane;
                }
                if (act && act.id === null) return act;
                return null;
            }
            return activePane();
        }

        function saveActivePaneState() {
            const pane = activePane();
            if (!pane) return;
            pane.turnActive = turnActive;
            pane.suppressTurnEnds = suppressTurnEnds;
            pane.pendingSessionResponse = pendingSessionResponse;
            pane.pendingResendContent = pendingResendContent;
            pane.pendingResendImages = pendingResendImages;
            pane.resendAwaitingHistory = resendAwaitingHistory;
            pane.compacting = compacting;
            pane.lastContextData = lastContextData;
            pane.contextBaseUsed = contextBaseUsed;
            pane.contextLimit = contextLimit;
            pane.contextEstAdded = contextEstAdded;
            pane.contextLimitResolved = contextLimitResolved;
            pane.nearCompactDismissed = nearCompactDismissed;
        }

        function loadActivePaneState() {
            const pane = activePane();
            if (!pane) return;
            turnActive = !!pane.turnActive;
            suppressTurnEnds = pane.suppressTurnEnds;
            pendingSessionResponse = pane.pendingSessionResponse;
            pendingResendContent = pane.pendingResendContent;
            pendingResendImages = pane.pendingResendImages;
            resendAwaitingHistory = pane.resendAwaitingHistory;
            // A restored still-pending resend needs its timeout back (the
            // old one fired no-op'd while another pane was active, or was
            // never armed for this pane); an idle restore drops any timer
            // that could fire against this pane's state.
            if (resendAwaitingHistory) armResendWatchdog();
            else { clearTimeout(resendWatchdog); resendWatchdog = 0; }
            compacting = pane.compacting;
            lastContextData = pane.lastContextData;
            contextBaseUsed = pane.contextBaseUsed;
            contextLimit = pane.contextLimit;
            contextEstAdded = pane.contextEstAdded;
            // The flag mirrors the active pane too: a stale value across a
            // switch could make the handleModels re-seed stamp the catalog
            // seed onto a resolved pane (or skip an unresolved one) before
            // the re-attach context echo corrects it.
            contextLimitResolved = !!pane.contextLimitResolved;
            nearCompactDismissed = !!pane.nearCompactDismissed;
            _prevTurnActive = turnActive;
            if (cancelBtn) cancelBtn.disabled = !turnActive;
            if (sendBtn) sendBtn.title = turnActive ? 'Queue message (runs after the current turn)' : 'Send';
            inputArea.placeholder = turnActive ? QUEUE_PLACEHOLDER : COMPOSER_PLACEHOLDER;
            document.getElementById('input-area').classList.toggle('turn-active', turnActive);
            if (!turnActive) toolsStartedThisTurn = false;
        }

        // Apply the per-session toolbar mirrors (mode, thinking level,
        // model, description, label) from a config/context echo. Used for
        // both the active pane (connect config handler) and background
        // panes (handleBackgroundMessage) so pane switches re-render the
        // correct chips and tooltip.
        function applyPaneMeta(pane, data) {
            pane.mode = data.mode || pane.mode;
            pane.thinkingLevel = data.thinkingLevel || pane.thinkingLevel;
            pane.reasoningEfforts = data.reasoningEfforts || pane.reasoningEfforts;
            pane.modelDescription = data.modelDescription || pane.modelDescription;
            // The unsupported flag is omitted when false (JSON omitempty): a
            // config echo for a DIFFERENT model without the flag means the
            // new model is supported or unknown — a stale "unsupported" from
            // the previous model must not linger. Read pane.model BEFORE the
            // update below so the comparison sees the previous model.
            if (data.reasoningEffortsUnsupported !== undefined) {
                pane.reasoningEffortsUnsupported = !!data.reasoningEffortsUnsupported;
            } else if (data.model && data.model !== pane.model) {
                pane.reasoningEffortsUnsupported = false;
            }
            pane.model = data.model || pane.model;
            if (data.sessionLabel) pane.label = data.sessionLabel;
        }

        // Release a session this connection no longer has open, but only
        // when no other pane still holds it: detaching a session a
        // background pane still shows would orphan that pane's event
        // stream. No-op when already detached or the socket is down.
        function sendSessionDetach(oldId) {
            if (oldId && !findPaneBySession(oldId) && ws && ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ type: 'session_detach', sessionId: oldId }));
            }
        }

        // A pending session change's reply names `id`: adopt it on the
        // initiator pane — drop the outgoing session's transcript cache and
        // epoch (they describe a different history) and release its
        // attachment. Returns true when the id was adopted; false when the
        // pane already holds it or another pane owns it (that reply resolves
        // through the focus-pane path instead). Shared by the active and
        // background handlers: a resume whose pane goes to the background
        // mid-flight must still re-key, or its pending flag would strand and
        // block every later session change.
        function adoptPendingSession(pane, id) {
            if (!pane || !id || pane.id === id) return false;
            const other = findPaneBySession(id);
            if (other && other !== pane) return false;
            const oldId = pane.id;
            pane.id = id;
            pane.domCache = null;
            pane.histEpoch = undefined;
            pane.renderedHistIdx = -1;
            pane.ignoreTurnEndsFor = null;
            releaseStrandedSuppress(pane);
            if (pane === activePane() && sessionInfoDiv) sessionInfoDiv.textContent = id;
            sendSessionDetach(oldId);
            return true;
        }

        // Sidebar open-pane ordering is by OUTPUT recency: a pane climbs
        // only when it produced newer output than the rest. Focus is NOT
        // activity — clicking a row highlights it without reordering — so
        // only output-bearing message types stamp the pane. Attach/config/
        // context echoes fire on every focus/switch and must not reorder.
        const OUTPUT_ACTIVITY_TYPES = new Set([
            'thinking', 'waiting', 'thinking_token',
            'stream', 'stream_end',
            'stream_retry',
            'tool_call_start', 'tool_call_delta', 'tool_call', 'tool_execute', 'tool_result',
            'term_opened', 'term_output', 'term_exit',
            'turn_end', 'cancelled',
        ]);

        function touchPaneActivity(pane, type) {
            if (pane && OUTPUT_ACTIVITY_TYPES.has(type)) pane.lastActivity = Date.now();
        }

        // Apply a session-scoped message to a background pane without touching
        // the DOM: turn/badge state, context data, and config mirrors. The
        // transcript is re-derived from the server when the pane is focused.
        function handleBackgroundMessage(pane, data) {
            const beforeTurn = pane.turnActive;
            const beforeLabel = pane.label;
            const beforeId = pane.id;
            switch (data.type) {
                case 'thinking':
                case 'waiting':
                case 'thinking_token':
                case 'stream':
                case 'stream_end':
                case 'tool_call_start':
                case 'tool_call_delta':
                case 'tool_call':
                case 'tool_execute':
                case 'tool_result':
                case 'term_opened':
                case 'term_output':
                case 'term_exit':
                    pane.turnActive = true;
                    break;
                case 'cancelled':
                    pane.turnActive = false;
                    // No decrement here: the active pane's cancelled handler
                    // also ignores the message (it returns early), and the
                    // paired turn_end below is the single decrement point for
                    // a cancel cycle. Decrementing on BOTH left the pane's
                    // counter at -1 when the pane went background between the
                    // cancel and the terminal messages, disabling suppression
                    // for the pane's next cancel (send-while-busy / resend).
                    break;
                case 'turn_end':
                    pane.turnActive = false;
                    if (pane.suppressTurnEnds > 0) pane.suppressTurnEnds--;
                    break;
                case 'session_state':
                    // A resume reply can land on this pane while it is in the
                    // background (the user switched away mid-flight): adopt
                    // the target id exactly like the active handler — the
                    // transcript re-derives when the pane is focused — and
                    // settle the pending change, or the pane's flag would
                    // strand and block every later session change.
                    if (data.sessionId && pane.pendingSessionResponse) {
                        if (pane.id !== null) adoptPendingSession(pane, data.sessionId);
                        pane.pendingSessionResponse = false;
                        pane.ignoreTurnEndsFor = null;
                    }
                    // An id-less pane (the command was issued before this
                    // pane ever adopted a session) has no old id to release
                    // and no cache to drop: adopt the id directly, exactly
                    // like the active handler's fallback — otherwise the
                    // history/config frames that follow route to no pane and
                    // the transcript never re-derives.
                    if (data.sessionId && pane.id === null) {
                        pane.id = data.sessionId;
                    }
                    pane.turnActive = !!data.turnActive;
                    // Keep the convergence latch in sync with the active
                    // pane's handler: an idle attach means the snapshot is
                    // complete, a busy attach needs the turn-end refetch.
                    pane.needsFreshHistory = !!pane.turnActive;
                    // Seed the pane's queue mirror from server truth (the
                    // transcript re-derives from the server when focused,
                    // which re-renders the queued bubbles then).
                    applyQueueUpdate(pane, data, false);
                    break;
                case 'queue_update':
                    applyQueueUpdate(pane, data, false);
                    break;
                case 'context':
                    pane.lastContextData = data || pane.lastContextData;
                    if (data.sessionLabel) pane.label = data.sessionLabel;
                    if (data.usedSource !== 'estimated') {
                        pane.contextBaseUsed = data.usedTokens || 0;
                        pane.contextLimit = data.contextLimit || 0;
                        pane.contextEstAdded = 0;
                    }
                    break;
                case 'config':
                    applyPaneMeta(pane, data);
                    // A resend whose reply sequence (response → clear_chat →
                    // history → config) completed while this pane was in the
                    // background: the active pane's config handler never ran,
                    // so the pending message was never flushed. The response
                    // case above already adopted the new session id, so send
                    // the message on the wire now — the transcript re-derives
                    // on focus and the server's history snapshot then contains
                    // both the message and its reply. No DOM work here: a
                    // background pane's transcript is rebuilt on focus.
                    if (pane.resendAwaitingHistory) {
                        const text = pane.pendingResendContent;
                        const images = pane.pendingResendImages;
                        pane.resendAwaitingHistory = false;
                        pane.pendingResendContent = null;
                        pane.pendingResendImages = null;
                        if (text && ws && ws.readyState === WebSocket.OPEN) {
                            const bgPayload = { type: 'message', content: text, sessionId: pane.id };
                            if (images && images.length > 0) bgPayload.images = images;
                            ws.send(JSON.stringify(bgPayload));
                        }
                    }
                    break;
                case 'clear_chat':
                    // Background panes have no DOM; the re-attach on focus
                    // rebuilds the transcript. Just clear stale flags.
                    pane.pendingSessionResponse = false;
                    break;
                case 'response':
                    pane.pendingSessionResponse = false;
                    pane.ignoreTurnEndsFor = null;
                    // A response completes the pane's pending operation. The
                    // active pane's handler clears the compacting flag here;
                    // a compact that finishes while this pane is in the
                    // background must do the same, or the restored state on
                    // focus would block future compacts and mis-handle the
                    // next normal response (toast + duplicate system message).
                    pane.compacting = false;
                    // An error reply to the pane's own session change (e.g. a
                    // fork/new from a resend that failed) must cancel a
                    // pending resend like the active pane's Error branch does;
                    // otherwise the config flush below would send the pending
                    // message to the old session anyway.
                    if (String(data.content || '').startsWith('Error:')) {
                        pane.pendingResendContent = null;
                        pane.pendingResendImages = null;
                        pane.resendAwaitingHistory = false;
                    }
                    // A session command's reply (typed /new, /resume, /fork)
                    // with a sessionAction carries the NEW session id. When
                    // this pane initiated the change but is now a background
                    // pane (the user switched away while the reply was in
                    // flight), still adopt the id and release the old
                    // session's attachment — the transcript re-derives on
                    // focus. Refuse to adopt an id another pane already
                    // holds, and never detach a session another pane still
                    // has open (that would orphan the other pane's event
                    // stream).
                    if (data.sessionAction && data.sessionId && pane.id !== data.sessionId) {
                        const other = findPaneBySession(data.sessionId);
                        if (!other || other === pane) {
                            const oldId = pane.id;
                            pane.id = data.sessionId;
                            // The pane now belongs to a different session: its
                            // transcript cache / epoch describe the old one.
                            pane.domCache = null;
                            pane.histEpoch = undefined;
                            pane.renderedHistIdx = -1;
                            sendSessionDetach(oldId);
                        }
                    }
                    break;
                case 'history':
                    // Ignored — the transcript re-derives on focus.
                    break;
                case 'delete_approval':
                    // A background pane's turn needs an approval: surface the
                    // modal (it pauses input anyway) with the session tagged
                    // so the response routes correctly.
                    showDeleteApproval(data);
                    break;
                case 'command_approval':
                    // A blocked shell command escalated to the user: reuse the
                    // approval modal with the session tagged for correct routing.
                    showCommandApproval(data);
                    break;
                default:
                    break;
            }
            if (pane.turnActive && !pane._notifiedBusy) {
                pane._notifiedBusy = true;
                sendNotification('GoGen', `Pane ${pane.label || pane.id || 'session'} is responding.`, 'gogen-pane-busy');
            } else if (!pane.turnActive) {
                pane._notifiedBusy = false;
            }
            // Re-render the sidebar session rows only when something the
            // list shows actually changed (label / id / busy state) — stream
            // chunks for a background pane would otherwise rebuild the list
            // on every token.
            if (pane.turnActive !== beforeTurn || pane.label !== beforeLabel || pane.id !== beforeId) {
                refreshSidebarSessions();
            }
        }

        // Hand the transcript over from the outgoing (still-active) pane to
        // `pane` and make it the active one: save the outgoing pane's flags,
        // cache its rendered transcript when it is settled, re-key the
        // module state to the incoming pane and load its saved flags. Every
        // pane switch funnels through here (focusPane, replaceActivePane,
        // openSessionPane, forkSession, newSession); the save/capture are
        // no-ops when the outgoing pane was already deleted (closePane →
        // replaceActivePane). See the per-pane transcript cache above for
        // why the cache-vs-clear decision matters.
        function transitionToPane(pane) {
            saveActivePaneState();
            const outgoing = activePane();
            // Remember the outgoing pane's reading position BEFORE its DOM
            // is cached or wiped — the anchor measurement needs live
            // layout (see capturePaneScrollAnchor).
            capturePaneScrollAnchor(outgoing);
            // Cache the outgoing pane's transcript when it is settled; only
            // non-cacheable panes (mid-turn, pending acks/change) and the
            // already-deleted outgoing pane (closePane) pay the old
            // clear-and-rederive cost. The cache path still resets the
            // module-level state (it belongs to the outgoing pane) — the
            // DOM itself moves into the cache instead of being wiped.
            if (!outgoing || !cachePaneTranscript(outgoing)) {
                clearChat();
            } else {
                resetChatTranscriptState();
            }
            activePaneKey = pane.key;
            // Load the incoming pane's state AFTER the outgoing state was
            // saved/reset so the restored flags survive.
            loadActivePaneState();
        }

        // Make a pane the active/visible one. The transcript is restored
        // from the pane's offscreen cache when one was captured (settled
        // pane) and re-attached so the server resends the pane's state —
        // with the pane's history fingerprint, so an unchanged session
        // skips the history snapshot entirely (conditional attach).
        // On phones the chat sidebar is an overlay drawer (styles.css
        // <=768px). Any in-sidebar action that switches the chat — selecting
        // a session, focusing a pane, starting a new one — must dismiss it:
        // the tap lands INSIDE #sidebar, so the outside-click closer never
        // fires and the drawer would otherwise stay over the transcript (and
        // focusPane's inputArea.focus() would raise the keyboard under it).
        function closeMobileSidebar() {
            if (window.innerWidth > 768) return;
            document.getElementById('sidebar')?.classList.remove('open');
        }

        function focusPane(key) {
            closeMobileSidebar();
            if (key === activePaneKey) return;
            const pane = panes.get(key);
            if (!pane) return;
            transitionToPane(pane);
            let restored = false;
            if (!restorePaneTranscript(pane)) {
                ensureEmptyState();
            } else {
                restored = true;
                // The restored DOM predates any queue change made while this
                // pane was backgrounded (queue frames for a background pane
                // update the mirror only — never the DOM), so re-derive the
                // queued bubbles from the mirror: without this a queued
                // message stays invisible until the next queue frame.
                rerenderQueuedBubbles(pane);
            }
            // Mode/thinking/model are per-session; restore the toolbar to
            // this pane's last-known values.
            if (pane.mode) updateModeInfo(pane.mode);
            if (pane.thinkingLevel || pane.reasoningEffortsUnsupported) {
                renderThinkingChips(pane.thinkingLevel, pane.reasoningEfforts, pane.reasoningEffortsUnsupported);
            }
            if (pane.model) updateModelInfo(pane.model, pane.modelDescription);
            if (sessionInfoDiv) sessionInfoDiv.textContent = pane.id || '';
            refreshSidebarSessions();
            if (pane.id && ws && ws.readyState === WebSocket.OPEN) {
                const attachMsg = { type: 'session_attach', sessionId: pane.id };
                if (restored && pane.histEpoch !== undefined) {
                    // Conditional attach: tell the server what this pane has
                    // already rendered (last payload's epoch + the newest
                    // rendered index). The server skips the history snapshot
                    // while the session is unchanged and ships a rewind-only
                    // frame when just an in-flight round is missing.
                    const newestIdx = newestDomHistIdx();
                    if (newestIdx >= 0) {
                        attachMsg.knownHistoryEpoch = pane.histEpoch;
                        attachMsg.knownHistoryIndex = newestIdx;
                    }
                }
                ws.send(JSON.stringify(attachMsg));
            }
            // The click moved focus to the sidebar row (or the pane switch
            // came from a UI action): hand it back to the composer so the
            // user can type without re-clicking the input.
            inputArea.focus();
        }

        // A session command named a session another pane already holds:
        // focus that pane instead of re-keying this one onto a duplicate
        // (two panes for one session desync each other's transcript and
        // event routing). Shared by the typed /resume interception and the
        // session-change reply handler (the palette's "resume latest" can
        // resolve server-side to a session already open here). Focusing
        // also re-attaches the session, which re-syncs the connection's
        // current pane with the server. Returns true when it switched.
        function focusSessionInOtherPane(id) {
            const other = findPaneBySession(id);
            if (!other || other === activePane()) return false;
            showToast('Session already open in another pane — switched to it', 'info');
            focusPane(other.key);
            return true;
        }

        // Create a fresh active pane: the sidebar "New" / /new / session
        // removal paths all need a new id-less pane adopted by the server's
        // next session_new reply. `pendingSessionResponse` gates the reply
        // so the config handler re-keys the pane.
        function replaceActivePane() {
            const p = makePane();
            // Same transcript-cache decision as focusPane: a settled pane
            // (e.g. /new typed into an idle session) keeps its transcript
            // cacheable; makePane already refreshed the sidebar with the
            // new pane first, which does not touch the transcript DOM.
            transitionToPane(p);
            ensureEmptyState();
            if (ws && ws.readyState === WebSocket.OPEN) {
                pendingSessionResponse = true;
                ws.send(JSON.stringify({ type: 'session_new' }));
            }
        }

        // Close a pane: the session is explicitly closed — the server
        // cancels any in-flight turn and unregisters the runtime (the
        // session stays saved server-side) — then remove it from the UI.
        // Reopening it later reloads it from the saved list.
        function closePane(key) {
            const pane = panes.get(key);
            if (!pane) return;
            if (pane.id && ws && ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ type: 'session_close', sessionId: pane.id }));
            }
            panes.delete(key);
            // The pane is explicitly closed: its scroll anchor (if any)
            // described the last reading position of a session the user
            // walked away from — reopening starts fresh at the bottom.
            if (pane.id) paneScrollAnchors.delete(pane.id);
            if (activePaneKey === key) {
                if (panes.size === 0) {
                    replaceActivePane();
                } else {
                    // Newest-output remaining pane (the map holds creation
                    // order now that MRU reordering is gone).
                    focusPane(mostRecentlyActivePane().key);
                }
            } else {
                refreshSidebarSessions();
            }
            // The closed session is saved, not deleted: refresh the list so
            // the closed session's row reappears (and, if the active pane
            // was closed, the new active pane is marked).
            requestSessionList();
        }

        // Open (or focus) a session as the active pane — used by the sidebar
        // saved-sessions list. The session is attached (loaded from the store
        // when not currently active) and the transcript re-derives from the
        // server's attach response.
        function openSessionPane(id) {
            closeMobileSidebar();
            if (!ensureConnected()) return;
            if (!id) return;
            if (resendAwaitingHistory) {
                showToast('Resend already in progress', 'info');
                return;
            }
            const existing = findPaneBySession(id);
            if (existing) {
                focusPane(existing.key);
                return;
            }
            // Seed the pane's recency stamp from the session's REAL last
            // activity: activating a SAVED session must not jump it to the
            // top of the sidebar. Its row keeps its earned position and
            // moves again only when the session produces new output.
            const savedEntry = (getLastSessions() || []).find((s) => s.id === id);
            const seeded = savedEntry && savedEntry.updatedAt ? Date.parse(savedEntry.updatedAt) : NaN;
            const pane = makePane(Number.isNaN(seeded) ? undefined : seeded);
            pane.id = id;
            // Same outgoing-pane transcript handling as focusPane: cache the
            // pane we are leaving (if settled) instead of wiping it.
            transitionToPane(pane);
            // makePane refreshed the sidebar while the id was still unknown.
            // Refresh again so the new active row is marked "current": the
            // attach reply's config echo carries the SAME id, so the config
            // handler would not re-render (id unchanged) and the row would
            // stay stale until the next pane switch.
            refreshSidebarSessions();
            ensureEmptyState();
            if (sessionInfoDiv) sessionInfoDiv.textContent = id;
            pendingSessionResponse = false;
            ws.send(JSON.stringify({ type: 'session_attach', sessionId: id }));
            requestSessionList();
            // Same as focusPane: the sidebar click took focus away from the
            // composer — return it so the user can type immediately.
            inputArea.focus();
        }

        function abortInFlightUI(message) {
            finalizeThinking();
            endStream();
            setInputProgress(null);
            // Remove waiting spinners on pending cards
            for (const id of Object.keys(pendingToolCards)) {
                const cardInfo = pendingToolCards[id];
                if (cardInfo && cardInfo.waiting) {
                    cardInfo.waiting.remove();
                    cardInfo.waiting = null;
                }
            }
            for (const idx of Object.keys(streamingToolCards)) {
                const cardInfo = streamingToolCards[idx];
                if (cardInfo && cardInfo.argsStream) {
                    cardInfo.argsStream.classList.remove('cursor');
                }
            }
            streamingToolCards = {};
            // Keep completed pending cards; clear only in-flight stream map
            if (message) appendMessage('system', message);
        }

        // ===== Message timestamps =====
        // Single relative-time helper for messages and session rows. `now` is
        // passed in by the periodic refresh so all timestamps tick together
        // without re-reading the clock per element.
        function formatRelativeTime(date, now = Date.now()) {
            const diff = Math.max(0, Math.floor((now - date.getTime()) / 1000));
            if (diff < 5) return 'now';
            if (diff < 60) return `${diff}s ago`;
            if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
            if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
            if (diff < 604800) return `${Math.floor(diff / 86400)}d ago`;
            return date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
        }

        function relativeTime(value, now = Date.now()) {
            if (!value) return '';
            const date = value instanceof Date ? value : new Date(value);
            if (Number.isNaN(date.getTime())) return '';
            return formatRelativeTime(date, now);
        }

        function formatExactTime(date) {
            return date.toLocaleString(undefined, {
                year: 'numeric', month: 'short', day: 'numeric',
                hour: '2-digit', minute: '2-digit'
            });
        }

        // Relative labels ("now", "3m ago") go stale; re-derive them on a
        // light cadence (and when the tab becomes visible again) so the
        // transcript stays truthful without re-rendering anything.
        function refreshMessageTimestamps() {
            if (document.hidden) return;
            const now = Date.now();
            for (const el of messagesDiv.querySelectorAll('.message-time')) {
                const msg = el.closest('.message');
                if (!msg || !msg.dataset.createdAt) continue;
                const t = Date.parse(msg.dataset.createdAt);
                if (Number.isNaN(t)) continue;
                const text = formatRelativeTime(new Date(t), now);
                if (el.textContent !== text) el.textContent = text;
            }
        }
        // Same staleness class for the sidebar session rows: their relative
        // times are computed at render time, so re-derive them in place on
        // the same cadence without re-rendering the list.
        function refreshSessionRowTimes() {
            if (document.hidden || !sessionListDiv) return;
            const now = Date.now();
            for (const el of sessionListDiv.querySelectorAll('.session-row-time')) {
                const upd = el.dataset.updated;
                if (!upd) continue;
                const text = relativeTime(upd, now);
                if (el.textContent !== text) el.textContent = text;
            }
        }
        setInterval(() => { refreshMessageTimestamps(); refreshSessionRowTimes(); }, 30000);
        document.addEventListener('visibilitychange', () => { refreshMessageTimestamps(); refreshSessionRowTimes(); });

        function addTimestampToMsg(msgDiv, date) {
            if (msgDiv.querySelector('.message-time')) return; // already added
            const timeEl = document.createElement('span');
            timeEl.className = 'message-time';
            timeEl.textContent = formatRelativeTime(date);
            timeEl.title = formatExactTime(date);
            msgBody(msgDiv).appendChild(timeEl);
        }
        // === Reply-model attribution (Settings → Chat → "Show reply model") ===
        // The provider-reported model is kept in dataset.model regardless of
        // the preference so toggling the setting later can show chips on
        // already-rendered bubbles without refetching history. The
        // preference itself lives in components/settings.js
        // (getShowReplyModelPref).
        // Adds or removes the model chip on one assistant message element.
        function applyReplyModelChip(msgDiv) {
            if (!msgDiv || !msgDiv.dataset) return;
            const model = msgDiv.dataset.model || '';
            const chip = msgDiv.querySelector('.msg-model');
            if (!model || !getShowReplyModelPref()) {
                if (chip) chip.remove();
                return;
            }
            if (chip) return; // already shown
            const newChip = document.createElement('span');
            newChip.className = 'msg-model';
            newChip.textContent = '· ' + model;
            newChip.title = 'Replied with ' + model;
            // Appended last: sits below the timestamp at the bubble's
            // bottom-right, mirroring the .message-time footer.
            msgBody(msgDiv).appendChild(newChip);
        }

        // Syncs chips on every already-rendered assistant bubble after the
        // preference changes (this tab or another via the storage event).
        function applyReplyModelChips() {
            for (const el of document.querySelectorAll('.message.assistant[data-model]')) {
                applyReplyModelChip(el);
            }
        }
        // ==============================

        /** The server-side message index a fork button sends for its bubble.
         *  It is read at CLICK time, never captured when the button is
         *  created: the server stamps a live bubble with hist_index when its
         *  round is appended (handleHistIndex), which can land after the
         *  button already exists.
         *  -1 is the only safe fallback — the server reads it as "fork from
         *  the last assistant message that produced output". Never fall back
         *  to dataset.msgIdx: that is a page-local bubble counter, not a
         *  server index, so forking at it silently truncates at the WRONG
         *  message. */
        function forkTargetIndex(msgDiv) {
            const idx = parseInt(msgDiv.dataset.histIdx, 10);
            return Number.isFinite(idx) && idx >= 0 ? idx : -1;
        }

        /** Send a fork request.
         *  The target is the clicked bubble's server-side message index, or
         *  -1 ("last assistant") when the bubble has none (see
         *  forkTargetIndex).
         *  The fork opens a NEW pane with the fork's history; the
         *  source session is left untouched. */
        function forkSession(messageIndex) {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            if (resendAwaitingHistory) {
                showToast('Resend already in progress', 'info');
                return;
            }
            if (pendingSessionResponse) {
                showToast('Session change already in progress', 'info');
                return;
            }
            // Same stale-index gate as beginResend: a compaction reshaped
            // the server's indexes since this DOM was stamped.
            if (activePane() && activePane().indexesDirty) {
                showToast('Transcript indexes are refreshing after compaction — try again in a moment', 'info');
                return;
            }
            const srcId = activePane().id;
            // Open a new pane; the source pane stays open in the background.
            const pane = makePane();
            // Same outgoing-pane transcript handling as focusPane: cache the
            // source pane (if settled) instead of wiping it.
            transitionToPane(pane);
            ensureEmptyState();
            pendingSessionResponse = true;
            ws.send(JSON.stringify({ type: 'session_fork', messageIndex, sessionId: srcId }));
        }

        function cancelActiveTurn() {
            if (!turnActive || !ws || ws.readyState !== WebSocket.OPEN) return;
            if (!activePane() || !activePane().id) return;
            suppressTurnEnds++;
            // Record the cancelled turn's pending-ack entry (the newest
            // send, if still awaiting its ack) so the trailing
            // "cancelled" frame can drop it: a turn cancelled before the
            // server emitted user_acked never acks, and its stale flag
            // would otherwise steal the next ack. lastCancelTarget is
            // picked up by the markPendingAck of the send that follows
            // (every cancelActiveTurn here precedes one).
            lastCancelTarget = pendingAcks.length ? pendingAcks[pendingAcks.length - 1] : null;
            pendingCancels.push(lastCancelTarget);
            ws.send(JSON.stringify({ type: 'cancel', sessionId: activePane().id }));
            setTurnActive(false);
            abortInFlightUI(null);
            // The trailing cancelled + turn_end frames for the old turn are
            // consumed by suppressTurnEnds, so they can no longer reset the
            // page title for us — do it here (covers the cancel-button path,
            // which previously relied on the trailing turn_end for this).
            updateTitle('idle');
        }

        // A successful session re-key strands every suppressTurnEnds unit
        // still held for the OLD session: paneForMessage drops the old
        // turn's trailing cancelled/turn_end frames once no pane holds that
        // id, so the units can never be consumed — the NEXT turn_end would
        // be swallowed and turnActive would stick true (busy chrome, dead
        // edit guard). Frames that arrived before the re-key already
        // consumed their unit, so whatever is still held at re-key time is
        // a leak by definition. Re-keys that KEEP the old session (the
        // focus-pane / rekeySkipped paths) must not call this: their old
        // session's frames still route here and still need their unit.
        function releaseStrandedSuppress(pane) {
            if (pane) pane.suppressTurnEnds = 0;
            if (!pane || pane === activePane()) suppressTurnEnds = 0;
        }

        function clearPendingResend() {
            clearTimeout(resendWatchdog);
            resendWatchdog = 0;
            pendingResendContent = null;
            pendingResendImages = null;
            resendAwaitingHistory = false;
            resendRebuildPending = false;
        }

        // A lost session-change reply (a write dropped while the socket
        // somehow stays open) would leave resendAwaitingHistory latched
        // forever — every later edit/resend then dead-ends on "Resend
        // already in progress" until a reconnect. The reply normally lands
        // in milliseconds; past the timeout it is treated as lost so the
        // flags clear with an explicit error instead of poisoning the UI.
        // Connection-scoped (reset alongside onclose's), not mirrored per
        // pane; re-armed when a pane restore brings a still-pending resend
        // back to the foreground.
        function armResendWatchdog() {
            clearTimeout(resendWatchdog);
            resendWatchdog = setTimeout(() => {
                // Flags already gone (a racing clear beat the timer): just
                // drop the stale id.
                if (!resendAwaitingHistory) { resendWatchdog = 0; return; }
                // Clear through clearPendingResend WHILE the id still points
                // at this timer (harmless in a browser — the timer has just
                // fired — but it is what disarms an externally-fired copy).
                clearPendingResend();
                // The reply this resend was waiting on is the one that
                // never came: leaving pendingSessionResponse set would trip
                // the in-flight guard on every later session change (and a
                // very late reply still re-keys on its sessionAction — it
                // does not need this flag).
                pendingSessionResponse = false;
                showToast('Resend failed — no reply from the server', 'error');
            }, RESEND_FLUSH_TIMEOUT_MS);
        }

        /** Fork/new to before a user message, then send content as a normal turn. */
        function beginResend(histIdx, content, images) {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            if (histIdx === undefined || histIdx < 0 || !content) return;
            if (resendAwaitingHistory || pendingResendContent !== null) {
                showToast('Resend already in progress', 'info');
                return;
            }
            // Same guard resumeSession/deleteSession/forkSession carry: a
            // typed /new|/resume|/fork reply may already be in flight, and
            // two interleaved session changes would flush the edited message
            // into the WRONG session (the first reply's config passes the
            // rebuild gate — its history preceded it).
            if (pendingSessionResponse) {
                showToast('Session change already in progress', 'info');
                return;
            }
            // Compaction shifted the server's indexes since this DOM was
            // stamped: forking at the cached index would truncate at the
            // WRONG point (silently keeping the wrong prefix). The dirty
            // flag clears when the post-compact rebuild replays.
            if (activePane() && activePane().indexesDirty) {
                showToast('Transcript indexes are refreshing after compaction — try again in a moment', 'info');
                return;
            }
            cancelActiveTurn();
            pendingResendContent = content;
            pendingResendImages = (images && images.length > 0) ? images : null;
            resendAwaitingHistory = true;
            resendRebuildPending = true;
            pendingSessionResponse = true;
            armResendWatchdog();
            enableFollow();
            // D3: typed /new and the resend flow REPLACE the pane's session
            // (the config reply re-keys this pane to the new session id).
            const sid = activePane().id;
            if (histIdx === 0) {
                ws.send(JSON.stringify({ type: 'session_new', sessionId: sid }));
            } else {
                ws.send(JSON.stringify({ type: 'session_fork', messageIndex: histIdx - 1, sessionId: sid }));
            }
        }

        function flushPendingResend() {
            if (!resendAwaitingHistory) return;
            const text = pendingResendContent;
            const images = pendingResendImages;
            clearPendingResend();
            // The resent turn streams live in this pane — no convergence refetch needed.
            if (activePane()) activePane().needsFreshHistory = false;
            // The fork re-keyed this pane, so the cancelled OLD turn's trailing
            // cancelled/turn_end frames can no longer reach us (paneForMessage
            // drops frames for a session no pane holds): a suppressTurnEnds
            // unit still held for them would swallow the RESENT turn's
            // turn_end and leave turnActive stuck true — which silently
            // blocks the edit button's mid-turn guard. Units already consumed
            // sit at 0, so this only clears leaked ones.
            if (activePane()) activePane().suppressTurnEnds = 0;
            suppressTurnEnds = 0;
            if (!text || !ws || ws.readyState !== WebSocket.OPEN) return;
            const el = appendMessage('user', text, undefined, undefined, images);
            markPendingAck(el);
            streamingToolCards = {};
            pendingToolCards = {};
            bgTermCards = {};
            toolsStartedThisTurn = false;
            contextEstAdded = 0;
            const payload = { type: 'message', content: text, sessionId: activePane().id };
            if (images && images.length > 0) payload.images = images;
            ws.send(JSON.stringify(payload));
            setTurnActive(true);
            enableFollow();
        }

        /** Attach resend/edit controls once a user bubble has a server histIdx. */
        function ensureUserResendActions(msgDiv) {
            if (!msgDiv || msgDiv.querySelector('.resend-btn')) return;
            if (msgDiv.dataset.histIdx === undefined) return;
            const histIdx = parseInt(msgDiv.dataset.histIdx, 10);
            if (Number.isNaN(histIdx) || histIdx < 0) return;

            const resendBtn = document.createElement('button');
            resendBtn.className = 'resend-btn';
            resendBtn.type = 'button';
            resendBtn.innerHTML = icon('rotate');
            resendBtn.title = 'Resend this message';
            resendBtn.addEventListener('click', (e) => {
                e.stopPropagation();
                const idx = parseInt(msgDiv.dataset.histIdx, 10);
                const content = msgDiv.dataset.rawContent || '';
                if (!content || Number.isNaN(idx) || idx < 0) return;
                beginResend(idx, content, messageImageStore.get(msgDiv));
            });
            msgDiv.appendChild(resendBtn);

            const editBtn = document.createElement('button');
            editBtn.className = 'edit-btn';
            editBtn.type = 'button';
            editBtn.innerHTML = icon('pen');
            editBtn.title = 'Edit and resend';
            editBtn.addEventListener('click', (e) => {
                e.stopPropagation();
                if (turnActive) {
                    // Never fail silently: without feedback this guard reads
                    // as "the edit button is broken" whenever the client
                    // believes a turn is running (including the turn a prior
                    // edit just started).
                    showToast('The agent is busy — stop the turn to edit this message', 'info');
                    return;
                }
                if (msgDiv.querySelector('.inline-edit-bar')) return;
                const tw = msgDiv.querySelector('.msg-text');
                if (!tw) return;
                startInlineEdit(msgDiv, tw, msgDiv.dataset.rawContent || '');
            });
            msgDiv.appendChild(editBtn);
        }

        // Enter inline-edit mode for a user message: replace the rendered
        // markdown with an editable plain-text surface, add the send/cancel
        // bar, and wire Enter/Esc (finishEdit restores the markdown when
        // cancelled).
        function startInlineEdit(msgDiv, tw, raw) {
            msgDiv.querySelectorAll('.resend-btn, .edit-btn').forEach(b => { b.style.display = 'none'; });

            // Plain text — markdown DOM is not a reliable contenteditable surface.
            tw.textContent = raw;
            tw.contentEditable = 'true';
            tw.classList.add('editing');
            tw.focus();
            const range = document.createRange();
            range.selectNodeContents(tw);
            range.collapse(false);
            const sel = window.getSelection();
            sel.removeAllRanges();
            sel.addRange(range);

            const bar = document.createElement('div');
            bar.className = 'inline-edit-bar';
            const sendBtn2 = document.createElement('button');
            sendBtn2.className = 'inline-edit-send';
            sendBtn2.type = 'button';
            sendBtn2.innerHTML = icon('play');
            sendBtn2.title = 'Send (Enter)';
            const cancelBtn2 = document.createElement('button');
            cancelBtn2.className = 'inline-edit-cancel';
            cancelBtn2.type = 'button';
            cancelBtn2.innerHTML = '×';
            cancelBtn2.title = 'Cancel (Esc)';
            bar.appendChild(sendBtn2);
            bar.appendChild(cancelBtn2);
            msgDiv.appendChild(bar);

            function onEditKeydown(ev) {
                if (ev.key === 'Escape') {
                    ev.preventDefault();
                    finishEdit(true);
                } else if (ev.key === 'Enter' && !ev.shiftKey) {
                    ev.preventDefault();
                    sendBtn2.click();
                }
            }

            function finishEdit(restore) {
                tw.removeEventListener('keydown', onEditKeydown);
                tw.contentEditable = 'false';
                tw.classList.remove('editing');
                if (restore) {
                    setMessageMarkdown(msgDiv, escapeHtml(msgDiv.dataset.rawContent || ''));
                }
                bar.remove();
                msgDiv.querySelectorAll('.resend-btn, .edit-btn').forEach(b => { b.style.display = ''; });
            }

            cancelBtn2.addEventListener('click', (ev) => {
                ev.stopPropagation();
                finishEdit(true);
            });

            tw.addEventListener('keydown', onEditKeydown);

            sendBtn2.addEventListener('click', (ev) => {
                ev.stopPropagation();
                const newContent = tw.innerText.replace(/\u00a0/g, ' ').trim();
                if (!newContent) return;
                finishEdit(false);
                const idx = parseInt(msgDiv.dataset.histIdx, 10);
                if (Number.isNaN(idx) || idx < 0) return;
                beginResend(idx, newContent, messageImageStore.get(msgDiv));
            });
        }

        function appendMessage(role, text, date, histIdx, images) {
            return appendMessageAtTime(role, text, date || new Date(), histIdx, images);
        }

        function appendMessageAtTime(role, text, date, histIdx, images, model) {
            removeEmptyState();
            const msgDiv = document.createElement('div');
            msgDiv.className = `message ${role}`;
            if (histIdx !== undefined && histIdx >= 0) {
                msgDiv.dataset.histIdx = histIdx;
            }
            // Flow content goes in the .message-body wrapper; hover buttons
            // stay on msgDiv itself, outside the wrapper, so containment on
            // the wrapper (if ever added) can't clip their overhang.
            const body = msgBody(msgDiv);
            if (role === 'user' && images && images.length) {
                // Render user-attached images (vision input) above the text.
                const imgRow = document.createElement('div');
                imgRow.className = 'chat-image-row';
                for (const img of images) {
                    if (!img || !img.dataUrl) continue;
                    const imgEl = document.createElement('img');
                    imgEl.className = 'chat-image';
                    imgEl.src = img.dataUrl;
                    imgEl.alt = 'attached image';
                    imgEl.loading = 'lazy';
                    imgRow.appendChild(imgEl);
                }
                if (imgRow.childNodes.length) body.insertBefore(imgRow, body.firstChild);
            }
            if (role === 'assistant' || role === 'user') {
                // User messages go through markdown too, but HTML tags are escaped first
                // to prevent pasted HTML from influencing the page layout.
                const safe = role === 'user' ? escapeHtml(text) : text;
                setMessageMarkdown(msgDiv, safe);
            } else {
                const span = document.createElement('span');
                span.textContent = text;
                body.appendChild(span);
                messageRawStore.set(msgDiv, text);
            }
            msgDiv.dataset.createdAt = date.toISOString();
            addTimestampToMsg(msgDiv, date);
            if (role === 'assistant') {
                // Remember the provider-reported model so the settings toggle
                // can show/hide the chip on already-rendered bubbles.
                if (model) msgDiv.dataset.model = model;
                applyReplyModelChip(msgDiv);
                const forkBtn = document.createElement('button');
                forkBtn.className = 'fork-btn';
                forkBtn.innerHTML = icon('fork');
                forkBtn.title = 'Fork session from this message';
                forkBtn.addEventListener('click', (e) => {
                    e.stopPropagation();
                    forkSession(forkTargetIndex(msgDiv));
                });
                msgDiv.appendChild(forkBtn);
            }
            if (role === 'user') {
                msgDiv.dataset.rawContent = text;
                if (images && images.length > 0) messageImageStore.set(msgDiv, images);
                ensureUserResendActions(msgDiv);
            }
            appendTranscriptChild(msgDiv);
            // Every user bubble is created here (live sends, resend, history
            // replay, attach fallback), so this single hook keeps the TOC
            // rail in sync with the transcript.
            if (role === 'user') appendTocDot(msgDiv);
            // Content is final here; let the item be paint-skipped off-screen
            // once paint-skip.js has measured its real height.
            enablePaintSkip(body);
            smartScroll();
            return msgDiv;
        }

        function flushStreamRender() {
            streamRafPending = false;
            streamLastRender = performance.now();
            if (!currentStreamDiv) return;
            const keepCursor = currentStreamDiv.classList.contains('cursor');
            renderStreamMarkdown(currentStreamDiv, currentStreamRaw);
            if (keepCursor) currentStreamDiv.classList.add('cursor');
            // Scroll after DOM height actually changes (tokens arrive before paint).
            smartScroll();
        }

        function scheduleStreamRender() {
            // Coalesce multiple token arrivals into a single paint-aligned render
            if (streamRafPending) return;
            const elapsed = performance.now() - streamLastRender;
            if (elapsed >= STREAM_RENDER_INTERVAL) {
                streamRafPending = true;
                requestAnimationFrame(() => {
                    if (!streamRafPending) return;
                    flushStreamRender();
                });
            }
        }

        // Show (or re-label) the waiting placeholder bubble. The elapsed
        // timer runs from the FIRST show of the current window, so a
        // stream_retry re-label does not restart the clock — the user sees
        // how long the whole silent stretch has lasted.
        function showWaitingPlaceholder(text) {
            if (!waitingPlaceholder || !waitingPlaceholder.isConnected) {
                removeEmptyState();
                waitingPlaceholder = document.createElement('div');
                waitingPlaceholder.className = 'message assistant waiting-placeholder';
                const spin = document.createElement('span');
                spin.className = 'waiting-spinner';
                const label = document.createElement('span');
                label.className = 'waiting-placeholder-label';
                waitingPlaceholder.appendChild(spin);
                waitingPlaceholder.appendChild(label);
                appendTranscriptChild(waitingPlaceholder);
                waitingPlaceholderStart = Date.now();
                waitingPlaceholderTimer = setInterval(tickWaitingPlaceholder, 1000);
                smartScroll();
            }
            if (text) waitingPlaceholderBase = text;
            tickWaitingPlaceholder();
        }

        function tickWaitingPlaceholder() {
            if (!waitingPlaceholder) return;
            // The transcript is re-derived from the server when a pane is
            // focused or history reloads: a detached placeholder is dead
            // DOM, stop the timer with it.
            if (!waitingPlaceholder.isConnected) {
                clearWaitingPlaceholder();
                return;
            }
            const s = Math.floor((Date.now() - waitingPlaceholderStart) / 1000);
            const elapsed = s < 60 ? s + 's' : Math.floor(s / 60) + 'm ' + (s % 60) + 's';
            const label = waitingPlaceholder.querySelector('.waiting-placeholder-label');
            if (label) label.textContent = waitingPlaceholderBase + ' ' + elapsed;
        }

        function clearWaitingPlaceholder() {
            if (waitingPlaceholderTimer) {
                clearInterval(waitingPlaceholderTimer);
                waitingPlaceholderTimer = null;
            }
            if (waitingPlaceholder) {
                waitingPlaceholder.remove();
                waitingPlaceholder = null;
            }
            waitingPlaceholderBase = '';
        }

        function startStream() {
            removeEmptyState();
            streamRafPending = false;
            streamLastRender = 0;
            const msgDiv = document.createElement('div');
            msgDiv.className = 'message assistant cursor md';
            // Content wrapper up front: the fork button appended in
            // endStream must land on msgDiv, outside the contained body.
            msgBody(msgDiv);
            appendTranscriptChild(msgDiv);
            smartScroll();
            currentStreamDiv = msgDiv;
            lastStreamDiv = msgDiv;
            currentStreamRaw = '';
            streamContentPos = 0;
        }

        // Trim a received chunk against the position of what the client has
        // already rendered: keep only [currentPos..endPos]. Positions are
        // server-stamped cumulative character offsets in the same per-round
        // buffer, so this is an exact merge — never a duplicate, never a gap.
        // When endPos is absent (older server) the chunk is kept as-is.
        function trimToEnd(text, endPos, currentPos) {
            if (!text) return '';
            if (endPos === undefined || endPos === null) return text;
            const cur = currentPos || 0;
            if (endPos <= cur) return '';
            const start = endPos - text.length;
            const cut = cur - start;
            if (cut <= 0) return text;
            return text.slice(cut);
        }

        // Void the active pane's rendered-index claim: the transcript no
        // longer provably matches what the pane rendered, so every
        // conditional attach (turn-end convergence, focus) must fall back to
        // a full snapshot until a history payload re-earns it. Both claim
        // sites require histEpoch to be defined and renderedHistIdx to be
        // advanced, so clearing them withholds the claim.
        function voidTranscriptClaim() {
            const pane = activePane();
            if (!pane) return;
            pane.histEpoch = undefined;
            pane.renderedHistIdx = -1;
        }

        // True when a position-stamped chunk starts PAST the position the
        // client has rendered: a frame was lost in between (the server's
        // send-queue timeout can drop one), and trimToEnd below can only
        // skip the hole, never fill it. The caller voids the pane's claim so
        // the next attach takes the full snapshot that heals it.
        function streamGap(text, endPos, cur) {
            if (!text || endPos === undefined || endPos === null) return false;
            return endPos - text.length > (cur || 0);
        }

        function appendStreamToken(token, endPos) {
            if (!currentStreamDiv) return;
            if (streamGap(token, endPos, streamContentPos)) voidTranscriptClaim();
            const keep = trimToEnd(token, endPos, streamContentPos);
            if (!keep) return;
            currentStreamRaw += keep;
            streamContentPos = Math.max(streamContentPos, endPos || 0);
            bumpContextEstimate(keep);
            scheduleStreamRender();
        }

        // Build the "Thinking" card shared by the live streaming path
        // (showThinking) and the history replay (renderHistoryEntry): header
        // with the italic label + collapse chevron, markdown body, and the
        // collapse toggle wired. The caller fills the body (live markdown
        // stream or the replayed reasoning render), adds any metadata, and
        // appends the card to the transcript.
        function createThoughtCard() {
            const div = document.createElement('div');
            div.className = 'thought-card';

            const header = document.createElement('div');
            header.style.cssText = 'display:flex;align-items:center;gap:6px;cursor:pointer;user-select:none;font-size:0.85em;color:var(--fg-muted);padding:2px 0;';
            const label = document.createElement('span');
            label.style.fontStyle = 'italic';
            label.textContent = 'Thinking';
            const toggle = document.createElement('span');
            toggle.innerHTML = icon('chevron-down');
            toggle.style.fontSize = '0.8em';
            header.appendChild(label);
            header.appendChild(toggle);

            // Markdown is rendered live into this body (like the assistant
            // stream); base size stays compact, typography comes from CSS.
            const body = document.createElement('div');
            body.style.cssText = 'font-size:0.85em;padding:4px 0 0 0;';

            div.appendChild(header);
            div.appendChild(body);

            // Collapse toggle active from the start
            header.addEventListener('click', () => {
                const collapsed = body.style.display === 'none';
                body.style.display = collapsed ? '' : 'none';
                toggle.innerHTML = icon(collapsed ? 'chevron-down' : 'chevron-right');
            });

            return { card: div, header, toggle, body };
        }

        function showThinking() {
            removeEmptyState();
            finalizeThinking();
            thinkingContentPos = 0;
            const { card: div, body } = createThoughtCard();
            appendTranscriptChild(div);

            smartScroll();
            currentThinkingRaw = '';
            currentThinkingDiv = div;
            currentThinkingSpan = body;
        }

        function appendThinkingToken(token, endPos) {
            // Match TUI: ignore thinking after tool calls start in this round.
            // Cleared again on the next round's "thinking" (OnRoundStart).
            if (toolsStartedThisTurn) return;
            if (streamGap(token, endPos, thinkingContentPos)) voidTranscriptClaim();
            const keep = trimToEnd(token, endPos, thinkingContentPos);
            if (!keep) return;
            if (!currentThinkingSpan) {
                showThinking();
            }
            currentThinkingRaw += keep;
            thinkingContentPos = Math.max(thinkingContentPos, endPos || 0);
            bumpContextEstimate(keep);
            scheduleThinkingRender();
        }

        function flushThinkingRender() {
            thinkingRafPending = false;
            thinkingLastRender = performance.now();
            if (!currentThinkingSpan) return;
            // Live markdown render of the reasoning content, exactly like the
            // assistant stream (same cadence, same incremental renderer).
            renderStreamMarkdown(currentThinkingSpan, currentThinkingRaw);
            smartScroll();
        }

        function scheduleThinkingRender() {
            // Coalesce thinking-token arrivals into a single paint-aligned
            // render, exactly like the assistant stream (STREAM_RENDER_INTERVAL).
            if (thinkingRafPending) return;
            const elapsed = performance.now() - thinkingLastRender;
            if (elapsed >= STREAM_RENDER_INTERVAL) {
                thinkingRafPending = true;
                requestAnimationFrame(() => {
                    if (!thinkingRafPending) return;
                    flushThinkingRender();
                });
            }
        }

        // --- Input-area progress indicator (replaces textarea during streaming) ---
        let currentProgressPhase = null;

        // Smoothed token rate from the server's shared SpeedMeter
        // (stream_stats frames; same meter, interval, and estimator as
        // the TUI's progress line). Reset at every round start and turn
        // end; only displayed while the phase is 'streaming'.
        let lastStreamSpeed = 0;

        function streamProgressLabel() {
            return 'Streaming\u2026' + (lastStreamSpeed >= 1 ? ' ' + Math.round(lastStreamSpeed) + ' tok/s' : '');
        }

        // Thinking tokens feed the same meter, so the rate is meaningful
        // in the thinking phase too; it stays blank until the first
        // stream_stats frame (the meter is silent before the first token,
        // so "waiting for the model" never shows a rate).
        function thinkingProgressLabel() {
            return 'Thinking\u2026' + (lastStreamSpeed >= 1 ? ' ' + Math.round(lastStreamSpeed) + ' tok/s' : '');
        }

        // The progress strip coexists with the composer (steering): the
        // textarea is never hidden, so no focus handoff is needed between
        // the indicator and the composer.

        // Cached progress-indicator node refs. The spinner/label structure
        // is static (index.html #input-progress), so resolve them once when
        // the indicator first appears instead of querySelector-ing per frame.
        let progressSpinnerEl = null;
        let progressLabelEl = null;
        // Last rendered label text: the label carries a live tok/s rate that
        // changes a few times per second, so the guard must be text-based
        // (a phase-only guard would freeze the rate display).
        let progressLabelText = '';

        /**
         * Show/update the progress indicator in the input area.
         * Phase: 'thinking' | 'streaming' | 'tool' | null
         * When phase is null, the indicator is hidden and the textarea is restored.
         * Cheap to call repeatedly per frame: DOM writes happen only on the
         * first show, on phase change (spinner class), or on label-text change.
         */
        function setInputProgress(phase, label) {
            if (!inputProgress) return;
            if (phase == null) {
                // Hide the progress strip; the composer was never hidden
                // (steering keeps it editable while a turn runs). The wrap
                // class flips with it so the textarea reclaims the strip's
                // row (same bar height in both states).
                inputProgress.classList.remove('active');
                if (inputWrap) inputWrap.classList.remove('progress-active');
                currentProgressPhase = null;
                progressLabelText = '';
                return;
            }
            const phaseChanged = currentProgressPhase !== phase;
            if (currentProgressPhase === null) {
                inputProgress.classList.add('active');
                if (inputWrap) inputWrap.classList.add('progress-active');
                progressSpinnerEl = inputProgress.querySelector('.progress-spinner');
                progressLabelEl = inputProgress.querySelector('.progress-label');
            }
            currentProgressPhase = phase;
            if (progressSpinnerEl && phaseChanged) {
                progressSpinnerEl.className = 'progress-spinner ' + phase;
            }
            // Queued steering messages are part of the input bar's truth:
            // "· N queued" rides every progress label while items wait.
            const nq = queuedCount();
            const text = (label || phase) + (nq > 0 ? ' · ' + nq + ' queued' : '');
            if (progressLabelEl && text !== progressLabelText) {
                progressLabelEl.textContent = text;
                progressLabelText = text;
            }
        }

        function queuedCount() {
            const pane = activePane();
            return (pane && pane.queueItems || []).length;
        }

        // Re-render the progress label after a queue-count change that did
        // not come from a stream event (optimistic enqueue, queue_update).
        // setInputProgress is the single writer of the label (it re-appends
        // the queued suffix).
        function updateQueuedProgressLabel() {
            if (currentProgressPhase === null) return;
            setInputProgress(currentProgressPhase, progressLabelText.replace(/ · \d+ queued$/, ''));
        }

        function finalizeThinking() {
            if (!currentThinkingDiv) return;
            const div = currentThinkingDiv;
            const content = (currentThinkingRaw || '').trim();
            if (content) {
                // Keep the finalized block (raw + position) so a rewind merge
                // can still reach it after the buffers are cleared.
                lastFinalizedThinking = { raw: currentThinkingRaw, pos: thinkingContentPos };
                // Final full render: corrects any block-split artifacts from
                // the incremental live stream (e.g. a list split at a blank
                // line) so the collapsed card matches a single-shot render.
                setMessageMarkdown(currentThinkingSpan, currentThinkingRaw);
                // The card is complete; allow the paint skip.
                enablePaintSkip(div);
            }
            currentThinkingDiv = null;
            currentThinkingSpan = null;
            currentThinkingRaw = '';
            thinkingContentPos = 0;
            thinkingRafPending = false;
            if (!content) {
                div.remove();
                return;
            }
            // Card is already fully structured from showThinking() —
            // the collapsible toggle is already active, no rebuild needed.
        }

        function endStream() {
            if (currentStreamDiv) {
                // Final full render: the incremental live renderer keeps
                // stable blocks plus an in-flight tail, so a full pass here
                // guarantees the final DOM exactly matches a single-shot
                // render (fixes any block-split artifacts from streaming).
                setMessageMarkdown(currentStreamDiv, currentStreamRaw || '');
                currentStreamDiv.classList.remove('cursor');
                addTimestampToMsg(currentStreamDiv, new Date());
                // The bubble is final: give it its fork button.
                const forkBtn = document.createElement('button');
                forkBtn.className = 'fork-btn';
                forkBtn.innerHTML = icon('fork');
                forkBtn.title = 'Fork session from this message';
                // Read the bubble's own server index at click, like the
                // history-replayed button does. A live bubble carries one as
                // soon as its round is appended (hist_index), so a fork from
                // an EARLIER live bubble truncates there — it must not fall
                // back to "the last assistant message", which is what the
                // hardcoded -1 did (silently forking at the newest turn).
                const forkBubble = currentStreamDiv;
                forkBtn.addEventListener('click', (e) => {
                    e.stopPropagation();
                    forkSession(forkTargetIndex(forkBubble));
                });
                currentStreamDiv.appendChild(forkBtn);
                // The bubble is complete; allow the paint skip off-screen.
                enablePaintSkip(msgBody(currentStreamDiv));
            }
            currentStreamDiv = null;
            currentStreamRaw = '';
        }

        // Also flush when the tab becomes visible again after being hidden
        // (rAF is paused in hidden tabs, so pending renders may be stale).
        document.addEventListener('visibilitychange', () => {
            if (!document.hidden && currentStreamDiv && streamRafPending) {
                streamRafPending = false;
                scheduleStreamRender();
            }
            if (!document.hidden && currentThinkingSpan && thinkingRafPending) {
                thinkingRafPending = false;
                scheduleThinkingRender();
            }
        });

        // Render a single tool-arg value for a card header: strings are
        // quoted, long ones truncated to bigArgChars with an ellipsis,
        // everything else String()-ed.
        function formatToolArgValue(value) {
            if (typeof value === 'string' && value.length > bigArgChars()) {
                return `"${value.substring(0, bigArgChars() - 3)}..."`;
            }
            if (typeof value === 'string') {
                return `"${value}"`;
            }
            return String(value);
        }

        // http(s) URLs only: a model-supplied value must never become a
        // javascript:/data: href (it would execute on click).
        const URL_ARG_PATTERN = /^https?:\/\/\S+$/i;

        /**
         * Create a document fragment for tool args with clickable file paths
         * and URLs. The single header renderer for both card sources (the
         * live finalize path and history replay), so the two can't drift.
         * Diffs are shown in Monaco; the header keeps only a char-count
         * stub. `url` args (web_fetch, download_file) become real anchors
         * that open in a new tab.
         */
        function formatToolArgsFragment(args) {
            if (!args || typeof args !== 'object') return document.createTextNode('');
            const frag = document.createDocumentFragment();
            const FILE_KEYS = ['file_path', 'path', 'source', 'destination'];
            const entries = Object.entries(args);
            for (let i = 0; i < entries.length; i++) {
                const [key, value] = entries[i];
                if (i > 0) frag.appendChild(document.createTextNode(', '));
                frag.appendChild(document.createTextNode(`${key}=`));
                if (key === 'diff' && typeof value === 'string') {
                    frag.appendChild(document.createTextNode(`<${value.length} chars>`));
                } else if (FILE_KEYS.includes(key) && typeof value === 'string' && value) {
                    const link = document.createElement('span');
                    link.className = 'file-link';
                    link.textContent = value;
                    link.title = 'Click to open in editor';
                    link.onclick = (e) => { e.stopPropagation(); openFileAtLine(value).catch(() => {}); };
                    frag.appendChild(link);
                } else if (key === 'url' && typeof value === 'string' && URL_ARG_PATTERN.test(value)) {
                    const link = document.createElement('a');
                    link.className = 'url-link';
                    link.href = value;
                    link.textContent = value;
                    link.target = '_blank';
                    link.rel = 'noopener noreferrer';
                    link.title = 'Open in a new tab';
                    // The header click toggles the card; a link click must
                    // not collapse/expand it.
                    link.onclick = (e) => e.stopPropagation();
                    frag.appendChild(link);
                } else {
                    frag.appendChild(document.createTextNode(formatToolArgValue(value)));
                }
            }
            return frag;
        }

        // Cap for the "does an unknown tool's args carry a diff key?" probe:
        // the key can only appear near the start of the JSON, so scanning the
        // growing args beyond this is pure cost for a viewer that cannot
        // upgrade (see appendStreamingToolArgs).
        const MAX_DIFF_KEY_PROBE = 8192;

        // Cheap completeness probe for the streaming args JSON: true when the
        // last non-whitespace character is a closing brace. A truncated JSON
        // body can never end in '}' for the tools we stream diffs for, so
        // this avoids a failed JSON.parse of the whole args string per frame.
        function argsLookComplete(raw) {
            for (let i = raw.length - 1; i >= 0; i--) {
                const c = raw[i];
                if (c === ' ' || c === '\n' || c === '\t' || c === '\r') continue;
                return c === '}';
            }
            return false;
        }

        function createToolCard(name, args, options = {}) {
            toolCallCounter++;
            const cardId = `tool-card-${toolCallCounter}`;
            const { streaming = false, streamIndex = null } = options;

            const card = document.createElement('div');
            card.className = 'tool-card';
            card.id = cardId;
            if (streamIndex !== null) {
                card.dataset.streamIndex = String(streamIndex);
            }

            const { header, toolArgs, toggle } = buildToolCardHeader(name, args, streaming);
            card.appendChild(header);
            const { waiting, argsStream, monacoHost } = buildToolCardBody(card, name, args, streaming);

            appendTranscriptChild(card);
            smartScroll();

            header.addEventListener('click', () => {
                const collapsed = toggle.classList.toggle('collapsed');
                const body = card.querySelector('.tool-result-body');
                if (body) body.classList.toggle('collapsed', collapsed);
                if (argsStream) argsStream.classList.toggle('collapsed', collapsed);
                if (monacoHost) monacoHost.style.display = collapsed ? 'none' : 'block';
                const live = card.querySelector('.tool-live-output');
                if (live) live.classList.toggle('collapsed', collapsed);
                // Toggling changes content height below the fold; keep the
                // scroll system in sync (re-pin if following, refresh the
                // jump-button if detached).
                scheduleRepinIfPinned();
                if (!isPinned()) updateScrollBottomBtn();
            });

            const cardInfo = {
                cardId,
                card,
                waiting,
                toggle,
                toolArgs,
                argsStream,
                monacoHost,
                monacoEditor: null,
                monacoMounting: null,
                toolName: name,
                args: args || {},
                rawArgs: '',
                pendingDiff: '',
                diffComplete: false,
                diffScan: null,
                argsCompacted: false,
                finalized: false,
                rafScheduled: false,
                lastFallbackText: '',
                scheduleFrameUpdate() {
                    // Coalesce per-delta tool-arg updates into one pass per
                    // frame: diff extraction, fallback/Monaco update, compact
                    // header, and scroll all run at most once per frame.
                    if (this.rafScheduled) return;
                    this.rafScheduled = true;
                    requestAnimationFrame(() => {
                        this.rafScheduled = false;
                        if (this.finalized || !this.card.isConnected) return;
                        if (this.monacoHost) {
                            let text;
                            if (this.diffComplete) {
                                // Diff value is final once its closing quote
                                // was seen; don't re-extract it every frame.
                                text = this.pendingDiff || '';
                            } else {
                                // Incremental scan: rawArgs only grows, so
                                // each frame decodes just the new tail. The
                                // old whole-value re-extract was O(args²)
                                // over the stream (hundreds of ms for a
                                // large diff).
                                const scan = this.diffScan || (this.diffScan = { keyed: false, complete: false, pos: 0, valueStart: -1, out: '' });
                                const extracted = scanDiffValueIncremental(scan, this.rawArgs);
                                if (extracted.complete) {
                                    this.diffComplete = true;
                                    this.pendingDiff = extracted.value;
                                }
                                text = extracted.ok ? extracted.value : (this.pendingDiff || '');
                            }
                            if (!this.monacoEditor) {
                                // Fallback <pre> is visible only until Monaco
                                // paints; create it on first sight and then
                                // only touch it when the text actually changes.
                                const pre = this.monacoHost.querySelector('.diff-fallback');
                                if (!pre || this.lastFallbackText !== text) {
                                    this.lastFallbackText = text;
                                    updateDiffFallback(this.monacoHost, text);
                                }
                            } else {
                                updateDiffEditor(this.monacoEditor, text);
                            }
                        } else if (this.argsStream) {
                            this.argsStream.textContent = this.rawArgs;
                        }
                        // Compact the header once the JSON is parseable. Two
                        // cheap gates first: the JSON can only parse once its
                        // closing brace arrived (a failed JSON.parse of the
                        // whole args string every frame was O(args²) over a
                        // stream), and once compacted there is nothing to do.
                        if (!this.argsCompacted && argsLookComplete(this.rawArgs)) {
                            const compact = formatArgsCompact(this.rawArgs);
                            if (compact && this.toolArgs && this.toolArgs.textContent !== compact) {
                                this.toolArgs.textContent = compact;
                                this.argsCompacted = true;
                                if (this.argsStream) {
                                    this.argsStream.remove();
                                    this.argsStream = null;
                                }
                            }
                        }
                        smartScroll();
                    });
                },
                setCollapsed: (c) => { toggle.classList.toggle('collapsed', c); },
            };
            return cardInfo;
        }

        // Build the tool-card header row (icon, name, live args, collapse
        // toggle). Streaming cards start with an empty args span; finalized
        // cards render the formatted args fragment.
        function buildToolCardHeader(name, args, streaming) {
            const header = document.createElement('div');
            header.className = 'tool-call-header';
            // Named iconEl (not `icon`) so it cannot shadow the imported
            // icon() helper used for the chevron toggle below.
            const iconEl = document.createElement('span');
            iconEl.className = 'tool-icon';
            const toolName = document.createElement('span');
            toolName.className = 'tool-name';
            toolName.textContent = name;
            const toolArgs = document.createElement('span');
            toolArgs.className = 'tool-args';
            if (streaming) toolArgs.textContent = '';
            else toolArgs.appendChild(formatToolArgsFragment(args));
            const toggle = document.createElement('span');
            toggle.className = 'tool-toggle';
            toggle.innerHTML = icon('chevron-down');
            header.appendChild(iconEl);
            header.appendChild(toolName);
            header.appendChild(toolArgs);
            header.appendChild(toggle);
            return { header, toolArgs, toggle };
        }

        // Build the card body under the header: a diff host for patch_file
        // (streaming or finalized with a diff), a raw args stream for other
        // streaming tools, or the "executing..." waiting line. Returns the
        // pieces the caller tracks for later updates.
        function buildToolCardBody(card, name, args, streaming) {
            let waiting = null;
            let argsStream = null;
            let monacoHost = null;
            // Always prepare a diff host for patch_file; other streaming tools
            // keep the raw args stream until finalized.
            if (streaming && name === 'patch_file') {
                monacoHost = makeDiffHostElement();
                card.appendChild(monacoHost);
            } else if (streaming) {
                argsStream = document.createElement('div');
                argsStream.className = 'tool-args-streaming cursor';
                argsStream.textContent = '(';
                card.appendChild(argsStream);
            } else if (name === 'patch_file' && args && typeof args.diff === 'string') {
                monacoHost = makeDiffHostElement();
                card.appendChild(monacoHost);
                waiting = document.createElement('div');
                waiting.className = 'tool-waiting';
                waiting.textContent = 'executing...';
                card.appendChild(waiting);
            } else {
                waiting = document.createElement('div');
                waiting.className = 'tool-waiting';
                waiting.textContent = 'executing...';
                card.appendChild(waiting);
            }
            return { waiting, argsStream, monacoHost };
        }

        // Cap for the live-output region on tool cards: mirrors the server's
        // tool-result cap (128 KB) so an unbounded `yes`-style command cannot
        // balloon the transcript DOM. The terminal dock still shows the full
        // log (its own scrollback cap).
        const TOOL_CARD_LIVE_OUTPUT_LIMIT = 128 * 1024;

        // Attaches the live-output region to a tool card (once). Shell tools
        // (execute_command, run_tests, run_lint) stream their stdout/stderr
        // here as the command runs — the same chunks that feed the terminal
        // dock — so the chat card shows the output live instead of only the
        // final tool_result at the end.
        function ensureToolCardLiveOutput(cardInfo) {
            if (!cardInfo) return null;
            if (cardInfo.liveOutput) return cardInfo.liveOutput;
            const live = document.createElement('div');
            live.className = 'tool-live-output';
            // Insert above the "executing..." row so the log reads top-down
            // with the status line beneath it.
            const waiting = cardInfo.card.querySelector('.tool-waiting');
            if (waiting) cardInfo.card.insertBefore(live, waiting);
            else cardInfo.card.appendChild(live);
            cardInfo.liveOutput = live;
            cardInfo.liveOutputText = '';
            cardInfo.liveOutputTruncated = false;
            return live;
        }

        // Appends a chunk of streamed tool output to the card's live region,
        // capping the accumulated text so a huge command cannot balloon the
        // transcript (the terminal dock keeps the full log).
        function appendToolCardLiveOutput(cardInfo, chunk) {
            if (!cardInfo || !chunk) return;
            const live = ensureToolCardLiveOutput(cardInfo);
            if (cardInfo.liveOutputTruncated) return;
            if (cardInfo.liveOutputText.length + chunk.length > TOOL_CARD_LIVE_OUTPUT_LIMIT) {
                chunk = '\n… output truncated in card (see terminal tab for the full log)';
                cardInfo.liveOutputTruncated = true;
            }
            cardInfo.liveOutputText += chunk;
            // Incremental append instead of a full-buffer rewrite: chatty
            // commands stream ~30 frames/sec and the buffer can hold the
            // full 128 KB cap, so `live.textContent = liveOutputText`
            // re-serialized the entire buffer (and forced a layout) on
            // every frame. The DOM mirrors exactly what was appended, so
            // textContent stays byte-identical to liveOutputText.
            const last = live.lastChild;
            if (last && last.nodeType === Node.TEXT_NODE) {
                last.data += chunk;
            } else {
                live.appendChild(document.createTextNode(chunk));
            }
            // Keep the newest output in view, like a terminal.
            live.scrollTop = live.scrollHeight;
            scheduleRepinIfPinned();
        }

        // The chat diff host is a fixed-height box in Monaco mode (the editor
        // scrolls internally) and an auto-height box in tokenizer mode (the
        // fallback <pre> IS the viewer, sized to its content up to the 400px
        // cap). Shared by createToolCard (hosts pre-created for patch_file)
        // and createDiffHost (late upgrades), so streaming patch cards get
        // the same class as every other viewer instead of a hidden 100px
        // scroller that emits synthetic scroll events on every delta.
        function makeDiffHostElement() {
            const host = document.createElement('div');
            host.className = 'monaco-tool-host' + (diffViewerMode() === 'tokenizer' ? ' diff-static' : '');
            return host;
        }

        function createDiffHost(cardInfo) {
            const host = makeDiffHostElement();
            cardInfo.monacoHost = host;
            return host;
        }

        async function ensurePatchViewer(cardInfo, diffText) {
            if (!cardInfo) return;
            if (diffText != null && diffText !== '') {
                cardInfo.pendingDiff = diffText;
            }
            if (!cardInfo.monacoHost) {
                // Upgrade a non-diff streaming card into a patch viewer.
                if (cardInfo.argsStream) {
                    cardInfo.argsStream.remove();
                    cardInfo.argsStream = null;
                }
                const host = createDiffHost(cardInfo);
                // Insert before waiting/status if present
                const waiting = cardInfo.card.querySelector('.tool-waiting');
                if (waiting) cardInfo.card.insertBefore(host, waiting);
                else cardInfo.card.appendChild(host);
            }
            updateDiffFallback(cardInfo.monacoHost, cardInfo.pendingDiff || '');

            if (diffViewerMode() === 'tokenizer') {
                // Static path: the fallback <pre> IS the viewer (line-numbered
                // rows, colored by diff line type). Streaming updates flow
                // through updateDiffFallback; no Monaco editor is created.
                return;
            }
            if (cardInfo.monacoEditor) {
                if (cardInfo.pendingDiff) updateDiffEditor(cardInfo.monacoEditor, cardInfo.pendingDiff);
                return;
            }
            if (cardInfo.monacoMounting) {
                await cardInfo.monacoMounting;
                if (cardInfo.monacoEditor && cardInfo.pendingDiff) {
                    updateDiffEditor(cardInfo.monacoEditor, cardInfo.pendingDiff);
                }
                return;
            }
            cardInfo.monacoMounting = mountDiffEditor(cardInfo.monacoHost, cardInfo.pendingDiff || '')
                .then((ed) => {
                    cardInfo.monacoEditor = ed;
                    if (ed && cardInfo.pendingDiff) updateDiffEditor(ed, cardInfo.pendingDiff);
                    return ed;
                })
                .catch((err) => {
                    console.warn('patch viewer monaco failed', err);
                    updateDiffFallback(cardInfo.monacoHost, cardInfo.pendingDiff || '');
                    return null;
                })
                .finally(() => {
                    cardInfo.monacoMounting = null;
                });
            await cardInfo.monacoMounting;
        }

        function startStreamingToolCard(index, name) {
            finalizeThinking();
            endStream();
            toolsStartedThisTurn = true;
            const cardInfo = createToolCard(name, {}, { streaming: true, streamIndex: index });
            cardInfo.argsPos = 0;
            streamingToolCards[index] = cardInfo;
            if (name === 'patch_file') {
                ensurePatchViewer(cardInfo, '').catch(() => {});
            }
            return cardInfo;
        }

        function appendStreamingToolArgs(index, argsDelta, endPos) {
            const cardInfo = streamingToolCards[index];
            if (!cardInfo) return;
            if (streamGap(argsDelta, endPos, cardInfo.argsPos || 0)) voidTranscriptClaim();
            const keep = trimToEnd(argsDelta, endPos, cardInfo.argsPos || 0);
            if (!keep) return;
            cardInfo.rawArgs = (cardInfo.rawArgs || '') + keep;
            cardInfo.argsPos = Math.max(cardInfo.argsPos || 0, endPos || 0);
            bumpContextEstimate(keep);

            const tool = cardInfo.toolName;
            if (cardInfo.monacoHost) {
                // Patch viewer already exists (patch_file cards get one at
                // start): all per-delta work is coalesced into one rAF pass.
                cardInfo.toolName = 'patch_file';
            } else {
                // Not a patch viewer yet. A patch tool always upgrades; any
                // other tool only when a "diff" key shows up in the
                // still-short args prefix (the key can only appear near the
                // start of the JSON). Scanning the whole accumulated args per
                // delta was O(args²) on a large stream — e.g. write_file's
                // content field — for a viewer it can never upgrade.
                const extracted = tool === 'patch_file' || cardInfo.rawArgs.length <= MAX_DIFF_KEY_PROBE
                    ? extractDiffValue(cardInfo.rawArgs)
                    : { ok: false, value: '' };
                if (tool === 'patch_file' || extracted.ok) {
                    cardInfo.toolName = 'patch_file';
                    ensurePatchViewer(cardInfo, extracted.ok ? extracted.value : (cardInfo.pendingDiff || '')).catch(() => {});
                }
            }
            cardInfo.scheduleFrameUpdate();
        }

        function finalizeStreamingToolCard(index, id, name, args) {
            let cardInfo = streamingToolCards[index];
            if (!cardInfo) {
                cardInfo = createToolCard(name, args || {});
            } else {
                delete streamingToolCards[index];
                if (cardInfo.argsStream) {
                    cardInfo.argsStream.classList.remove('cursor');
                    cardInfo.argsStream.remove();
                    cardInfo.argsStream = null;
                }
                // Fragment (not textContent) so file paths and URLs render
                // as clickable links, exactly like the replay path.
                cardInfo.toolArgs.replaceChildren(formatToolArgsFragment(args));
                cardInfo.args = args || {};

                if (!cardInfo.waiting) {
                    const waiting = document.createElement('div');
                    waiting.className = 'tool-waiting';
                    waiting.textContent = 'executing...';
                    cardInfo.card.appendChild(waiting);
                    cardInfo.waiting = waiting;
                }
            }

            cardInfo.finalized = true;
            cardInfo.toolName = name || cardInfo.toolName;
            cardInfo.args = args || cardInfo.args || {};
            if (id) {
                pendingToolCards[id] = cardInfo;
            }

            // patch_file: the diff lives in args, not the later tool_result summary.
            if ((name === 'patch_file' || cardInfo.toolName === 'patch_file') && args && typeof args.diff === 'string') {
                ensurePatchViewer(cardInfo, args.diff).catch(() => {});
            }
            return cardInfo;
        }

        async function updateToolCardWithResult(cardInfo, result, success, truncated, toolName) {
            const { card, waiting } = cardInfo;
            cardInfo.finalized = true;
            if (waiting) waiting.remove();
            cardInfo.waiting = null;

            const body = document.createElement('div');
            body.className = 'tool-result-body';

            const statusBar = document.createElement('div');
            statusBar.className = `tool-status-bar ${success ? 'success' : 'error'}`;
            statusBar.textContent = success ? 'OK' : 'FAILED';
            body.appendChild(statusBar);

            const name = toolName || cardInfo.toolName || '';
            const argsDiff = cardInfo.args && typeof cardInfo.args.diff === 'string' ? cardInfo.args.diff : '';
            const pending = cardInfo.pendingDiff || argsDiff;

            try {
                if (name === 'patch_file') {
                    // Ensure the patch (from args) is visible; result is usually a short summary.
                    if (pending) {
                        ensurePatchViewer(cardInfo, pending).catch(() => {});
                    }
                    if (result) {
                        appendExpandableResult(body, result, success, truncated);
                    }
                    card.appendChild(body);
                } else if (name === 'show_diff') {
                    // Same viewer machinery as patch_file (mount guard, fallback
                    // sync, resize → gogen-colorized → scroll re-pin), so the
                    // card behaves identically to a patch card.
                    if (!cardInfo.monacoHost) {
                        cardInfo.monacoHost = createDiffHost(cardInfo);
                        body.appendChild(cardInfo.monacoHost);
                    }
                    card.appendChild(body);
                    ensurePatchViewer(cardInfo, result || '').catch(() => {});
                } else if (name === 'read_file') {
                    const path = readFilePathFromArgs(cardInfo.args);
                    appendExpandableResult(body, result, success, truncated, {
                        language: languageFromPath(path),
                    });
                    card.appendChild(body);
                } else if (cardInfo.liveOutput && cardInfo.liveOutputText
                    && !(cardInfo.args && cardInfo.args.background === true)) {
                    // The output already streamed into the card while the
                    // command ran; appending the final result again would
                    // duplicate it. Keep the streamed log as the result body
                    // (plus a copy affordance / truncation note).
                    // Background jobs are the exception: their result is the
                    // job id / poll hint, NOT a duplicate of the streamed
                    // output (which keeps streaming after this result).
                    appendStreamedResultBody(body, cardInfo);
                    card.appendChild(body);
                } else {
                    appendExpandableResult(body, result, success, truncated);
                    card.appendChild(body);
                }
            } finally {
                // Always re-sync the scroll position after the card update,
                // even if a viewer mount failed or rejected.
                smartScroll();
                // The card is complete (Monaco xterm mounts grow it while it
                // is on screen, where the remembered size tracks them); allow
                // the paint skip off-screen.
                enablePaintSkip(card);
            }
        }

        // Builds the result body for a shell tool card whose output already
        // streamed in live (tool-live-output): the status bar was appended by
        // the caller, and here we add a copy affordance for the streamed text
        // plus a truncation note when the card cap was hit (the terminal dock
        // still holds the full log).
        function appendStreamedResultBody(body, cardInfo) {
            const liveText = cardInfo.liveOutputText;
            const copyBtn = document.createElement('button');
            copyBtn.className = 'tool-result-copy';
            copyBtn.textContent = 'Copy';
            copyBtn.onclick = () => {
                copyTextToClipboard(liveText).then((ok) => showToast(ok ? 'Copied' : 'Copy failed', ok ? 'success' : 'error'));
            };
            body.appendChild(copyBtn);
            if (cardInfo.liveOutputTruncated) {
                const note = document.createElement('div');
                note.className = 'tool-live-truncated';
                note.textContent = 'Output truncated in card; open the terminal tab for the full log.';
                body.appendChild(note);
            }
        }

        // A history replay rebuilds the whole pane with awaits (Monaco mounts,
        // colorize, image loads). If one of those awaits never settles,
        // replayInProgress stays true forever and every smartScroll() becomes a
        // no-op: the view silently stops following new messages. The watchdog
        // clears the flag and pins so the UI recovers even if a restore hangs.
        const REPLAY_WATCHDOG_MS = 20000;
        // Messages rendered per rAF batch during a chunked replay. Small
        // enough that each batch stays within a frame budget, so the
        // transcript paints incrementally instead of one giant dirty region.
        const REPLAY_CHUNK_SIZE = 12;
        // Per-batch watchdog allowance: covers frame stalls (low refresh
        // rate, slow machines) on top of the base deadline.
        const REPLAY_CHUNK_BUDGET_MS = 100;
        let replayWatchdog = null;
        function armReplayWatchdog(historyLength) {
            clearTimeout(replayWatchdog);
            // The chunked replay yields one frame per batch, so a very large
            // history can legitimately take many frames: scale the deadline
            // with the number of batches instead of weakening the base.
            const batches = Math.ceil((historyLength || 0) / REPLAY_CHUNK_SIZE);
            replayWatchdog = setTimeout(() => {
                replayWatchdog = null;
                if (!replayInProgress) return;
                console.warn('history replay watchdog fired; re-enabling scroll follow');
                replayInProgress = false;
                // Lift the rebuild blur too: a wedged replay must not leave
                // the transcript blurred (or the strip labelled) forever.
                endRestoring();
                // The replay is considered stuck: release the colorize gate
                // too (replayHistory's finally is a no-op once its token is
                // gone) so a wedged rebuild can never block colorization
                // forever. Ends the newest active deferral.
                endColorizeDeferral();
                flushDeferredResultColorize();
                flushReplayEventBuffer();
                pinToBottom();
            }, REPLAY_WATCHDOG_MS + batches * REPLAY_CHUNK_BUDGET_MS);
        }
        function disarmReplayWatchdog() {
            clearTimeout(replayWatchdog);
            replayWatchdog = null;
        }

        // Flush live events buffered during a chunked replay, in arrival
        // order. Called from handleHistory's afterHistory — AFTER the rewind
        // merge, so stream positions are seeded before the live tokens are
        // appended (appendStreamToken drops tokens with no stream bubble) —
        // and from the watchdog, so events are not lost if a replay hangs.
        function flushReplayEventBuffer() {
            while (replayEventBuffer.length) {
                if (replayInProgress) {
                    // A nested replay started (a buffered history payload for
                    // a session switch): hand the buffer to that replay's
                    // afterHistory flush.
                    return;
                }
                const data = replayEventBuffer.shift();
                const handler = WS_HANDLERS[data.type];
                if (!handler) continue;
                // The active pane may have changed while the replay ran:
                // route exactly as ws.onmessage would.
                const pane = paneForMessage(data);
                if (!pane) {
                    if (data.type === 'session_state') sendSessionDetach(data.sessionId);
                    continue;
                }
                if (pane === activePane()) handler(data);
                else handleBackgroundMessage(pane, data);
            }
        }

        // settleScroll (optional) overrides the final scroll settlement:
        // default pin-to-bottom; the history handler passes a hook that
        // restores the reader's scroll anchor when one is valid (see
        // paneScrollRestoreSettle).
        async function replayHistory(history, settleScroll) {
            // The replay always rebuilds the ACTIVE pane's transcript
            // (handleHistory resolves it): track how far that pane's
            // rendered-index mark advances as entries are applied.
            const replayedPane = activePane();
            // Session/history loads should always land at the bottom.
            enableFollow();
            historyToolCallArgs = {};
            // Self-contained: a history snapshot replaces the whole pane, so
            // reset leftover tool-card state rather than relying on the caller
            // having run clearChat() first.
            streamingToolCards = {};
            pendingToolCards = {};
            bgTermCards = {};
            toolsStartedThisTurn = false;
            // Suppress per-message smartScroll while rebuilding; the final
            // settle below does a single scroll pass instead of O(n)
            // forced layouts. The history handler's error fallback also resets
            // this so a failed replay still scrolls while re-appending.
            replayInProgress = true;
            // No Monaco import or code tokenization while the replay builds
            // the transcript (see the colorize scheduler in editor.js): the
            // restored session paints first, colors follow on idle slices.
            // The token is this replay's own: if the watchdog ends it first
            // and a NEW replay starts, this replay's finally must not end the
            // new deferral.
            const replayDeferral = beginColorizeDeferral();
            // Follow the rebuild as it grows when the pane will land at the
            // bottom anyway (no reading anchor to restore). Every per-message
            // smartScroll stays suppressed below — the final settle is still
            // the single correctness pass — but without this the viewport sat
            // wherever it was while the whole transcript painted BELOW the
            // fold, and only jumped to the bottom at the very end: the render
            // looked finished long before the scroll arrived. pinToBottom is
            // one scrollTo (no scrollHeight read, no forced layout), so once
            // per chunk — not per message — is cheap. A reading anchor
            // (settleScroll) must not be touched, and a user who scrolls up
            // mid-rebuild un-pins, so the isPinned() check stops following.
            const followWhileReplaying = !settleScroll;
            armReplayWatchdog(history.length);
            function msgDate(createdAt) {
                return createdAt ? new Date(createdAt) : new Date();
            }
            function renderHistoryEntry(h) {
                if (h.role === 'user') {
                    appendMessageAtTime('user', h.content || '', msgDate(h.createdAt), h.index, h.images);
                } else if (h.role === 'assistant') {
                    // Skip when reasoning was promoted into content (same text).
                    if (h.reasoning && h.reasoning !== h.content) {
                        const { card: div, body } = createThoughtCard();
                        // Same live-style markdown render as streaming thinking.
                        setMessageMarkdown(body, h.reasoning || '');
                        if (h.createdAt) {
                            div.title = formatExactTime(msgDate(h.createdAt));
                            div.dataset.createdAt = h.createdAt;
                        }
                        appendTranscriptChild(div);
                        // Replayed reasoning is final; allow the paint skip.
                        enablePaintSkip(div);
                        smartScroll();
                    }
                    // Render refusal text through the normal assistant bubble
                    // when there is no content (OpenAI-style refusals).
                    const assistantText = h.content || h.refusal;
                    if (assistantText) appendMessageAtTime('assistant', assistantText, msgDate(h.createdAt), h.index, undefined, h.model);
                    if (Array.isArray(h.toolCalls)) {
                        for (const tc of h.toolCalls) {
                            historyToolCallArgs[tc.id] = tc.args || {};
                            const cardInfo = createToolCard(tc.name, tc.args || {});
                            cardInfo.toolName = tc.name;
                            cardInfo.args = tc.args || {};
                            if (tc.name === 'patch_file' && tc.args && typeof tc.args.diff === 'string') {
                                if (cardInfo.waiting) {
                                    cardInfo.waiting.remove();
                                    cardInfo.waiting = null;
                                }
                                // NOT awaited: the per-message render path
                                // stays synchronous within its batch.
                                // ensurePatchViewer renders the static
                                // fallback immediately and mounts the Monaco
                                // diff editor in the background. (The
                                // chunk-level rAF yields below are safe:
                                // live events arriving during them are
                                // buffered and flushed after the replay.)
                                ensurePatchViewer(cardInfo, tc.args.diff).catch(() => {});
                            }
                            if (tc.id) pendingToolCards[tc.id] = cardInfo;
                        }
                    }
                } else if (h.role === 'tool') {
                    const id = h.toolCallId;
                    let cardInfo = id ? pendingToolCards[id] : null;
                    const args = (id && historyToolCallArgs[id]) || {};
                    const name = cardInfo ? cardInfo.toolName : '';
                    if (!cardInfo) {
                        // Orphan tool result — create a minimal card
                        cardInfo = createToolCard(name || 'tool', args);
                        cardInfo.toolName = name || 'tool';
                        cardInfo.args = args;
                    }
                    if (id) delete pendingToolCards[id];
                    const success = !String(h.content || '').trim().startsWith('Error:');
                    updateToolCardWithResult(cardInfo, h.content || '', success, false, cardInfo.toolName);
                }
            }
            // Chunked: render REPLAY_CHUNK_SIZE entries per synchronous
            // batch, then yield one frame so the rebuild paints in small
            // tasks instead of one giant dirty region (the forced-reflow
            // spike in the profiler). Live events arriving during a yield
            // are buffered in ws.onmessage and flushed in arrival order
            // after the replay (flushReplayEventBuffer), so they can't
            // interleave mid-rebuild.
            try {
                for (let i = 0; i < history.length; i += REPLAY_CHUNK_SIZE) {
                    const end = Math.min(i + REPLAY_CHUNK_SIZE, history.length);
                    for (let j = i; j < end; j++) {
                        const entry = history[j];
                        // Every applied entry — tool entries included, which
                        // render no bubble of their own — advances the pane's
                        // rendered-index mark, so a later conditional attach
                        // can prove the transcript is current.
                        if (replayedPane && typeof entry.index === 'number' && entry.index > replayedPane.renderedHistIdx) {
                            replayedPane.renderedHistIdx = entry.index;
                        }
                        renderHistoryEntry(entry);
                    }
                    if (end < history.length) {
                        // Keep a following viewport at the bottom as the
                        // rebuild grows (see followWhileReplaying above).
                        if (followWhileReplaying && isPinned()) pinToBottom();
                        // rAF aligns each batch with a paint; the timeout keeps
                        // the replay moving when rAF is throttled (hidden tab),
                        // where the old synchronous loop would have completed
                        // anyway.
                        await new Promise((r) => {
                            requestAnimationFrame(r);
                            setTimeout(r, 50);
                        });
                    }
                }

            } finally {
                // Whatever happens (including a thrown render), the deferral
                // must end or every later colorize job would queue forever.
                // A stale token (watchdog already ended it) is a no-op.
                endColorizeDeferral(replayDeferral);
            }
            replayInProgress = false;
            disarmReplayWatchdog();
            flushDeferredResultColorize();
            // Settle twice: once immediately, then again after a frame to
            // catch late DOM mutations (Monaco colorization, image loads,
            // etc.). Default settlement is pin-to-bottom; a settle hook
            // (paneScrollRestoreSettle) re-positions the reader's scroll
            // anchor with the same double-pass cadence instead.
            const settle = settleScroll || pinToBottom;
            settle();
            requestAnimationFrame(() => {
                settle();
                // Dispatch event so the gogen-colorized handler re-scrolls if
                // Monaco finishes coloring after this rAF.
                window.dispatchEvent(
                    new CustomEvent('gogen-colorized', { bubbles: false })
                );
            });
        }

        // ── Attach rewind merge ──
        // A mid-turn attach's history payload carries the in-flight reply
        // (the server's live-turn buffer) in `rewind`. It is rendered
        // through the normal stream machinery below, then merged with any
        // content the client already rendered live before the snapshot
        // arrived. Both the rewind and the live-rendered content are
        // contiguous slices of the same per-round buffer, so trimming
        // against the server-stamped positions is exact: never a duplicate
        // chunk, never a dropped one.

        // Last server-side message index in a history payload (-1 when none).
        function lastHistoryIndex(history) {
            let max = -1;
            for (const h of history || []) {
                if (h.index !== undefined && h.index >= 0 && h.index > max) max = h.index;
            }
            return max;
        }

        // Newest server-side message index currently rendered in the DOM.
        function newestDomHistIdx() {
            let max = -1;
            for (const el of messagesDiv.querySelectorAll('.message[data-hist-idx]')) {
                const v = parseInt(el.dataset.histIdx, 10);
                if (Number.isFinite(v) && v > max) max = v;
            }
            return max;
        }

        // Live in-flight render state: stream/thinking nodes or streaming /
        // pending tool cards. Used by the stale-snapshot skip (converge at
        // turn_end) and by the pane-switch cache decision (a live pane's
        // transcript must not be cached — background events never reach it).
        function hasLiveInFlight() {
            return !!(currentStreamDiv || currentThinkingDiv
                || Object.keys(streamingToolCards).length > 0
                || Object.keys(pendingToolCards).length > 0);
        }

        // Snapshot the live stream state so the rewind merge can splice the
        // already-rendered tail exactly (positions are server-stamped).
        function captureLiveStreamState() {
            const toolCards = {};
            for (const [index, card] of Object.entries(streamingToolCards)) {
                toolCards[index] = { rawArgs: card.rawArgs || '', argsPos: card.argsPos || 0 };
            }
            return {
                streamRaw: currentStreamRaw,
                streamPos: streamContentPos,
                thinkingRaw: currentThinkingRaw,
                thinkingPos: thinkingContentPos,
                thinkingFinalized: lastFinalizedThinking,
                toolCards,
            };
        }

        // Render the attach snapshot's in-flight turn through the normal
        // streaming machinery, then merge any live-rendered tail. Returns
        // true when anything was rendered (so the caller can skip the
        // generic "Resuming…" indicator).
        function renderRewindAndMerge(rewind, captured) {
            if (!rewind) return false;
            let rendered = false;
            // Thinking: open a thought card from the rewind, then append any
            // live-rendered thinking tail beyond the rewind's end.
            if (rewind.thinking || captured.thinkingRaw) {
                const basePos = rewind.thinkingPos || 0;
                appendThinkingToken(rewind.thinking || '', basePos);
                let capRaw = captured.thinkingRaw;
                let capPos = captured.thinkingPos || 0;
                if (!capRaw && captured.thinkingFinalized && captured.thinkingFinalized.raw) {
                    capRaw = captured.thinkingFinalized.raw;
                    capPos = captured.thinkingFinalized.pos || 0;
                }
                const tail = trimToEnd(capRaw, capPos, basePos);
                if (tail) appendThinkingToken(tail, Math.max(basePos, capPos));
                rendered = true;
            }
            // Content: start the assistant bubble, then append the tail.
            if (rewind.content || captured.streamRaw) {
                finalizeThinking();
                startStream();
                appendStreamToken(rewind.content || '', rewind.contentPos || 0);
                const tail = trimToEnd(captured.streamRaw, captured.streamPos || 0, rewind.contentPos || 0);
                if (tail) appendStreamToken(tail, Math.max(rewind.contentPos || 0, captured.streamPos || 0));
                rendered = true;
            }
            // In-progress tool calls: waiting cards continued by live events.
            if (rewind.toolCalls && rewind.toolCalls.length) {
                for (const tc of rewind.toolCalls) {
                    const cardInfo = startStreamingToolCard(tc.index, tc.name || 'tool');
                    if (tc.args) appendStreamingToolArgs(tc.index, tc.args, tc.argsPos);
                    const cap = captured.toolCards && captured.toolCards[tc.index];
                    if (cap && cap.rawArgs) {
                        const tail = trimToEnd(cap.rawArgs, cap.argsPos || 0, tc.argsPos || 0);
                        if (tail) appendStreamingToolArgs(tc.index, tail, Math.max(tc.argsPos || 0, cap.argsPos || 0));
                    }
                    // Register the card under the call's id: the call is
                    // still running, and the client MISSED its live
                    // tool_call frame (that is why it is in the rewind), so
                    // nothing else would match the tool_result frame to this
                    // card — it would fall through to the raw
                    // "[tool] <result>" system blob. The live tool_call
                    // frame (when it still arrives) re-registers the same
                    // card, which is a no-op.
                    if (tc.id) pendingToolCards[tc.id] = cardInfo;
                }
                rendered = true;
            }
            // Progress phase reflects what was painted.
            if (rewind.toolCalls && rewind.toolCalls.length) setInputProgress('tool', 'Running tool\u2026');
            else if (rewind.content) setInputProgress('streaming', streamProgressLabel());
            else if (rewind.thinking) setInputProgress('thinking', thinkingProgressLabel());
            return rendered;
        }

        // ===== Terminal panel =====
        // The docked terminal strip (pinned user shell + read-only agent
        // command tabs) lives in components/terminal.js.
        initTerminal({ getWs: () => ws });

        function connect() {
            const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
            ws = new WebSocket(`${protocol}//${window.location.host}/ws`);

            ws.onopen = () => {
                clearTimeout(reconnectTimer);
                reconnectTimer = null;
                reconnectDelay = RECONNECT_BASE_MS;
                setConnState('connected');
                // Replace (don't append) on every connect: the server may omit
                // history when the session is empty, and reconnect must not
                // keep a stale transcript. Connection status lives in the
                // indicator, not as chat system messages.
                clearChat();
                // Reset the lazy model-catalog guard so a fresh connection can fetch again.
                modelsRequested = false;
                // Only request the cheap, local session list eagerly. The
                // model catalog is a remote /v1/models call that can dominate
                // startup latency, so fetch it after first paint (idle).
                ws.send(JSON.stringify({ type: 'list_sessions' }));
                // The server spawns the interactive user shell lazily (first
                // user_term_request), not at connect. Ask for it now when the
                // terminal panel is already visible; a fresh socket also
                // revives a shell that died with the previous connection.
                terminalUserEnsure(true);
                const scheduleModels = window.requestIdleCallback
                    || ((fn) => setTimeout(fn, 200));
                scheduleModels(() => ensureModelsLoaded());

                // Re-attach every open pane: the server resends
                // session_state + history + config + context per pane. The
                // active pane's transcript rebuilds from its attach response;
                // background panes just re-register (their transcript
                // re-derives when focused) and request NO history — the
                // full snapshot would be discarded client-side, so the
                // server neither builds nor sends it (noHistory). Panes
                // with no session yet (first load) get their state from the
                // connect handshake.
                //
                // The server re-points its per-connection "current pane" at
                // EVERY attach, so the ACTIVE pane is attached LAST:
                // otherwise the pointer would land on the last-inserted pane
                // and messages typed in the active pane would route to the
                // wrong session until the next attach.
                const activeP = activePane();
                for (const pane of panes.values()) {
                    if (pane.id && pane !== activeP) {
                        ws.send(JSON.stringify({ type: 'session_attach', sessionId: pane.id, noHistory: true }));
                    }
                }
                if (activeP && activeP.id) {
                    ws.send(JSON.stringify({ type: 'session_attach', sessionId: activeP.id }));
                }
                // Panes whose session creation never completed before the
                // disconnect have no id yet: re-issue session_new so they
                // get a real session instead of sitting at "creating…"
                // forever (or being adopted onto the handshake's default
                // session). Only on a RECONNECT — on first load the initial
                // pane (also id-less) must adopt the server's default session
                // from the connect handshake, not create a fresh one.
                if (wasDisconnected) {
                    for (const pane of panes.values()) {
                        if (!pane.id) {
                            pane.pendingSessionResponse = true;
                            if (pane === activeP) pendingSessionResponse = true;
                            ws.send(JSON.stringify({ type: 'session_new' }));
                        }
                    }
                }

                // Notify on reconnection after a disconnect
                if (wasDisconnected) {
                    wasDisconnected = false;
                    sendNotification('GoGen', 'Reconnected to server.', 'gogen-reconnect');
                }
            };

            ws.onmessage = (event) => {
                let data;
                try {
                    data = JSON.parse(event.data);
                } catch (err) {
                    console.error('Failed to parse server message:', err, 'raw:', event.data.slice(0, 200));
                    return;
                }
                // Editor responses (requestId-keyed) arrive on the separate
                // /ws/editor socket handled by editor.js; the chat socket
                // only ever carries chat/session/terminal messages.
                // Multi-pane routing: connection-scoped messages (no
                // sessionId) and messages for the active pane run the normal
                // handler below; background-pane messages update that pane's
                // state only (the transcript re-derives when focused).
                // Subagent events are sidebar-global: a child of ANY pane
                // this connection is attached to updates the nested rows,
                // regardless of which pane is focused.
                if (data.type === 'subagent_started' || data.type === 'subagent_finished') {
                    handleSubagentEvent(data);
                    return;
                }
                // Delete approvals are global UI (a modal overlay): show
                // them even for sessions without an open pane — board-started
                // agents are background-attached to the initiating tab, so
                // their approvals pop the modal while the user stays on the
                // board. The response carries the sessionId, so routing by
                // pane is not needed (and paneForMessage would drop it).
                if (data.type === 'delete_approval') {
                    handleDeleteApproval(data);
                    return;
                }
                if (data.type === 'command_approval') {
                    handleCommandApproval(data);
                    return;
                }
                // Display-relay frames are connection/process-global: every
                // attached tab may relay them (the reporter is process-wide),
                // so they are handled before pane routing.
                if (data.type === 'status_display') {
                    handleStatusDisplayFrame(data);
                    return;
                }
                const msgPane = paneForMessage(data);
                if (!msgPane) {
                    // The connect handshake attaches this connection to the
                    // restored default session even when none of this tab's
                    // panes is it (a reconnect whose panes are other
                    // sessions). Release that attachment: an idle default
                    // must be free to orphan-evict, or it would pin the
                    // "resume to continue" indicator for the tab's life.
                    // Only the handshake sends session_state for a session
                    // this tab has no pane for — attach replies always name
                    // a pane the tab opened, and in-flight session changes
                    // route to their initiator pane (paneForMessage).
                    if (data.type === 'session_state') sendSessionDetach(data.sessionId);
                    return; // stale message for a closed session
                }
                // Stamp output recency for BOTH active and background panes
                // (drives sidebar open-pane order); meta types are no-ops.
                touchPaneActivity(msgPane, data.type);
                if (msgPane !== activePane()) {
                    handleBackgroundMessage(msgPane, data);
                    return;
                }
                const handler = WS_HANDLERS[data.type];
                if (!handler) return;
                if (replayInProgress && !replayImmediate(data)) {
                    // The chunked replay yields to rAF between batches:
                    // buffer the event and flush it in arrival order after
                    // the replay (and its rewind merge) completes, so live
                    // stream events can't interleave mid-rebuild. Frames that
                    // never touch the transcript render immediately (see
                    // REPLAY_IMMEDIATE_TYPES).
                    replayEventBuffer.push(data);
                    return;
                }
                handler(data);
            };

            ws.onclose = () => {
                setConnState('disconnected');
                endStream();
                streamingToolCards = {};
                pendingToolCards = {};
                bgTermCards = {};
                clearPendingResend();
                suppressTurnEnds = 0;
                // Acks in flight during the disconnect are lost with the
                // socket; the re-attach re-derives histIdx from history.
                pendingAcks.length = 0;
                pendingCancels.length = 0;
                pendingSessionResponse = false;
                compacting = false;
                setTurnActive(false);
                // Reset per-pane in-flight state; the re-attach on reconnect
                // re-syncs each pane from the server (session_state).
                for (const pane of panes.values()) {
                    pane.turnActive = false;
                    pane.needsFreshHistory = false;
                    // A resume that never completed (connection dropped
                    // before the reply) must not leave a stale ignore flag:
                    // after reconnect this pane's own turn_end must count.
                    pane.ignoreTurnEndsFor = null;
                    pane.suppressTurnEnds = 0;
                    pane.pendingSessionResponse = false;
                    pane.pendingResendContent = null;
                    pane.pendingResendImages = null;
                    pane.resendAwaitingHistory = false;
                    pane.compacting = false;
                    pane._notifiedBusy = false;
                }
                terminalInterruptAll();
                setInputProgress(null);
                clearTimeout(reconnectTimer);
                setConnState('reconnecting');
                wasDisconnected = true;
                reconnectTimer = setTimeout(connect, reconnectDelay);
                reconnectDelay = Math.min(reconnectDelay * 2, RECONNECT_MAX_MS);
            };
        }

        // ── WebSocket message dispatch ──
        // connect()'s onmessage routes each server message type to a
        // dedicated handler below. The map is the single source of truth;
        // unknown types are ignored (matching the pre-refactor else-if
        // chain's implicit fall-through).
        const WS_HANDLERS = {
            compacting: handleCompacting,
            thinking: handleThinking,
            waiting: handleWaiting,
            thinking_token: handleThinkingToken,
            stream: handleStream,
            stream_stats: handleStreamStats,
            stream_retry: handleStreamRetry,
            stream_end: handleStreamEnd,
            model_used: handleModelUsed,
            tool_call_start: handleToolCallStart,
            tool_call_delta: handleToolCallDelta,
            hist_index: handleHistIndex,
            tool_call: handleToolCall,
            tool_execute: handleToolExecute,
            tool_result: handleToolResult,
            term_opened: handleTermOpened,
            term_output: handleTermOutput,
            term_exit: handleTermExit,
            user_term_opened: handleUserTermOpened,
            user_term_output: handleUserTermOutput,
            user_term_exit: handleUserTermExit,
            cancelled: handleCancelled,
            cancel_ack: handleCancelAck,
            turn_end: handleTurnEnd,
            clear_chat: handleClearChat,
            user_acked: handleUserAcked,
            queue_update: handleQueueUpdate,
            sessions: handleSessions,
            session_state: handleSessionState,
            session_removed: handleSessionRemoved,
            session_detached: handleSessionDetached,
            response: handleResponse,
            models: handleModels,
            history: handleHistory,
            config: handleConfig,
            context: handleContext,
            delete_approval: handleDeleteApproval,
            command_approval: handleCommandApproval,
            board_state: handleBoardState,
            automations_state: handleAutomationsState,
            automations_runs: handleAutomationsRuns,
            notice: handleNotice,
            provider_test: handleProviderTest,
            mcp_test: handleMCPTest,
            condensed: handleCondensed,
        };

        function handleProviderTest(data) {
            const res = data.providerTest;
            if (!res) return;
            if (res.ok) {
                const models = res.models || [];
                const first = models.length > 0 ? ' (' + models[0].id + ')' : '';
                showProviderTest(
                    '✓ Connected — ' + models.length + ' model' + (models.length === 1 ? '' : 's') + ' in ' + res.latencyMs + ' ms' + first,
                    'success'
                );
            } else {
                showProviderTest('✗ Failed: ' + (res.error || 'unknown error'), 'error');
            }
        }

        function handleMCPTest(data) {
            const res = data.mcpTestResult;
            if (!res) return;
            if (res.ok) {
                const tools = res.tools || [];
                const names = tools.slice(0, 5).map((t) => t.name).join(', ');
                const suffix = names ? ' (' + names + (tools.length > 5 ? ', …' : '') + ')' : '';
                showMCPTest(
                    '✓ Connected — ' + tools.length + ' tool' + (tools.length === 1 ? '' : 's') + ' in ' + res.latencyMs + ' ms' + suffix,
                    'success'
                );
            } else {
                showMCPTest('✗ Failed: ' + (res.error || 'unknown error'), 'error');
            }
        }

        function handleCompacting(data) {

            // Auto-compaction inside a normal turn (or the manual
            // /compact command): keep a live indicator until the next
            // progress event (thinking/stream/tool) or the compact
            // response replaces it.
            setInputProgress('compacting', 'Compacting context\u2026');

            // Compaction rewrites the middle of the message list — every
            // index after the summary shifts, and NO history frame follows a
            // compact to re-stamp the DOM (data-hist-idx feeds edit/resend/
            // fork). Void the fingerprint claim so no attach can prove
            // "already current" against the old epoch, and mark the indexes
            // dirty: the next turn_end's convergence attach replays the
            // compacted transcript with fresh stamps, and edit/resend/fork
            // are gated (indexesDirty) until it lands. A background pane's
            // compacting frame is dropped by its handler — its STALE epoch
            // value then mismatches the server's on focus, which forces the
            // same full snapshot. (Condensation — the `condensed` frame —
            // rewrites content in place and shifts nothing: no invalidation.)
            const cp = activePane();
            if (cp && (!data.sessionId || cp.id === data.sessionId)) {
                cp.histEpoch = undefined;
                cp.indexesDirty = true;
            }
        }

        function handleCondensed(data) {

            // Phase 0e last-resort condensation: a message was condensed
            // in place because it could not fit the context window. Show
            // the announcement as a dismissible banner above the composer
            // (in-band — the condensed message itself is in the history).
            if (!condensedBanner || !data.content) return;
            condensedBannerText.textContent = data.content;
            condensedBanner.hidden = false;

        }

        function handleThinking(data) {

            setTurnActive(true);
            updateTitle('thinking');
            // New model round (OnStart / OnRoundStart): clear per-round
            // tool state so reasoning shows again, matching TUI.
            toolsStartedThisTurn = false;
            streamingToolCards = {};
            lastStreamDiv = null; // new round: forget any prior bubble
            lastStreamSpeed = 0; // the server re-arms its meter per round
            finalizeThinking();
            // New round = a fresh silent window: restart the placeholder
            // (and its elapsed clock) for this round.
            clearWaitingPlaceholder();
            showWaitingPlaceholder('Thinking\u2026');
            setInputProgress('thinking', 'Thinking\u2026');

        }

        function handleWaiting(data) {

            setTurnActive(true);
            updateTitle('thinking');
            // The stream socket is open but no chunk has arrived yet:
            // re-label the placeholder (the elapsed clock keeps running
            // from the round start).
            showWaitingPlaceholder('Waiting for model\u2026');
            setInputProgress('thinking', 'Waiting for model\u2026');

        }

        function handleThinkingToken(data) {

            setTurnActive(true);
            // The model is producing: the placeholder's job is done.
            clearWaitingPlaceholder();
            appendThinkingToken(data.content || '', data.thinkingPos);
            // The model is producing: the round-start "Waiting for
            // model…" label is stale once the first thinking token
            // lands, and the rate (when known) rides along like in the
            // streaming phase.
            if (currentProgressPhase === 'thinking') {
                setInputProgress('thinking', thinkingProgressLabel());
            }

        }

        function handleStreamRetry(data) {

            // A failed stream attempt is being retried with live delivery
            // muted (streaming retry, or the non-streaming fallback): the
            // following silent stretch is a whole regeneration, not a hang.
            // The next thinking/stream/stats frame restores the normal
            // label. Older handlers simply never registered this type.
            setTurnActive(true);
            updateTitle('thinking');
            // Re-label the placeholder with the failure cause; the elapsed
            // clock keeps running from the round start, so the user sees
            // the whole silent stretch, not just the retry's.
            showWaitingPlaceholder('Retrying stream (' + (data.reason || 'connection issue') + ')\u2026');
            setInputProgress('thinking', 'Retrying stream (' + (data.reason || 'connection issue') + ')\u2026');

        }

        function handleStream(data) {

            setTurnActive(true);
            updateTitle('streaming');
            // First content token: the placeholder is replaced by the real
            // bubble startStream creates below.
            clearWaitingPlaceholder();
            // The rate (when known) is carried in the label so stream
            // frames and stream_stats frames never fight over it.
            setInputProgress('streaming', streamProgressLabel());
            finalizeThinking();
            if (!currentStreamDiv) startStream();
            appendStreamToken(data.content, data.contentPos);

        }

        function handleStreamStats(data) {

            // Timer-driven (a few per second) while the round streams;
            // the server stops its meter at round end, so this always
            // precedes stream_end. Only touch the label while the
            // streaming or thinking indicator is up — a stats frame must
            // not clobber the "Running tool…" label.
            lastStreamSpeed = data.tokensPerSec || 0;
            if (currentProgressPhase === 'streaming') {
                setInputProgress('streaming', streamProgressLabel());
            } else if (currentProgressPhase === 'thinking') {
                setInputProgress('thinking', thinkingProgressLabel());
            }

        }

        function handleStreamEnd(data) {

            // Round over without any token (e.g. an empty completion): do
            // not leave the placeholder up for the next frame.
            clearWaitingPlaceholder();
            finalizeThinking();
            endStream();

        }

        function handleModelUsed(data) {

            // Stamp the last (still-live) assistant bubble with the
            // provider-reported model. The server sends this before
            // stream_end; history replay attributes older messages.
            if (data.model) {
                const bubble = currentStreamDiv || lastStreamDiv;
                if (bubble) {
                    bubble.dataset.model = data.model;
                    applyReplyModelChip(bubble);
                }
            }

        }

        function handleToolCallStart(data) {

            setTurnActive(true);
            // The round produced a tool call instead of text: the tool card
            // takes over from the placeholder.
            clearWaitingPlaceholder();
            setInputProgress('tool', 'Running ' + (data.tool || 'tool') + '\u2026');
            startStreamingToolCard(data.index, data.tool);

        }

        function handleToolCallDelta(data) {

            finalizeThinking();
            if (!streamingToolCards[data.index]) {
                startStreamingToolCard(data.index, data.tool || 'tool');
            } else if (data.tool) {
                streamingToolCards[data.index].toolName = data.tool;
            }
            appendStreamingToolArgs(data.index, data.argsDelta || '', data.argsPos);

        }

        function handleToolCall(data) {

            finalizeThinking();
            endStream();
            toolsStartedThisTurn = true;
            finalizeStreamingToolCard(data.index, data.toolCallId, data.tool, data.args || {});

        }

        function handleToolExecute(data) {

            // Update waiting text on the matching pending card if present
            const name = data.tool || '';
            setInputProgress('tool', name ? ('Running ' + name + '\u2026') : 'Running tool\u2026');
            for (const id of Object.keys(pendingToolCards)) {
                const cardInfo = pendingToolCards[id];
                if (cardInfo && cardInfo.waiting && (!name || cardInfo.toolName === name)) {
                    cardInfo.waiting.textContent = name ? `running ${name}...` : 'executing...';
                }
            }

        }

        function handleToolResult(data) {

            finalizeThinking();
            // Account for tool result tokens in the live context estimate.
            bumpContextEstimate(data.result);
            // Stay busy until turn_end — more rounds may follow.
            const cardInfo = pendingToolCards[data.toolCallId];
            if (cardInfo) {
                updateToolCardWithResult(
                    cardInfo,
                    data.result,
                    // Server serializes Success with omitempty, so a
                    // failed tool omits the field entirely (undefined).
                    // Only an explicit true means success.
                    data.success === true,
                    data.resultTruncated,
                    data.tool || cardInfo.toolName
                );
                delete pendingToolCards[data.toolCallId];
                // A background job's output keeps streaming AFTER this
                // result (the job outlives the turn): keep the card
                // reachable by term id so handleTermOutput keeps mirroring
                // into its live region until term_exit.
                if (cardInfo.args && cardInfo.args.background === true) {
                    bgTermCards[data.toolCallId] = cardInfo;
                }
            } else {
                // No card matched this result (the call was announced to a
                // different client, or before this pane attached). Render a
                // minimal card exactly like the history replay's orphan tool
                // result — a raw "[tool] <result>" text blob in the
                // transcript reads as corruption (and is what an uncarded
                // result used to look like).
                const orphan = createToolCard(data.tool || 'tool', {});
                orphan.toolName = data.tool || 'tool';
                orphan.args = {};
                updateToolCardWithResult(
                    orphan,
                    data.result || '',
                    data.success === true,
                    data.resultTruncated,
                    orphan.toolName
                );
            }

        }

        function handleTermOpened(data) {

            terminalOpen(data.termId, data.tool || '', data.content || '');
            // Mirror the command echo into the pending tool card so
            // shell output streams into the chat, not just the
            // (collapsed-by-default) terminal dock. bgTermCards covers
            // background jobs, whose terminal can open around the tool
            // result (the job outlives the turn).
            const cardInfo = pendingToolCards[data.termId] || bgTermCards[data.termId];
            if (cardInfo && data.content) {
                // Echo the command like the terminal tab does, on its
                // own line ("$ ..." then the streaming output).
                appendToolCardLiveOutput(cardInfo, data.content + '\n');
            }

        }

        function handleTermOutput(data) {

            terminalWrite(data.termId, data.content || '');
            // bgTermCards fallback: a background job's tool result already
            // cleared the card from pendingToolCards while the job (and its
            // output stream) is still running.
            const cardInfo = pendingToolCards[data.termId] || bgTermCards[data.termId];
            if (cardInfo) {
                appendToolCardLiveOutput(cardInfo, data.content || '');
            }

        }

        function handleTermExit(data) {

            // Success is omitted when false, so only an explicit true
            // marks the tab as ok; anything else is a failure.
            terminalExit(data.termId, data.success === true);
            terminalFitSoon();
            const cardInfo = pendingToolCards[data.termId] || bgTermCards[data.termId];
            if (cardInfo && cardInfo.liveOutput) {
                cardInfo.liveOutput.classList.add('done');
                cardInfo.liveOutput.classList.add(data.success === true ? 'success' : 'error');
            }
            // The job's output stream is over: drop the post-result mirror
            // reference (also covers a term_exit that raced ahead of the
            // tool result for a fast job).
            delete bgTermCards[data.termId];

        }

        function handleUserTermOpened(data) {

            terminalUserOpened(data.content || 'shell', data.workingDir || '');

        }

        function handleUserTermOutput(data) {

            terminalEnsureUserTab();
            terminalWrite(USER_TERM_ID, data.content || '');

        }

        function handleUserTermExit(data) {

            terminalUserExited(data.code || 0);

        }

        // The server's answer to a cancel that found nothing in flight
        // (session gone, turn already ended, or another connection's cancel
        // won the race): the cancelled/turn_end pair that would normally
        // consume the unit held by cancelActiveTurn never comes. Consume
        // exactly ONE unit — the net effect of that pair (pending-cancel
        // record shifted + unit decremented + stale cancel target dropped)
        // — and nothing else: no turnActive / pendingSessionResponse /
        // notification side effects, so it is safe in every interleaving. A
        // real pair that was already broadcast precedes this frame on the
        // socket and drains the unit first, leaving this a no-op.
        function handleCancelAck() {
            if (suppressTurnEnds <= 0) return;
            suppressTurnEnds--;
            const rec = pendingCancels.shift();
            if (rec && pendingAcks.includes(rec)) dropPendingAck(rec);
            lastCancelTarget = null;
        }

        function handleCancelled(data) {

            // A turn cancelled before the server emitted its user_acked
            // frame never acks: drop its pending entry now, or the stale
            // flag steals the next ack (stamping the new message's index
            // onto the old bubble). A local cancel always leaves
            // suppressTurnEnds > 0 at frame time (its increment is
            // released only by the follow-up turn_end), so a suppressed
            // frame consumes the record cancelActiveTurn pushed: the
            // record's entry is stale exactly when it is still queued
            // (an in-flight ack arrives before this frame — same-socket
            // FIFO — and leaves the queue first). An unsuppressed frame
            // (another tab/TUI cancelled the turn) targets the current
            // turn, whose un-acked send is the only pending bubble.
            if (suppressTurnEnds > 0) {
                const rec = pendingCancels.shift();
                if (rec && pendingAcks.includes(rec)) dropPendingAck(rec);
            } else if (pendingAcks.length === 1) {
                dropPendingAck(pendingAcks[0]);
            }
            // The cancel this frame belongs to is resolved: a later send
            // must not inherit its target (covers the standalone
            // cancel-button path, which has no markPendingAck to consume
            // it).
            lastCancelTarget = null;
            // Stale cancel from a turn we replaced with resend/send — ignore.
            if (suppressTurnEnds > 0) return;
            abortInFlightUI(data.content || 'Cancelled.');
            setTurnActive(false);
            clearWaitingPlaceholder();
            lastStreamSpeed = 0;
            setInputProgress(null);
            refreshSidebarSessions();

        }

        function handleTurnEnd(data) {

            // Ignore turn_end from a turn we cancelled to start a resend/send.
            // Otherwise it clears pendingSessionResponse / turnActive mid-flow.
            if (suppressTurnEnds > 0) {
                suppressTurnEnds--;
                return;
            }
            // A turn we deliberately left behind when resuming another
            // session: the old session keeps running headless (resume
            // does not cancel it), so its turn_end must not clear the
            // pending flag or block the resumed session's convergence.
            // (Before the reply re-keys the pane, the old session is
            // still this pane's id; after the re-key, paneForMessage
            // drops the old id entirely.)
            const pane = activePane();
            if (pane && pane.ignoreTurnEndsFor && data.sessionId === pane.ignoreTurnEndsFor) {
                pane.ignoreTurnEndsFor = null;
                return;
            }
            setTurnActive(false);
            updateTitle('idle');
            clearWaitingPlaceholder();
            lastStreamSpeed = 0;
            setInputProgress(null);
            pendingSessionResponse = false;
            // A pane attached mid-turn never received the in-flight
            // reply (assistant messages are appended only
            // when a round completes). Now that the turn is done the
            // full history is available — re-attach once so the
            // transcript converges.
            const tp = pane;
            if (tp && (tp.needsFreshHistory || tp.indexesDirty)) {
                tp.needsFreshHistory = false;
                if (tp.id && ws && ws.readyState === WebSocket.OPEN) {
                    const attachMsg = { type: 'session_attach', sessionId: tp.id };
                    // Conditional attach: this pane rendered the turn LIVE
                    // (tokens, tool cards, or the attach rewind) and the
                    // server stamped every appended message with hist_index,
                    // so claim the fingerprint and let the server answer
                    // "already current" — the turn-end convergence refetch
                    // used to rebuild the whole transcript here. Both bounds
                    // are things this pane actually applied; if either trails
                    // (a missed frame, a backgrounded stretch), the server
                    // falls back to the full snapshot exactly as before.
                    const newest = Math.max(
                        Number.isFinite(tp.renderedHistIdx) ? tp.renderedHistIdx : -1,
                        newestDomHistIdx()
                    );
                    if (tp.histEpoch !== undefined && newest >= 0) {
                        attachMsg.knownHistoryEpoch = tp.histEpoch;
                        attachMsg.knownHistoryIndex = newest;
                    }
                    ws.send(JSON.stringify(attachMsg));
                }
            }
            refreshSidebarSessions();
            // The turn just finished: fetch a fresh sessions payload
            // instead of re-rendering the cached one, so the sidebar's
            // saved-section order and per-row relative times reflect
            // the completed turn's new Updated timestamp (the cache is
            // only refreshed on explicit list_sessions round-trips).
            requestSessionList();

        }

        /** Re-render a pane's queued steering bubbles from its server-truth
         * queue mirror (queued items are not part of history — they come from
         * session_state/queue_update): transcript rebuilds wipe the DOM, and a
         * cache restore brings back a DOM that predates queue changes made
         * while the pane was backgrounded (background queue frames only update
         * the mirror, never the DOM).
         *
         * Idempotent: it first drops the queued bubbles the container still
         * holds (only ones actually marked queued — a bubble merely TAGGED
         * with a correlation id is an in-flight send and must stay), then
         * renders the mirror. A locally-sent item that is still queued keeps
         * its pending-ack correlation, so its user_acked still resolves FIFO
         * through pendingAcks instead of the DOM fallback. */
        function rerenderQueuedBubbles(pane) {
            if (!pane) return;
            const bubbles = queuedBubblesFor(pane);
            const local = new Set(bubbles.keys());
            for (const el of messagesDiv.querySelectorAll('.message.user.queued')) {
                cancelTarget.delete(el);
                el.remove();
            }
            prunePendingAcks();
            bubbles.clear();
            for (const it of pane.queueItems || []) {
                const el = appendMessage('user', it.text);
                markQueuedBubble(el, it.id);
                if (local.has(it.id)) rearmPendingAck(el);
                bubbles.set(it.id, el);
            }
            // Each bubble was appended (and anchored) before it was registered
            // in the map, so the rebuild could land them in reverse order:
            // re-anchor the completed map once, in queue order.
            anchorQueuedBubbles();
            refreshQueuedSendNow(pane);
        }

        /**
         * Keep queued (steering) bubbles at the TAIL of the transcript.
         *
         * A queued item has no canonical position yet: the server appends it
         * to the conversation only when its turn runs — after every round of
         * the in-flight turn. Rendering it where the user typed it left it
         * inside the reply being written (the next round's bubble and its
         * tool cards landed BELOW it), and a rebuild then moved it to the
         * tail: the message visibly jumped, and the live order disagreed
         * with the rebuilt one. Anchoring it after every transcript append
         * keeps both orders identical.
         */
        function anchorQueuedBubbles() {
            const pane = activePane();
            const bubbles = pane && pane.queuedBubbles;
            if (!bubbles || !bubbles.size) return;
            const els = [];
            for (const el of bubbles.values()) {
                if (el && el.isConnected) els.push(el);
            }
            if (!els.length) return;
            const children = messagesDiv.children;
            const start = children.length - els.length;
            let anchored = start >= 0;
            for (let i = 0; anchored && i < els.length; i++) {
                if (children[start + i] !== els[i]) anchored = false;
            }
            if (anchored) return;
            // Re-append in queue order (insertion order), moving every queued
            // bubble behind whatever was appended after them.
            for (const el of els) messagesDiv.appendChild(el);
        }

        // Append a transcript element, keeping the queued bubbles behind it.
        // Every transcript append goes through here (message bubbles, stream
        // rounds, waiting placeholders, thought cards, tool cards, replayed
        // history entries) so the tail invariant cannot be missed.
        function appendTranscriptChild(el) {
            messagesDiv.appendChild(el);
            anchorQueuedBubbles();
        }

        function handleClearChat(data) {

            clearChat();
            rerenderQueuedBubbles(activePane());
            // Do NOT re-arm pendingSessionResponse here: the response
            // handler already re-keyed the pane to the new session id
            // and cleared the flag, so the follow-up history/config
            // route by session id without the fallback. Re-arming left
            // the flag stuck true until the next turn_end / reconnect,
            // which permanently blocked resumeSession()/deleteSession()
            // ("Session change already in progress") and made
            // paneForMessage route stale messages to the active pane
            // (which then wrongly re-keyed it).

        }

        function handleUserAcked(data) {

            // Server assigned the Messages index for the user turn just started.
            // Index is in content so 0 is not dropped by JSON omitempty.
            const idx = parseInt(data.content, 10);
            if (!Number.isFinite(idx) || idx < 0) return;
            // The ack belongs to the oldest still-pending bubble (see
            // pendingAcks) — NOT the first flagged bubble in the DOM,
            // which a stale flag from a cancelled/rejected turn would
            // poison.
            const el = resolveAckTarget();
            if (el) {
                dropPendingAck(el);
                el.dataset.histIdx = String(idx);
                ensureUserResendActions(el);
                // The user message is applied: advance the pane's
                // rendered-index mark (see hist_index for the rest).
                const ackPane = activePane();
                if (ackPane && idx > ackPane.renderedHistIdx) ackPane.renderedHistIdx = idx;
                // A queued steering message just became a real user turn:
                // its chip clears (the pending-ack FIFO still held — drains
                // are FIFO, so acks resolve in send order).
                clearQueuedMark(el);
                // Clean the optimistic queue mirror when this bubble was
                // marked queued locally but the server raced it idle (the
                // turn started immediately — no queue_update will come).
                const pane = activePane();
                if (pane) {
                    const bubbles = queuedBubblesFor(pane);
                    for (const [id, e] of [...bubbles]) {
                        if (e === el) {
                            bubbles.delete(id);
                            pane.queueItems = (pane.queueItems || []).filter((it) => it.id !== id);
                            updateQueuedProgressLabel();
                            break;
                        }
                    }
                    refreshQueuedSendNow(pane);
                }
            }

        }

        function handleHistIndex(data) {

            // Server-stamped index of a message the turn just appended
            // (assistant rounds and tool results — see the server's
            // OnMessageAppended). Only the pane that is ACTIVELY rendering
            // the turn advances its rendered-index mark: a background pane's
            // DOM is stale until focused, and background frames never reach
            // here anyway (they route to handleBackgroundMessage). A mark
            // that trails — a missed frame, a backgrounded stretch — only
            // costs the full history snapshot the conditional attach would
            // otherwise skip, never correctness. The index is in Content
            // because WSMessage.Index has omitempty and index 0 is valid
            // (the same convention as user_acked).
            const idx = parseInt(data.content, 10);
            if (!Number.isFinite(idx) || idx < 0) return;
            const pane = activePane();
            if (!pane) return;
            if (idx > pane.renderedHistIdx) pane.renderedHistIdx = idx;
            // Stamp the assistant bubble this round finalized, so
            // edit/resend/fork work on live messages without waiting for any
            // refetch. A tool-result index is harmless here: the round's
            // bubble (if any) either already carries an index or the stale
            // last bubble does — stamped once, never re-stamped.
            const el = lastStreamDiv;
            if (el && el.isConnected && el.classList && el.classList.contains('message')
                && el.dataset.histIdx === undefined) {
                el.dataset.histIdx = String(idx);
            }

        }

        /**
         * Reconcile a pane's queued state from server truth (queue_update
         * frames and the Queue snapshot on session_state). For the ACTIVE
         * pane the DOM is reconciled too:
         *   - an item that left the queue because its turn STARTED
         *     (queueLeft) only clears its chip — the paired user_acked is
         *     next on the socket (worker broadcast happens-before turn
         *     start) and its pending entry resolves there;
         *   - an item that left the queue any other way (✕, clear-all,
         *     interrupt) is cancelled-before-running: its pending entry is
         *     dropped (it has no terminal frame to do it) and the bubble
         *     restyles;
         *   - an item with no bubble (sent by another tab, or the attach
         *     snapshot) renders as a new queued bubble.
         */
        function applyQueueUpdate(pane, data, dom) {
            const items = data.queue || [];
            const left = data.queueLeft || [];
            const leftSet = new Set(left);
            const live = new Set(items.map((it) => it.id));
            const bubbles = queuedBubblesFor(pane);
            const started = new Map();
            for (const [id, el] of [...bubbles]) {
                if (!el.isConnected) { bubbles.delete(id); continue; }
                if (live.has(id)) continue;
                bubbles.delete(id);
                if (leftSet.has(id)) started.set(id, el);
                else resolveCancelledQueued(el);
            }
            // queueLeft is in queue order, and content is the message the
            // started turn runs.
            joinStartedBubbles(left.map((id) => started.get(id)).filter(Boolean), data.content || '');
            if (dom) {
                for (const it of items) {
                    if (bubbles.has(it.id)) continue;
                    // The sending tab's optimistic bubble may exist but be
                    // unmarked (its turnActive mirror raced the server's):
                    // adopt it instead of creating a duplicate.
                    const existing = messagesDiv.querySelector(
                        '.message.user[data-queue-id="' + CSS.escape(it.id) + '"]');
                    if (existing) {
                        markQueuedBubble(existing, it.id);
                        bubbles.set(it.id, existing);
                        continue;
                    }
                    const el = appendMessage('user', it.text);
                    markQueuedBubble(el, it.id);
                    bubbles.set(it.id, el);
                }
                // Adopted and newly appended bubbles were marked after their
                // append-time anchor ran: re-anchor the completed set in
                // queue order.
                anchorQueuedBubbles();
                refreshQueuedSendNow(pane);
            }
            pane.queueItems = items;
            if (pane === activePane()) updateQueuedProgressLabel();
        }

        // joinStartedBubbles turns the bubbles of a run that started as one
        // turn (join_queued) into that turn's one user message: the first
        // keeps the delivered text, rendered like any user message, and
        // takes every image from the others, which are removed. A single
        // started bubble only loses its queued mark.
        function joinStartedBubbles(els, text) {
            if (!els.length) return;
            const keep = els[0];
            if (els.length > 1) {
                const body = msgBody(keep);
                let row = body.querySelector(':scope > .chat-image-row');
                for (const el of els.slice(1)) {
                    for (const img of el.querySelectorAll('.chat-image-row > .chat-image')) {
                        if (!row) {
                            row = document.createElement('div');
                            row.className = 'chat-image-row';
                            body.insertBefore(row, body.firstChild);
                        }
                        row.appendChild(img);
                    }
                    dropPendingAck(el);
                    el.remove();
                }
                keep.dataset.rawContent = text;
                setMessageMarkdown(keep, escapeHtml(text));
            }
            clearQueuedMark(keep);
        }

        function handleQueueUpdate(data) {
            applyQueueUpdate(activePane(), data, true);
        }

        function handleSessions(data) {

            // Structured list only — never dump slash help text into chat.
            // Deliberately do NOT finalize/endStream here: the sessions
            // payload is sidebar metadata that can arrive while the
            // active pane's turn is still streaming (e.g. closing a
            // background pane's ✕ calls requestSessionList mid-turn).
            // Finalizing would split the in-flight reply into a second
            // assistant bubble and stamp a fork button on a partial one.
            // Prune stale nested (subagent) records the payload no longer
            // lists (deleted elsewhere, or pruned by the per-parent cap):
            // a FINISHED child is always persisted, so an absent record
            // that is not running and not open as a pane is stale. Running
            // children may not have hit their first save yet — keep them.
            // NOTE: the record's `running` flag is deliberately NOT refreshed
            // from the payload here. It is an optimistic spawn flag, and a
            // released child's saved session STAYS in the payload, so absence
            // is the only signal this prune may act on — and a running record
            // must survive it (a child that has not saved yet is absent by
            // definition). Rendering does not depend on the flag once the
            // payload lists the child: collectNestedRows prefers the payload
            // for any listed child, which is what stops an interrupted child
            // from being pinned at "responding".
            const payloadIds = new Set((data.sessions || []).map((s) => s.id));
            for (const [id, rec] of nestedSessions) {
                if (rec.running) continue;
                if (findPaneBySession(id)) continue;
                if (!payloadIds.has(id)) nestedSessions.delete(id);
            }
            renderSessionList(data.sessions || []);
            if (data.contextLimit || data.usedTokens) { updateContextInfo(data); }

        }

        function handleSessionState(data) {

            // Attach reply: tells us whether a headless turn is
            // running so we can render "resuming…". It also carries
            // the session id: adopt it when the pane does not have
            // one yet, so the pane is usable immediately (routing,
            // send guard, sidebar "open" row) even while a running
            // turn keeps the config echo — which normally adopts the
            // id — pending until the turn ends.
            const pane = activePane();
            // session_state opens an attach rebuild (state → history →
            // config). For a pending edit-resend this is the rebuild
            // sequence reaching the client (e.g. a pane switched away and
            // back mid-fork): the config that follows may flush.
            resendRebuildPending = false;
            // A session change in flight (typed /resume or the palette's
            // "resume latest"): the resume reply IS the attach sequence, so
            // session_state is the re-key frame — adopt the target id here
            // (mirroring the config adoption) so the follow-up history and
            // configs route by id and the pane is usable immediately.
            // The outgoing session's rendered transcript and its cache/epoch
            // describe a different history: wipe them (the moment the
            // resume's clear_chat frame used to) and let the history frame
            // replay the target's. The wipe also resets the live-stream
            // state, so the in-flight rewind merge cannot splice the two
            // sessions' partial output together.
            let rekeyed = false;
            if (data.sessionId && pendingSessionResponse) {
                // Adopt the target id before clearing the DOM, so the history
                // and configs that follow route by id (the helper also drops
                // the outgoing session's cache/epoch and releases it).
                if (pane.id !== null) rekeyed = adoptPendingSession(pane, data.sessionId);
                if (rekeyed) clearChat();
                // The change settles here whether or not this pane adopted
                // the id: a session another pane already owns leaves this
                // pane on its current session, and a pane that already holds
                // the target just settles. The pending flag can never
                // strand and block every later session change.
                pendingSessionResponse = false;
                pane.ignoreTurnEndsFor = null;
            }
            if (data.sessionId && pane.id === null) {
                pane.id = data.sessionId;
            }
            pane.turnActive = !!data.turnActive;
            setTurnActive(pane.turnActive, { silent: true });
            // Seed the queue mirror from server truth. DOM-only: the queued
            // bubbles re-render with the transcript rebuild (session_state
            // precedes the history payload on attach; afterHistory re-renders
            // them from this mirror).
            applyQueueUpdate(pane, data, false);
            if (rekeyed) {
                // The re-key wiped the DOM: re-derive the target session's
                // queued steering bubbles from the mirror just seeded. The
                // history replay's afterHistory does the same; an empty
                // target session never reaches it.
                rerenderQueuedBubbles(pane);
            }
            if (pane.turnActive) {
                setInputProgress('thinking', 'Resuming\u2026');
                // Attached to a running turn: the history snapshot
                // cannot contain the in-flight reply, so latch the
                // turn-end refetch that converges the transcript.
                pane.needsFreshHistory = true;
            } else {
                // Idle attach: the history snapshot is complete — no
                // convergence refetch needed. Also clears a latch
                // left over from an earlier busy attach whose turn_end
                // was already consumed (e.g. focus after the turn
                // ended).
                pane.needsFreshHistory = false;
            }
            // Reflect the busy/resuming state in the sidebar session
            // row (same staleness class: the row shows turn state).
            refreshSidebarSessions();
            if (rekeyed) {
                // The re-key released the left-behind session (detach above):
                // fetch a fresh list so its row drops any stale "resume to
                // continue" state — the response-based switch refreshed the
                // list here too.
                requestSessionList();
            }

        }

        function handleSessionRemoved(data) {

            // The session no longer exists (deleted elsewhere, or an
            // attach failed after a restart with pruning). Close its
            // pane. For the ACTIVE pane: when THIS connection
            // initiated the delete, the server re-keys the pane via
            // the follow-up config (pendingSessionResponse is set);
            // when ANOTHER connection deleted it, no follow-up comes
            // — start a fresh session so the pane is not orphaned on
            // a deleted runtime (messages would route to a stale
            // agent and silently re-persist the deleted file).
            const pane = findPaneBySession(data.sessionId);
            // The session is gone (store file deleted): its scroll anchor
            // is meaningless.
            paneScrollAnchors.delete(data.sessionId);
            if (pane === activePane() && !pendingSessionResponse) {
                // Drop the stale pane FIRST (mirrors session_detached):
                // leaving it in the map rendered a ghost sidebar row for
                // the deleted session, and clicking it re-attached the
                // deleted id — the server replied session_removed again,
                // spawning another fresh session (and another ghost) per
                // click. Its state is discarded with the session, so no
                // saveActivePaneState is needed.
                panes.delete(pane.key);
                replaceActivePane();
            } else if (pane && pane !== activePane()) {
                panes.delete(pane.key);
                refreshSidebarSessions();
            }
            // A nested (subagent) child deleted elsewhere must stop
            // rendering under its parent too: drop its live-event record
            // and the cached payload entry (the next sessions payload no
            // longer lists it either).
            let nestedChanged = false;
            if (nestedSessions.delete(data.sessionId)) nestedChanged = true;
            // Deleting a PARENT cascades to its nested children server-side
            // (the store deletes their files recursively). Drop their
            // records + payload entries here so no ghost rows survive; the
            // server also broadcasts session_removed for each evicted child
            // runtime, which re-runs this cleanup idempotently.
            for (const [id, rec] of nestedSessions) {
                if (rec.parentId === data.sessionId) {
                    nestedSessions.delete(id);
                    // Cascade also drops the child's scroll anchor (the
                    // store deletes child sessions recursively).
                    paneScrollAnchors.delete(id);
                    nestedChanged = true;
                }
            }
            const cachedSessions = getLastSessions();
            if (cachedSessions) {
                const before = cachedSessions.length;
                const remaining = cachedSessions.filter((s) => s.id !== data.sessionId && s.parentId !== data.sessionId);
                if (remaining.length !== before) {
                    setLastSessions(remaining);
                    nestedChanged = true;
                }
            }
            if (nestedChanged) refreshSidebarSessions();
            requestSessionList();

        }

        function handleSessionDetached(data) {

            // The runtime is gone: the registry evicted this session
            // (web_max_active_sessions cap), or it was
            // explicitly closed (session_close) while this tab raced
            // to attach. It is saved, not deleted: close the pane
            // like a removal — the session stays in the saved-
            // sessions list and can be reopened (re-attach reloads
            // it from the store). Closing avoids sending messages to
            // a runtime that is no longer registered.
            const pane = findPaneBySession(data.sessionId);
            if (pane) {
                panes.delete(pane.key);
                if (activePaneKey === pane.key) {
                    if (panes.size === 0) {
                        replaceActivePane();
                    } else {
                        focusPane(mostRecentlyActivePane().key);
                    }
                } else {
                    refreshSidebarSessions();
                }
            }
            requestSessionList();

        }

        function handleResponse(data) {

            // A response frame is the terminal for a send that never
            // acked (command reply, busy/error rejection, or a turn that
            // failed before its first stream round): drop the entries it
            // terminates, or their stale flags steal the next ack (see
            // dropPendingForTerminal for the cut rule).
            dropPendingForTerminal();
            finalizeThinking();
            endStream();
            // A session command (typed /new, /resume, /fork, resend)
            // replies with the NEW session id on the response message
            // — the first message of the change, before clear_chat/
            // history/config arrive. Adopt it here so those follow-up
            // messages route to this pane (its id is otherwise still
            // the old session, and paneForMessage would drop them).
            // Sidebar-new and resend already set pendingSessionResponse
            // and go through this same path.
            const rp = activePane();
            let rekeySkipped = false;
            // The pane already showing the reply's session, when the
            // re-key is refused because ANOTHER pane owns it ("resume
            // latest" resolving to an open session). The pending branch
            // below focuses it so the command lands on the session the
            // user asked for instead of dead-ending on a toast.
            let rekeyExistingPane = null;
            if (rp && data.sessionId && rp.id !== data.sessionId) {
                // A session change (typed /new, /resume, /fork,
                // resend, delete-of-current) is the only legitimate
                // reason to adopt a different id here — every such
                // reply carries a sessionAction. A plain response
                // with a different id (e.g. a delete whose reply was
                // tagged with a stale server-side pane) must not
                // hijack this pane onto another session. Also refuse
                // to adopt an id another pane already holds (e.g.
                // "resume latest" resolving to a session open
                // elsewhere): that would create a duplicate pane for
                // one session and desync one of the two.
                const other = findPaneBySession(data.sessionId);
                // The server's focus-pane reply (a resume that resolved to
                // a session this connection already shows as a pane): never
                // re-key onto it — the pane that owns the session is the one
                // to focus, and the server deliberately did not switch.
                // The literal must match sessionCommandFocusPane in
                // session_ops.go.
                const focusOpen = data.sessionAction === 'focus_pane';
                if (!focusOpen && (rp.id === null || data.sessionAction) && (!other || other === rp)) {
                    const oldId = rp.id;
                    rp.id = data.sessionId;
                    releaseStrandedSuppress(rp);
                    refreshSidebarSessions();
                    // Release the old session's attachment only when
                    // no other pane still has it open: detaching a
                    // session a background pane still shows would
                    // orphan that pane's event stream.
                    sendSessionDetach(oldId);
                } else {
                    rekeySkipped = true;
                    if (other && other !== rp) rekeyExistingPane = other;
                }
            }
            // Whether the change succeeded or failed, the pane is (or
            // stays) on a known session: from here its own turn_end
            // must be processed normally. (On success the old
            // session's id no longer routes here; on failure it is
            // still this pane's session and its turn_end matters.)
            // needsFreshHistory is deliberately NOT reset here — the
            // session_state sent before the reply owns the latch, and
            // a resumed session may be mid-turn and needs the
            // turn-end convergence refetch.
            rp.ignoreTurnEndsFor = null;
            if (compacting) {
                // Terminal event for the manual /compact command:
                // clear the persistent indicator and surface the
                // outcome (the context message that follows will
                // refresh the badge/banner).
                compacting = false;
                setInputProgress(null);
                const text = data.content || '';
                if (String(text).startsWith('Error:')) {
                    showToast(text, 'error');
                } else {
                    showToast(text, 'success');
                }
                appendMessage('system', text);
            } else if (pendingSessionResponse) {
                pendingSessionResponse = false;
                nearCompactDismissed = false; // new session: allow the banner again
                // Only track the session id when the pane actually
                // adopted it (a skipped re-key keeps the old id).
                if (data.sessionId && rp.id === data.sessionId) sessionInfoDiv.textContent = data.sessionId;
                updateContextInfo(data);
                const msg = (data.content || '').split('\n')[0] || 'Session updated';
                if (rekeySkipped) {
                    // The reply named a session another pane already
                    // owns: this pane stays on its current session.
                    // Focus that pane so the command lands on the
                    // session the user asked for (the same switch the
                    // typed /resume interception performs); the focus
                    // re-attaches it and re-syncs the connection's
                    // current pane server-side.
                    if (rekeyExistingPane) {
                        focusSessionInOtherPane(rekeyExistingPane.id);
                    } else {
                        // No pane holds the id (a stale/id-less reply):
                        // nothing to switch to, just report it.
                        showToast('Session already open in another pane', 'info');
                    }
                    requestSessionList();
                } else if (String(data.content || '').startsWith('Error:')) {
                    clearPendingResend();
                    showToast(data.content, 'error');
                    appendMessage('system', data.content);
                } else if (!resendAwaitingHistory) {
                    // Normal session switch — toast. Resend follow-up
                    // suppresses the toast; flush happens after history.
                    showToast(msg, 'success');
                    requestSessionList();
                } else {
                    requestSessionList();
                }
            } else if (data.content && String(data.content).startsWith('Error:')) {
                showToast(data.content, 'error');
                appendMessage('system', data.content);
                // Remember the stop reason so the turn-end notification (sent
                // when turn_end arrives right after this response) reports the
                // error instead of "Agent finished responding.".
                lastTurnError = data.content;
                // A turn that never became active (no thinking/stream/tool
                // event arrived — the model's stream failed before the first
                // chunk, or the message was rejected before the turn started)
                // is invisible to the turn-end notification: setTurnActive
                // only announces on an active→idle transition, and some
                // failures (busy rejection, validation) never send a turn_end
                // at all. Fire the error notification here so an unsuccessful
                // stop is announced even then. Mid-stream errors are
                // untouched: turnActive is true, so this is skipped and the
                // turn_end transition still notifies exactly once as before.
                if (!turnActive) {
                    announceLive('Agent stopped with an error.');
                    sendNotification('GoGen — Error', lastTurnError, 'gogen-turn-error');
                    lastTurnError = null;
                }
            } else {
                appendMessage('assistant', data.content);
            }

        }

        function handleModels(data) {

            // A models frame is the terminal for a bare /models list,
            // which starts no turn and never acks: drop the entries it
            // terminates like handleResponse does (see
            // dropPendingForTerminal).
            dropPendingForTerminal();
            if (data.content) {
                finalizeThinking();
                endStream();
                appendMessage('assistant', data.content);
            }
            if (data.models) {
                updateModelSelect(data.models, data.model, data.modelProvider);
                // The settings modal's subagent picker renders from the
                // same catalog — refresh it when the modal is open (the
                // effort options come from the catalog's per-model
                // reasoningEfforts too).
                if (isSettingsOpen()) subagentPicker.render();
                // The catalog loads lazily (idle callback) and can arrive
                // AFTER the attach context message: re-seed the badge if
                // the window is still unresolved.
                if (!contextLimitResolved) {
                    const seed = catalogContextLimit(activePane() && activePane().model);
                    if (seed) {
                        lastContextData = Object.assign({}, lastContextData, { contextLimit: seed });
                        updateToolbarContext(lastContextData);
                    }
                }
            } else if (data.model) {
                // (provider, id): the bare id is not unique across
                // registered providers.
                const owner = data.modelProvider || 'default';
                updateModelInfo(data.model, availableModels.find((m) => m.id === data.model && (m.provider || 'default') === owner)?.description);
            }
            // list_models replies omit context stats — don't wipe the indicator
            if (data.contextLimit || data.usedTokens || data.usedSource) {
                updateContextInfo(data);
            }

        }

        function handleHistory(data) {

            // Full snapshot — replace the pane so reconnect / session
            // restore never stacks duplicate transcripts.
            const histPane = activePane();

            // Conditional attach, rewind-only frame: the server proved this
            // pane's transcript is already current (the knownHistoryEpoch /
            // Index sent with the attach matched the session's fingerprint)
            // and carries ONLY the in-flight round's partial output. Keep
            // the restored transcript — no clear, no replay — and merge the
            // rewind onto it: the cache path restored a clean live state
            // (settled panes only), so this is exactly the fresh-attach
            // rewind render. needsFreshHistory stays latched (session_state
            // preceded this frame), so the turn_end convergence refetch
            // still stamps the round's messages with their server indexes.
            // (A rewind-only frame without a rewind payload — an idle race
            // on the server's snapshot decision — still keeps the DOM: the
            // frame's whole contract is "your transcript is current".)
            if (data.rewindOnly) {
                histPane.histEpoch = data.historyEpoch || 0;
                const rendered = data.rewind
                    ? renderRewindAndMerge(data.rewind, {
                        streamRaw: '',
                        streamPos: 0,
                        thinkingRaw: '',
                        thinkingPos: 0,
                        thinkingFinalized: null,
                        toolCards: {},
                    })
                    : false;
                if (histPane.turnActive && !rendered) {
                    setTurnActive(true, { silent: true });
                    setInputProgress('thinking', 'Resuming\u2026');
                }
                updateTocActive();
                flushReplayEventBuffer();
                return;
            }
            // A stale snapshot — the attach's deep clone finished
            // after the turn completed, landing after the turn_end
            // convergence refetch — must not wipe the rendered
            // transcript. Skip only when the snapshot is older than
            // the DOM (its last message index ≤ the newest rendered
            // one), the server's history was not reshaped since we
            // rendered (epoch match — compaction/rollback reset
            // indexes, making the comparison meaningless), and
            // nothing is still streaming (converge at turn_end).
            if (!histPane.needsFreshHistory && !hasLiveInFlight()
                && histPane.histEpoch !== undefined
                && (data.historyEpoch || 0) === histPane.histEpoch
                && lastHistoryIndex(data.history) >= 0
                && newestDomHistIdx() >= 0
                && lastHistoryIndex(data.history) <= newestDomHistIdx()) {
                return;
            }
            // Mid-turn attach with live content already rendering and
            // no rewind in the snapshot (older server): keep the live
            // content — the turn_end convergence refetch paints the
            // full transcript.
            if (histPane.needsFreshHistory && hasLiveInFlight() && !data.rewind) {
                return;
            }
            // The rebuild a pending edit-resend waited for has begun: the
            // wipe + replay below replace the transcript the fork asked for,
            // so its config may now flush the resent message. Configs that
            // raced AHEAD of this frame (the fork's model-validation push)
            // stayed gated by resendRebuildPending.
            resendRebuildPending = false;
            // Capture the live stream state BEFORE clearing so the
            // rewind (the in-flight reply) can merge exactly with
            // whatever the client already rendered live — never
            // duplicating or dropping a character.
            const captured = captureLiveStreamState();
            let rewindRendered = false;
            // A rebuild that replaces content this pane ALREADY shows (the
            // turn-end convergence refetch — a bare re-attach after a
            // mid-turn attach — or a stale-pane re-attach of the same
            // session) must not blur: nothing is being restored, and the blur
            // flashed whenever a turn completed. Only a rebuild that
            // introduces content the user has not seen gets the restoring
            // treatment: a cold pane, or a different session's history.
            const hadTranscript = !!messagesDiv.firstElementChild && !hasEmptyState();
            const sameSession = histPane.id === data.sessionId;
            clearChat();
            if (!hadTranscript || !sameSession) beginRestoring();
            // 0 (never reshaped) is omitted by the wire's omitempty: store a
            // NUMBER so the stale-skip epoch comparison works for sessions
            // that were never compacted/rolled back either.
            histPane.histEpoch = data.historyEpoch || 0;
            // A full rebuild re-stamps every index — compaction's stale
            // indexes are resolved (see handleCompacting).
            histPane.indexesDirty = false;
            // The rebuild replaces the whole transcript: the rendered-index
            // mark restarts empty and is re-advanced by the replay below (the
            // rewind-only and stale-skip paths return before this, keeping
            // the mark they already earned).
            histPane.renderedHistIdx = -1;
            // The rebuild wipes the reading position; restore it from the
            // per-session scroll anchor when one is valid for this
            // snapshot (null settle → the replay's default pin-to-bottom).
            const scrollSettle = paneScrollRestoreSettle(histPane, data.sessionId, data.historyEpoch || 0);
            const afterHistory = () => {
                // The rebuild is settled (scrolled to the bottom or restored
                // to the reading anchor): lift the blur so the transcript
                // unblurs in place.
                endRestoring();
                // Render the in-flight partial (the server's live-turn
                // buffer) through the normal stream machinery, then
                // merge any content the client had already rendered
                // live. Runs on both the replay and the fallback path.
                if (data.rewind) {
                    rewindRendered = renderRewindAndMerge(data.rewind, captured);
                }
                // A headless turn may be running (session_state said
                // turnActive): restore the busy indicator now that the
                // transcript is rebuilt (clearChat reset it). The
                // rewind render already set the phase when it painted
                // the in-flight reply.
                const pane = activePane();
                if (pane && pane.turnActive && !rewindRendered) {
                    setTurnActive(true, { silent: true });
                    setInputProgress('thinking', 'Resuming\u2026');
                }
                // Re-render the pane's queued steering bubbles: the rebuild
                // wiped the DOM (clearChat) and queued items are not part of
                // history — they come from the pane's queue mirror.
                rerenderQueuedBubbles(activePane());
                // Refresh the TOC active dot once the transcript is rebuilt:
                // the replay path pins (updateTocActive in pinToBottom), the
                // fallback path has no scroll event to trigger it.
                updateTocActive();
                // Live events that arrived during the chunked replay are
                // flushed last — after the rewind merge, matching the old
                // ordering where the synchronous replay blocked the event
                // loop until the whole rebuild (and this afterHistory
                // microtask) had run.
                flushReplayEventBuffer();
                // NOTE: the resend message is flushed from the config
                // handler, which adopts the new session id first (the
                // server sends history before config) — once this rebuild
                // cleared resendRebuildPending.
            };
            if (data.history && data.history.length) {
                replayHistory(data.history, scrollSettle).then(afterHistory).catch((err) => {
                    console.warn('history replay failed', err);
                    // Ensure the replay scroll-suppression flag is off
                    // so the fallback re-append below scrolls normally.
                    replayInProgress = false;
                    disarmReplayWatchdog();
                    flushDeferredResultColorize();
                    for (const h of data.history) {
                        if (h.role === 'user' || h.role === 'assistant') {
                            if (h.content) {
                                appendMessageAtTime(
                                    h.role,
                                    h.content,
                                    h.createdAt ? new Date(h.createdAt) : new Date(),
                                    h.index,
                                    h.images,
                                    h.model
                                );
                            }
                        }
                    }
                    // Same settlement contract as the replay path: anchor
                    // when valid, bottom otherwise (the fallback's appends
                    // just pinned to bottom via smartScroll).
                    if (scrollSettle) scrollSettle();
                    afterHistory();
                });
            } else {
                afterHistory();
            }

        }

        function handleConfig(data) {

            // The pane's session identity may have changed (new /
            // resume / fork / attach): adopt the new id and keep the
            // per-session toolbar mirrors current.
            const pane = activePane();
            const oldId = pane.id;
            if (data.sessionId && pane.id !== data.sessionId) {
                // Re-key ONLY at startup, when the pane has no id yet. An
                // identified pane must NOT adopt a session id from a config:
                // every session change announces itself earlier and more
                // precisely — the reply's `response` (typed /new|/fork,
                // delete, edit-resend fork) or its `session_state` (resume,
                // attach) re-keys the pane, and a config that re-keyed too
                // could only be stale or mis-timed. In particular, an OLD
                // session's lagging config (its ContextStats tokenization
                // runs seconds behind) landing between the fork's early
                // validation push and the fork's response would flip the
                // pane BACK to the outgoing session and detach the fork —
                // stranding the resent turn's broadcast frames and sticking
                // turnActive forever. A stale full config from a pane we
                // left/closed is dropped here for the same reason —
                // paneForMessage would otherwise drop every message for the
                // session the user switched to ("can't switch to the
                // responding session").
                if (pane.id === null) {
                    pane.id = data.sessionId;
                    pane.needsFreshHistory = false; // new session: no mid-turn gap from the old one
                    // The pane now shows a DIFFERENT session: any transcript
                    // cache / epoch describes the old session's history and
                    // must not survive the re-key.
                    pane.domCache = null;
                    pane.histEpoch = undefined;
                    pane.renderedHistIdx = -1;
                    refreshSidebarSessions();
                    // The pane's session changed (typed /new, /resume,
                    // fork, resend): release the old session's
                    // attachment — the server keeps panes attached
                    // until explicitly detached, and the old session
                    // is no longer open. Only when no other pane
                    // still has the old session open (detaching it
                    // would orphan that pane's event stream).
                    sendSessionDetach(oldId);
                }
            }
            applyPaneMeta(pane, data);
            applyServerConfig(data);
            // The resend message must carry the (possibly new) session
            // id — by the time a config arrives, the reply's response /
            // session_state has already adopted it (config never re-keys,
            // see above). Gated on the rebuild having begun: a config that
            // arrives BEFORE the reply's clear_chat/history (the fork's
            // model-validation push races ahead of the reply) must not
            // flush — the optimistic bubble it appends would be wiped by
            // that clear_chat, and the replayed prefix does not contain
            // the resent message.
            if (resendAwaitingHistory && !resendRebuildPending) flushPendingResend();

        }

        function handleContext(data) {

            updateContextInfo(data);
            updateCurrentSessionLabel(data.sessionLabel);

        }

        function handleDeleteApproval(data) {
            showDeleteApproval(data);
        }

        function handleCommandApproval(data) {
            showCommandApproval(data);
        }

        // ── Nested (subagent) sessions ──
        // Children render as indented rows under their parent (D2). They
        // are attachable: clicking a nested row opens the child as a normal
        // pane (live transcript + Cancel), which is the escape hatch for a
        // subagent stuck in a loop.
        const nestedSessions = new Map(); // id → {id, parentId, label, job, running, success, summary}

        function handleSubagentEvent(data) {
            if (data.type === 'subagent_started') {
                nestedSessions.set(data.subagentId, {
                    id: data.subagentId,
                    parentId: data.subagentParent,
                    label: data.subagentLabel || 'subagent',
                    job: data.subagentJob || '',
                    running: true,
                    success: false,
                    summary: '',
                });
            } else if (data.type === 'subagent_finished') {
                const rec = nestedSessions.get(data.subagentId);
                if (rec) {
                    rec.running = false;
                    rec.success = !!data.subagentSuccess;
                    rec.summary = data.subagentSummary || '';
                }
                // Failure display: a transient toast, NEVER a chat line. The
                // completion notice already reaches the parent as a
                // persisted user message (the delivery service) and the
                // nested sidebar row shows the failed state, so a rendered
                // system bubble would duplicate the same failure in the
                // transcript (and it was ephemeral anyway — any history
                // rebuild removed it). The event is sidebar-global, so the
                // toast is shown regardless of which pane is focused.
                if (!data.subagentSuccess) {
                    const label = (rec && rec.label) || data.subagentLabel || 'subagent';
                    showToast(
                        `\u26a0 subagent ${label} failed${data.subagentSummary ? ': ' + data.subagentSummary : ''}`,
                        'error');
                }
            }
            renderSessionList(getLastSessions() || []);
        }

        // Collect the nested (subagent) child RECORDS for parentId in
        // render order — pure, no DOM: sessions.js owns placement so the
        // keyed diff can keep a child row under its parent when the parent
        // reorders (appending to the container end, as the old
        // appendNestedRows did, is only correct on a wiped list).
        // Sources: the live subagent_started/finished event records
        // (running/summary state, children not yet saved) plus — after a
        // page reload or a late attach, when the events are gone — the
        // persisted children in the sessions payload (the server now
        // includes them). Children open as panes in this tab render here
        // TOO (buildNestedSessionRow overlays their live pane state), so
        // opening a subagent keeps its row under the parent —
        // renderSessionList skips those children when it builds the flat
        // open-pane rows. Nesting is recursive (depth >= 2):
        // grandchildren render under their child rows, mirroring the
        // server's recursive cascade.
        function collectNestedRows(parentId, depth) {
            if (depth > 16) return []; // cycle guard (subagentMaxDepth max is 10)
            const out = [];
            const rendered = new Set();
            // Live-event records (subagent_started) are OPTIMISTIC: running is
            // true from the moment the child is announced, and the ONLY event
            // that clears it is subagent_finished — which an interrupted child
            // never sends (interrupt leaves it continuable). So once the
            // sessions payload lists the child, the payload is server truth
            // (turnActive + the recorded outcome) and the record must not
            // shadow it, or an interrupted child stays pinned at "responding"
            // until the page is reloaded. The record still supplies the job
            // text, and is the whole row while the payload does not list the
            // child yet (a spawn the in-flight list predates). Absent is NOT
            // evidence: the list request may be older than the spawn.
            const listed = new Set((getLastSessions() || []).map((s) => s.id));
            for (const rec of nestedSessions.values()) {
                if (rec.parentId !== parentId) continue;
                if (listed.has(rec.id)) continue;
                out.push(rec);
                rendered.add(rec.id);
            }
            for (const s of getLastSessions() || []) {
                if (!s.parentId || s.parentId !== parentId) continue;
                if (rendered.has(s.id)) continue;
                const liveRec = nestedSessions.get(s.id);
                // The payload carries the runtime's LIVE turn state
                // (turnActive) and the child's PERSISTED outcome
                // (subagentStatus/subagentSummary), so a registered-but-
                // idle child (open as a pane, resumed after a restart) is
                // never mistaken for running or failed, and a child that
                // really failed stays failed.
                //
                // A recorded "running" status (or a legacy "") with no
                // evidence of current activity never reached a terminal state:
                // the child was interrupted, cancelled with its host, or its
                // process went away. That must NOT read as done — unfinished
                // work used to render green "done" after a restart.
                //
                // The evidence is the payload's `active` flag, which means
                // "open in a tab, or a turn is running" (sessionEntries/
                // activeSet) — NOT "the runtime is registered". Folding in
                // turnActive keeps a mid-turn child on "responding" and leaves
                // an idle headless one reading as interrupted, which is the
                // state it is actually in: continuable, not working.
                const status = s.subagentStatus || '';
                const attended = !!s.active || !!s.turnActive;
                const interrupted = !attended && (status === 'running' || status === '');
                out.push({
                    id: s.id,
                    parentId: s.parentId,
                    // The spawn event carries the label the parent wrote; the
                    // store's label can lag (or be empty before the first
                    // save), so prefer the live one when there is one.
                    label: (liveRec && liveRec.label) || s.label || s.id,
                    job: (liveRec && liveRec.job) || '',
                    running: !!s.turnActive,
                    success: status !== 'failed' && !interrupted,
                    interrupted,
                    active: !!s.active,
                    summary: s.subagentSummary || (s.turnActive ? '' : (s.messageCount ? s.messageCount + ' msgs' : '')),
                });
                rendered.add(s.id);
            }
            // Recursion: grandchildren render under their child rows too.
            for (const id of rendered) out.push(...collectNestedRows(id, (depth || 0) + 1));
            return out;
        }

        function buildNestedSessionRow(rec) {
            // Open child panes render their LIVE state here (running flag,
            // current marker, label, close button) instead of moving to the
            // flat open-panes section: the row stays under its parent.
            const pane = findPaneBySession(rec.id);
            const running = pane ? !!pane.turnActive : !!rec.running;
            const label = pane && pane.label ? pane.label : rec.label;
            const isCurrent = !!pane && pane === activePane();
            const row = document.createElement('div');
            row.className = 'session-row nested' + (running ? ' busy' : '') + (isCurrent ? ' current' : '');
            row.dataset.sessionId = rec.id;
            const content = document.createElement('div');
            content.className = 'session-row-content';
            const title = document.createElement('div');
            title.className = 'session-row-title';
            title.textContent = label;
            const meta = document.createElement('div');
            meta.className = 'session-row-meta';
            // A colored dot means the child is LIVE somewhere — responding,
            // open as a pane in this tab, or live-registered elsewhere —
            // and its color carries the state: amber responding, red
            // failed, green done. A settled child that is neither open nor
            // live renders a muted TEXT status instead, so archived
            // outcomes do not read as active sessions. Summary text still
            // follows when one exists.
            const frags = [];
            const live = !!pane || !!rec.active;
            let stateLabel = '';
            let stateClass = '';
            let stateDot = false;
            if (running) {
                stateLabel = 'responding';
                stateClass = 'amber';
                stateDot = true;
            } else if (rec.interrupted) {
                // Settled without a terminal outcome (see collectNestedRows):
                // muted text, not a colored dot — the child is neither live
                // nor done. Checked BEFORE the failed fallback, because an
                // interrupted record is neither a success nor a failure.
                stateLabel = 'interrupted';
            } else if (!rec.success) {
                stateLabel = 'failed';
                stateClass = 'red';
                stateDot = live;
            } else {
                stateLabel = 'done';
                stateClass = 'green';
                stateDot = live;
            }
            if (stateDot) {
                const group = document.createElement('span');
                group.className = 'session-state';
                const dot = document.createElement('span');
                dot.className = 'session-state-dot ' + stateClass;
                dot.title = stateLabel;
                dot.setAttribute('aria-label', stateLabel);
                const stateText = document.createElement('span');
                stateText.className = 'session-state-label';
                stateText.textContent = stateLabel;
                group.appendChild(dot);
                group.appendChild(stateText);
                frags.push(group);
            } else if (stateLabel) {
                const text = document.createElement('span');
                text.className = 'session-state-label';
                text.textContent = stateLabel;
                frags.push(text);
            }
            if (!running && rec.summary) frags.push(rec.summary.slice(0, 80));
            for (let i = 0; i < frags.length; i++) {
                if (i > 0) meta.appendChild(document.createTextNode(' · '));
                if (typeof frags[i] === 'string') {
                    meta.appendChild(document.createTextNode(frags[i]));
                } else {
                    meta.appendChild(frags[i]);
                }
            }
            content.appendChild(title);
            content.appendChild(meta);
            content.title = rec.job || label;
            // Clicking focuses the open pane; otherwise it opens the child
            // as a pane (the escape hatch for a stuck subagent).
            content.onclick = () => { if (pane) focusPane(pane.key); else openSessionPane(rec.id); };
            row.appendChild(content);
            // Same ✕ mechanics as the flat rows: an OPEN child's ✕ closes
            // the pane (the session stays saved and returns to a nested
            // row); a CLOSED child's ✕ deletes the session behind the
            // standard confirm modal. Both report back to the parent
            // agent server-side.
            const closeBtn = document.createElement('button');
            closeBtn.className = 'session-row-del';
            closeBtn.innerHTML = icon('x');
            if (pane) {
                closeBtn.title = 'Close session (stays saved)';
                closeBtn.onclick = (e) => {
                    e.stopPropagation();
                    closePane(pane.key);
                };
            } else {
                closeBtn.title = 'Delete this session';
                closeBtn.onclick = (e) => {
                    e.stopPropagation();
                    deleteSession(rec.id, rec.label);
                };
            }
            row.appendChild(closeBtn);
            return row;
        }

        // Parent id for a session that is a nested (subagent) child, or ''
        // for a normal session. Mirrors collectNestedRows' sources: the live
        // subagent_started/finished records first, then the persisted
        // parentId in the sessions payload.
        function nestedParentIdOf(id, list) {
            const rec = nestedSessions.get(id);
            if (rec && rec.parentId) return rec.parentId;
            const entry = (list || []).find((s) => s.id === id);
            return (entry && entry.parentId) || '';
        }

        // Reports whether id's row will render in this pass: as a flat row
        // (id in rowIds) or as a nested row under a parent whose own chain
        // reaches a flat row (collectNestedRows recurses through the whole
        // chain). seen guards against corrupt ParentID cycles.
        function nestedRowWillRender(id, list, rowIds, seen) {
            if (rowIds.has(id)) return true;
            if (seen.has(id)) return false;
            seen.add(id);
            const parentId = nestedParentIdOf(id, list);
            if (!parentId) return false;
            return nestedRowWillRender(parentId, list, rowIds, seen);
        }

        // ── Kanban board tab ──
        // Rendering, board_op messaging and the per-ticket "Start agent"
        // popover live in components/board.js (initBoard below).

        // ── Notice channel (non-chat UI feedback) ──
        // "notice" messages toast and NEVER touch the chat transcript or
        // stream state — no finalizeThinking/endStream/appendMessage here,
        // so a notice arriving mid-stream can never split the in-flight
        // assistant bubble. Kind scopes follow-ups.
        function handleNotice(data) {
            if (data.success) {
                if (data.content) showToast(data.content, 'success');
                return;
            }
            showToast(data.content || 'Operation failed', 'error');
            // Failed board/automation ops resync their tab — but only while
            // the tab is visible: a rejected op when the feature is
            // disabled must not re-trigger another op (toast/resync loop).
            if (data.kind === 'board' && boardTabVisible()) requestBoardState();
            if (data.kind === 'automations' && automationsPaneVisible()) requestAutomationsState();
            // A refused mcp_save must drop its pending "Saving…" line: the
            // error itself is the toast above.
            if (data.kind === 'mcp') noteMCPOpFailed();
        }

        function sendMessage() {
            const text = inputArea.value.trim();
            if (!text && getPendingAttachments().length === 0) return;
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                // Toast, not a transcript message: the reconnect already
                // rebuilds the transcript, so a system bubble here would be
                // transient junk that survives into exports.
                showToast('Not connected — wait for reconnection.', 'error');
                return;
            }
            if (!activePane() || !activePane().id) {
                showToast('Session not ready yet — try again in a moment', 'info');
                return;
            }
            if (resendAwaitingHistory) {
                showToast('Resend already in progress', 'info');
                return;
            }
            // A typed /resume of a session already open in another pane
            // would create a duplicate pane for one session (the re-key
            // reply and the follow-up detach would desync one of the two
            // panes). Switch to the existing pane instead (the same
            // focusSessionInOtherPane the reply handler runs when "resume
            // latest" resolves to an open session). "resume del" targets a
            // session for deletion and "resume latest" resolves
            // server-side, so both pass through.
            if (/^\/resume(?:\s|$)/.test(text.trim())) {
                const target = text.trim().replace(/^\/resume\s+/, '').trim();
                if (target && target !== 'del' && !target.startsWith('del ')) {
                    const other = findPaneBySession(target);
                    if (other && other !== activePane()) {
                        // Clear before focusing: the helper hands focus
                        // back to the composer, and the command text must
                        // not survive into the switched pane's input.
                        inputArea.value = '';
                        focusSessionInOtherPane(target);
                        return;
                    }
                }
            }
            // Sending while busy QUEUES the message (steering): it is
            // delivered as the next user turn when the in-flight one ends,
            // FIFO with the server's delivery queue. Interrupt is explicit
            // (Cancel / Esc / cancel frame) — never implicit in a send.
            // A turn started from the focused pane (now, or when the queue
            // drains) is fully live-rendered — no turn_end convergence
            // refetch needed.
            if (activePane()) activePane().needsFreshHistory = false;
            hideSlashSuggest();
            enableFollow();
            // Typed session commands (/new, /resume, /fork) re-key the pane:
            // mark the change in flight so the response handler treats the
            // reply as a session change (toast + list refresh) instead of an
            // assistant message, matching the sidebar flow.
            if (/^\/(new|resume|fork)(\s|$)/.test(text.trim())) {
                pendingSessionResponse = true;
                // These commands re-key this pane away from its current
                // session; the old session's turn (if running) keeps going
                // headless, so ignore its turn_end — it must not clear the
                // pending flag mid-change (mirrors resumeSession).
                const p = activePane();
                if (p && p.id) p.ignoreTurnEndsFor = p.id;
            }
            // Images ride the payload's images array; text-file attachments
            // (dropped on the composer) are inlined into the content by the
            // composer — the wire format carries no other attachment kind.
            // The bubble shows the composed content, so what the user sees
            // (and later edits/resends via rawContent) matches what the
            // model received. Queue labels keep the short typed text.
            const images = getPendingAttachments()
                .filter((a) => a.dataUrl)
                .map((a) => ({ dataUrl: a.dataUrl }));
            const content = composeMessageContent(text);
            const el = appendMessage('user', content, undefined, undefined, images);
            markPendingAck(el);
            // Reset the per-turn card state ONLY when this send starts a
            // new turn. While a turn is running the message is QUEUED
            // (steering) and the in-flight turn keeps going: wiping the
            // maps here (the old cancel-then-send reset) orphaned every
            // tool call that was already announced — the next tool_result
            // found no pending card and fell through to the raw
            // "[tool] <result>" system blob, the live terminal mirror lost
            // its card, and a still-streaming call's arg deltas opened a
            // SECOND card beside the orphaned one. hasLiveInFlight() is
            // checked as well as the turnActive mirror: its live-render
            // state is what makes the clobber dangerous, and the mirror can
            // lag the server's busy state (the same race the queueId
            // adoption below covers).
            if (!activePane().turnActive && !hasLiveInFlight()) {
                streamingToolCards = {};
                pendingToolCards = {};
                bgTermCards = {};
                toolsStartedThisTurn = false;
                contextEstAdded = 0;
            }
            const payload = { type: 'message', content, sessionId: activePane().id };
            if (images.length > 0) payload.images = images;
            // Correlation id: when the pane's turn is active the message is
            // queued server-side; this id ties the optimistic bubble to its
            // queue_update entry and lets other tabs render the same item.
            // The bubble is TAGGED with it unconditionally: if the send races
            // the server's busy state (our turnActive mirror still says idle
            // while the server queues the message), the queue_update adopts
            // this very bubble by data-queue-id instead of appending a
            // duplicate. markQueuedBubble below adds the chip/✕ on top.
            const qid = nextQueueId();
            payload.queueId = qid;
            el.dataset.queueId = qid;
            ws.send(JSON.stringify(payload));
            // Optimistic queued mark when the pane is busy: the bubble shows
            // a queued chip immediately; the server's queue_update (or
            // user_acked, when the send raced an idle turn) reconciles it.
            if (activePane().turnActive) {
                markQueuedBubble(el, qid);
                queuedBubblesFor(activePane()).set(qid, el);
                activePane().queueItems = (activePane().queueItems || []).concat([{ id: qid, text }]);
                // The bubble was appended before it was known to be queued
                // (the anchor in appendTranscriptChild runs at append time):
                // re-anchor now so an older queued bubble cannot end up
                // behind this one.
                anchorQueuedBubbles();
                refreshQueuedSendNow(activePane());
                updateQueuedProgressLabel();
            }
            inputArea.value = '';
            clearAttachments();
            // Mouse-click sends leave focus on the Send button; hand it
            // back to the composer. Guarded so Enter-sends (already
            // focused) and starter-prompt clicks (focus elsewhere) are
            // untouched — no keyboard yank on mobile.
            if (document.activeElement === sendBtn) inputArea.focus();
        }
        sendBtn.onclick = sendMessage;

        // Route through the same guarded path as send-while-busy / resend /
        // delete-of-active: the trailing cancelled + turn_end frames for the
        // cancelled turn are consumed by suppressTurnEnds instead of being
        // processed against whatever UI state follows (the cancel race).
        cancelBtn.onclick = cancelActiveTurn;

        // Single per-keystroke composer listener: slash suggestions plus the
        // command-mode indicator (the input-area wrapper is styled while the
        // first token starts with '/'). The palette's command fill dispatches
        // a synthetic 'input' event and runs through here too; other
        // programmatic value writes (applySlashCompletion, the clear after
        // send) skip the event, so the indicator catches up on the next real
        // keystroke.
        inputArea.addEventListener('input', () => {
            updateSlashSuggest();
            inputAreaWrap.classList.toggle('command-mode', inputArea.value.startsWith('/') && inputArea.value.length > 0);
        });

        /* Textarea auto-resize is handled natively by CSS
           `field-sizing: content` (see styles.css) — no JS size tracking.
           The browser grows/shrinks the box with its content and clamps it
           via min-height/max-height. Firefox 152+ / Chrome 123+ / Safari
           26.2+; older engines keep the box at min-height (static). */

        inputArea.addEventListener('blur', () => {
            // Delay so mousedown on a suggestion can fire first.
            setTimeout(hideSlashSuggest, 150);
        });

        inputArea.onkeydown = (e) => {
            // The slash-suggest box handles its own keys (components/
            // composer.js); true = consumed, false = fall through to
            // send / turn-cancel below.
            if (slashKeydown(e)) return;
            if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault();
                sendMessage();
            }
            if (e.key === 'Escape' && turnActive) {
                e.preventDefault();
                cancelBtn.onclick();
            }
        };

        setDirBtn.onclick = () => {
            if (!isGlobalMode) {
                showToast('Working directory can only be changed in global mode', 'error');
                return;
            }
            const dir = dirInput.value;
            if (!dir) return;
            if (!ensureConnected()) return;
            // The working dir is workspace-global; the sessionId scopes the
            // cancel-then-lock to the requesting pane.
            ws.send(JSON.stringify({ type: 'config', workingDir: dir, sessionId: activePane().id }));
            showToast('Working directory updated', 'success');
            setTimeout(() => requestSessionList(), 100);
        };

        function newSession() {
            closeMobileSidebar();
            if (!ensureConnected()) return;
            if (resendAwaitingHistory) {
                showToast('Resend already in progress', 'info');
                return;
            }
            if (pendingSessionResponse) {
                // Two session changes on one connection would interleave:
                // both replies land in this pane and their clear_chat/
                // history/config follow-ups can repaint the wrong
                // transcript. Match deleteSession/resumeSession and wait.
                showToast('Session change already in progress', 'info');
                return;
            }
            // The sidebar "New" button opens a NEW pane; the previous
            // pane stays open in the background. The new pane's session id
            // arrives in the config reply (re-key via the config handler).
            const pane = makePane();
            // Same outgoing-pane transcript handling as focusPane: cache the
            // previous pane (if settled) instead of wiping it.
            transitionToPane(pane);
            ensureEmptyState();
            pendingSessionResponse = true;
            ws.send(JSON.stringify({ type: 'session_new' }));
            // Hand control back to the composer: the click moved focus to the
            // button (or the palette closed), so the user would otherwise
            // have to re-click the input before typing their first message.
            inputArea.focus();
        }

        function switchMainPane(pane) {
            // Switching to a regular pane on mobile dismisses the
            // full-screen terminal overlay so it can't cover the chat.
            if (pane !== 'terminal') terminalDismissMobile();
            document.querySelectorAll('.main-tab').forEach((t) => {
                const on = t.dataset.pane === pane;
                t.classList.toggle('active', on);
                // Expose the active view to assistive tech (the tabs are
                // plain buttons; the visual state is the .active class).
                if (on) t.setAttribute('aria-current', 'true');
                else t.removeAttribute('aria-current');
            });
            document.querySelectorAll('.pane').forEach((p) => {
                p.classList.toggle('active', p.id === `${pane}-pane`);
            });
            if (pane === 'editor') {
                initMonaco().then(() => refreshExplorer()).catch(() => {});
            }
            if (pane === 'board') {
                // Paint the stored lastBoardState now — board re-renders are
                // gated on pane visibility (handleBoardState skips hidden
                // panes) — and request a fresh snapshot; the reply renders
                // through the same now-visible pane.
                renderBoard();
                requestBoardState();
            }
            if (pane === 'automations') {
                // Same visibility-gated render contract as the board:
                // paint the stored snapshot, then refresh it.
                renderAutomations();
                requestAutomationsState();
            }
        }

        const PALETTE_COMMANDS = [
            { id: 'new-session', label: 'New session', hint: '', run: () => newSession() },
            { id: 'resume-latest', label: 'Resume latest session', hint: '', run: () => resumeSession('latest') },
            { id: 'refresh-sessions', label: 'Refresh sessions', hint: '', run: () => requestSessionList() },
            { id: 'toggle-mode', label: 'Toggle Act / Plan mode', hint: '', run: () => {
                ws?.send(JSON.stringify({ type: 'set_mode', mode: currentMode === 'plan' ? 'act' : 'plan', sessionId: activePane().id }));
            }},
            { id: 'pane-chat', label: 'Switch to Chat', hint: '', run: () => switchMainPane('chat') },
            { id: 'pane-editor', label: 'Switch to Editor', hint: '', run: () => switchMainPane('editor') },
            { id: 'keybindings', label: 'Keyboard shortcuts', hint: '', run: () => {
                const overlay = document.getElementById('keybindings-overlay');
                if (overlay) openModal(overlay);
            }},
            { id: 'pane-board', label: 'Switch to Board', hint: '', run: () => {
                // Board tab is hidden until the feature is enabled; never
                // switch to a pane that renders nothing.
                if (!boardTabVisible()) {
                    showToast('Board is disabled in settings', 'info');
                    return;
                }
                switchMainPane('board');
            }},
            { id: 'toggle-terminal', label: 'Toggle terminal', hint: 'Ctrl+`', run: () => {
                // Same branch as the Ctrl+` handler: mobile gets the
                // full-screen terminal, desktop toggles the strip.
                terminalTogglePanel();
            }},
            { id: 'find-files', label: 'Find in files', hint: 'Ctrl+Shift+F', run: () => focusFindInFiles() },
            { id: 'export', label: 'Export chat', hint: 'Ctrl+Shift+E', run: () => exportChat() },
            { id: 'undo', label: 'Undo', hint: 'Ctrl+Z', run: () => editorUndo() },
            { id: 'redo', label: 'Redo', hint: 'Ctrl+Shift+Z', run: () => editorRedo() },
            { id: 'refresh-explorer', label: 'Refresh file explorer', hint: '', run: () => refreshExplorer().catch((e) => showToast(e.message, 'error')) },
            { id: 'save', label: 'Save file', hint: 'Ctrl+S', run: () => saveActive() },
            { id: 'save-all', label: 'Save all', hint: '', run: () => saveAll() },
            { id: 'compact', label: 'Compact context', hint: '', run: () => {
                startCompact();
            }},
            { id: 'context-detail', label: 'Show context usage', hint: '', run: () => {
                if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'message', content: '/context', sessionId: activePane().id }));
            }},
            { id: 'list-models', label: 'List / switch models', hint: '', run: () => {
                if (ws?.readyState === WebSocket.OPEN) {
                    modelsRequested = false; // palette is an explicit request; allow a refetch
                    ensureModelsLoaded();
                }
            }},
            { id: 'open-settings', label: 'Open settings', hint: '', run: () => openSettings() },
            { id: 'toggle-notifications', label: 'Toggle notifications', hint: '', run: () => {
                const cur = getNotificationPref();
                const next = cur === 'off' ? 'background' : 'off';
                setNotificationPref(next);
                showToast(`Notifications: ${next === 'off' ? 'off' : 'on (' + next + ')'}`, 'info');
            }},
        ];
        // Slash commands are also reachable from the palette. Picking one
        // PREFILLS the composer — never auto-sends: /new, /resume and /fork
        // re-key the pane, and that must stay a deliberate Enter press.
        // This keeps the palette free of any session-state coupling.
        for (const c of getSlashCommands()) {
            PALETTE_COMMANDS.push({
                id: 'slash-' + c.name.slice(1),
                label: c.name,
                hint: c.description,
                run: () => {
                    closePalette();
                    inputArea.value = c.name + ' ';
                    inputArea.focus();
                    // Re-run the composer's input handling so command-mode
                    // styling and slash suggestions react to the fill.
                    inputArea.dispatchEvent(new Event('input', { bubbles: true }));
                },
            });
        }

        document.getElementById('session-new-btn')?.addEventListener('click', () => newSession());
        // Auto-refreshed on session mutations — no manual refresh button needed
        document.getElementById('export-chat-btn')?.addEventListener('click', () => exportChat());

        document.addEventListener('keydown', (e) => {
            const mod = e.ctrlKey || e.metaKey;
            const tag = (e.target && e.target.tagName) || '';
            const typing = tag === 'INPUT' || tag === 'TEXTAREA' || (e.target && e.target.isContentEditable);

            if (mod && e.key.toLowerCase() === 'k') {
                e.preventDefault();
                if (isPaletteOpen()) closePalette();
                else openPalette();
                return;
            }
            if (mod && e.shiftKey && e.key.toLowerCase() === 'f') {
                e.preventDefault();
                focusFindInFiles();
                return;
            }
            if (mod && e.shiftKey && e.key.toLowerCase() === 'e') {
                e.preventDefault();
                exportChat();
                return;
            }
            if (mod && e.key === '`') {
                // Toggle the terminal panel (Ctrl+` like VS Code). On mobile
                // this opens the full-screen terminal; on desktop it switches
                // between the strip and the expanded panel.
                e.preventDefault();
                terminalTogglePanel();
                return;
            }
            if (e.key === 'Escape') {
                hideTocTooltip();
                if (terminalHideMoreMenu()) {
                    e.preventDefault();
                    return;
                }
                if (isSettingsOpen()) {
                    e.preventDefault();
                    closeSettings();
                    return;
                }
                const kbOverlay = document.getElementById('keybindings-overlay');
                if (kbOverlay && kbOverlay.classList.contains('active')) {
                    e.preventDefault();
                    closeModal(kbOverlay);
                    return;
                }
                if (isPaletteOpen()) {
                    e.preventDefault();
                    closePalette();
                    return;
                }
                // Decision dialogs handle Esc themselves when focus is inside;
                // this guard covers the backdrop/focus-outside case so Esc
                // can't leak through to the turn-cancel below.
                const decisionModal = document.querySelector(
                    '#delete-approval-overlay.active, #session-delete-overlay.active, ' +
                    '#close-tab-overlay.active, #replace-preview-overlay.active'
                );
                if (decisionModal) {
                    e.preventDefault();
                    return;
                }
                if (!typing && turnActive) {
                    e.preventDefault();
                    cancelBtn.onclick();
                }
            }
        });

        // === Composer block height (--composer-h) ===
        // The scroll-to-bottom button floats above the composer block
        // (toolbar + near-compact banner + input row). Measure the block's
        // live height so the button never overlaps it — the toolbar wraps on
        // narrow screens and the banner appears/disappears, so a constant
        // drifts. Pane switches hide the block entirely (offsetHeight 0).
        function composerRefreshVar() {
            let h = 0;
            for (const id of ['composer-toolbar', 'near-compact-banner', 'input-area']) {
                const el = document.getElementById(id);
                if (el) h += el.offsetHeight;
            }
            document.documentElement.style.setProperty('--composer-h', h + 'px');
        }
        if (typeof ResizeObserver !== 'undefined') {
            const composerObs = new ResizeObserver(() => composerRefreshVar());
            for (const id of ['composer-toolbar', 'near-compact-banner', 'input-area']) {
                const el = document.getElementById(id);
                if (el) composerObs.observe(el);
            }
        }
        composerRefreshVar();

        // === Visual viewport height (--vvh) ===
        // The mobile full-screen terminal overlay is sized to the visual
        // viewport so the OS keyboard never covers its bottom rows. Without
        // visualViewport support the CSS falls back to the layout viewport.
        function visualViewportRefresh() {
            const vv = window.visualViewport;
            if (!vv) return;
            document.documentElement.style.setProperty('--vvh', vv.height + 'px');
        }
        if (window.visualViewport) {
            visualViewportRefresh();
            window.visualViewport.addEventListener('resize', visualViewportRefresh);
            window.visualViewport.addEventListener('scroll', visualViewportRefresh);
        }

        // === Conversation TOC rail ===
        initToc({
            getMessages: () => messagesDiv,
            getDistanceFromBottom: () => distanceFromBottom(),
            isPinned: () => isPinned(),
            getNearBottomPx: () => nearBottomPx(),
            isReplaying: () => replayInProgress,
            unpin: () => unpinFromBottom(),
        });

        // === Collapsible sidebar toggle ===
        const sidebarToggle = document.getElementById('sidebar-toggle');
        const sidebar = document.getElementById('sidebar');
        sidebarToggle.addEventListener('click', () => {
            sidebar.classList.toggle('open');
        });
        // Close sidebar when clicking outside on mobile
        document.addEventListener('click', (e) => {
            if (window.innerWidth <= 768 && sidebar.classList.contains('open')
                && !sidebar.contains(e.target) && e.target !== sidebarToggle) {
                sidebar.classList.remove('open');
            }
        });

        // === Resizable sidebar via drag handle ===
        function initSidebarResize() {
            const handles = document.querySelectorAll('.sidebar-resize-handle');

            handles.forEach(handle => {
                const targetId = handle.dataset.target;
                const sidebar = document.getElementById(targetId);
                if (!sidebar) return;

                // Restore saved width
                const saved = storageGet(targetId + '-width');
                if (saved) {
                    sidebar.style.width = saved;
                    sidebar.style.flexGrow = '0';
                    sidebar.style.flexShrink = '0';
                }

                handle.addEventListener('mousedown', (e) => {
                    e.preventDefault();
                    const startX = e.clientX;
                    const startWidth = sidebar.getBoundingClientRect().width;

                    handle.classList.add('active');
                    sidebarDragStart();
                    // The flex lock is constant for the whole drag: write it
                    // once here instead of on every mousemove (each redundant
                    // style write dirties style state for no benefit).
                    sidebar.style.flexGrow = '0';
                    sidebar.style.flexShrink = '0';

                    // rAF-gate the width write: mousemove can fire far more
                    // often than frames are painted, and every unbatched
                    // style.write forces a full flex relayout of the page
                    // (sidebar + chat pane re-wrap) plus the ResizeObserver
                    // cascade below it. Coalesce to at most one width apply
                    // per animation frame, using only the latest pointer X.
                    let rafId = 0;
                    let latestClientX = e.clientX;

                    function clampWidth(clientX) {
                        const delta = clientX - startX;
                        let newWidth = startWidth + delta;
                        // Clamp percentage-of-viewport so it scales on 4K/8K screens
                        const minPct = 0.12;  // at least 12% of viewport
                        const maxPct = 0.50;  // at most 50% of viewport
                        const minW = Math.max(120, window.innerWidth * minPct);
                        const maxW = window.innerWidth * maxPct;
                        return Math.max(minW, Math.min(maxW, newWidth));
                    }

                    function applyWidth() {
                        rafId = 0;
                        sidebar.style.width = clampWidth(latestClientX) + 'px';
                    }

                    function onMouseMove(ev) {
                        latestClientX = ev.clientX;
                        if (!rafId) rafId = requestAnimationFrame(applyWidth);
                    }

                    function onMouseUp() {
                        handle.classList.remove('active');
                        document.removeEventListener('mousemove', onMouseMove);
                        document.removeEventListener('mouseup', onMouseUp);
                        // Flush a pending frame synchronously so the DOM ends
                        // up exactly where the user stopped before persisting.
                        if (rafId) {
                            cancelAnimationFrame(rafId);
                            rafId = 0;
                            applyWidth();
                        }
                        // Persist the width
                        storageSet(targetId + '-width', sidebar.style.width);
                        // One final TOC/rail sync against the settled layout.
                        sidebarDragEnd();
                    }

                    document.addEventListener('mousemove', onMouseMove);
                    document.addEventListener('mouseup', onMouseUp);
                });

                // Touch support: forward touch events to mouse handlers
                handle.addEventListener('touchstart', (e) => {
                    const touch = e.touches[0];
                    const mouseEvent = new MouseEvent('mousedown', {
                        clientX: touch.clientX,
                        clientY: touch.clientY,
                    });
                    handle.dispatchEvent(mouseEvent);
                }, { passive: true });
            });

            // Safety net: if mouseup is missed (button released outside the
            // window, alt-tab mid-drag), the flag must not freeze TOC updates.
            window.addEventListener('blur', sidebarDragEnd);
        }

        initSidebarResize();

        // === Favicon (handled via link tag) ===

        // === Page title updates ===
        const baseTitle = 'GoGen AI Agent';
        function updateTitle(status) {
            let title;
            if (status === 'thinking' || status === 'streaming') {
                title = `⏳ ${baseTitle}`;
            } else if (status === 'disconnected') {
                title = `🔴 ${baseTitle}`;
            } else {
                title = baseTitle;
            }
            // Avoid rewriting the title bar on every call.
            if (document.title !== title) document.title = title;
        }
        // The title is driven by the WS state transitions (thinking /
        // stream / turn_end / cancel) rather than a MutationObserver on
        // the transcript: observing the whole subtree generated mutation
        // records for every token append, innerHTML swap, and class
        // toggle during streaming, and the callback only ever derived
        // state from the same variables the handlers already update.

        // === Context tooltip ===
        const contextTooltip = document.getElementById('context-tooltip');
        const fmtCost = (usd) => {
            if (usd < 0.01) return `$${usd.toFixed(4)}`;
            if (usd < 1) return `$${usd.toFixed(3)}`;
            return `$${usd.toFixed(2)}`;
        };

        // === Chat auto-scroll / follow system (components/scroll.js) ===
        // The module wires its listeners at import time; initScroll only
        // hands it the replay flag (replayInProgress stays with the
        // history-replay code above).
        initScroll({
            isReplaying: () => replayInProgress,
        });

        // === Command palette (components/palette.js) ===
        // The catalog array is passed by reference: the slash-command
        // prefill entries were already pushed at top level above.
        initPalette({ commands: PALETTE_COMMANDS });

        // === Delete-approval modal (components/delete-approval.js) ===
        initDeleteApproval({
            getWs: () => ws,
        });

        // === Composer input helpers (components/composer.js) ===
        initComposer({
            showToast: (message, kind) => showToast(message, kind),
        });

        // === Markdown rendering pipeline (components/markdown.js) ===
        initMarkdown({
            showToast: (message, kind) => showToast(message, kind),
            copyTextToClipboard: (text) => copyTextToClipboard(text),
            getMessageRawStore: () => messageRawStore,
        });

        // === Tool-result rendering (components/tool-result.js) ===
        initToolResult({
            showToast: (message, kind) => showToast(message, kind),
            copyTextToClipboard: (text) => copyTextToClipboard(text),
            isReplaying: () => replayInProgress,
        });

        // === Settings modal + persisted preferences (components/settings.js) ===
        // The tabbed settings overlay, the server-backed feature/runtime
        // config, the MCP test flow, the providers list, the theme, the
        // editor preferences, desktop notifications, the "show reply model"
        // toggle and the accent color all live in the module; app.js keeps
        // the ws dispatch (config / test_mcp / provider_test) and forwards
        // payloads to its apply*Settings functions.
        initSettings({
            getWs: () => ws,
            getPane: () => activePane(),
            getModels: () => availableModels,
            requestModels: () => {
                modelsRequested = false;
                ensureModelsLoaded();
            },
            switchMainPane: (pane) => switchMainPane(pane),
            showToast: (message, kind) => showToast(message, kind),
            applyReplyModelChips: () => applyReplyModelChips(),
            setSubagentModel: (v) => { subagentPickerState.model = v; },
            setSubagentThinkingLevel: (v) => { subagentPickerState.thinkingLevel = v; },
            renderSubagentPicker: () => subagentPicker.render(),
            setReviewAgentModel: (v) => { reviewAgentPickerState.model = v; },
            setReviewAgentThinkingLevel: (v) => { reviewAgentPickerState.thinkingLevel = v; },
            renderReviewAgentPicker: () => reviewAgentPicker.render(),
        });

        // === Sidebar session list (components/sessions.js) ===
        initSessions({
            getWs: () => ws,
            getPane: () => activePane(),
            getPanes: () => panes,
            getNestedSessions: () => nestedSessions,
            nestedParentIdOf: (id, list) => nestedParentIdOf(id, list),
            nestedRowWillRender: (id, list, rowIds, seen) => nestedRowWillRender(id, list, rowIds, seen),
            collectNestedRows: (parentId, depth) => collectNestedRows(parentId, depth),
            buildNestedRow: (rec) => buildNestedSessionRow(rec),
            relativeTime: (value, now) => relativeTime(value, now),
            focusPane: (key) => focusPane(key),
            openSessionPane: (id) => openSessionPane(id),
            closePane: (key) => closePane(key),
            ensureConnected: () => ensureConnected(),
            cancelActiveTurn: () => cancelActiveTurn(),
            showToast: (message, kind) => showToast(message, kind),
            isResendAwaitingHistory: () => resendAwaitingHistory,
            isPendingSessionResponse: () => pendingSessionResponse,
            setPendingSessionResponse: (v) => { pendingSessionResponse = v; },
            getSessionInfoDiv: () => sessionInfoDiv,
            getMessagesDiv: () => messagesDiv,
            getMessageRawStore: () => messageRawStore,
        });

        // === Kanban board tab (drag-and-drop) ===
        initBoard({
            getWs: () => ws,
            requestModels: () => {
                modelsRequested = false;
                ensureModelsLoaded();
            },
            onSessionListRequested: () => requestSessionList(),
            onOpenAgent: (sessionId) => {
                switchMainPane('chat');
                openSessionPane(sessionId);
            },
            getModels: () => availableModels,
            getPane: () => activePane(),
            // The same relative-age policy the sidebar rows and the
            // transcript timestamps use (board activity rows used to carry
            // their own copy that rounded differently).
            relativeTime: (value, now) => relativeTime(value, now),
        });

        // === Automations tab (scheduled jobs) ===
        initAutomations({
            getWs: () => ws,
            showToast: (message, kind) => showToast(message, kind),
            switchMainPane: (pane) => switchMainPane(pane),
        });

        // Create the initial pane (the server's default session). Its session
        // id arrives in the connect handshake's config and is adopted by the
        // config handler.
        const initialPane = makePane();
        activePaneKey = initialPane.key;
        setTurnActive(false);
        setConnState('disconnected');
        connect();
        // The editor runs on its own /ws/editor socket; open it once here.
        // It manages its own reconnection and explorer refresh.
        connectEditorSocket();
