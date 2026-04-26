// URL 分发面板：实时展示每个上游 URL 的轮询命中数 / 延迟 / 成功率 / 闲置状态
// 数据源：GET /admin/url-distribution（聚合内存中的 URLSelector + URLSmoothWeightedRR 状态）
const t = window.t;

let autoRefreshTimer = null;
const AUTO_REFRESH_MS = 5000;

// 客户端分页/搜索状态：4000+ URLs 全渲染会卡，分页解决
const pageState = {
  pageSize: 50,
  page: 1,
  search: '',
  // 缓存最近一次拉到的完整数据，分页/搜索不重新请求
  data: null,
};

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
// 颜色：>20% 总流量为"扎堆"红色，<0.5% 总流量为"低利用"灰色，中间正常蓝色
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

// 把搜索关键词在渠道名 / URL 里做不区分大小写匹配
function filterEntries(entries, kw) {
  if (!kw) return entries;
  const needle = kw.toLowerCase();
  return entries.filter(e =>
    (e.url && e.url.toLowerCase().includes(needle)) ||
    (e.channel_name && e.channel_name.toLowerCase().includes(needle)) ||
    String(e.channel_id) === needle
  );
}

// 渲染分页控件：[上一页] [1] [2] ... [N] [下一页]
function renderPagination(totalItems, currentPage, pageSize) {
  const container = document.getElementById('urldist-pagination');
  if (!container) return;
  const totalPages = Math.max(1, Math.ceil(totalItems / pageSize));
  if (currentPage > totalPages) currentPage = totalPages;
  pageState.page = currentPage;

  const start = totalItems === 0 ? 0 : (currentPage - 1) * pageSize + 1;
  const end = Math.min(currentPage * pageSize, totalItems);
  const info = t('urlDist.pageInfo', { start, end, total: totalItems });

  // 页码按钮：当前页前后各 2 个，加首尾，省略号填充
  const pages = computePageButtons(currentPage, totalPages);
  const buttons = pages.map(p => {
    if (p === '...') return `<span style="padding: 4px 6px;">…</span>`;
    const cls = p === currentPage ? 'is-current' : '';
    return `<button type="button" class="${cls}" data-page="${p}">${p}</button>`;
  }).join('');

  container.innerHTML = `
    <span class="urldist-page-info">${escapeHtml(info)}</span>
    <span class="urldist-page-buttons">
      <button type="button" data-page="${currentPage - 1}" ${currentPage <= 1 ? 'disabled' : ''}>${escapeHtml(t('urlDist.prev'))}</button>
      ${buttons}
      <button type="button" data-page="${currentPage + 1}" ${currentPage >= totalPages ? 'disabled' : ''}>${escapeHtml(t('urlDist.next'))}</button>
    </span>
  `;

  container.querySelectorAll('button[data-page]').forEach(btn => {
    btn.addEventListener('click', e => {
      const p = parseInt(e.currentTarget.getAttribute('data-page'), 10);
      if (!Number.isNaN(p) && p >= 1 && p <= totalPages) {
        pageState.page = p;
        renderTable();
      }
    });
  });
}

// 计算要显示哪些页码按钮，避免 1000 页时全部塞出来
function computePageButtons(current, total) {
  if (total <= 7) {
    return Array.from({ length: total }, (_, i) => i + 1);
  }
  const pages = [1];
  const left = Math.max(2, current - 2);
  const right = Math.min(total - 1, current + 2);
  if (left > 2) pages.push('...');
  for (let i = left; i <= right; i++) pages.push(i);
  if (right < total - 1) pages.push('...');
  pages.push(total);
  return pages;
}

// 渲染当前页表格
function renderTable() {
  const tbody = document.getElementById('urldist-tbody');
  if (!tbody) return;
  const data = pageState.data;
  if (!data) {
    tbody.innerHTML = `<tr><td colspan="8" class="urldist-empty">${escapeHtml(t('urlDist.loadFailed'))}</td></tr>`;
    return;
  }

  const allEntries = Array.isArray(data.entries) ? data.entries : [];
  const filtered = filterEntries(allEntries, pageState.search.trim());

  if (filtered.length === 0) {
    tbody.innerHTML = `<tr><td colspan="8" class="urldist-empty">${escapeHtml(pageState.search ? t('urlDist.noMatch') : t('urlDist.empty'))}</td></tr>`;
    renderPagination(0, 1, pageState.pageSize);
    return;
  }

  // 柱状图相对宽度按"全局"最大值，避免分页/筛选改变视觉对比基线
  let maxSelections = 0;
  for (const e of allEntries) {
    if (e.selections > maxSelections) maxSelections = e.selections;
  }
  const totalSelections = data.total_selections || 0;
  const idleThresholdMs = data.idle_threshold_ms || 5 * 60 * 1000;

  // 切当前页
  const start = (pageState.page - 1) * pageState.pageSize;
  const pageEntries = filtered.slice(start, start + pageState.pageSize);

  const rows = pageEntries.map(e => {
    const lastUsed = e.idle_ms < 0
      ? t('urlDist.never')
      : t('urlDist.idleAgo', { age: humanizeMs(e.idle_ms) });
    // 优先展示 configured_weight（直观）；老接口兼容到 current_weight
    const weight = (e.configured_weight !== undefined && e.configured_weight !== null)
      ? e.configured_weight
      : e.current_weight;
    return `<tr>
      <td>${channelLabel(e)}</td>
      <td class="urldist-cell-mono" title="${escapeHtml(e.url)}">${escapeHtml(shortenURL(e.url))}</td>
      <td class="urldist-bar-cell">${renderBar(e.selections, maxSelections, totalSelections)}</td>
      <td class="urldist-cell-num">${escapeHtml(formatSuccessRate(e.success_rate))}</td>
      <td class="urldist-cell-num">${escapeHtml(formatLatency(e.ttfb_latency_ms))}</td>
      <td class="urldist-cell-num">${escapeHtml(String(weight))}</td>
      <td class="urldist-cell-num">${escapeHtml(lastUsed)}</td>
      <td>${statusBadge(e, idleThresholdMs)}</td>
    </tr>`;
  }).join('');
  tbody.innerHTML = rows;

  renderPagination(filtered.length, pageState.page, pageState.pageSize);
}

async function loadDistribution() {
  const tbody = document.getElementById('urldist-tbody');
  if (!tbody) return;
  try {
    const data = await window.fetchDataWithAuth('/admin/url-distribution');
    pageState.data = data || { entries: [] };
    updateSummary(pageState.data);
    renderTable();
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

// 简单防抖，避免每个键盘按键都重排表
function debounce(fn, ms) {
  let timer = null;
  return function (...args) {
    if (timer) clearTimeout(timer);
    timer = setTimeout(() => fn.apply(this, args), ms);
  };
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
  const pageSizeSel = document.getElementById('urldist-pagesize-select');
  if (pageSizeSel && !pageSizeSel.dataset.bound) {
    pageSizeSel.addEventListener('change', e => {
      const v = parseInt(e.target.value, 10);
      if (!Number.isNaN(v) && v > 0) {
        pageState.pageSize = v;
        pageState.page = 1;
        renderTable();
      }
    });
    pageSizeSel.dataset.bound = '1';
  }
  const search = document.getElementById('urldist-search');
  if (search && !search.dataset.bound) {
    const handler = debounce(() => {
      pageState.search = search.value || '';
      pageState.page = 1;
      renderTable();
    }, 200);
    search.addEventListener('input', handler);
    search.dataset.bound = '1';
  }
}

window.initPageBootstrap({
  topbarKey: 'url-distribution',
  run: () => {
    bindControls();
    void reloadAll();
    if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
      window.i18n.onLocaleChange(() => { renderTable(); updateTimestamp(); });
    }
    window.addEventListener('ccload:bfcache-restore', () => { void reloadAll(); });
  },
});
