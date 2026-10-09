// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

(() => {
    const main = document.querySelector('main');
    if (!main) return;
    const status = document.createElement('p');
    status.className = 'action-status';
    status.setAttribute('role', 'status');
    const pageRoot = document.body || main;
    pageRoot.append(status);
    let toastTimer;
    function notify(message) {
        clearTimeout(toastTimer);
        status.textContent = message;
        toastTimer = setTimeout(() => { status.textContent = ''; }, 4000);
    }

    // A native dialog supplies focus containment, Escape and an inert background.
    // A small fallback covers browsers without HTMLDialogElement.showModal.
    function openDialog({ title, description, content, confirmLabel, onConfirm }) {
        const opener = document.activeElement;
        const dialog = document.createElement('dialog');
        dialog.className = 'action-dialog';
        dialog.setAttribute('aria-labelledby', 'action-dialog-title');
        dialog.setAttribute('aria-describedby', 'action-dialog-description');
        dialog.innerHTML = '<div class="dialog-heading"><span class="dialog-symbol" aria-hidden="true">!</span><h2 id="action-dialog-title"></h2></div><p id="action-dialog-description"></p><div class="dialog-content"></div><p class="dialog-error" role="alert" hidden></p><div class="dialog-actions"><button type="button" class="button button-secondary" data-cancel></button><button type="button" class="button button-danger-solid" data-confirm></button></div>';
        dialog.querySelector('h2').textContent = title;
        dialog.querySelector('#action-dialog-description').textContent = description;
        dialog.querySelector('.dialog-content').append(content);
        const cancel = dialog.querySelector('[data-cancel]');
        const confirm = dialog.querySelector('[data-confirm]');
        const error = dialog.querySelector('.dialog-error');
        cancel.textContent = onConfirm ? '取消' : '关闭';
        confirm.textContent = confirmLabel || '';
        confirm.hidden = !onConfirm;
        let busy = false;
        const close = () => {
            if (busy) return;
            if (dialog.close) dialog.close();
            dialog.remove();
            main.removeAttribute('inert');
            if (opener && opener.isConnected) opener.focus();
        };
        cancel.addEventListener('click', close);
        dialog.addEventListener('cancel', event => { event.preventDefault(); close(); });
        dialog.addEventListener('click', event => { if (event.target === dialog) {
            const bounds = dialog.getBoundingClientRect();
            if (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom) close();
        } });
        dialog.addEventListener('keydown', event => {
            if (event.key === 'Escape') { event.preventDefault(); close(); }
            if (event.key !== 'Tab') return;
            const controls = [...dialog.querySelectorAll('button, input, textarea, [tabindex]')].filter(el => !el.disabled && !el.hidden);
            const first = controls[0], last = controls[controls.length - 1];
            if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
            if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
        });
        confirm.addEventListener('click', async () => {
            if (busy) return;
            busy = true;
            confirm.disabled = cancel.disabled = true;
            confirm.textContent = '正在删除…';
            error.hidden = true;
            try { await onConfirm(); busy = false; close(); }
            catch (err) {
                busy = false;
                confirm.disabled = cancel.disabled = false;
                confirm.textContent = '重试删除';
                error.textContent = err.message;
                error.hidden = false;
            }
        });
        pageRoot.append(dialog);
        if (typeof dialog.showModal === 'function') dialog.showModal();
        else { dialog.setAttribute('open', ''); dialog.classList.add('dialog-fallback'); main.setAttribute('inert', ''); }
        cancel.focus();
        return dialog;
    }

    async function copyValue(value, label, opener) {
        try {
            if (navigator.clipboard && window.isSecureContext) await navigator.clipboard.writeText(value);
            else {
                const input = document.createElement('textarea');
                input.value = value;
                input.className = 'clipboard-input';
                document.body.append(input);
                input.select();
                try { if (!document.execCommand('copy')) throw new Error('Clipboard unavailable'); }
                finally { input.remove(); if (opener) opener.focus(); }
            }
            notify(label + '已复制');
        } catch (_) {
            const input = document.createElement('textarea');
            input.value = value;
            input.readOnly = true;
            input.className = 'copy-path-input';
            input.setAttribute('aria-label', label);
            openDialog({ title: '复制' + label, description: '浏览器暂时无法写入剪贴板，请选中后手动复制。', content: input });
            input.focus(); input.select();
        }
    }

    document.querySelectorAll('[data-copy-path]').forEach(el => {
        const copyLabel = '完整路径';
        const value = el.dataset.copyPath || el.dataset.sessionId || el.title;
        const copyTarget = document.createElement('button');
        copyTarget.type = 'button';
        copyTarget.className = 'copy-path-button';
        copyTarget.textContent = '复制';
        copyTarget.title = '复制完整路径';
        copyTarget.setAttribute('aria-label', copyTarget.title + '：' + value);
        el.after(copyTarget);
        copyTarget.addEventListener('click', async event => {
            event.preventDefault();
            event.stopPropagation();
            await copyValue(value, copyLabel, copyTarget);
        });
    });

    const sessionIDs = document.querySelectorAll('.session-id[title], .session-id-value[data-session-id]');
    if (sessionIDs.length) {
        const popup = document.createElement('button');
        popup.type = 'button';
        popup.className = 'session-copy-popup';
        popup.textContent = '复制会话 ID';
        popup.hidden = true;
        document.body.append(popup);
        let activeID;
        let hideTimer;
        const showPopup = el => {
            clearTimeout(hideTimer);
            activeID = el;
            const bounds = el.getBoundingClientRect();
            popup.style.left = Math.max(8, Math.min(window.innerWidth - 140, bounds.left)) + 'px';
            popup.style.top = Math.min(window.innerHeight - 40, bounds.bottom + 6) + 'px';
            popup.hidden = false;
        };
        const scheduleHide = () => {
            hideTimer = setTimeout(() => { if (activeID) { popup.hidden = true; activeID = null; } }, 180);
        };
        sessionIDs.forEach(el => {
            el.addEventListener('mouseenter', () => showPopup(el));
            el.addEventListener('mouseleave', scheduleHide);
            el.addEventListener('focus', () => showPopup(el));
            el.addEventListener('blur', scheduleHide);
        });
        popup.addEventListener('mouseenter', () => clearTimeout(hideTimer));
        popup.addEventListener('mouseleave', scheduleHide);
        popup.addEventListener('click', async () => {
            if (!activeID) return;
            await copyValue(activeID.dataset.sessionId || activeID.title, '会话 ID', popup);
        });
    }

    document.querySelectorAll('[data-delete-session]').forEach(button => {
        button.addEventListener('click', () => {
            if (document.querySelector('.action-dialog')) return;
            const record = document.createElement('code');
            record.className = 'dialog-session-id';
            record.textContent = button.dataset.sessionId;
            openDialog({ title: '删除这条会话？', description: '删除后无法恢复。仅删除审核记录，不影响项目源码。', content: record, confirmLabel: '确认删除', onConfirm: async () => {
                let response;
                try { response = await fetch(button.dataset.deleteSession, { method: 'DELETE', headers: { 'X-OCR-Confirm': 'delete' }, credentials: 'same-origin' }); }
                catch (_) { throw new Error('无法连接服务，请检查连接后重试。'); }
                if (!response.ok && response.status !== 404) throw new Error(response.status === 403 ? '请求未通过验证，请刷新页面后重试。' : '删除失败，请稍后重试。');
                try { localStorage.removeItem('ocr-viewer-marks:' + button.dataset.deleteSession.replace(/\/delete$/, '')); }
                catch (_) { /* Storage failure must not undo a successful deletion. */ }
                if (button.dataset.returnUrl) window.location.assign(button.dataset.returnUrl);
                else window.location.reload();
            } });
        });
    });

    document.querySelectorAll('[data-delete-repository]').forEach(button => {
        button.addEventListener('click', () => {
            if (document.querySelector('.action-dialog')) return;
            const record = document.createElement('code');
            record.className = 'dialog-session-id';
            record.textContent = button.dataset.repository || '';
            openDialog({ title: '删除这个仓库？', description: '删除后无法恢复，将删除该仓库的全部审核记录，不影响项目源码。', content: record, confirmLabel: '确认删除', onConfirm: async () => {
                let response;
                try { response = await fetch(button.dataset.deleteRepository, { method: 'DELETE', headers: { 'X-OCR-Confirm': 'delete' }, credentials: 'same-origin' }); }
                catch (_) { throw new Error('无法连接服务，请检查连接后重试。'); }
                if (!response.ok && response.status !== 404) throw new Error(response.status === 403 ? '请求未通过验证，请刷新页面后重试。' : '删除失败，请稍后重试。');
                window.location.reload();
            } });
        });
    });
})();
