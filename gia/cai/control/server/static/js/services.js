// services.js - Gestión de servicios IoT

const SERVICES = [
    {
        id: 'SVC-C1',
        name: 'C1 - Panel Web',
        description: 'Dashboard principal y UI de control',
        icon: 'fa-desktop',
        color: '#2563eb',
        url: 'https://bridgecarlos.onrender.com',
        healthUrl: 'https://bridgecarlos.onrender.com/health',
        status: 'active',
        urls: [
            { label: 'Panel', url: 'https://bridgecarlos.onrender.com' },
            { label: 'Login', url: 'https://bridgecarlos.onrender.com/login' },
            { label: '/api/state', url: 'https://bridgecarlos.onrender.com/api/state' },
        ],
    },
    {
        id: 'SVC-C2',
        name: 'C2 - Gateway DeepSeek',
        description: 'Proxy Manager + DeepSeek Gateway + Function Calling',
        icon: 'fa-door-open',
        color: '#7c3aed',
        url: 'https://carlos-gateway.onrender.com/proxy/ui',
        healthUrl: 'https://carlos-gateway.onrender.com/health',
        status: 'active',
        urls: [
            { label: 'Health', url: 'https://carlos-gateway.onrender.com/health' },
            { label: '/v1/models', url: 'https://carlos-gateway.onrender.com/v1/models' },
            { label: '/v1/chat/completions', url: 'https://carlos-gateway.onrender.com/v1/chat/completions' },
            { label: '/api/auth', url: 'https://carlos-gateway.onrender.com/api/auth' },
            { label: '/proxy/ui', url: 'https://carlos-gateway.onrender.com/proxy/ui' },
            { label: '/proxy/status', url: 'https://carlos-gateway.onrender.com/proxy/status' },
            { label: '/proxy/logs', url: 'https://carlos-gateway.onrender.com/proxy/logs' },
        ],
    },
    {
        id: 'SVC-C3',
        name: 'C3 - Carlos Code',
        description: 'Orquestador de tareas, planificación y ejecución',
        icon: 'fa-brain',
        color: '#f59e0b',
        url: 'https://carlos-code.onrender.com/health',
        healthUrl: 'https://carlos-code.onrender.com/health',
        status: 'active',
        urls: [
            { label: 'Health', url: 'https://carlos-code.onrender.com/health' },
            { label: '/plan', url: 'https://carlos-code.onrender.com/plan' },
            { label: '/tools', url: 'https://carlos-code.onrender.com/tools' },
            { label: '/update', url: 'https://carlos-code.onrender.com/update' },
            { label: '/finalize', url: 'https://carlos-code.onrender.com/finalize' },
            { label: '/git', url: 'https://carlos-code.onrender.com/git' },
        ],
    },
    {
        id: 'SVC-C4',
        name: 'C4 - Engram MCP',
        description: 'Memoria persistente y contexto (SQLite)',
        icon: 'fa-puzzle-piece',
        color: '#ec4899',
        url: 'https://engram-mcp-q9ln.onrender.com/health',
        healthUrl: 'https://engram-mcp-q9ln.onrender.com/health',
        status: 'active',
        urls: [
            { label: 'Health', url: 'https://engram-mcp-q9ln.onrender.com/health' },
            { label: '/search', url: 'https://engram-mcp-q9ln.onrender.com/search' },
            { label: '/recent', url: 'https://engram-mcp-q9ln.onrender.com/recent' },
            { label: '/stats', url: 'https://engram-mcp-q9ln.onrender.com/stats' },
        ],
    },
    {
        id: 'SVC-C5',
        name: 'C5 - Context7+grep',
        description: 'Documentación de librerías y búsqueda en GitHub',
        icon: 'fa-search',
        color: '#06b6d4',
        url: 'https://context7-grep.onrender.com/health',
        healthUrl: 'https://context7-grep.onrender.com/health',
        status: 'active',
        urls: [
            { label: 'Health', url: 'https://context7-grep.onrender.com/health' },
            { label: '/context7/resolve', url: 'https://context7-grep.onrender.com/context7/resolve' },
            { label: '/context7/docs', url: 'https://context7-grep.onrender.com/context7/docs' },
            { label: '/grep_app/search', url: 'https://context7-grep.onrender.com/grep_app/search' },
            { label: '/search', url: 'https://context7-grep.onrender.com/search' },
        ],
    },
    {
        id: 'SVC-C6',
        name: 'C6 - Worker Code',
        description: 'Código de ejecución para tareas distribuidas',
        icon: 'fa-code',
        color: '#10b981',
        url: 'https://worker-code.onrender.com/health',
        healthUrl: 'https://worker-code.onrender.com/health',
        status: 'active',
        urls: [
            { label: 'Health', url: 'https://worker-code.onrender.com/health' },
        ],
    },
    {
        id: 'SVC-DB',
        name: 'Supabase DB',
        description: 'Base de datos Supabase (pooler IPv4)',
        icon: 'fa-database',
        color: '#059669',
        url: 'https://supabase.com/dashboard/project/qqaocynurdhtmmxktzcz',
        status: 'active',
        dbCheck: true,
        urls: [
            { label: 'Dashboard', url: 'https://supabase.com/dashboard/project/qqaocynurdhtmmxktzcz' },
        ],
    },
    // Workers adicionales (C7-C10) se añaden aquí cuando se desplieguen.
];

let serviceStatusCache = {};

// Custom services from C1 registry (Supabase): merged into SERVICES so new
// Render accounts appear in this view like the built-ins.
async function loadCustomServices() {
    try {
        const d = await apiJSON('/api/services');
        for (const c of (d.custom || [])) {
            if (SERVICES.some(s => s.id === c.id)) continue;
            SERVICES.push({
                id: c.id,
                name: c.name,
                description: 'Servicio agregado por el usuario',
                icon: 'fa-plug',
                color: c.color || '#64748b',
                url: c.dashboard_url || c.health_url || '#',
                healthUrl: c.health_url || null,
                status: 'active',
                custom: true,
                urls: [
                    ...(c.health_url ? [{ label: 'Health', url: c.health_url }] : []),
                    ...(c.dashboard_url ? [{ label: 'Dashboard', url: c.dashboard_url }] : []),
                ],
            });
        }
    } catch { /* registry offline: built-ins only */ }
}

async function checkServiceHealth(service) {
    // SVC-DB: status from /api/state db_stage field
    if (service.dbCheck) {
        try {
            const resp = await fetch('/api/state', { signal: AbortSignal.timeout(5000) });
            if (!resp.ok) return 'error';
            const data = await resp.json();
            return data.db_stage === 'connected' ? 'active' : 'error';
        } catch { return 'error'; }
    }
    if (!service.healthUrl) return service.status;
    try {
        const resp = await fetch(service.healthUrl, { method: 'GET', signal: AbortSignal.timeout(5000) });
        if (resp.ok) return 'active';
        return 'error';
    } catch {
        return 'error';
    }
}

async function refreshServiceStatuses() {
    for (const svc of SERVICES) {
        const status = await checkServiceHealth(svc);
        serviceStatusCache[svc.id] = status;
    }
    renderServices();
}

function renderServices() {
    const container = document.getElementById('servicesContainer');
    if (!container) return;

    container.innerHTML = SERVICES.map(service => {
        const status = serviceStatusCache[service.id] || service.status;
        const statusLabel = status === 'active' ? 'Activo' : status === 'error' ? 'Error' : 'Inactivo';
        const chips = (service.urls || []).map(u =>
            `<a class="service-chip" href="${u.url}" target="_blank" rel="noopener" onclick="event.stopPropagation()">${u.label}</a>`
        ).join('');
        return `
        <div class="service-card" data-id="${service.id}" data-url="${service.url}" style="position:relative">
            <button class="svc-menu-btn" data-svc-menu="${service.id}" title="Opciones"
                style="position:absolute;top:8px;right:8px;background:none;border:none;cursor:pointer;font-size:18px;line-height:1;color:var(--color-primary,#2563eb)">⋮</button>
            <div class="svc-menu" id="svc-menu-${service.id}" hidden
                style="position:absolute;top:32px;right:8px;z-index:20;background:var(--color-surface,#fff);border:1px solid var(--color-border,#ddd);border-radius:8px;padding:6px;box-shadow:0 4px 12px rgba(0,0,0,.15)">
                <button class="atom-btn atom-btn--ghost" data-svc-table="${service.id}">Tabla</button>
                ${service.custom ? `<button class="atom-btn atom-btn--danger" data-svc-del="${service.id}">Eliminar</button>` : ''}
            </div>
            <div class="service-icon" style="background: ${service.color}20; color: ${service.color}">
                <i class="fas ${service.icon}"></i>
            </div>
            <div class="service-content">
                <h3 class="service-name">${service.name}</h3>
                <p class="service-description">${service.description}</p>
                <div class="service-chips">${chips}</div>
                <div class="service-footer">
                    <span class="service-status ${status}">
                        <i class="fas fa-circle"></i> ${statusLabel}
                    </span>
                    <span class="service-id">${service.id}</span>
                </div>
            </div>
        </div>`;
    }).join('') + `
        <div class="service-card" id="svc-add-card" style="cursor:pointer;border-style:dashed;align-items:center;justify-content:center;min-height:120px">
            <div class="service-content" style="text-align:center">
                <div style="font-size:28px">＋</div>
                <h3 class="service-name">Agregar servicio</h3>
                <p class="service-description">Nueva cuenta de Render u otro servicio</p>
            </div>
        </div>`;

    document.querySelectorAll('.service-card').forEach(card => {
        card.addEventListener('click', function() {
            const url = this.dataset.url;
            if (url && url !== '#') {
                window.open(url, '_blank');
            }
        });
    });
    // ⋮ menus (stopPropagation so the card link doesn't fire).
    container.querySelectorAll('[data-svc-menu]').forEach(b =>
        b.addEventListener('click', (e) => {
            e.stopPropagation();
            const id = b.getAttribute('data-svc-menu');
            const menu = document.getElementById('svc-menu-' + id);
            container.querySelectorAll('.svc-menu').forEach(m => { if (m !== menu) m.hidden = true; });
            if (menu) menu.hidden = !menu.hidden;
        }));
    container.querySelectorAll('[data-svc-table]').forEach(b =>
        b.addEventListener('click', (e) => { e.stopPropagation(); svcOpenTable(b.getAttribute('data-svc-table')); }));
    container.querySelectorAll('[data-svc-del]').forEach(b =>
        b.addEventListener('click', (e) => { e.stopPropagation(); svcDelete(b.getAttribute('data-svc-del')); }));
    const addCard = document.getElementById('svc-add-card');
    if (addCard) addCard.addEventListener('click', (e) => { e.stopPropagation(); svcAddForm(); });
}

// ---- Per-service key/value table (2 columns, string values, Supabase) ----
async function svcOpenTable(serviceId) {
    const svc = SERVICES.find(s => s.id === serviceId);
    const title = 'Tabla — ' + (svc ? svc.name : serviceId);
    openModal(title, `<p class="atom-muted">Cargando…</p>`);
    try {
        const d = await apiJSON('/api/services/' + encodeURIComponent(serviceId) + '/table');
        svcRenderTable(serviceId, d.rows || []);
    } catch (e) {
        openModal(title, `<p class="atom-muted">Error al cargar la tabla.</p>`);
    }
}

function svcRenderTable(serviceId, rows) {
    const svc = SERVICES.find(s => s.id === serviceId);
    const title = 'Tabla — ' + (svc ? svc.name : serviceId);
    const trs = rows.map(r => `
        <tr data-row="${escapeHtml(r.key)}">
            <td><input class="atom-input svc-key-input" value="${escapeHtml(r.key)}"></td>
            <td><input class="atom-input svc-value-input" value="${escapeHtml(r.value)}"></td>
            <td><input class="atom-input svc-optional-input" placeholder="opcional"></td>
            <td style="white-space:nowrap; font-size:10px">
                <button class="atom-btn atom-btn--ghost row-edit-btn" data-key="${escapeHtml(r.key)}">Editar</button>
                <button class="atom-btn atom-btn--danger row-del-btn" data-key="${escapeHtml(r.key)}">Eliminar</button>
            </td>
        </tr>`).join('');
    openModal(title, `
        <table class="atom-table" style="width:100%">
            <thead><tr><th>Clave</th><th>Valor</th><th>Opcional</th><th></th></tr></thead>
            <tbody>${trs || `<tr><td colspan="4" class="atom-muted">Sin filas. Agrega la primera abajo.</td></tr>`}</tbody>
        </table>
        <h3 style="margin-top:var(--space-3)">Agregar / editar fila</h3>
        <div class="org-form-row">
            <input class="atom-input svc-row-key" placeholder="clave (p. ej. gmail_cuenta_2)">
            <input class="atom-input svc-row-value" placeholder="valor">
            <input class="atom-input svc-row-optional" placeholder="opcional (ej. tipo gmail)">
            <button class="atom-btn atom-btn--primary" id="svc-row-save">Guardar</button>
        </div>
        <p class="atom-muted">Los valores son texto y se guardan en Supabase. Carlos Code puede leer y modificar esta tabla.</p>`);
    document.querySelectorAll('.row-del-btn').forEach(b =>
        b.onclick = async () => {
            await apiJSON('/api/services/' + encodeURIComponent(serviceId) + '/table?key=' + encodeURIComponent(b.getAttribute('data-key')), 'DELETE');
            svcOpenTable(serviceId);
        });
    document.querySelectorAll('.row-edit-btn').forEach(b => {
        const r = rows.find(r => r.key === b.getAttribute('data-key'));
        if (r) {
            document.getElementById('svc-row-key').value = r.key;
            document.getElementById('svc-row-value').value = r.value;
            document.getElementById('svc-row-optional').value = r.optional || '';
            document.getElementById('svc-row-key').focus();
        }
    });
    document.getElementById('svc-row-save').onclick = async () => {
        const k = document.getElementById('svc-row-key').value.trim();
        const v = document.getElementById('svc-row-value').value;
        const o = document.getElementById('svc-row-optional').value;
        if (!k) return;
        await apiJSON('/api/services/' + encodeURIComponent(serviceId) + '/table', 'POST', { key: k, value: v, optional: o });
        svcOpenTable(serviceId);
    };
}

async function svcDelete(serviceId) {
    if (!confirm('¿Eliminar ' + serviceId + ' y su tabla?')) return;
    try {
        await apiJSON('/api/services/' + encodeURIComponent(serviceId), 'DELETE');
        const i = SERVICES.findIndex(s => s.id === serviceId);
        if (i >= 0) SERVICES.splice(i, 1);
        renderServices();
    } catch (e) { showToast('Error al eliminar', 'error'); }
}

function svcAddForm() {
    openModal('Agregar servicio', `
        <div class="org-form-section">
            <label class="atom-label">Nombre</label>
            <input class="atom-input" id="svc-new-name" placeholder="Render cuenta 2">
        </div>
        <div class="org-form-section">
            <label class="atom-label">Health URL</label>
            <input class="atom-input" id="svc-new-health" placeholder="https://mi-servicio.onrender.com/health">
        </div>
        <div class="org-form-section">
            <label class="atom-label">Dashboard URL (opcional)</label>
            <input class="atom-input" id="svc-new-dash" placeholder="https://dashboard.render.com/web/srv-...">
        </div>
        <div class="org-form-section">
            <label class="atom-label">Color (opcional)</label>
            <input class="atom-input" id="svc-new-color" placeholder="#64748b">
        </div>
        <div style="margin-top:var(--space-3)">
            <button class="atom-btn atom-btn--primary" id="svc-new-save">Agregar</button>
        </div>`);
    document.getElementById('svc-new-save').onclick = async () => {
        const body = {
            name: document.getElementById('svc-new-name').value.trim(),
            health_url: document.getElementById('svc-new-health').value.trim(),
            dashboard_url: document.getElementById('svc-new-dash').value.trim(),
            color: document.getElementById('svc-new-color').value.trim() || '#64748b',
        };
        if (!body.name || !body.health_url) return;
        try {
            const d = await apiJSON('/api/services', 'POST', body);
            closeModal();
            await loadCustomServices();
            renderServices();
            showToast('Servicio ' + (d.service ? d.service.id : '') + ' agregado', 'success');
        } catch (e) { showToast('Error al agregar', 'error'); }
    };
}

function refreshServices() {
    refreshServiceStatuses();
    showToast('Servicios actualizados', 'success');
}

function showToast(message, type = 'info') {
    const toast = document.createElement('div');
    toast.className = `toast toast-${type}`;
    toast.textContent = message;
    document.body.appendChild(toast);
    setTimeout(() => toast.classList.add('show'), 10);
    setTimeout(() => {
        toast.classList.remove('show');
        setTimeout(() => toast.remove(), 300);
    }, 3000);
}

// ========== KEEPALIVE SYSTEM (server-side) ==========
// El toggle vive en C1 (persistido en Supabase): el browser solo lo enciende/
// apaga y pinta el estado. El bucle de pings corre en el server cada 5 min,
// así funciona aunque el navegador esté cerrado.

let kaServices = {};

async function fetchKeepaliveState() {
    try {
        const resp = await fetch('/api/keepalive', { signal: AbortSignal.timeout(8000) });
        if (!resp.ok) return null;
        return await resp.json();
    } catch { return null; }
}

function renderKeepaliveStatus(state) {
    const label = document.getElementById('keepalive-label');
    const toggle = document.getElementById('keepalive-toggle');
    const box = document.getElementById('keepalive-status');
    if (!label || !toggle || !box) return;
    if (!state) {
        label.textContent = 'Keep-alive sin estado';
        return;
    }
    toggle.checked = !!state.enabled;
    label.textContent = state.enabled ? `Keep-alive ON (cada ${Math.round(state.interval_sec / 60)} min)` : 'Keep-alive OFF';
    kaServices = state.services || {};
    if (!state.enabled) {
        box.innerHTML = '';
        return;
    }
    const rows = Object.values(kaServices).map(s => {
        const cls = s.status === 'ok' ? 'ka-ok' : s.status === 'error' ? 'ka-err' : 'ka-pending';
        const icon = s.status === 'ok' ? '🟢' : s.status === 'error' ? '🔴' : '⚪';
        const lat = s.latency_ms != null ? `${s.latency_ms}ms` : '—';
        const at = s.checked_at ? new Date(s.checked_at).toLocaleTimeString() : '—';
        return `<div class="ka-row ${cls}"><span>${icon} ${s.name || s.id}</span><span>${lat}</span><span class="ka-dim">${at}</span></div>`;
    }).join('');
    box.innerHTML = `<div class="ka-box"><div class="ka-head">Último heartbeat: ${state.last_run ? new Date(state.last_run).toLocaleTimeString() : 'pendiente'} · próximo en ~${Math.round(state.interval_sec / 60)} min</div>${rows || '<div class="ka-dim">Sin checks todavía</div>'}</div>`;
}

async function toggleKeepalive() {
    const toggle = document.getElementById('keepalive-toggle');
    if (!toggle) return;
    const newState = toggle.checked;
    const prevState = !newState;
    // Optimistic flip: UI updates immediately
    renderKeepaliveLabel(newState);
    try {
        const resp = await fetch('/api/keepalive', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ enabled: newState }),
            signal: AbortSignal.timeout(8000),
        });
        const data = await resp.json();
        if (!resp.ok) {
            // Revert on error
            toggle.checked = prevState;
            renderKeepaliveLabel(prevState);
            showToast(data.error || 'Error al cambiar keep-alive', 'error');
            return;
        }
        toggle.checked = !!data.enabled;
        renderKeepaliveLabel(data.enabled);
        showToast(data.enabled ? 'Keep-alive activado (cada 5 min)' : 'Keep-alive desactivado', data.enabled ? 'success' : 'warning');
        const state = await fetchKeepaliveState();
        renderKeepaliveStatus(state);
    } catch {
        // Revert on network error
        toggle.checked = prevState;
        renderKeepaliveLabel(prevState);
        showToast('No se pudo conectar con el servidor', 'error');
    }
}

function renderKeepaliveLabel(enabled) {
    const label = document.getElementById('keepalive-label');
    if (label) {
        label.textContent = enabled ? 'Keep-alive ON (cada 5 min)' : 'Keep-alive OFF';
    }
}

async function refreshKeepalive() {
    const state = await fetchKeepaliveState();
    renderKeepaliveStatus(state);
}

// CSS for toggle switch + status box (injected dynamically)
const kaStyle = document.createElement('style');
kaStyle.textContent = `
.ka-switch { position: relative; display: inline-block; width: 52px; height: 28px; }
.ka-switch input { opacity: 0; width: 0; height: 0; }
.ka-slider { position: absolute; cursor: pointer; inset: 0; background: var(--color-border, #555); border-radius: 28px; transition: 0.3s; }
.ka-slider:before { content: ""; position: absolute; height: 22px; width: 22px; left: 3px; top: 3px; background: white; border-radius: 50%; transition: 0.3s; }
.ka-switch input:checked + .ka-slider { background: #10b981; }
.ka-switch input:checked + .ka-slider:before { transform: translateX(24px); }
.ka-status { margin-bottom: var(--space-3); }
.ka-box { background: var(--color-surface, #111827); border: 1px solid var(--color-border, #333); border-radius: var(--radius-lg, 10px); padding: var(--space-3); font-size: 13px; }
.ka-head { font-weight: 600; margin-bottom: 8px; color: #e5e7eb; }
.ka-row { display: flex; justify-content: space-between; gap: 12px; padding: 4px 0; border-bottom: 1px dashed #333; }
.ka-row:last-child { border-bottom: 0; }
.ka-row.ka-ok { color: #6ee7b7; }
.ka-row.ka-err { color: #fca5a5; }
.ka-row.ka-pending { color: #d1d5db; }
.ka-dim { color: #6b7280; font-size: 12px; }
.service-chips { display: flex; flex-wrap: wrap; gap: 4px; margin-top: 4px; }
.service-chip {
    display: inline-block;
    font-size: 11px;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--color-surface-2, #e5e7eb);
    color: var(--color-text-muted, #6b7280);
    text-decoration: none;
    border: 1px solid var(--color-border, #d1d5db);
    transition: background .15s ease, color .15s ease;
    cursor: pointer;
}
.service-chip:hover { background: var(--color-primary, #2563eb); color: white; border-color: var(--color-primary, #2563eb); }
`;

document.addEventListener('DOMContentLoaded', () => {
    loadCustomServices().then(() => {
        renderServices();
        refreshServiceStatuses();
    });
    refreshKeepalive();

    // 30s poll for keepalive state (spec:49-57)
    setInterval(refreshKeepalive, 30000);

    const refreshBtn = document.getElementById('refreshServicesBtn');
    if (refreshBtn) {
        refreshBtn.addEventListener('click', refreshServices);
    }

    const keepaliveToggle = document.getElementById('keepalive-toggle');
    if (keepaliveToggle) {
        keepaliveToggle.addEventListener('change', toggleKeepalive);
    }
});

// CSS for toast notifications (injected dynamically)
const style = document.createElement('style');
style.textContent = `
.toast {
    position: fixed;
    bottom: 20px;
    right: 20px;
    padding: 12px 24px;
    border-radius: 8px;
    color: white;
    font-weight: 500;
    z-index: 1000;
    transform: translateY(100px);
    opacity: 0;
    transition: all 0.3s ease;
    box-shadow: 0 4px 12px rgba(0,0,0,0.15);
}
.toast.show {
    transform: translateY(0);
    opacity: 1;
}
.toast-success { background: #16a34a; }
.toast-error { background: #dc2626; }
.toast-info { background: #2563eb; }
.toast-warning { background: #f59e0b; }
`;
document.head.appendChild(kaStyle);
document.head.appendChild(style);
