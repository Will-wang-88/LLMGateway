// LLM Gateway dashboard. Auth: Basic auth or Bearer token via prompt.
(function () {
  'use strict';

  const ADMIN = '/admin';
  const storage = window.sessionStorage;

  // ---- auth -----------------------------------------------------------------
  function authHeader() {
    const t = storage.getItem('admin_token');
    if (t) return 'Bearer ' + t;
    const u = storage.getItem('admin_user');
    const p = storage.getItem('admin_pass');
    if (u !== null && p !== null) return 'Basic ' + btoa(u + ':' + p);
    return '';
  }

  async function login() {
    const choice = prompt('Authenticate with admin token? Enter bearer token, or leave blank to use username/password:');
    if (choice === null) return false;
    if (choice !== '') {
      storage.setItem('admin_token', choice);
      storage.removeItem('admin_user');
      storage.removeItem('admin_pass');
    } else {
      const u = prompt('Username:');
      if (u === null) return false;
      const p = prompt('Password:');
      if (p === null) return false;
      storage.setItem('admin_user', u);
      storage.setItem('admin_pass', p);
      storage.removeItem('admin_token');
    }
    return true;
  }

  function logout() {
    storage.removeItem('admin_token');
    storage.removeItem('admin_user');
    storage.removeItem('admin_pass');
    location.reload();
  }

  // ---- api ------------------------------------------------------------------
  async function api(path, opts = {}) {
    opts.headers = opts.headers || {};
    const a = authHeader();
    if (a) opts.headers['Authorization'] = a;
    if (opts.body && typeof opts.body !== 'string') {
      opts.body = JSON.stringify(opts.body);
      opts.headers['Content-Type'] = 'application/json';
    }
    const resp = await fetch(ADMIN + path, opts);
    if (resp.status === 401) {
      if (await login()) {
        return api(path, opts);
      }
      throw new Error('unauthorized');
    }
    const text = await resp.text();
    let data = null;
    if (text) {
      try { data = JSON.parse(text); } catch (e) { data = text; }
    }
    if (!resp.ok) {
      const msg = (data && data.error && data.error.message) || ('HTTP ' + resp.status);
      const code = (data && data.error && data.error.code) || '';
      const err = new Error(msg);
      err.code = code;
      err.status = resp.status;
      throw err;
    }
    return data;
  }

  // ---- toast ----------------------------------------------------------------
  let toastTimer = null;
  function toast(msg, kind = 'ok') {
    const el = document.getElementById('toast');
    el.className = 'toast ' + kind;
    el.textContent = msg;
    el.classList.remove('hidden');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.classList.add('hidden'), 3500);
  }

  // ---- modal ----------------------------------------------------------------
  function openModal(title, bodyHTML, onSubmit) {
    const m = document.getElementById('modal');
    document.getElementById('modal-title').textContent = title;
    document.getElementById('modal-body').innerHTML = bodyHTML;
    m.classList.remove('hidden');
    const submit = document.getElementById('modal-submit');
    const cancel = document.getElementById('modal-cancel');
    const close = document.getElementById('modal-close');
    const cleanup = () => m.classList.add('hidden');
    submit.onclick = async () => {
      try {
        if (await onSubmit(document.getElementById('modal-body'))) cleanup();
      } catch (e) {
        toast(e.message, 'error');
      }
    };
    cancel.onclick = cleanup;
    close.onclick = cleanup;
  }

  // ---- formatting ----------------------------------------------------------
  function fmt(n) {
    if (n === null || n === undefined) return '-';
    if (typeof n !== 'number') return n;
    if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
    if (n >= 1000) return (n / 1000).toFixed(1) + 'k';
    return String(n);
  }
  function ts(s) {
    if (!s) return '';
    return new Date(s).toLocaleString();
  }
  function escapeHTML(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    }[c]));
  }

  // ---- navigation -----------------------------------------------------------
  function showPage(name) {
    document.querySelectorAll('.page').forEach(p => p.classList.toggle('active', p.id === 'page-' + name));
    document.querySelectorAll('.topbar nav a').forEach(a => a.classList.toggle('active', a.dataset.page === name));
    loaders[name]();
  }
  document.querySelectorAll('.topbar nav a').forEach(a => {
    a.addEventListener('click', e => { e.preventDefault(); showPage(a.dataset.page); });
  });
  document.getElementById('logout').addEventListener('click', logout);

  // ---- loaders --------------------------------------------------------------
  const loaders = {
    overview: loadOverview,
    models: loadModels,
    backends: loadBackends,
    keys: loadKeys,
    logs: loadLogs,
    analytics: loadAnalytics,
    audit: loadAudit,
    users: loadUsers,
    settings: loadSettings,
  };

  async function loadMe() {
    try {
      const me = await api('/me');
      if (me.authenticated) {
        document.getElementById('who').textContent = me.username + ' · ' + me.role;
      }
    } catch (e) { /* ignore */ }
  }

  async function loadOverview() {
    const s = await api('/stats/overview');
    const cards = [
      { l: 'Backends', v: s.backends_total, k: '' },
      { l: 'Healthy', v: s.backends_healthy, k: s.backends_healthy === s.backends_total ? 'ok' : 'warn' },
      { l: 'API keys', v: s.api_keys_total, k: '' },
      { l: 'Models', v: s.models_total, k: '' },
      { l: 'Active', v: s.active_requests, k: '' },
      { l: 'Total req', v: fmt(s.total_requests), k: '' },
      { l: 'Errors', v: fmt(s.total_errors), k: s.total_errors > 0 ? 'warn' : 'ok' },
    ];
    document.getElementById('overview-cards').innerHTML = cards.map(c =>
      `<div class="card ${c.k}"><div class="l">${escapeHTML(c.l)}</div><div class="v">${escapeHTML(String(c.v))}</div></div>`).join('');
    const rows = (s.backends || []).map(b => `<tr>
      <td>${escapeHTML(b.id)}</td>
      <td><span class="badge ${escapeHTML(b.status)}">${escapeHTML(b.status)}</span></td>
      <td>${b.active_requests}</td>
      <td>${b.total_requests}</td>
      <td>${b.total_errors}</td>
      <td>${b.last_latency_ms}ms</td>
    </tr>`).join('');
    document.querySelector('#overview-backends tbody').innerHTML = rows || '<tr><td colspan="6" class="small">No backends configured</td></tr>';
  }

  async function loadModels() {
    const m = await api('/models');
    const rows = (m.data || []).map(x => `<tr>
      <td class="mono">${escapeHTML(x.name)}</td>
      <td>${escapeHTML(x.type || 'chat')}</td>
      <td>${x.enabled ? '<span class="badge enabled">on</span>' : '<span class="badge disabled">off</span>'}</td>
      <td>${escapeHTML(x.capability_mode || 'passthrough')}</td>
      <td>${escapeHTML(x.routing_policy || '')}</td>
      <td><button class="ghost" data-delete="${escapeHTML(x.name)}">Delete</button></td>
    </tr>`).join('');
    document.querySelector('#models-table tbody').innerHTML = rows;
    document.querySelectorAll('#models-table [data-delete]').forEach(btn => {
      btn.onclick = async () => {
        if (!confirm('Delete model ' + btn.dataset.delete + '?')) return;
        await api('/models/' + encodeURIComponent(btn.dataset.delete), { method: 'DELETE' });
        toast('Deleted');
        loadModels();
      };
    });
    const a = await api('/model-aliases');
    const arows = (a.data || []).map(x => `<tr>
      <td class="mono">${escapeHTML(x.alias)}</td>
      <td class="mono">${escapeHTML(x.internal_model)}</td>
      <td>${escapeHTML(x.forwarding_mode)}</td>
      <td>${x.enabled ? '<span class="badge enabled">on</span>' : '<span class="badge disabled">off</span>'}</td>
      <td><button class="ghost" data-delete="${escapeHTML(x.alias)}">Delete</button></td>
    </tr>`).join('');
    document.querySelector('#aliases-table tbody').innerHTML = arows;
    document.querySelectorAll('#aliases-table [data-delete]').forEach(btn => {
      btn.onclick = async () => {
        if (!confirm('Delete alias ' + btn.dataset.delete + '?')) return;
        await api('/model-aliases/' + encodeURIComponent(btn.dataset.delete), { method: 'DELETE' });
        toast('Deleted');
        loadModels();
      };
    });
  }

  document.getElementById('model-add').onclick = () => {
    openModal('Add model', `
      <div class="form-row"><label>Name</label><input name="name" placeholder="llama-3.1-70b"></div>
      <div class="form-row"><label>Type</label><input name="type" value="chat"></div>
      <div class="form-row"><label>Capability mode</label>
        <select name="capability_mode"><option value="passthrough">passthrough</option><option value="declared">declared</option><option value="strict">strict</option></select></div>
      <div class="form-row"><label>Routing policy</label>
        <select name="routing_policy"><option value="">(default)</option><option>weighted_round_robin</option><option>round_robin</option><option>least_connections</option><option>least_latency</option><option>random</option><option>hash</option><option>sticky</option></select></div>
      <div class="form-row"><label>Context length</label><input name="context_length" type="number" value="0"></div>
    `, async (body) => {
      const f = (n) => body.querySelector(`[name="${n}"]`).value;
      const data = {
        name: f('name'), type: f('type') || 'chat',
        capability_mode: f('capability_mode'), routing_policy: f('routing_policy'),
        context_length: parseInt(f('context_length') || '0', 10),
        enabled: true,
      };
      if (!data.name) throw new Error('name required');
      await api('/models', { method: 'POST', body: data });
      toast('Saved');
      loadModels();
      return true;
    });
  };

  document.getElementById('alias-add').onclick = () => {
    openModal('Add alias', `
      <div class="form-row"><label>Alias</label><input name="alias"></div>
      <div class="form-row"><label>Internal model</label><input name="internal_model"></div>
      <div class="form-row"><label>Forwarding mode</label>
        <select name="forwarding_mode"><option value="use_internal">use_internal</option><option value="keep_external">keep_external</option></select></div>
    `, async (body) => {
      const f = (n) => body.querySelector(`[name="${n}"]`).value;
      const data = { alias: f('alias'), internal_model: f('internal_model'), forwarding_mode: f('forwarding_mode'), enabled: true };
      if (!data.alias || !data.internal_model) throw new Error('alias and internal_model are required');
      await api('/model-aliases', { method: 'POST', body: data });
      toast('Saved');
      loadModels();
      return true;
    });
  };

  async function loadBackends() {
    const m = await api('/backends');
    const rows = (m.data || []).map(b => `<tr>
      <td class="mono">${escapeHTML(b.id)}</td>
      <td>${escapeHTML(b.name || '')}</td>
      <td class="mono small">${escapeHTML(b.base_url)}</td>
      <td class="small mono">${(b.models || []).map(escapeHTML).join(', ')}</td>
      <td><span class="badge ${escapeHTML(b.status)}">${escapeHTML(b.status)}</span></td>
      <td>${b.enabled ? '<span class="badge enabled">on</span>' : '<span class="badge disabled">off</span>'}</td>
      <td>${b.weight}</td>
      <td>${b.active_requests}/${b.max_concurrent_requests || '∞'}</td>
      <td>${b.total_requests}</td>
      <td>${b.total_errors}</td>
      <td class="actions-cell">
        ${b.enabled ? `<button class="ghost" data-disable="${escapeHTML(b.id)}">Disable</button>` : `<button class="ghost" data-enable="${escapeHTML(b.id)}">Enable</button>`}
        <button class="ghost" data-check="${escapeHTML(b.id)}">Check</button>
        <button class="danger" data-delete="${escapeHTML(b.id)}">Delete</button>
      </td>
    </tr>`).join('');
    document.querySelector('#backends-table tbody').innerHTML = rows;
    document.querySelectorAll('#backends-table [data-disable]').forEach(b => b.onclick = async () => { await api('/backends/' + encodeURIComponent(b.dataset.disable) + '/disable', { method: 'POST' }); toast('Disabled'); loadBackends(); });
    document.querySelectorAll('#backends-table [data-enable]').forEach(b => b.onclick = async () => { await api('/backends/' + encodeURIComponent(b.dataset.enable) + '/enable', { method: 'POST' }); toast('Enabled'); loadBackends(); });
    document.querySelectorAll('#backends-table [data-check]').forEach(b => b.onclick = async () => { await api('/backends/' + encodeURIComponent(b.dataset.check) + '/health-check', { method: 'POST' }); toast('Health check triggered'); loadBackends(); });
    document.querySelectorAll('#backends-table [data-delete]').forEach(b => b.onclick = async () => { if (!confirm('Delete ' + b.dataset.delete + '?')) return; await api('/backends/' + encodeURIComponent(b.dataset.delete), { method: 'DELETE' }); toast('Deleted'); loadBackends(); });
  }

  document.getElementById('backend-add').onclick = () => {
    openModal('Add backend', `
      <div class="form-row"><label>ID</label><input name="id" placeholder="gpu-04"></div>
      <div class="form-row"><label>Name</label><input name="name"></div>
      <div class="form-row"><label>Base URL</label><input name="base_url" placeholder="http://10.0.0.4:8000/v1"></div>
      <div class="form-row"><label>Backend API key (optional)</label><input name="api_key" type="password"></div>
      <div class="form-row"><label>Models (comma-separated)</label><input name="models"></div>
      <div class="form-row"><label>Weight</label><input name="weight" type="number" value="1"></div>
      <div class="form-row"><label>Max concurrent</label><input name="max_concurrent_requests" type="number" value="32"></div>
      <div class="form-row"><label>Timeout (ms)</label><input name="timeout_ms" type="number" value="120000"></div>
    `, async (body) => {
      const f = (n) => body.querySelector(`[name="${n}"]`).value;
      const data = {
        id: f('id'), name: f('name'), base_url: f('base_url'), api_key: f('api_key'),
        models: f('models').split(',').map(s => s.trim()).filter(Boolean),
        weight: parseInt(f('weight') || '1', 10),
        max_concurrent_requests: parseInt(f('max_concurrent_requests') || '32', 10),
        timeout_ms: parseInt(f('timeout_ms') || '120000', 10),
        enabled: true,
      };
      if (!data.id || !data.base_url || data.models.length === 0) throw new Error('id, base_url and models are required');
      await api('/backends', { method: 'POST', body: data });
      toast('Saved');
      loadBackends();
      return true;
    });
  };

  async function loadKeys() {
    const m = await api('/api-keys');
    const rows = (m.data || []).map(k => `<tr>
      <td class="mono">${escapeHTML(k.id)}</td>
      <td>${escapeHTML(k.name || '')}</td>
      <td class="mono small">${escapeHTML(k.key_prefix || '')}</td>
      <td>${k.enabled ? '<span class="badge enabled">on</span>' : '<span class="badge disabled">off</span>'}</td>
      <td class="small mono">${(k.allowed_models || []).map(escapeHTML).join(', ') || '*'}</td>
      <td>${(k.rate_limit && k.rate_limit.requests_per_minute) || '-'}</td>
      <td>${(k.rate_limit && k.rate_limit.concurrent_requests) || '-'}</td>
      <td>${k.delay_ms || 0}ms</td>
      <td>${fmt(k.total_requests)}</td>
      <td>${fmt(k.total_tokens)}</td>
      <td class="actions-cell">
        <button class="ghost" data-usage="${escapeHTML(k.id)}">Usage</button>
        <button class="danger" data-delete="${escapeHTML(k.id)}">Delete</button>
      </td>
    </tr>`).join('');
    document.querySelector('#keys-table tbody').innerHTML = rows;
    document.querySelectorAll('#keys-table [data-delete]').forEach(b => b.onclick = async () => {
      if (!confirm('Delete API key ' + b.dataset.delete + '?')) return;
      await api('/api-keys/' + encodeURIComponent(b.dataset.delete), { method: 'DELETE' });
      toast('Deleted');
      loadKeys();
    });
    document.querySelectorAll('#keys-table [data-usage]').forEach(b => b.onclick = async () => {
      const u = await api('/api-keys/' + encodeURIComponent(b.dataset.usage) + '/usage');
      alert('Today: ' + u.day_requests + ' reqs / ' + u.day_tokens + ' tokens\nThis month: ' + u.month_requests + ' reqs / ' + u.month_tokens + ' tokens');
    });
  }

  document.getElementById('key-add').onclick = () => {
    openModal('Create API key', `
      <div class="form-row"><label>ID (optional)</label><input name="id"></div>
      <div class="form-row"><label>Name</label><input name="name" placeholder="Team A"></div>
      <div class="form-row"><label>Key (will be hashed and shown once)</label><input name="key" placeholder="sk-prod-..."></div>
      <div class="form-row"><label>Allowed models (comma-separated, supports wildcards)</label><input name="allowed_models" value="*"></div>
      <div class="form-row"><label>Denied models</label><input name="denied_models"></div>
      <div class="form-row"><label>Requests / min</label><input name="rpm" type="number" value="600"></div>
      <div class="form-row"><label>Concurrent limit</label><input name="conc" type="number" value="20"></div>
      <div class="form-row"><label>Tokens / min</label><input name="tpm" type="number" value="0"></div>
      <div class="form-row"><label>Daily request quota</label><input name="day_req" type="number" value="0"></div>
      <div class="form-row"><label>Daily token quota</label><input name="day_tok" type="number" value="0"></div>
      <div class="form-row"><label>Delay (ms)</label><input name="delay_ms" type="number" value="0"></div>
    `, async (body) => {
      const f = (n) => body.querySelector(`[name="${n}"]`).value;
      const data = {
        id: f('id'), name: f('name'), key: f('key'),
        allowed_models: f('allowed_models').split(',').map(s => s.trim()).filter(Boolean),
        denied_models: f('denied_models').split(',').map(s => s.trim()).filter(Boolean),
        rate_limit: {
          enabled: true,
          requests_per_minute: parseInt(f('rpm') || '0', 10),
          concurrent_requests: parseInt(f('conc') || '0', 10),
          tokens_per_minute: parseInt(f('tpm') || '0', 10),
        },
        quota: {
          daily_requests: parseInt(f('day_req') || '0', 10),
          daily_tokens: parseInt(f('day_tok') || '0', 10),
        },
        delay_ms: parseInt(f('delay_ms') || '0', 10),
        enabled: true,
      };
      if (!data.key) throw new Error('key required');
      const out = await api('/api-keys', { method: 'POST', body: data });
      toast('Saved');
      alert('Key created. Save this value now - it will not be shown again:\n\n' + out.key);
      loadKeys();
      return true;
    });
  };

  async function loadLogs(extra) {
    const form = document.getElementById('logs-form');
    const params = new URLSearchParams();
    for (const el of form.elements) {
      if (el.name && el.value) params.set(el.name, el.value);
    }
    if (extra) for (const k in extra) params.set(k, extra[k]);
    params.set('limit', '200');
    const m = await api('/logs?' + params.toString());
    const rows = (m.data || []).map(l => `<tr>
      <td class="small mono">${escapeHTML(ts(l.created_at))}</td>
      <td class="mono small">${escapeHTML(l.endpoint || '')}</td>
      <td class="mono">${escapeHTML(l.model || '')}</td>
      <td class="mono small">${escapeHTML(l.backend_id || '')}</td>
      <td class="mono small">${escapeHTML(l.api_key_id || '')}</td>
      <td>${statusBadge(l.status_code)}</td>
      <td>${l.stream ? '<span class="badge stream">stream</span>' : ''}</td>
      <td>${fmt(l.total_tokens)}</td>
      <td>${l.latency_ms}ms</td>
      <td>${l.ttft_ms || 0}ms</td>
      <td class="mono small">${escapeHTML(l.request_id || '')}</td>
    </tr>`).join('');
    document.querySelector('#logs-table tbody').innerHTML = rows || '<tr><td colspan="11" class="small">No logs match.</td></tr>';
  }
  function statusBadge(c) {
    if (!c) return '<span class="badge unknown">?</span>';
    if (c >= 200 && c < 300) return `<span class="badge ok">${c}</span>`;
    return `<span class="badge error">${c}</span>`;
  }
  document.getElementById('logs-form').onsubmit = (e) => { e.preventDefault(); loadLogs(); };

  async function loadAnalytics() {
    const since = new Date(Date.now() - 24 * 3600 * 1000).toISOString();
    const stats = await api('/stats/range?since=' + encodeURIComponent(since));
    document.getElementById('analytics-cards').innerHTML = [
      { l: 'Requests (24h)', v: fmt(stats.total_requests || 0) },
      { l: 'Success', v: fmt(stats.success_total || 0), k: 'ok' },
      { l: 'Errors', v: fmt(stats.error_total || 0), k: (stats.error_total || 0) > 0 ? 'warn' : 'ok' },
      { l: 'Prompt tokens', v: fmt(stats.prompt_tokens || 0) },
      { l: 'Completion tokens', v: fmt(stats.completion_tokens || 0) },
      { l: 'Total tokens', v: fmt(stats.total_tokens || 0) },
    ].map(c => `<div class="card ${c.k || ''}"><div class="l">${escapeHTML(c.l)}</div><div class="v">${escapeHTML(String(c.v))}</div></div>`).join('');
    renderTop('#analytics-models tbody', stats.by_model || {});
    renderTop('#analytics-backends tbody', stats.by_backend || {});
    renderTop('#analytics-keys tbody', stats.by_api_key || {});
  }
  function renderTop(sel, data) {
    const rows = Object.entries(data || {})
      .sort((a, b) => (b[1].requests || 0) - (a[1].requests || 0))
      .slice(0, 20)
      .map(([k, v]) => `<tr><td class="mono">${escapeHTML(k || '(anonymous)')}</td><td>${v.requests || 0}</td><td>${v.errors || 0}</td><td>${fmt(v.tokens || 0)}</td></tr>`)
      .join('');
    document.querySelector(sel).innerHTML = rows;
  }

  async function loadAudit() {
    const m = await api('/audit');
    const rows = (m.data || []).map(e => `<tr>
      <td class="small mono">${escapeHTML(ts(e.created_at))}</td>
      <td>${escapeHTML(e.admin_user || '')}</td>
      <td class="mono">${escapeHTML(e.action || '')}</td>
      <td class="mono small">${escapeHTML(e.target_type)}:${escapeHTML(e.target_id || '')}</td>
      <td class="small mono">${escapeHTML(e.ip || '')}</td>
    </tr>`).join('');
    document.querySelector('#audit-table tbody').innerHTML = rows || '<tr><td colspan="5" class="small">No audit entries.</td></tr>';
  }

  async function loadUsers() {
    try {
      const m = await api('/users');
      const rows = (m.data || []).map(u => `<tr>
        <td class="mono">${escapeHTML(u.username)}</td>
        <td>${escapeHTML(u.email || '')}</td>
        <td>${escapeHTML(u.role)}</td>
        <td><button class="danger" data-delete="${escapeHTML(u.username)}">Delete</button></td>
      </tr>`).join('');
      document.querySelector('#users-table tbody').innerHTML = rows || '<tr><td colspan="4" class="small">No users configured.</td></tr>';
      document.querySelectorAll('#users-table [data-delete]').forEach(b => b.onclick = async () => {
        if (!confirm('Delete user ' + b.dataset.delete + '?')) return;
        await api('/users/' + encodeURIComponent(b.dataset.delete), { method: 'DELETE' });
        toast('Deleted');
        loadUsers();
      });
    } catch (e) {
      document.querySelector('#users-table tbody').innerHTML = `<tr><td colspan="4" class="small">${escapeHTML(e.message)}</td></tr>`;
    }
  }

  document.getElementById('user-add').onclick = () => {
    openModal('Add admin user', `
      <div class="form-row"><label>Username</label><input name="username"></div>
      <div class="form-row"><label>Password</label><input name="password" type="password"></div>
      <div class="form-row"><label>Email</label><input name="email" type="email"></div>
      <div class="form-row"><label>Role</label>
        <select name="role"><option>super_admin</option><option>admin</option><option>operator</option><option>viewer</option><option>auditor</option></select></div>
    `, async (body) => {
      const f = (n) => body.querySelector(`[name="${n}"]`).value;
      const data = { username: f('username'), password: f('password'), email: f('email'), role: f('role') };
      if (!data.username || !data.password) throw new Error('username and password required');
      await api('/users', { method: 'POST', body: data });
      toast('Saved');
      loadUsers();
      return true;
    });
  };

  async function loadSettings() {
    const cfg = await api('/settings');
    const counts = cfg.counts || {};
    document.getElementById('settings-info').innerHTML = [
      { l: 'Backends', v: counts.backends || 0 },
      { l: 'Models', v: counts.models || 0 },
      { l: 'Aliases', v: counts.model_aliases || 0 },
      { l: 'API keys', v: counts.api_keys || 0 },
      { l: 'Admin users', v: counts.admin_users || 0 },
    ].map(c => `<div class="card"><div class="l">${escapeHTML(c.l)}</div><div class="v">${escapeHTML(String(c.v))}</div></div>`).join('');

    const flatten = (obj, prefix = '') => {
      const rows = [];
      for (const k of Object.keys(obj || {})) {
        const v = obj[k];
        const key = prefix ? prefix + '.' + k : k;
        if (v && typeof v === 'object' && !Array.isArray(v)) {
          rows.push(...flatten(v, key));
        } else {
          rows.push([key, Array.isArray(v) ? v.join(', ') : String(v == null ? '' : v)]);
        }
      }
      return rows;
    };
    const renderRows = (selector, obj) => {
      const rows = flatten(obj).map(([k, v]) => `<tr><td class="mono small">${escapeHTML(k)}</td><td>${escapeHTML(v)}</td></tr>`).join('');
      document.querySelector(selector).innerHTML = rows;
    };
    renderRows('#settings-routing tbody', { routing: cfg.routing, health_check: cfg.health_check, auth: cfg.auth });
    renderRows('#settings-limits tbody', { rate_limit: cfg.rate_limit });
    renderRows('#settings-storage tbody', { storage: cfg.storage, logging: cfg.logging, metrics: cfg.metrics });
    renderRows('#settings-misc tbody', { queue: cfg.queue, tracing: cfg.tracing, dashboard: cfg.dashboard });
  }

  // ---- boot ----------------------------------------------------------------
  (async function () {
    await loadMe();
    try {
      await loadOverview();
    } catch (e) {
      toast(e.message, 'error');
    }
  })();
})();
