// URL 表格管理（与 API Key 表格一致的交互模式）
const CHANNEL_URL_MAX_LENGTH = 2048;
const CHANNEL_URL_IMPORT_MAX_INPUT_LENGTH = 50000;
const CHANNEL_URL_IMPORT_ERROR_PREVIEW_LIMIT = 8;

function parseChannelURLs(input) {
  if (!input || !input.trim()) return [];

  return input
    .split('\n')
    .map(url => url.trim())
    .filter(Boolean);
}

function splitImportedChannelURLs(input) {
  if (!input || !input.trim()) return [];

  return input
    .split(/[\n,]/)
    .map(url => url.trim())
    .filter(Boolean);
}

function isIPv4Address(host) {
  const parts = host.split('.');
  if (parts.length !== 4) return false;
  return parts.every((part) => /^\d+$/.test(part) && Number(part) >= 0 && Number(part) <= 255);
}

function isPrivateIPv4(host) {
  if (!isIPv4Address(host)) return false;

  const [a, b] = host.split('.').map(Number);
  if (a === 10 || a === 127 || a === 0) return true;
  if (a === 169 && b === 254) return true;
  if (a === 172 && b >= 16 && b <= 31) return true;
  if (a === 192 && b === 168) return true;
  if (a === 100 && b >= 64 && b <= 127) return true;
  if (a === 198 && (b === 18 || b === 19)) return true;
  return false;
}

function isPrivateIPv6(host) {
  const normalized = host.toLowerCase();
  if (!normalized.includes(':')) return false;

  if (normalized === '::' || normalized === '::1') return true;
  if (normalized.startsWith('fc') || normalized.startsWith('fd')) return true;
  if (normalized.startsWith('fe8') || normalized.startsWith('fe9') || normalized.startsWith('fea') || normalized.startsWith('feb')) return true;

  const mappedPrefix = '::ffff:';
  if (normalized.startsWith(mappedPrefix)) {
    return isPrivateIPv4(normalized.slice(mappedPrefix.length));
  }

  return false;
}

function isPrivateHostname(hostname) {
  const normalized = (hostname || '').toLowerCase().replace(/^\[/, '').replace(/\]$/, '');
  if (!normalized) return true;
  if (normalized === 'localhost' || normalized.endsWith('.localhost') || normalized.endsWith('.local')) return true;
  if (isPrivateIPv4(normalized) || isPrivateIPv6(normalized)) return true;
  return false;
}

function getChannelURLIdentityKey(value) {
  const trimmed = (value || '').trim();
  if (!trimmed) return '';

  try {
    const parsed = new URL(trimmed);
    return parsed.toString();
  } catch {
    return trimmed;
  }
}

function validateChannelURL(value) {
  const trimmed = (value || '').trim();
  if (!trimmed) {
    return { valid: false, reasonKey: 'channels.urlValidationMalformed' };
  }
  if (trimmed.length > CHANNEL_URL_MAX_LENGTH) {
    return { valid: false, reasonKey: 'channels.urlValidationTooLong' };
  }

  let parsed;
  try {
    parsed = new URL(trimmed);
  } catch {
    return { valid: false, reasonKey: 'channels.urlValidationMalformed' };
  }

  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    return { valid: false, reasonKey: 'channels.urlValidationProtocol' };
  }
  if (isPrivateHostname(parsed.hostname)) {
    return { valid: false, reasonKey: 'channels.urlValidationPrivate' };
  }

  return {
    valid: true,
    identityKey: parsed.toString(),
    url: trimmed
  };
}

function getChannelURLValidationMessage(reasonKey) {
  return window.t(reasonKey);
}

function parseURLImportInput(input) {
  const rawInput = input || '';
  const trimmedInput = rawInput.trim();
  const result = {
    validURLs: [],
    invalidEntries: [],
    duplicateCount: 0,
    totalEntries: 0,
    tooLong: false
  };

  if (!trimmedInput) {
    return result;
  }
  if (rawInput.length > CHANNEL_URL_IMPORT_MAX_INPUT_LENGTH) {
    result.tooLong = true;
    result.invalidEntries.push({
      value: '',
      reasonKey: 'channels.urlImportInputTooLong'
    });
    return result;
  }

  const entries = splitImportedChannelURLs(trimmedInput);
  result.totalEntries = entries.length;

  const seen = new Set();
  entries.forEach((entry) => {
    const validation = validateChannelURL(entry);
    if (!validation.valid) {
      result.invalidEntries.push({
        value: entry,
        reasonKey: validation.reasonKey
      });
      return;
    }

    if (seen.has(validation.identityKey)) {
      result.duplicateCount++;
      return;
    }

    seen.add(validation.identityKey);
    result.validURLs.push(validation.url);
  });

  return result;
}

function getInvalidInlineURLDetails() {
  return getValidInlineURLs()
    .map((url, index) => ({ index, url, validation: validateChannelURL(url) }))
    .filter((entry) => !entry.validation.valid)
    .map((entry) => ({
      index: entry.index,
      url: entry.url,
      reasonKey: entry.validation.reasonKey
    }));
}

function getValidInlineURLs() {
  return inlineURLTableData
    .map(url => (url || '').trim())
    .filter(Boolean);
}

function syncInlineURLInput() {
  const hiddenInput = document.getElementById('channelUrl');
  if (!hiddenInput) return;
  hiddenInput.value = getValidInlineURLs().join('\n');
}

function updateInlineURLCount() {
  const countEl = document.getElementById('inlineUrlCount');
  if (!countEl) return;
  countEl.textContent = inlineURLTableData.length;
}

function updateURLBatchDeleteButton() {
  const btn = document.getElementById('batchDeleteUrlsBtn');
  if (!btn) return;

  const count = selectedURLIndices.size;
  btn.disabled = count === 0;
  btn.style.opacity = count === 0 ? '0.5' : '1';

  const textEl = btn.querySelector('span');
  if (textEl) {
    textEl.textContent = count > 0
      ? window.t('channels.deleteSelectedCount', { count })
      : window.t('channels.deleteSelected');
  }
}

function updateSelectAllURLsCheckbox() {
  const checkbox = document.getElementById('selectAllURLs');
  if (!checkbox) return;

  const total = inlineURLTableData.length;
  const selected = selectedURLIndices.size;

  if (total === 0 || selected === 0) {
    checkbox.checked = false;
    checkbox.indeterminate = false;
    return;
  }

  if (selected === total) {
    checkbox.checked = true;
    checkbox.indeterminate = false;
    return;
  }

  checkbox.checked = false;
  checkbox.indeterminate = true;
}

function createURLRow(index) {
  const tplData = {
    index: index,
    displayIndex: index + 1,
    url: inlineURLTableData[index] || '',
    mobileLabelUrl: window.t('channels.tableApiUrl'),
    mobileLabelActions: window.t('common.actions')
  };

  const row = TemplateEngine.render('tpl-url-row', tplData);
  if (!row) return null;

  const checkbox = row.querySelector('.url-checkbox');
  if (checkbox && selectedURLIndices.has(index)) {
    checkbox.checked = true;
  }

  // 多URL时注入统计列
  if (hasURLStats()) {
    const url = (inlineURLTableData[index] || '').trim();
    const stat = urlStatsMap[url];
    const actionsTd = row.querySelectorAll('td');
    const lastTd = actionsTd[actionsTd.length - 1]; // actions列

    // 权重列
    const weightTd = document.createElement('td');
    weightTd.className = 'inline-url-cell-center inline-url-cell-metric inline-url-col-weight';
    weightTd.setAttribute('data-mobile-label', window.t('channels.urlWeight'));
    weightTd.innerHTML = formatURLWeight(stat);

    const statusTd = document.createElement('td');
    statusTd.className = 'inline-url-cell-center inline-url-col-status';
    statusTd.setAttribute('data-mobile-label', window.t('common.status'));
    statusTd.innerHTML = formatURLStatus(stat);

    const latencyTd = document.createElement('td');
    latencyTd.className = 'inline-url-cell-center inline-url-cell-metric inline-url-col-latency';
    latencyTd.setAttribute('data-mobile-label', window.t('stats.latency'));
    latencyTd.innerHTML = formatURLLatencyHTML(stat);

    const requestsTd = document.createElement('td');
    requestsTd.className = 'inline-url-cell-center inline-url-cell-metric inline-url-col-requests';
    requestsTd.setAttribute('data-mobile-label', window.t('common.requests'));
    requestsTd.innerHTML = formatURLRequests(stat);

    row.insertBefore(weightTd, lastTd);
    row.insertBefore(statusTd, lastTd);
    row.insertBefore(latencyTd, lastTd);
    row.insertBefore(requestsTd, lastTd);

    // thinking 黑名单标记
    const blocked = noThinkingMap[url];
    if (blocked && blocked.length > 0) {
      const tag = document.createElement('div');
      tag.className = 'no-thinking-tag';
      tag.title = (window.t('channels.noThinkingModels') || 'No thinking') + ': ' + blocked.join(', ');
      tag.innerHTML = '<span class="no-thinking-icon">!</span> ' + blocked.map(m => {
        const short = m.replace(/^claude-/, '').replace(/-thinking$/, '-think');
        return `<span class="no-thinking-model">${short}</span>`;
      }).join(' ');
      // 清除按钮
      const clearBtn = document.createElement('button');
      clearBtn.className = 'no-thinking-clear-btn';
      clearBtn.textContent = 'x';
      clearBtn.title = window.t('channels.clearNoThinking') || 'Clear blacklist';
      clearBtn.onclick = (e) => {
        e.stopPropagation();
        clearNoThinkingList(editingChannelId, url);
      };
      tag.appendChild(clearBtn);
      // 插到URL输入框下面
      const urlCell = row.querySelector('.inline-url-col-url');
      if (urlCell) urlCell.appendChild(tag);
    }
  }

  return row;
}

function initInlineURLTableEventDelegation() {
  const tbody = document.getElementById('inlineUrlTableBody');
  if (!tbody || tbody.dataset.delegated) return;

  tbody.dataset.delegated = 'true';

  tbody.addEventListener('change', (e) => {
    const checkbox = e.target.closest('.url-checkbox');
    if (checkbox) {
      const index = parseInt(checkbox.dataset.index, 10);
      toggleURLSelection(index, checkbox.checked);
      return;
    }

    const input = e.target.closest('.inline-url-input');
    if (input) {
      const index = parseInt(input.dataset.index, 10);
      updateInlineURL(index, input.value);
    }
  });

  tbody.addEventListener('click', (e) => {
    const testBtn = e.target.closest('.inline-url-test-btn');
    if (testBtn) {
      const index = parseInt(testBtn.dataset.index, 10);
      testInlineURL(index, testBtn);
      return;
    }

    const deleteBtn = e.target.closest('.inline-url-delete-btn');
    if (deleteBtn) {
      const index = parseInt(deleteBtn.dataset.index, 10);
      deleteInlineURL(index);
    }
  });
}

function renderInlineURLTable() {
  const tbody = document.getElementById('inlineUrlTableBody');
  if (!tbody) return;

  if (inlineURLTableData.length === 0) {
    inlineURLTableData = [''];
  }

  initInlineURLTableEventDelegation();
  updateInlineURLCount();
  syncInlineURLInput();
  updateURLStatsHeader();

  tbody.innerHTML = '';
  inlineURLTableData.forEach((_, index) => {
    const row = createURLRow(index);
    if (row) tbody.appendChild(row);
  });

  updateSelectAllURLsCheckbox();
  updateURLBatchDeleteButton();
}

function setInlineURLTableData(rawURL) {
  inlineURLTableData = parseChannelURLs(rawURL);
  if (inlineURLTableData.length === 0) {
    inlineURLTableData = [''];
  }
  selectedURLIndices.clear();
  urlStatsMap = {};
  renderInlineURLTable();
}

function addInlineURL() {
  const newIndex = inlineURLTableData.length;
  inlineURLTableData.push('');
  renderInlineURLTable();
  markChannelFormDirty();

  setTimeout(() => {
    const input = document.querySelector(`.inline-url-input[data-index="${newIndex}"]`);
    if (input) input.focus();
  }, 0);
}

function updateInlineURL(index, value) {
  const nextValue = (value || '').trim();
  if (inlineURLTableData[index] === nextValue) return;

  inlineURLTableData[index] = nextValue;
  syncInlineURLInput();
  markChannelFormDirty();
}

function toggleURLSelection(index, checked) {
  if (checked) {
    selectedURLIndices.add(index);
  } else {
    selectedURLIndices.delete(index);
  }

  updateSelectAllURLsCheckbox();
  updateURLBatchDeleteButton();
}

function toggleSelectAllURLs(checked) {
  if (checked) {
    inlineURLTableData.forEach((_, index) => selectedURLIndices.add(index));
  } else {
    selectedURLIndices.clear();
  }

  renderInlineURLTable();
}

function deleteInlineURL(index) {
  if (index < 0 || index >= inlineURLTableData.length) return;

  if (inlineURLTableData.length === 1) {
    inlineURLTableData[0] = '';
    selectedURLIndices.clear();
    renderInlineURLTable();
    markChannelFormDirty();
    return;
  }

  inlineURLTableData.splice(index, 1);

  const nextSelected = new Set();
  selectedURLIndices.forEach(i => {
    if (i < index) {
      nextSelected.add(i);
    } else if (i > index) {
      nextSelected.add(i - 1);
    }
  });
  selectedURLIndices = nextSelected;

  renderInlineURLTable();
  markChannelFormDirty();
}

function batchDeleteSelectedURLs() {
  const count = selectedURLIndices.size;
  if (count === 0) return;

  if (!confirm(window.t('channels.confirmBatchDeleteUrls', { count }))) {
    return;
  }

  const indices = Array.from(selectedURLIndices).sort((a, b) => b - a);
  indices.forEach(index => {
    inlineURLTableData.splice(index, 1);
  });

  if (inlineURLTableData.length === 0) {
    inlineURLTableData = [''];
  }

  selectedURLIndices.clear();
  renderInlineURLTable();
  markChannelFormDirty();
}

function setURLImportConfirmEnabled(enabled) {
  const button = document.getElementById('confirmUrlImportBtn');
  if (!button) return;

  button.disabled = !enabled;
}

function resetURLImportPreview() {
  const previewContent = document.getElementById('urlImportPreviewContent');
  const previewMeta = document.getElementById('urlImportPreviewMeta');
  const errorBox = document.getElementById('urlImportErrorBox');
  const errorSummary = document.getElementById('urlImportErrorSummary');
  const errorList = document.getElementById('urlImportErrorList');
  const countSpan = document.getElementById('urlImportCount');

  if (countSpan) countSpan.textContent = '0';
  if (previewContent) previewContent.classList.add('hidden');
  if (previewMeta) {
    previewMeta.textContent = '';
    previewMeta.classList.add('hidden');
  }
  if (errorSummary) errorSummary.textContent = '';
  if (errorList) errorList.innerHTML = '';
  if (errorBox) errorBox.classList.add('hidden');

  setURLImportConfirmEnabled(false);
}

function buildURLImportPreviewMeta(result) {
  const segments = [];
  if (result.duplicateCount > 0) {
    segments.push(window.t('channels.urlImportDuplicatesIgnored', { count: result.duplicateCount }));
  }
  if (result.invalidEntries.length > 0) {
    segments.push(window.t('channels.urlImportInvalidEntries', { count: result.invalidEntries.length }));
  }
  return segments.join(' · ');
}

function updateURLImportPreview() {
  const textarea = document.getElementById('urlImportTextarea');
  const previewContent = document.getElementById('urlImportPreviewContent');
  const previewMeta = document.getElementById('urlImportPreviewMeta');
  const errorBox = document.getElementById('urlImportErrorBox');
  const errorSummary = document.getElementById('urlImportErrorSummary');
  const errorList = document.getElementById('urlImportErrorList');
  const countSpan = document.getElementById('urlImportCount');
  if (!textarea || !previewContent || !previewMeta || !errorBox || !errorSummary || !errorList || !countSpan) return;

  const result = parseURLImportInput(textarea.value);
  const hasInput = Boolean(textarea.value.trim());
  const hasResult = hasInput && (result.validURLs.length > 0 || result.invalidEntries.length > 0 || result.tooLong);

  if (!hasResult) {
    resetURLImportPreview();
    return;
  }

  countSpan.textContent = String(result.validURLs.length);
  previewContent.classList.remove('hidden');

  const metaText = buildURLImportPreviewMeta(result);
  if (metaText) {
    previewMeta.textContent = metaText;
    previewMeta.classList.remove('hidden');
  } else {
    previewMeta.textContent = '';
    previewMeta.classList.add('hidden');
  }

  errorList.innerHTML = '';
  if (result.invalidEntries.length > 0 || result.tooLong) {
    errorBox.classList.remove('hidden');
    errorSummary.textContent = result.tooLong
      ? window.t('channels.urlImportInputTooLong', { max: CHANNEL_URL_IMPORT_MAX_INPUT_LENGTH })
      : window.t('channels.urlImportInvalidSummary', { count: result.invalidEntries.length });

    result.invalidEntries.slice(0, CHANNEL_URL_IMPORT_ERROR_PREVIEW_LIMIT).forEach((entry) => {
      const item = document.createElement('li');
      const reason = getChannelURLValidationMessage(entry.reasonKey);
      item.textContent = entry.value ? `${entry.value} - ${reason}` : reason;
      errorList.appendChild(item);
    });

    if (result.invalidEntries.length > CHANNEL_URL_IMPORT_ERROR_PREVIEW_LIMIT) {
      const item = document.createElement('li');
      item.textContent = window.t('channels.urlImportInvalidMore', {
        count: result.invalidEntries.length - CHANNEL_URL_IMPORT_ERROR_PREVIEW_LIMIT
      });
      errorList.appendChild(item);
    }
  } else {
    errorSummary.textContent = '';
    errorBox.classList.add('hidden');
  }

  setURLImportConfirmEnabled(result.validURLs.length > 0 && !result.tooLong);
}

function setupURLImportPreview() {
  const textarea = document.getElementById('urlImportTextarea');
  if (!textarea || textarea.dataset.previewBound) return;

  textarea.addEventListener('input', updateURLImportPreview);
  textarea.dataset.previewBound = '1';
}

function openURLImportModal() {
  const textarea = document.getElementById('urlImportTextarea');
  if (!textarea) return;

  textarea.value = '';
  resetURLImportPreview();
  document.getElementById('urlImportModal').classList.add('show');
  setTimeout(() => textarea.focus(), 100);
}

function closeURLImportModal() {
  document.getElementById('urlImportModal').classList.remove('show');
}

function confirmURLImport() {
  const textarea = document.getElementById('urlImportTextarea');
  if (!textarea) return;

  const input = textarea.value.trim();
  if (!input) {
    window.showNotification(window.t('channels.enterAtLeastOneUrl'), 'warning');
    return;
  }

  const result = parseURLImportInput(textarea.value);
  if (result.tooLong) {
    window.showNotification(window.t('channels.urlImportInputTooLong', { max: CHANNEL_URL_IMPORT_MAX_INPUT_LENGTH }), 'warning');
    return;
  }
  if (result.validURLs.length === 0) {
    window.showNotification(window.t('channels.noValidUrlParsed'), 'warning');
    return;
  }

  const existingKeys = new Set(
    inlineURLTableData
      .map((url) => getChannelURLIdentityKey(url))
      .filter(Boolean)
  );
  let addedCount = 0;
  let existingCount = 0;

  if (inlineURLTableData.length === 1 && !String(inlineURLTableData[0] || '').trim()) {
    inlineURLTableData = [];
  }

  result.validURLs.forEach((url) => {
    const identityKey = getChannelURLIdentityKey(url);
    if (existingKeys.has(identityKey)) {
      existingCount++;
      return;
    }

    inlineURLTableData.push(url);
    existingKeys.add(identityKey);
    addedCount++;
  });

  closeURLImportModal();
  renderInlineURLTable();
  if (addedCount > 0) markChannelFormDirty();

  if (addedCount === 0) {
    window.showNotification(window.t('channels.allUrlsExist'), 'info');
    return;
  }

  const skippedCount = result.duplicateCount + result.invalidEntries.length + existingCount;
  const message = skippedCount > 0
    ? window.t('channels.urlImportPartialSuccess', { added: addedCount, skipped: skippedCount })
    : window.t('channels.urlImportSuccess', { added: addedCount });
  window.showNotification(message, 'success');
}

// 弹出模型选择框，返回选中的模型名，取消返回 null
function showModelSelectDialog(models, urlIndex) {
  return new Promise(resolve => {
    // 遮罩
    const overlay = document.createElement('div');
    Object.assign(overlay.style, {
      position: 'fixed', inset: '0', background: 'rgba(0,0,0,0.4)',
      zIndex: '10000', display: 'flex', alignItems: 'center', justifyContent: 'center',
    });

    const dialog = document.createElement('div');
    Object.assign(dialog.style, {
      background: 'var(--bg-primary, #fff)', borderRadius: '8px', padding: '16px 20px',
      minWidth: '240px', maxWidth: '360px', boxShadow: '0 4px 20px rgba(0,0,0,0.15)',
    });

    const title = document.createElement('div');
    title.textContent = `URL #${urlIndex + 1} - ${window.t ? window.t('channels.selectTestModel') : 'Select model to test'}`;
    Object.assign(title.style, { fontWeight: '600', marginBottom: '12px', fontSize: '14px' });
    dialog.appendChild(title);

    // 每个模型一个按钮
    for (const m of models) {
      const btn = document.createElement('button');
      btn.textContent = m;
      Object.assign(btn.style, {
        display: 'block', width: '100%', padding: '8px 12px', marginBottom: '6px',
        border: '1px solid var(--border-color, #ddd)', borderRadius: '6px',
        background: 'var(--bg-secondary, #f5f5f5)', cursor: 'pointer',
        textAlign: 'left', fontSize: '13px',
      });
      btn.onmouseenter = () => btn.style.background = 'var(--bg-hover, #e8e8e8)';
      btn.onmouseleave = () => btn.style.background = 'var(--bg-secondary, #f5f5f5)';
      btn.onclick = () => { overlay.remove(); resolve(m); };
      dialog.appendChild(btn);
    }

    // 取消按钮
    const cancel = document.createElement('button');
    cancel.textContent = window.t ? window.t('common.cancel') : 'Cancel';
    Object.assign(cancel.style, {
      display: 'block', width: '100%', padding: '8px', marginTop: '8px',
      border: 'none', borderRadius: '6px', background: 'transparent',
      cursor: 'pointer', color: 'var(--text-secondary, #888)', fontSize: '13px',
    });
    cancel.onclick = () => { overlay.remove(); resolve(null); };
    dialog.appendChild(cancel);

    // ESC 关闭
    overlay.onkeydown = (e) => { if (e.key === 'Escape') { overlay.remove(); resolve(null); } };
    overlay.tabIndex = -1;
    overlay.onclick = (e) => { if (e.target === overlay) { overlay.remove(); resolve(null); } };

    overlay.appendChild(dialog);
    document.body.appendChild(overlay);
    overlay.focus();
  });
}

async function testInlineURL(index, buttonElement) {
  if (!editingChannelId) {
    alert(window.t('channels.cannotGetChannelId'));
    return;
  }

  const models = redirectTableData
    .map(r => r.model)
    .filter(m => m && m.trim());
  if (models.length === 0) {
    alert(window.t('channels.configModelsFirst'));
    return;
  }

  const url = (inlineURLTableData[index] || '').trim();
  if (!url) {
    alert(window.t('channels.fillApiUrlFirst'));
    return;
  }

  const firstKey = (inlineKeyTableData[0] || '').trim();
  if (!firstKey) {
    alert(window.t('channels.emptyKeyCannotTest'));
    return;
  }

  // 让用户选测哪个模型（只有一个就直接用）
  let selectedModel;
  if (models.length === 1) {
    selectedModel = models[0];
  } else {
    selectedModel = await showModelSelectDialog(models, index);
    if (!selectedModel) return; // 用户取消
  }

  const channelTypeRadios = document.querySelectorAll('input[name="channelType"]');
  let channelType = 'anthropic';
  for (const radio of channelTypeRadios) {
    if (radio.checked) {
      channelType = radio.value.toLowerCase();
      break;
    }
  }

  if (!buttonElement) return;
  const originalHTML = buttonElement.innerHTML;
  buttonElement.disabled = true;
  buttonElement.innerHTML = '<span style="font-size: 10px;">...</span>';

  try {
    const testResult = await fetchDataWithAuth(`/admin/channels/${editingChannelId}/test-url`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        model: selectedModel,
        stream: true,
        content: 'test',
        channel_type: channelType,
        key_index: 0,
        base_url: url
      })
    });

    await refreshKeyCooldownStatus();

    if (testResult.success) {
      window.showNotification(window.t('channels.urlTestSuccess', { index: index + 1 }), 'success');
    } else {
      const errorMsg = testResult.error || window.t('common.failed');
      window.showNotification(window.t('channels.urlTestFailed', { index: index + 1, error: errorMsg }), 'error');
    }
  } catch (error) {
    console.error('URL test failed', error);
    window.showNotification(window.t('channels.urlTestRequestFailed', { index: index + 1, error: error.message }), 'error');
  } finally {
    buttonElement.disabled = false;
    buttonElement.innerHTML = originalHTML;
  }
}

// === URL 实时状态 ===

function hasURLStats() {
  return Object.keys(urlStatsMap).length > 0;
}

// thinking黑名单：url → [model1, model2, ...]
let noThinkingMap = {};

async function fetchURLStats(channelId) {
  if (!channelId) return;
  try {
    // 并行拉 URL stats 和 thinking 黑名单
    const [stats, noThinking] = await Promise.all([
      fetchDataWithAuth(`/admin/channels/${channelId}/url-stats`),
      fetchDataWithAuth(`/admin/channels/${channelId}/no-thinking`).catch(() => []),
    ]);
    urlStatsMap = {};
    if (Array.isArray(stats)) {
      for (const s of stats) {
        urlStatsMap[s.url] = s;
      }
    }
    // 按URL分组黑名单
    noThinkingMap = {};
    if (Array.isArray(noThinking)) {
      for (const entry of noThinking) {
        if (!noThinkingMap[entry.url]) noThinkingMap[entry.url] = [];
        noThinkingMap[entry.url].push(entry.model);
      }
    }
    if (hasURLStats() || Object.keys(noThinkingMap).length > 0) {
      renderInlineURLTable();
    }
  } catch (e) {
    console.error('Failed to fetch URL stats', e);
  }
}

// 清除指定渠道的 thinking 黑名单
async function clearNoThinkingList(channelId, url, model) {
  if (!channelId) return;
  try {
    let endpoint = `/admin/channels/${channelId}/no-thinking`;
    const params = [];
    if (url) params.push(`url=${encodeURIComponent(url)}`);
    if (model) params.push(`model=${encodeURIComponent(model)}`);
    if (params.length) endpoint += '?' + params.join('&');

    await fetchDataWithAuth(endpoint, { method: 'DELETE' });
    window.showNotification(window.t('channels.noThinkingCleared') || 'Thinking blacklist cleared', 'success');
    await fetchURLStats(channelId);
  } catch (e) {
    console.error('Failed to clear no-thinking list', e);
    window.showNotification(window.t('common.failed') + ': ' + e.message, 'error');
  }
}

function formatURLStatus(stat) {
  if (!stat) {
    return '<span class="inline-url-status-placeholder">--</span>';
  }
  if (stat.cooled_down) {
    const remain = humanizeMS(stat.cooldown_remain_ms);
    return `<span class="inline-url-status-badge inline-url-status-badge--cooldown" title="${window.t('channels.urlStatusCooldown')} ${remain}">`
      + '<span class="inline-url-status-dot inline-url-status-dot--cooldown"></span>'
      + `${remain}</span>`;
  }
  // 慢隔离
  if (stat.slow_isolated) {
    return '<span class="inline-url-status-badge inline-url-status-badge--slow" title="' + window.t('channels.urlStatusSlow') + '">'
      + '<span class="inline-url-status-dot inline-url-status-dot--slow"></span>'
      + window.t('channels.urlStatusSlow') + '</span>';
  }
  if (stat.latency_ms < 0) {
    return '<span class="inline-url-status-badge inline-url-status-badge--unknown">'
      + '<span class="inline-url-status-dot inline-url-status-dot--unknown"></span>'
      + `${window.t('channels.urlStatusUnknown')}</span>`;
  }
  return '<span class="inline-url-status-badge inline-url-status-badge--ok">'
    + '<span class="inline-url-status-dot inline-url-status-dot--ok"></span>'
    + `${window.t('channels.urlStatusNormal')}</span>`;
}

function formatURLLatency(stat) {
  if (!stat || stat.latency_ms < 0) return '--';
  const ms = Math.round(stat.latency_ms);
  if (ms < 1000) return ms + 'ms';
  return (ms / 1000).toFixed(1) + 's';
}

// 带tooltip的延迟，hover显示TTFB/探测/有效延迟详情
function formatURLLatencyHTML(stat) {
  const text = formatURLLatency(stat);
  if (!stat || stat.latency_ms < 0) return text;

  const parts = [];
  if (stat.latency_source) parts.push(window.t('channels.urlLatencySource') + ': ' + stat.latency_source);
  if (stat.ttfb_latency_ms > 0) parts.push('TTFB: ' + Math.round(stat.ttfb_latency_ms) + 'ms');
  if (stat.probe_latency_ms > 0) parts.push(window.t('channels.urlLatencyProbe') + ': ' + Math.round(stat.probe_latency_ms) + 'ms');
  if (stat.effective_latency_ms > 0) parts.push(window.t('channels.urlLatencyEffective') + ': ' + Math.round(stat.effective_latency_ms) + 'ms');
  if (parts.length === 0) return text;
  return `<span title="${parts.join('&#10;')}" style="cursor: help; border-bottom: 1px dotted var(--neutral-400);">${text}</span>`;
}

// 权重百分比
function formatURLWeight(stat) {
  if (!stat || !stat.weight || stat.weight <= 0) return '<span class="inline-url-status-placeholder">--</span>';

  // 算总权重转百分比
  let total = 0;
  for (const s of Object.values(urlStatsMap)) {
    if (s.weight > 0) total += s.weight;
  }
  if (total <= 0) return '<span class="inline-url-status-placeholder">--</span>';

  const pct = stat.weight / total * 100;
  const display = pct < 1 ? '<1%' : Math.round(pct) + '%';

  // 冷却或慢隔离用灰色
  if (stat.cooled_down || stat.slow_isolated) {
    return `<span class="inline-url-weight-muted">${display}</span>`;
  }
  return `<span class="inline-url-weight">${display}</span>`;
}

function formatURLRequests(stat) {
  if (!stat) return '--';
  const s = stat.requests || 0;
  const f = stat.failures || 0;
  if (s === 0 && f === 0) return '--';
  if (f === 0) return `<span style="color: #16A34A;">${s}</span>`;
  return `<span style="color: #16A34A;">${s}</span><span style="color: var(--neutral-300); margin: 0 2px;">/</span><span style="color: #DC2626;">${f}</span>`;
}

function updateURLStatsHeader() {
  const thead = document.querySelector('#inlineUrlTableBody')?.closest('table')?.querySelector('thead tr');
  if (!thead) return;

  // 移除已有的统计列头
  thead.querySelectorAll('.url-stats-th').forEach(el => el.remove());

  if (!hasURLStats()) return;

  const actionsTh = thead.querySelector('th:last-child');

  // 权重列头
  const weightTh = document.createElement('th');
  weightTh.className = 'url-stats-th inline-url-col-weight';
  weightTh.textContent = window.t('channels.urlWeight');

  const statusTh = document.createElement('th');
  statusTh.className = 'url-stats-th inline-url-col-status';
  statusTh.textContent = window.t('channels.urlStatus');

  const latencyTh = document.createElement('th');
  latencyTh.className = 'url-stats-th inline-url-col-latency';
  latencyTh.textContent = window.t('channels.urlLatency');

  const requestsTh = document.createElement('th');
  requestsTh.className = 'url-stats-th inline-url-col-requests';
  requestsTh.textContent = window.t('channels.urlRequests');

  thead.insertBefore(weightTh, actionsTh);
  thead.insertBefore(statusTh, actionsTh);
  thead.insertBefore(latencyTh, actionsTh);
  thead.insertBefore(requestsTh, actionsTh);
}

// 导出URL：弹窗选格式，生成 txt 下载
function exportURLs() {
  const urls = getValidInlineURLs();
  if (urls.length === 0) {
    alert(window.t('channels.noUrlsToExport') || 'No URLs to export');
    return;
  }

  // 拿第一个key（用于 url|key 格式）
  const firstKey = (inlineKeyTableData[0] || '').trim();

  // 弹窗选格式
  const overlay = document.createElement('div');
  Object.assign(overlay.style, {
    position: 'fixed', inset: '0', background: 'rgba(0,0,0,0.4)',
    zIndex: '10000', display: 'flex', alignItems: 'center', justifyContent: 'center',
  });

  const dialog = document.createElement('div');
  Object.assign(dialog.style, {
    background: 'var(--bg-primary, #fff)', borderRadius: '8px', padding: '16px 20px',
    minWidth: '260px', maxWidth: '380px', boxShadow: '0 4px 20px rgba(0,0,0,0.15)',
  });

  const title = document.createElement('div');
  title.textContent = window.t('channels.exportUrlsTitle') || 'Export URLs';
  Object.assign(title.style, { fontWeight: '600', marginBottom: '12px', fontSize: '14px' });
  dialog.appendChild(title);

  const hint = document.createElement('div');
  hint.textContent = `${urls.length} URLs`;
  Object.assign(hint.style, { marginBottom: '12px', fontSize: '12px', color: 'var(--text-secondary, #888)' });
  dialog.appendChild(hint);

  function makeBtn(label, onClick) {
    const btn = document.createElement('button');
    btn.textContent = label;
    Object.assign(btn.style, {
      display: 'block', width: '100%', padding: '10px 12px', marginBottom: '6px',
      border: '1px solid var(--border-color, #ddd)', borderRadius: '6px',
      background: 'var(--bg-secondary, #f5f5f5)', cursor: 'pointer',
      textAlign: 'left', fontSize: '13px',
    });
    btn.onmouseenter = () => btn.style.background = 'var(--bg-hover, #e8e8e8)';
    btn.onmouseleave = () => btn.style.background = 'var(--bg-secondary, #f5f5f5)';
    btn.onclick = () => { overlay.remove(); onClick(); };
    return btn;
  }

  // 格式1: 纯 URL
  dialog.appendChild(makeBtn(
    (window.t('channels.exportPlainUrl') || 'Plain URLs') + '  (url)',
    () => downloadTxt(urls.join('\n'), 'urls.txt'),
  ));

  // 格式2: url|key
  if (firstKey) {
    dialog.appendChild(makeBtn(
      (window.t('channels.exportUrlWithKey') || 'URLs with Key') + '  (url|key)',
      () => downloadTxt(urls.map(u => u + '|' + firstKey).join('\n'), 'urls_with_key.txt'),
    ));
  }

  // 取消
  const cancel = document.createElement('button');
  cancel.textContent = window.t('common.cancel') || 'Cancel';
  Object.assign(cancel.style, {
    display: 'block', width: '100%', padding: '8px', marginTop: '8px',
    border: 'none', borderRadius: '6px', background: 'transparent',
    cursor: 'pointer', color: 'var(--text-secondary, #888)', fontSize: '13px',
  });
  cancel.onclick = () => overlay.remove();
  dialog.appendChild(cancel);

  overlay.onclick = (e) => { if (e.target === overlay) overlay.remove(); };
  overlay.appendChild(dialog);
  document.body.appendChild(overlay);
}

function downloadTxt(content, filename) {
  const blob = new Blob([content], { type: 'text/plain;charset=utf-8' });
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = filename;
  a.click();
  URL.revokeObjectURL(a.href);
}
