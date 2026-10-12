// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

(() => {
    const appPrefix = (() => {
        const marker = window.location.pathname.indexOf('/r/');
        return marker < 0 ? '' : window.location.pathname.slice(0, marker);
    })();
    const table = document.getElementById("sessions-table");
    const pager = document.getElementById("sessions-pagination");
    const numbers = document.getElementById("sessions-page-numbers");
    if (!table) return;

    const input = document.querySelector('[data-session-search]');
    const empty = document.querySelector('[data-session-empty]');
    let query = '';
    const matchesSession = row => {
        if (!query) return true;
        const sessionId = row.dataset.sessionId || '';
        return `${sessionId} ${row.textContent}`.toLowerCase().includes(query);
    };
    const api = pager && numbers && window.ocrPager ? ocrPager({ table, pager, numbers, filter: matchesSession, onRender: rows => { if (empty) empty.hidden = rows.length !== 0; } }) : null;
    if (input) input.addEventListener('input', () => { query = input.value.trim().toLowerCase(); if (api) api.refresh(); });
    const form = document.querySelector('.compare-form');
    if (form) form.addEventListener('submit', event => {
        const before = form.elements.before, after = form.elements.after;
        after.setCustomValidity(before.value === after.value ? ocrT('请选择两个不同的会话') : '');
        if (!form.reportValidity()) event.preventDefault();
    });
    if (form) form.addEventListener('change', () => form.elements.after.setCustomValidity(''));
    ocrArrowScroll(document.querySelector(".sessions-page .table-scroll"));

    // Session JSONL files are append-only and can be read while a review is
    // running. Poll only active rows so the list shows each completed file
    // without resetting the user's search or pagination controls.
    const progressRows = Array.from(table.querySelectorAll('[data-progress-url]'));
    const updateText = (row, selector, value) => {
        const element = row.querySelector(selector);
        if (element && element.textContent !== String(value)) element.textContent = value;
    };
    const pollProgress = async () => {
        const activeRows = progressRows.filter(row => row.isConnected && row.dataset.sessionActive === 'true');
        if (document.hidden) {
            if (activeRows.length) window.setTimeout(pollProgress, 1500);
            return;
        }
        await Promise.all(activeRows.map(async row => {
            const controller = new AbortController();
            const timeout = window.setTimeout(() => controller.abort(), 10000);
            try {
                const progressURL = row.dataset.progressUrl;
                const url = appPrefix && progressURL.startsWith('/') && !progressURL.startsWith(`${appPrefix}/`) ? `${appPrefix}${progressURL}` : progressURL;
                const response = await fetch(url, { cache: 'no-store', credentials: 'same-origin', signal: controller.signal });
                if (response.status === 404) {
                    delete row.dataset.sessionActive;
                    return;
                }
                if (!response.ok) return;
                const data = await response.json();
                if (!row.isConnected) return;
                updateText(row, '[data-progress-percent]', `${data.percent}%`);
                updateText(row, '[data-session-file-count]', data.total);
                updateText(row, '[data-progress-completed]', data.completed);
                updateText(row, '[data-progress-active]', data.processing);
                updateText(row, '[data-progress-reused]', data.reused);
                updateText(row, '[data-session-findings]', data.findings);
                updateText(row, '[data-session-duration]', data.duration);
                const status = row.querySelector('[data-session-status]');
                if (status && !data.active) {
                    status.className = `status-pill status-${data.state || 'legacy'}`;
                    updateText(row, '[data-session-status]', data.label);
                }
                if (!data.active) {
                    delete row.dataset.sessionActive;
                }
            } catch (_) {
                // Retain the last values and retry after a connection failure.
            } finally {
                window.clearTimeout(timeout);
            }
        }));
        if (progressRows.some(row => row.isConnected && row.dataset.sessionActive === 'true')) window.setTimeout(pollProgress, 1500);
    };
    if (progressRows.some(row => row.dataset.sessionActive === 'true')) pollProgress();
    const formatElapsed = seconds => seconds < 60 ? `${seconds.toFixed(1)}s` : `${Math.floor(seconds / 60)}m${Math.floor(seconds % 60)}s`;
    const refreshElapsed = () => progressRows.filter(row => row.isConnected && row.dataset.sessionActive === 'true').forEach(row => {
        const started = Date.parse(row.dataset.sessionStart || '');
        if (!Number.isNaN(started)) updateText(row, '[data-session-duration]', formatElapsed(Math.max(0, (Date.now() - started) / 1000)));
    });
    refreshElapsed();
    window.setInterval(refreshElapsed, 1000);})();
