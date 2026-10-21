// web/src/tips.js
import { $, $$, esc, S, api, keyCaps } from './state.js';
import { on, emit } from './bus.js';
import { showToast } from './ui.js';
import { showRightInspector } from './inspector.js';
import { TIPS_URL, TIPS_STORAGE_KEY_HIDDEN, TIPS_STORAGE_KEY_CACHE, TIPS_STORAGE_KEY_INDEX, DEFAULT_TIPS, renderTipMarkdown } from './tips-util.js';

export {
  TIPS_URL,
  TIPS_STORAGE_KEY_HIDDEN,
  TIPS_STORAGE_KEY_CACHE,
  TIPS_STORAGE_KEY_INDEX,
  DEFAULT_TIPS,
  renderTipMarkdown
};

let loadedTips = [...DEFAULT_TIPS];
let currentTipIndex = 0;
let tipAutoTimer = null;
let tipsFilterQuery = '';

/**
 * Checks whether tips are permanently hidden by the user.
 */
export function isTipsPermanentlyHidden() {
  const localPref = localStorage.getItem(TIPS_STORAGE_KEY_HIDDEN);
  if (localPref === 'true') return true;
  if (S.settings && S.settings['tips.enabled'] === false) return true;
  return false;
}

/**
 * Permanently hides or enables tips.
 */
export async function setTipsPermanentlyHidden(hidden) {
  if (hidden) {
    localStorage.setItem(TIPS_STORAGE_KEY_HIDDEN, 'true');
    if (!S.settings) S.settings = {};
    S.settings['tips.enabled'] = false;
    hideTipWidget();
    stopTipAutoAdvance();
    showToast('i', 'Tips hidden permanently. Re-enable anytime from Help or Settings.');
  } else {
    localStorage.removeItem(TIPS_STORAGE_KEY_HIDDEN);
    if (!S.settings) S.settings = {};
    S.settings['tips.enabled'] = true;
    showToast('i', 'Tip popups enabled.');
  }

  // Update checkbox in inspector if present
  const toggleEl = $('#tips-toggle-popup');
  if (toggleEl) toggleEl.checked = !hidden;

  // Persist to server settings.json
  try {
    await api('/api/settings', { 'tips.enabled': !hidden }, 'POST');
  } catch {}
}

/**
 * Fetches tips from public URL (e.g. px0.ai/tips.json), falling back to local storage cache or defaults.
 */
export async function fetchTips() {
  const url = S.settings?.['tips.url'] || TIPS_URL;
  try {
    const ctrl = new AbortController();
    const timeout = setTimeout(() => ctrl.abort(), 4000);
    const resp = await fetch(url, { signal: ctrl.signal, cache: 'no-cache' });
    clearTimeout(timeout);

    if (resp.ok) {
      const data = await resp.json();
      if (Array.isArray(data) && data.length > 0) {
        loadedTips = data;
        localStorage.setItem(TIPS_STORAGE_KEY_CACHE, JSON.stringify(data));
      }
    }
  } catch (err) {
    // Attempt to restore from cached tips if network fails
    try {
      const cached = localStorage.getItem(TIPS_STORAGE_KEY_CACHE);
      if (cached) {
        const parsed = JSON.parse(cached);
        if (Array.isArray(parsed) && parsed.length > 0) {
          loadedTips = parsed;
        }
      }
    } catch {}
  }

  // Restore remembered tip index
  const savedIndex = parseInt(localStorage.getItem(TIPS_STORAGE_KEY_INDEX) || '0', 10);
  if (!isNaN(savedIndex) && savedIndex >= 0 && savedIndex < loadedTips.length) {
    currentTipIndex = savedIndex;
  }

  updateTipsCountBadge();
  return loadedTips;
}

function updateTipsCountBadge() {
  const badge = $('#tips-count-badge');
  if (badge) {
    badge.textContent = loadedTips.length + (loadedTips.length === 1 ? ' tip' : ' tips');
  }
}

/**
 * Shows the tip widget in the interface with the given tip or current tip.
 */
export function showTipWidget(index = currentTipIndex) {
  if (!loadedTips || loadedTips.length === 0) return;
  if (index < 0) index = loadedTips.length - 1;
  if (index >= loadedTips.length) index = 0;
  currentTipIndex = index;
  localStorage.setItem(TIPS_STORAGE_KEY_INDEX, String(index));

  const tip = loadedTips[index];
  const widget = $('#tip-widget');
  if (!widget) return;

  const tagEl = $('#tip-widget-tag');
  const countEl = $('#tip-widget-count');
  const titleEl = $('#tip-widget-title');
  const contentEl = $('#tip-widget-content');

  if (tagEl) {
    tagEl.textContent = tip.tag || tip.category || 'Tip';
    tagEl.onclick = () => navigateToTips(tip.tag || tip.category);
  }
  if (countEl) {
    countEl.textContent = `${index + 1}/${loadedTips.length}`;
  }
  if (titleEl) {
    titleEl.textContent = tip.title;
  }
  if (contentEl) {
    contentEl.innerHTML = renderTipMarkdown(tip.content);
    if (tip.shortcut) {
      const shortcutHtml = `<div class="tip-widget-shortcut"><span class="tip-shortcut-label">Shortcut:</span> <span class="tip-shortcut-keys">${keyCaps(tip.shortcut)}</span></div>`;
      contentEl.insertAdjacentHTML('beforeend', shortcutHtml);
    }
  }

  widget.hidden = false;
  widget.classList.add('visible');
}

/**
 * Dismisses the tip widget for the current session.
 */
export function hideTipWidget() {
  const widget = $('#tip-widget');
  if (widget) {
    widget.classList.remove('visible');
    widget.hidden = true;
  }
}

/**
 * Navigates to the previous tip in the widget.
 */
export function prevTip() {
  showTipWidget(currentTipIndex - 1);
}

/**
 * Navigates to the next tip in the widget.
 */
export function nextTip() {
  showTipWidget(currentTipIndex + 1);
}

/**
 * Starts automatic progression of tips.
 */
function startTipAutoAdvance() {
  stopTipAutoAdvance();
  tipAutoTimer = setInterval(() => {
    const widget = $('#tip-widget');
    if (widget && !widget.hidden && !widget.matches(':hover')) {
      nextTip();
    }
  }, 24000);
}

function stopTipAutoAdvance() {
  if (tipAutoTimer) {
    clearInterval(tipAutoTimer);
    tipAutoTimer = null;
  }
}

/**
 * Navigates to the Tips inspector pane and renders all tips as a list.
 */
export function navigateToTips(filter = '') {
  const query = typeof filter === 'string' ? filter : '';
  hideTipWidget();
  showRightInspector('tips');
  const filterInput = $('#tips-filter');
  if (filterInput) {
    filterInput.value = query;
  }
  tipsFilterQuery = query.trim().toLowerCase();
  renderTipsList();
  if (filterInput) filterInput.focus();
}

/**
 * Renders all tips matching the search query into the Tips panel.
 */
export function renderTipsList() {
  const listEl = $('#tips-list');
  if (!listEl) return;

  const q = tipsFilterQuery;
  const filtered = loadedTips.filter(t => {
    if (!q) return true;
    const hay = ((t.title || '') + ' ' + (t.content || '') + ' ' + (t.category || '') + ' ' + (t.tag || '')).toLowerCase();
    return hay.includes(q);
  });

  const countBadge = $('#tips-count-badge');
  if (countBadge) {
    countBadge.textContent = filtered.length + (filtered.length === 1 ? ' tip' : ' tips');
  }

  if (filtered.length === 0) {
    listEl.innerHTML = `<div class="hint">No tips found matching "<b>${esc(q)}</b>".</div>`;
    return;
  }

  let html = '';
  filtered.forEach((tip, idx) => {
    const tag = tip.tag || tip.category || 'Tip';
    html += `
      <article class="tip-item" data-id="${esc(tip.id || String(idx))}">
        <header class="tip-item-header">
          <div class="tip-item-title-group">
            <span class="tip-item-num">#${idx + 1}</span>
            <h4 class="tip-item-title">${esc(tip.title)}</h4>
          </div>
          <div class="tip-item-meta">
            <button class="tip-tag-pill" data-tag="${esc(tag)}" title="Filter by ${esc(tag)}">${esc(tag)}</button>
          </div>
        </header>
        <div class="tip-item-content">${renderTipMarkdown(tip.content)}</div>
      </article>
    `;
  });

  listEl.innerHTML = html;

  // Tag filter clicking
  listEl.querySelectorAll('.tip-tag-pill').forEach(btn => {
    btn.addEventListener('click', (e) => {
      e.stopPropagation();
      const tag = btn.dataset.tag;
      if (tag) {
        const filterInput = $('#tips-filter');
        if (filterInput) filterInput.value = tag;
        tipsFilterQuery = tag.toLowerCase();
        renderTipsList();
      }
    });
  });
}

/**
 * Initializes tips DOM listeners and state.
 */
export function initTips() {
  // Widget buttons
  $('#tip-widget-close')?.addEventListener('click', () => {
    hideTipWidget();
    stopTipAutoAdvance();
  });

  $('#tip-widget-prev')?.addEventListener('click', () => {
    prevTip();
  });

  $('#tip-widget-next')?.addEventListener('click', () => {
    nextTip();
  });

  $('#tip-widget-all')?.addEventListener('click', () => {
    navigateToTips();
  });

  $('#tip-widget-hide-perm')?.addEventListener('click', () => {
    setTipsPermanentlyHidden(true);
  });

  // Left bottom bar Help button
  $('#btn-help')?.addEventListener('click', () => {
    navigateToTips();
  });

  // Inspector Tips panel events
  $('#tips-filter')?.addEventListener('input', (e) => {
    tipsFilterQuery = e.target.value.trim().toLowerCase();
    renderTipsList();
  });

  $('#btn-refresh-tips')?.addEventListener('click', async () => {
    showToast('i', 'Fetching latest tips from px0.ai...');
    await fetchTips();
    renderTipsList();
    showToast('i', `Loaded ${loadedTips.length} tips.`);
  });

  const togglePopupEl = $('#tips-toggle-popup');
  if (togglePopupEl) {
    togglePopupEl.checked = !isTipsPermanentlyHidden();
    togglePopupEl.addEventListener('change', () => {
      setTipsPermanentlyHidden(!togglePopupEl.checked);
    });
  }

  // Right sidebar tab switch listener
  on('tips:shown', () => {
    renderTipsList();
    const toggleEl = $('#tips-toggle-popup');
    if (toggleEl) toggleEl.checked = !isTipsPermanentlyHidden();
  });

  // Add click handler to shortcuts sheet to link to tips
  $('#btn-switch-to-tips')?.addEventListener('click', (e) => {
    e.stopPropagation();
    const hs = $('#helpsheet');
    if (hs) hs.hidden = true;
    navigateToTips();
  });
}

/**
 * Kicks off asynchronous loading of tips and polite display.
 */
export async function startTipsAsync() {
  // If tips are permanently hidden, skip network fetch and background rendering on startup
  if (isTipsPermanentlyHidden()) return;

  await fetchTips();
  renderTipsList();

  if (isTipsPermanentlyHidden()) return;

  // Non-intrusive async delay before showing the tip popup in the interface
  setTimeout(() => {
    if (!isTipsPermanentlyHidden()) {
      showTipWidget(currentTipIndex);
      startTipAutoAdvance();
    }
  }, 1800);
}
