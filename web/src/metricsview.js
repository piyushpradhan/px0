// web/src/metricsview.js
// The status bar's CPU · RAM · LSP chip (#st-metrics) and the menu it opens
// (#metrics-menu). Shared by the app (through status.js) and the workbench
// page (workbench.js), so it depends on nothing but state.js.
import { $, esc } from './state.js';

export function fmtBytes(n) {
  if (n < 1024) return n + ' B';
  if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
  return (n / 1048576).toFixed(1) + ' MB';
}

const fmtCpu = m => `${(m.cpuUsage != null ? m.cpuUsage : 0).toFixed(1)}%`;

let lastMetrics = null;
let onMenuOpen = null;

const row = (label, val) => `
      <div class="metrics-row">
        <span class="metrics-label">${esc(label)}</span>
        <span class="metrics-val">${val}</span>
      </div>`;

// m.parts, when present (the workbench), breaks the totals down per process.
function renderMetricsMenu(m) {
  const menuEl = $('#metrics-menu');
  if (!menuEl || !m) return;
  const lspRow = m.lspEnabled ? row('Language Servers (RSS)', fmtBytes(m.lspMemBytes || 0)) : '';
  const parts = Array.isArray(m.parts) && m.parts.length > 1 ? `
    <div class="metrics-title"><span>By Process</span></div>
    <div class="metrics-grid">${m.parts.map(p => row(p.name, `${fmtBytes(p.rssBytes || 0)} · ${fmtCpu(p)}`)).join('')}
    </div>` : '';
  menuEl.innerHTML = `
    <div class="metrics-title">
      <span>Process Metrics</span>
      <span class="toast-chip">px0</span>
    </div>
    <div class="metrics-grid">${row('Resident RAM (RSS)', fmtBytes(m.rssBytes || 0))}${row('CPU Usage', fmtCpu(m))}${row('Active Goroutines', m.goroutines || 0)}${lspRow}
    </div>${parts}
  `;
}

export function closeMetricsMenu() {
  const menuEl = $('#metrics-menu');
  if (menuEl) menuEl.hidden = true;
}

function placeMetricsMenu() {
  const contEl = $('#st-metrics');
  const menuEl = $('#metrics-menu');
  if (!contEl || !menuEl) return;
  const r = contEl.getBoundingClientRect();
  menuEl.style.bottom = (innerHeight - r.top + 6) + 'px';
  menuEl.style.right = Math.max(8, innerWidth - r.right) + 'px';
  menuEl.style.left = 'auto';
}

export function toggleMetricsMenu() {
  const menuEl = $('#metrics-menu');
  if (!menuEl) return;
  if (!menuEl.hidden) {
    closeMetricsMenu();
    return;
  }
  if (lastMetrics) renderMetricsMenu(lastMetrics);
  menuEl.hidden = false;
  placeMetricsMenu();
  onMenuOpen?.();
}

// showMetrics paints the chip, and the menu if it is open.
export function showMetrics(m) {
  if (!m) return;
  lastMetrics = m;
  const cpuEl = $('#st-cpu');
  const ramEl = $('#st-ram');
  if (cpuEl) cpuEl.textContent = fmtCpu(m);
  if (ramEl) ramEl.textContent = fmtBytes(m.rssBytes || 0);
  const lspWrap = $('#st-lspmem-wrap');
  const lspEl = $('#st-lspmem');
  if (lspWrap) lspWrap.hidden = !m.lspEnabled;
  if (lspEl && m.lspEnabled) lspEl.textContent = fmtBytes(m.lspMemBytes || 0);
  const menuEl = $('#metrics-menu');
  if (menuEl && !menuEl.hidden) {
    renderMetricsMenu(m);
    placeMetricsMenu();
  }
}

// initMetricsMenu wires the chip to its menu; refresh runs whenever it opens.
export function initMetricsMenu(refresh) {
  onMenuOpen = refresh;
  const contEl = $('#st-metrics');
  if (contEl) {
    contEl.addEventListener('click', (e) => {
      e.stopPropagation();
      toggleMetricsMenu();
    });
    contEl.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        toggleMetricsMenu();
      }
    });
  }
  addEventListener('click', (e) => {
    const target = /** @type {HTMLElement|null} */ (e.target);
    if (!target?.closest('#metrics-menu, #st-metrics')) closeMetricsMenu();
  });
  addEventListener('keydown', (e) => {
    if (e.key === 'Escape') closeMetricsMenu();
  });
}
