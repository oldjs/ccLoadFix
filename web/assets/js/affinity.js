// 路由亲和状态页：渠道亲和 / URL 亲和 / URL warm
const t = window.t;

// 渠道 ID → 名称 映射，渲染时把数字 ID 翻成可读名
const channelNameMap = new Map();

let autoRefreshTimer = null;
const AUTO_REFRESH_MS = 5000;

// 把毫秒翻成可读时长，复用全局 i18n 模板
function humanizeMs(ms) {
  if (ms === null || ms === undefined || !Number.isFinite(ms)) return '-';
  if (ms < 0) ms = 0;
  let s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600);
  s = s % 3600;
  const m = Math.floor(s / 60);
  s = s % 60;
  if (h > 0) return window.t('common.timeHM', { h, m });
  if (m > 0) return window.t('common.timeMS', { m, s });
  return window.t('common.timeS', { s });
}

// 简易 HTML 转义，避免上游 URL/模型名里塞特殊字符把页面打花
function escapeHtml(str) {
  if (str === null || str === undefined) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

// 渠道展示：有名字就 "name #id"，没有就 #id
function channelLabel(id) {
  const name = channelNameMap.get(Number(id));
  if (name) return `${escapeHtml(name)} <span class="muted">#${id}</span>`;
  return `<span class="affinity-cell-mono">#${id}</span>`;
}

// 拉一次 /admin/channels，把 ID→名映射缓存起来
async function refreshChannelMap() {
  try {
    const data = await window.fetchDataWithAuth('/admin/channels');
    channelNameMap.clear();
    if (Array.isArray(data)) {
      for (const c of data) {
        if (c && c.id !== undefined) channelNameMap.set(Number(c.id), c.name || '');
      }
    }
  } catch (e) {
    // 渠道映射拉不到不致命，继续展示纯 ID
    console.warn('failed to load channels map:', e);
  }
}

function setEmptyRow(tbody, colspan, key) {
  tbody.innerHTML = `<tr><td colspan="${colspan}" class="affinity-empty">${escapeHtml(t(key))}</td></tr>`;
}

// ============================================================
// 渠道亲和
// ============================================================

async function renderChannelAffinity() {
  const tbody = document.getElementById('channel-affinity-tbody');
  if (!tbody) return;
  try {
    const list = await window.fetchDataWithAuth('/admin/channel-affinity');
    if (!Array.isArray(list) || list.length === 0) {
      setEmptyRow(tbody, 4, 'affinity.emptyChannel');
      return;
    }
    // 按模型字典序，方便人眼扫
    list.sort((a, b) => String(a.model).localeCompare(String(b.model)));
    const rows = list.map(item => {
      return `<tr>
        <td class="affinity-cell-mono">${escapeHtml(item.model)}</td>
        <td>${channelLabel(item.channel_id)}</td>
        <td>${escapeHtml(humanizeMs(item.age_ms))}</td>
        <td>${escapeHtml(humanizeMs(item.ttl_remain_ms))}</td>
      </tr>`;
    }).join('');
    tbody.innerHTML = rows;
  } catch (e) {
    console.error('load channel affinity failed:', e);
    setEmptyRow(tbody, 4, 'affinity.loadFailed');
  }
}

async function clearChannelAffinity() {
  if (!window.confirm(t('affinity.confirmClearChannel'))) return;
  try {
    await window.fetchDataWithAuth('/admin/channel-affinity', { method: 'DELETE' });
    await renderChannelAffinity();
    if (window.showInfo) window.showInfo(t('affinity.clearedChannel'));
  } catch (e) {
    console.error('clear channel affinity failed:', e);
    if (window.showError) window.showError(t('affinity.clearFailed'));
  }
}

// ============================================================
// URL 亲和
// ============================================================

async function renderURLAffinity() {
  const tbody = document.getElementById('url-affinity-tbody');
  if (!tbody) return;
  try {
    const list = await window.fetchDataWithAuth('/admin/url-affinity');
    if (!Array.isArray(list) || list.length === 0) {
      setEmptyRow(tbody, 4, 'affinity.emptyURL');
      return;
    }
    // 先按 channel_id，再按 model 排
    list.sort((a, b) => {
      const c = Number(a.channel_id) - Number(b.channel_id);
      if (c !== 0) return c;
      return String(a.model).localeCompare(String(b.model));
    });
    const rows = list.map(item => {
      return `<tr>
        <td>${channelLabel(item.channel_id)}</td>
        <td class="affinity-cell-mono">${escapeHtml(item.model)}</td>
        <td class="affinity-cell-mono">${escapeHtml(item.url)}</td>
        <td>${escapeHtml(humanizeMs(item.age_ms))}</td>
      </tr>`;
    }).join('');
    tbody.innerHTML = rows;
  } catch (e) {
    console.error('load url affinity failed:', e);
    setEmptyRow(tbody, 4, 'affinity.loadFailed');
  }
}

// ============================================================
// URL Warm
// ============================================================

async function renderURLWarm() {
  const tbody = document.getElementById('url-warm-tbody');
  if (!tbody) return;
  try {
    const list = await window.fetchDataWithAuth('/admin/url-warm');
    if (!Array.isArray(list) || list.length === 0) {
      setEmptyRow(tbody, 3, 'affinity.emptyWarm');
      return;
    }
    list.sort((a, b) => {
      const c = Number(a.channel_id) - Number(b.channel_id);
      if (c !== 0) return c;
      return String(a.model).localeCompare(String(b.model));
    });
    const rows = list.map(item => {
      const slots = Array.isArray(item.slots) ? item.slots : [];
      const slotHtml = slots.map((slot, idx) => {
        const cls = idx === 0 ? 'warm-url-primary' : 'warm-url-secondary';
        const ageLabel = t('affinity.warmSlotMeta', {
          age: humanizeMs(slot.age_ms),
          ttl: humanizeMs(slot.ttl_remain_ms),
        });
        return `<li class="${cls}"><span>${escapeHtml(slot.url)}</span>` +
               `<span class="warm-url-meta">${escapeHtml(ageLabel)}</span></li>`;
      }).join('');
      return `<tr>
        <td>${channelLabel(item.channel_id)}</td>
        <td class="affinity-cell-mono">${escapeHtml(item.model)}</td>
        <td><ul class="warm-url-list">${slotHtml}</ul></td>
      </tr>`;
    }).join('');
    tbody.innerHTML = rows;
  } catch (e) {
    console.error('load url warm failed:', e);
    setEmptyRow(tbody, 3, 'affinity.loadFailed');
  }
}

// ============================================================
// 协调：刷新所有 + 自动刷新
// ============================================================

function updateTimestamp() {
  const el = document.getElementById('affinity-updated');
  if (!el) return;
  const now = new Date();
  const hh = String(now.getHours()).padStart(2, '0');
  const mm = String(now.getMinutes()).padStart(2, '0');
  const ss = String(now.getSeconds()).padStart(2, '0');
  el.textContent = t('affinity.updatedAt', { time: `${hh}:${mm}:${ss}` });
}

async function reloadAll() {
  await refreshChannelMap();
  await Promise.all([
    renderChannelAffinity(),
    renderURLAffinity(),
    renderURLWarm(),
  ]);
  updateTimestamp();
}

function toggleAutoRefresh(checked) {
  if (autoRefreshTimer) {
    clearInterval(autoRefreshTimer);
    autoRefreshTimer = null;
  }
  if (checked) {
    autoRefreshTimer = setInterval(() => { void reloadAll(); }, AUTO_REFRESH_MS);
  }
}

function bindControls() {
  const refreshBtn = document.getElementById('affinity-refresh-btn');
  if (refreshBtn && !refreshBtn.dataset.bound) {
    refreshBtn.addEventListener('click', () => { void reloadAll(); });
    refreshBtn.dataset.bound = '1';
  }
  const autoBox = document.getElementById('affinity-autorefresh');
  if (autoBox && !autoBox.dataset.bound) {
    autoBox.addEventListener('change', e => toggleAutoRefresh(e.target.checked));
    autoBox.dataset.bound = '1';
  }
  const clearBtn = document.getElementById('clear-channel-affinity-btn');
  if (clearBtn && !clearBtn.dataset.bound) {
    clearBtn.addEventListener('click', () => { void clearChannelAffinity(); });
    clearBtn.dataset.bound = '1';
  }
}

window.initPageBootstrap({
  topbarKey: 'affinity',
  run: () => {
    bindControls();
    void reloadAll();
    // 切语言时把已渲染的"已存活/剩余 TTL"重新本地化
    if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
      window.i18n.onLocaleChange(() => { void reloadAll(); });
    }
    // bfcache 恢复时重新拉一次，避免显示陈旧数据
    window.addEventListener('ccload:bfcache-restore', () => { void reloadAll(); });
  },
});
