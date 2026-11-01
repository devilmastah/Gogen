// Renders the GoGen settings modal DOM from the declarative schema
// (components/settings-schema.js): one sidebar button + one hidden
// `.settings-group[role=tabpanel]` per SETTINGS_TABS entry, then each
// schema row appended into its tab panel in schema order.
//
// Simple rows (select/number/text/password/textarea/color) are built here,
// reproducing the markup/CSS contract index.html used to hand-write
// (#settings-dialog .setting-row > div > label + control). Composite
// blocks (shared model/effort pickers, MCP and provider editors) are
// moved in verbatim from the hidden #settings-composites container, so
// their ids and internal structure stay in one place in index.html.
//
// renderSettings() must run before settings.js looks the controls up by
// id (it is called at the top of settings.js, after the module imports).

import { SETTINGS_TABS, SETTINGS_SCHEMA } from '/components/settings-schema.js';

// Builds the input/select/textarea for one schema entry.
function buildControl(entry) {
    const al = entry.ariaLabel || entry.label;
    let el;
    switch (entry.type) {
        case 'select': {
            el = document.createElement('select');
            el.id = entry.id;
            el.setAttribute('aria-label', al);
            for (const [value, text] of entry.options) {
                const opt = document.createElement('option');
                opt.value = value;
                opt.textContent = text;
                el.appendChild(opt);
            }
            break;
        }
        case 'number': {
            el = document.createElement('input');
            el.type = 'number';
            el.id = entry.id;
            if (entry.min !== undefined) el.min = String(entry.min);
            if (entry.max !== undefined) el.max = String(entry.max);
            if (entry.step !== undefined) el.step = String(entry.step);
            el.setAttribute('aria-label', al);
            break;
        }
        case 'password': {
            el = document.createElement('input');
            el.type = 'password';
            el.id = entry.id;
            if (entry.placeholder) el.placeholder = entry.placeholder;
            el.autocomplete = 'new-password';
            el.setAttribute('aria-label', al);
            break;
        }
        case 'color': {
            el = document.createElement('input');
            el.type = 'color';
            el.id = entry.id;
            if (entry.value) el.value = entry.value;
            el.setAttribute('aria-label', al);
            break;
        }
        case 'textarea': {
            el = document.createElement('textarea');
            el.id = entry.id;
            el.rows = entry.rows || 6;
            el.spellcheck = false;
            el.setAttribute('aria-label', al);
            break;
        }
        default: { // text
            el = document.createElement('input');
            el.type = 'text';
            el.id = entry.id;
            el.autocomplete = 'off';
            el.setAttribute('aria-label', al);
            break;
        }
    }
    return el;
}

// Builds one `.setting-row` (label + control + optional button/note/reset).
// A separator is the line between the watch row and the rerank group.
function buildRow(entry) {
    if (entry.type === 'separator') {
        const rule = document.createElement('div');
        rule.className = 'settings-separator';
        rule.setAttribute('role', 'separator');
        return rule;
    }
    const row = document.createElement('div');
    row.className = 'setting-row';
    if (entry.rowId) row.id = entry.rowId;

    const inner = document.createElement('div');
    const label = document.createElement('label');
    label.textContent = entry.label;
    inner.appendChild(label);
    inner.appendChild(buildControl(entry));

    if (entry.button) {
        const b = document.createElement('button');
        b.type = 'button';
        b.id = entry.button.id;
        b.className = 'settings-btn-secondary';
        b.textContent = entry.button.label;
        inner.appendChild(b);
    }
    if (entry.note) {
        const p = document.createElement('p');
        p.className = 'settings-note';
        p.textContent = entry.note;
        inner.appendChild(p);
    }
    if (entry.progressId) {
        const p = document.createElement('p');
        p.className = 'settings-note';
        p.id = entry.progressId;
        p.textContent = 'Not indexed';
        inner.appendChild(p);
    }
    if (entry.resetToDefault) {
        const b = document.createElement('button');
        b.type = 'button';
        b.id = entry.resetId || (entry.id + '-reset-btn');
        b.className = 'settings-btn-secondary';
        b.textContent = 'Reset to default';
        inner.appendChild(b);
    }
    row.appendChild(inner);
    return row;
}

// Builds the whole modal body (sidebar + panels + rows). Idempotent.
export function renderSettings() {
    const sidebar = document.querySelector('.settings-sidebar');
    const content = document.querySelector('.settings-content');
    if (!sidebar || !content) return;
    sidebar.replaceChildren();
    content.replaceChildren();

    const panels = new Map();
    for (const tab of SETTINGS_TABS) {
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'settings-tab-btn';
        btn.dataset.tab = tab.id;
        btn.setAttribute('role', 'tab');
        btn.setAttribute('aria-selected', 'false');
        btn.setAttribute('aria-controls', 'settings-tab-' + tab.id);
        btn.textContent = tab.label;
        sidebar.appendChild(btn);

        const panel = document.createElement('div');
        panel.className = 'settings-group';
        panel.id = 'settings-tab-' + tab.id;
        panel.setAttribute('role', 'tabpanel');
        panel.hidden = true;
        const title = document.createElement('h4');
        title.className = 'settings-group-title';
        title.textContent = tab.label;
        panel.appendChild(title);
        content.appendChild(panel);
        panels.set(tab.id, panel);
    }

    for (const entry of SETTINGS_SCHEMA) {
        const panel = panels.get(entry.tab);
        if (!panel) continue;
        if (entry.custom) {
            const node = document.getElementById(entry.custom);
            if (node) panel.appendChild(node);
        } else {
            panel.appendChild(buildRow(entry));
        }
    }
}
