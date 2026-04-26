// URL 分发面板：实时展示每个上游 URL 的轮询命中数 / 延迟 / 成功率 / 闲置状态
// 数据源：GET /admin/url-distribution（聚合内存中的 URLSelector + URLSmoothWeightedRR 状态）
const t = window.t;

let autoRefreshTimer = null;
const AUTO_REFRESH_MS = 5000;

// 把毫秒翻成可读时长，与 affinity.js 保持一致
function humanizeMs(ms) {
  if (ms === null || ms === undefined || !Number.isFinite(ms)) return '-';
  if (ms < 0) return t('urlDist.never');
  let s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600);
  s = s % 3600;
  const m = Math.floor(s / 60);
  s = s % 60;
  if (h > 0) return window.t('common.timeHM', { h, m });
  if (m > 0) return window.t('common.timeMS', { m, s });
  return window.t('common.timeS', { s });
}

function escapeHtml(str) {
  if (str === null || str === undefined) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

// 把 URL 截短显示（保留 host 和最后一段 path），完整 URL 放在 title 里
function shortenURL(url) {
  if (!url) return '';
  try {
    const u = new URL(url);
    return u.host + u.pathname;
  } catch (_) {
    return url;
  }
}

function formatLatency(ms) {
  if (ms === null || ms === undefined || ms < 0 || !Number.isFinite(ms)) return '-';
  if (ms < 1000) return Math.round(ms) + ' ms';
  return (ms / 1000).toFixed(2) + ' s';
}

function formatSuccessRate(rate) {
  if (rate === null || rate === undefined || !Number.isFinite(rate)) return '-';
  return (rate * 100).toFixed(1) + '%';
}

// 状态徽章：冷却 > 慢隔离 > 闲置/活跃
function statusBadge(entry, idleThresholdMs) {
  if (entry.cooled_down) {
    return `<span class="urldist-status is-cooled">${escapeHtml(t('urlDist.statusCooled'))}</span>`;
  }
  if (entry.slow_isolated) {
    return `<span class="urldist-status is-slow">${escapeHtml(t('urlDist.statusSlow'))}</span>`;
  }
  if (entry.idle_ms < 0) {
    return `<span class="urldist-status is-idle">${escapeHtml(t('urlDist.statusUnused'))}</span>`;
  }
  if (entry.idle_ms > idleThresholdMs) {
    return `<span class="urldist-status is-idle">${escapeHtml(t('urlDist.statusIdle'))}</span>`;
  }
  return `<span class="urldist-status is-active">${escapeHtml(t('urlDist.statusActive'))}</span>`;
}

// 柱状条：宽度 = 该 URL 选次数 / 全局最大选次数
// 颜色：>20% 总流量为"扎堆"红色，<5% 总流量为"低利用"灰色，中间正常蓝色
function renderBar(selections, maxSelections, totalSelections) {
  if (maxSelections <= 0) {
    return `<div class="urldist-bar"><div class="urldist-bar-label"><span>0</span></div></div>`;
  }
  const widthPct = Math.max(2, (selections / maxSelections) * 100);
  let cls = '';
  if (totalSelections > 0) {
    const trafficShare = selections / totalSelections;
    if (trafficShare > 0.2) cls = 'is-hot';
    else if (trafficShare < 0.005) cls = 'is-cold';
  } else if (selections === 0) {
    cls = 'is-cold';
  }
  const sharePct = totalSelections > 0 ? ((selections / totalSelections) * 100).toFixed(1) : '0.0';
  return `<div class="urldist-bar">
    <div class="urldist-bar-fill ${cls}" style="width: ${widthPct.toFixed(1)}%"></div>
    <div class="urldist-bar-label"><span>${selections}</span><span>${sharePct}%</span></div>
  </div>`;
}

function channelLabel(entry) {
  const name = entry.channel_name || '';
  if (name) {
    return `${escapeHtml(name)} <span class="muted">#${entry.channel_id}</span>`;
  }
  return `<span class="urldist-cell-mono">#${entry.channel_id}</span>`;
}

async function loadDistribution() {
  const tbody = document.getElementById('urldist-tbody');
  if (!tbody) return;

  try {
    const data = await window.fetchDataWithAuth('/admin/url-distribution');
    if (!data) {
      tbody.innerHTML = `<tr><td colspan="8" class="urldist-empty">${escapeHtml(t('urlDist.loadFailed'))}</td></tr>`;
      return;
    }
    updateSummary(data);

    const entries = Array.isArray(data.entries) ? data.entries : [];
    if (entries.length === 0) {
      tbody.innerHTML = `<tr><td colspan="8" class="urldist-empty">${escapeHtml(t('urlDist.empty'))}</td></tr>`;
      return;
    }

    // 计算全局 max selections（用于柱状条相对宽度）
    let maxSelections = 0;
    for (const e of entries) {
      if (e.selections > maxSelections) maxSelections = e.selections;
    }
    const totalSelections = data.total_selections || 0;
    const idleThresholdMs = data.idle_threshold_ms || 5 * 60 * 1000;

    const rows = entries.map(e => {
      const lastUsed = e.idle_ms < 0
        ? t('urlDist.never')
        : t('urlDist.idleAgo', { age: humanizeMs(e.idle_ms) });
      return `<tr>
        <td>${channelLabel(e)}</td>
        <td class="urldist-cell-mono" title="${escapeHtml(e.url)}">${escapeHtml(shortenURL(e.url))}</td>
        <td class="urldist-bar-cell">${renderBar(e.selections, maxSelections, totalSelections)}</td>
        <td class="urldist-cell-num">${escapeHtml(formatSuccessRate(e.success_rate))}</td>
        <td class="urldist-cell-num">${escapeHtml(formatLatency(e.ttfb_latency_ms))}</td>
        <td class="urldist-cell-num">${escapeHtml(String(e.current_weight))}</td>
        <td class="urldist-cell-num">${escapeHtml(lastUsed)}</td>
        <td>${statusBadge(e, idleThresholdMs)}</td>
      </tr>`;
    }).join('');
    tbody.innerHTML = rows;
  } catch (e) {
    console.error('load url distribution failed:', e);
    tbody.innerHTML = `<tr><td colspan="8" class="urldist-empty">${escapeHtml(t('urlDist.loadFailed'))}</td></tr>`;
  }
}

function updateSummary(data) {
  const setText = (id, val) => {
    const el = document.getElementById(id);
    if (el) el.textContent = val;
  };
  setText('summary-total-selections', String(data.total_selections || 0));
  setText('summary-total-urls', String(data.total_urls || 0));
  setText('summary-active-urls', String(data.active_urls || 0));
  setText('summary-idle-urls', String(data.idle_urls || 0));
}

function updateTimestamp() {
  const el = document.getElementById('urldist-updated');
  if (!el) return;
  const now = new Date();
  const hh = String(now.getHours()).padStart(2, '0');
  const mm = String(now.getMinutes()).padStart(2, '0');
  const ss = String(now.getSeconds()).padStart(2, '0');
  el.textContent = t('urlDist.updatedAt', { time: `${hh}:${mm}:${ss}` });
}

async function reloadAll() {
  await loadDistribution();
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
  const refreshBtn = document.getElementById('urldist-refresh-btn');
  if (refreshBtn && !refreshBtn.dataset.bound) {
    refreshBtn.addEventListener('click', () => { void reloadAll(); });
    refreshBtn.dataset.bound = '1';
  }
  const autoBox = document.getElementById('urldist-autorefresh');
  if (autoBox && !autoBox.dataset.bound) {
    autoBox.addEventListener('change', e => toggleAutoRefresh(e.target.checked));
    autoBox.dataset.bound = '1';
  }
}

window.initPageBootstrap({
  topbarKey: 'url-distribution',
  run: () => {
    bindControls();
    void reloadAll();
    if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
      window.i18n.onLocaleChange(() => { void reloadAll(); });
    }
    window.addEventListener('ccload:bfcache-restore', () => { void reloadAll(); });
  },
});
