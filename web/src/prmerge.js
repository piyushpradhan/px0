// web/src/prmerge.js
// Merging a PR from the review bar, and keeping the review verdict honest
// about the branch's state. The forge's mergeability (/api/pr/merge-status)
// drives one status pill and one split button: "Squash and merge" (or
// whichever method was used last) when the PR can merge, "Resolve conflicts"
// / "Update branch" when it is behind its base -- which hands off to the git
// panel's agent rebase flow -- and "Force-push first" once that rebase sits
// unpushed. The merge itself goes through a dialog that shows exactly what
// lands on the base: method, commit title and message (rewritable by the
// harness, with the user's commit instructions), and anything that makes
// merging now a bad idea. Nothing here knows which forge it is talking to.
import { $, S, esc, api, apiPostJson, keyLabel } from './state.js';
import { showToast } from './ui.js';
import { on, emit } from './bus.js';
import { registerAgentPicker } from './agent.js';
import { openSettings } from './settings.js';

const METHODS = {
  merge: {
    label: 'Create a merge commit', button: 'Merge', confirm: 'Confirm merge', done: 'Merged',
    explain: (n, base) => `All ${pluralize(n, 'commit')} from this branch are added to ${base} with a merge commit.`,
  },
  squash: {
    label: 'Squash and merge', button: 'Squash and merge', confirm: 'Confirm squash and merge', done: 'Squashed and merged',
    explain: (n, base) => `The ${pluralize(n, 'commit')} from this branch are combined into one commit on ${base}.`,
  },
  rebase: {
    label: 'Rebase and merge', button: 'Rebase and merge', confirm: 'Confirm rebase and merge', done: 'Rebased and merged',
    explain: (n, base) => `The ${pluralize(n, 'commit')} from this branch are replayed onto ${base} one by one, with no merge commit.`,
  },
};
const METHOD_KEY = 'px0.prMergeMethod';

const pluralize = (n, word) => n + ' ' + word + (n === 1 ? '' : 's');
const codeHtml = s => '<code>' + esc(s || '') + '</code>';

let ctx = null;       // { meta: () => PR meta, onMerged: () => Promise }
let info = null;      // last /api/pr/merge-status reply
let sync = { running: false, inProgress: '', rewritten: false }; // from the git panel
let unknownRetries = 0;
let retryTimer = 0;
let lastFetch = 0;

/**
 * @param {{meta: () => any, onMerged: () => Promise<void>, onChange: () => void}} c
 */
export function initPRMerge(c) {
  ctx = c;
  on('git:sync', st => {
    sync = { ...sync, ...st };
    renderPRMerge();
    ctx.onChange();
  });
  $('#pr-merge-state')?.addEventListener('click', onPillClick);
  $('#pr-rebase-ai')?.addEventListener('click', () => {
    emit('pr:resolve');
  });
  $('#pr-merge')?.addEventListener('click', onMergeClick);
  $('#pr-merge-caret')?.addEventListener('click', e => { e.stopPropagation(); toggleMenu(); });
  document.addEventListener('click', e => { if (!e.target.closest?.('#pr-merge-group')) toggleMenu(false); });
  // Mergeability changes elsewhere (checks finish, someone pushes to the
  // base): re-read it when the tab comes back, at most every 20s.
  addEventListener('focus', () => { if (Date.now() - lastFetch > 20000) refreshMergeStatus(); });
  initDialog();
  refreshMergeStatus();
}

// The forge computes mergeability lazily: right after a push it often says
// "unknown". Ask again a few times rather than leaving the pill on it.
export async function refreshMergeStatus() {
  if (!ctx) return;
  clearTimeout(retryTimer);
  lastFetch = Date.now();
  try {
    info = await api('/api/pr/merge-status');
  } catch {
    info = null; // no pill beats a wrong one
  }
  if (info?.status?.state === 'unknown' && unknownRetries < 6) {
    unknownRetries++;
    retryTimer = setTimeout(refreshMergeStatus, 2500);
  } else {
    unknownRetries = 0;
  }
  renderPRMerge();
  ctx.onChange();
}

const state = () => info?.status?.state || '';
const baseBranch = () => info?.base || ctx?.meta()?.base || 'the base branch';
const allowed = () => info?.status?.methods || [];

function chosenMethod() {
  const methods = allowed();
  let m = '';
  try { m = localStorage.getItem(METHOD_KEY) || ''; } catch {}
  return methods.includes(m) ? m : (methods.includes('squash') ? 'squash' : methods[0] || 'merge');
}

function rememberMethod(m) {
  try { localStorage.setItem(METHOD_KEY, m); } catch {}
}

/**
 * Why the review verdict buttons can't be used right now, or null. The
 * checkout's own state comes first: while a rebase runs or sits unpushed,
 * the forge still holds the old commits, and approving would approve those.
 * @returns {{all: string|null, approve: string|null, approveNote: string|null}}
 */
export function reviewGate() {
  if (sync.running) return { all: 'Wait for the rebase to finish', approve: null, approveNote: null };
  if (sync.inProgress) return { all: `Finish or abort the ${sync.inProgress} first`, approve: null, approveNote: null };
  if (sync.rewritten) {
    return { all: null, approve: 'Force-push the rebased branch first: approving now would approve the old commits', approveNote: null };
  }
  if (state() === 'conflicts') {
    return { all: null, approve: null, approveNote: `This PR conflicts with ${baseBranch()}; approving won't make it mergeable until the conflicts are resolved` };
  }
  return { all: null, approve: null, approveNote: null };
}

// The pill: one word on whether this PR can merge, and the next step when
// it can't. The checkout's local state overrides the forge's, since it is
// what the forge will see next.
function pill() {
  if (sync.running) return { tone: 'busy', text: 'Rebasing...', title: 'The harness is bringing the branch up to date' };
  if (sync.inProgress) return { tone: 'warn', text: `${cap(sync.inProgress)} unfinished`, title: 'Open the git panel to finish or abort it', act: 'resolve' };
  if (sync.rewritten) return { tone: 'warn', text: 'Rebased, not pushed', title: 'Force-push to update the PR', act: 'push' };
  const st = info?.status;
  if (!st) return null;
  const reason = st.reason || '';
  switch (st.state) {
    case 'clean': return { tone: 'ok', text: 'Ready to merge', title: 'No conflicts with ' + baseBranch() };
    case 'unstable': return { tone: 'warn', text: 'Checks failing', title: reason || 'Mergeable, but some checks are failing' };
    case 'conflicts': return { tone: 'err', text: 'Conflicts with ' + baseBranch(), title: 'Resolve them with your coding harness', act: 'resolve' };
    case 'behind': return { tone: 'warn', text: 'Behind ' + baseBranch(), title: 'Bring the branch up to date with your coding harness', act: 'resolve' };
    case 'blocked': return { tone: 'muted', text: 'Merge blocked', title: reason || 'Branch protection refuses the merge' };
    case 'draft': return { tone: 'muted', text: 'Draft', title: reason || 'This PR is still a draft' };
    case 'unknown': return { tone: 'busy', text: 'Checking mergeability...', title: 'The forge is still computing whether this PR can merge' };
    case 'closed': return { tone: 'muted', text: 'Closed', title: 'This PR is closed' };
  }
  return null; // merged: the bar's own Merged badge says it
}

const cap = s => s ? s[0].toUpperCase() + s.slice(1) : s;

function onPillClick() {
  const p = pill();
  if (p?.act === 'resolve') emit('pr:resolve');
  else if (p?.act === 'push') emit('pr:force-push');
  else refreshMergeStatus();
}

// The split button's main half: always opens the merge dialog to review and merge.
function mainAction() {
  if (sync.running) return { label: 'Rebasing...', disabled: true, title: 'Wait for the rebase to finish' };
  const m = METHODS[chosenMethod()];
  return { label: m.button, title: `${m.label} into ${baseBranch()} (review the commit first)`, run: () => openDialog(chosenMethod()) };
}

function renderPRMerge() {
  const pillEl = $('#pr-merge-state');
  const p = pill();
  if (pillEl) {
    pillEl.hidden = !p;
    if (p) {
      pillEl.dataset.tone = p.tone;
      pillEl.textContent = p.text;
      pillEl.title = p.title + (p.act ? '' : ' (click to refresh)');
    }
  }
  const rebaseBtn = $('#pr-rebase-ai');
  if (rebaseBtn) {
    const showRebase = p?.act === 'resolve' && !sync.running;
    rebaseBtn.hidden = !showRebase;
    rebaseBtn.disabled = !!sync.running;
  }
  const group = $('#pr-merge-group');
  if (!group) return;
  const st = state();
  const show = !!info?.canMerge && allowed().length > 0 && st !== 'merged' && st !== 'closed';
  group.hidden = !show;
  if (!show) return;
  const a = mainAction();
  const btn = $('#pr-merge');
  btn.textContent = a.label;
  btn.title = a.title;
  btn.disabled = !!a.disabled;
  btn.dataset.tone = a.tone || '';
  $('#pr-merge-caret').disabled = !!sync.running;
  renderMenu();
}

function onMergeClick() {
  mainAction().run?.();
}

/* ---------- method menu (the split button's caret) ---------- */

function renderMenu() {
  const menu = $('#pr-merge-menu');
  if (!menu) return;
  const cur = chosenMethod();
  const n = info?.commits?.length || 0;
  menu.innerHTML = allowed().map(m =>
    `<button type="button" role="menuitemradio" aria-checked="${m === cur}" data-method="${m}">` +
    `<span class="pr-merge-menu-check">${m === cur ? '&#10003;' : ''}</span>` +
    `<span class="pr-merge-menu-text"><b>${esc(METHODS[m].label)}</b><span>${esc(METHODS[m].explain(n, baseBranch()))}</span></span></button>`
  ).join('');
  for (const b of menu.querySelectorAll('button')) {
    b.addEventListener('click', () => {
      rememberMethod(b.dataset.method);
      toggleMenu(false);
      renderPRMerge();
      const a = mainAction();
      // Picking a method is picking what to do: open the dialog for it when
      // merging is possible, otherwise just remember it for later.
      if (a.run && !a.disabled && a.label === METHODS[b.dataset.method].button) openDialog(b.dataset.method);
    });
  }
}

function toggleMenu(force) {
  const menu = $('#pr-merge-menu');
  if (!menu) return;
  menu.hidden = force === undefined ? !menu.hidden : !force;
}

/* ---------- the merge dialog ---------- */

let method = 'squash';
let drafts = {};      // method -> {title, message}, so switching methods keeps edits
let aiJob = 0;

const mergeModal = () => $('#pr-merge-modal');

function initDialog() {
  const m = mergeModal();
  if (!m) return;
  $('#pr-merge-close')?.addEventListener('click', closeDialog);
  $('#pr-merge-cancel')?.addEventListener('click', closeDialog);
  $('#pr-merge-confirm')?.addEventListener('click', confirmMerge);
  $('#pr-merge-ai')?.addEventListener('click', rewriteWithAI);
  $('#pr-merge-ai-checkbox')?.addEventListener('change', e => {
    if (e.target.checked) {
      $('#pr-merge-ai-row').hidden = false;
      rewriteWithAI();
    } else {
      drafts[method] = { ...(info?.defaults?.[method] || { title: '', message: '' }) };
      fillCommit();
      if (method === 'squash') $('#pr-merge-ai-row').hidden = true;
    }
  });
  $('#pr-merge-reset')?.addEventListener('click', e => {
    e.preventDefault();
    drafts[method] = { ...(info?.defaults?.[method] || { title: '', message: '' }) };
    fillCommit();
    const cb = $('#pr-merge-ai-checkbox');
    if (cb) cb.checked = false;
    if (method === 'squash') {
      const row = $('#pr-merge-ai-row');
      if (row) row.hidden = true;
    }
  });
  $('#pr-merge-instructions-link')?.addEventListener('click', e => {
    e.preventDefault();
    openSettings('ui', 'Git & Diff', 'git.commitMessageInstruction');
  });
  m.addEventListener('click', e => { if (e.target === m) closeDialog(); });
  m.addEventListener('keydown', e => {
    if (e.key === 'Escape') { e.preventDefault(); closeDialog(); }
    else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); confirmMerge(); }
  });
  for (const id of ['#pr-merge-commit-title', '#pr-merge-commit-body']) {
    $(id)?.addEventListener('input', saveDraft);
  }
  const harnessSelect = $('#pr-merge-harness');
  const modelSelect = $('#pr-merge-model');
  if (harnessSelect && modelSelect) registerAgentPicker({ el: m, harnessSelect, modelSelect });
  $('#pr-merge-hint').textContent = keyLabel('Mod+Enter') + ' to merge, Esc to cancel';
}

async function openDialog(m) {
  // Re-read right before showing it: what the dialog says is what merges.
  await refreshMergeStatus();
  if (!m || !allowed().includes(m)) m = chosenMethod();
  if (!info?.canMerge || !allowed().includes(m)) return;
  method = m;
  drafts = {};
  for (const k of Object.keys(info.defaults || {})) drafts[k] = { ...info.defaults[k] };
  const cb = $('#pr-merge-ai-checkbox');
  if (cb) cb.checked = false;
  setPRMergeErr('');
  renderDialog();
  mergeModal().hidden = false;
  (method === 'rebase' ? $('#pr-merge-confirm') : $('#pr-merge-commit-title'))?.focus();
}

function closeDialog() {
  const m = mergeModal();
  if (m) m.hidden = true;
}

function renderDialog() {
  const meta = ctx.meta();
  const n = info.commits?.length || 0;
  $('#pr-merge-title').textContent = `Merge pull request #${meta.number}`;
  $('#pr-merge-refs').innerHTML = `${codeHtml(meta.head)} &rarr; ${codeHtml(baseBranch())}`;

  const methods = $('#pr-merge-methods');
  methods.innerHTML = allowed().map(k =>
    `<button type="button" role="radio" aria-checked="${k === method}" data-method="${k}" class="${k === method ? 'on' : ''}">${esc(METHODS[k].label)}</button>`
  ).join('');
  for (const b of methods.querySelectorAll('button')) {
    b.addEventListener('click', () => {
      saveDraft();
      method = b.dataset.method;
      rememberMethod(method);
      renderPRMerge();
      renderDialog();
    });
  }
  $('#pr-merge-explain').textContent = METHODS[method].explain(n, baseBranch());

  const commitBox = $('#pr-merge-commit');
  commitBox.hidden = method === 'rebase';
  fillCommit();
  const instr = $('#pr-merge-instructions-note');
  if (instr) instr.hidden = !info.hasInstruction;
  const hasHarness = (S.meta?.agents || []).some(h => h.installed) || !!S.meta?.agent;
  const isSquash = method === 'squash';
  const cbWrap = $('#pr-merge-ai-checkbox-wrap');
  if (cbWrap) cbWrap.hidden = !isSquash || !hasHarness;
  if (isSquash) {
    $('#pr-merge-ai-row').hidden = !hasHarness || !$('#pr-merge-ai-checkbox')?.checked;
  } else {
    $('#pr-merge-ai-row').hidden = !hasHarness;
  }

  const list = $('#pr-merge-commit-list');
  list.hidden = method !== 'rebase';
  list.innerHTML = [...(info.commits || [])].reverse().map(c =>
    `<li><code>${esc(c.short)}</code> ${esc(c.subject)}</li>`).join('') || '<li class="empty">No commits</li>';

  renderWarnings();
  $('#pr-merge-confirm').textContent = METHODS[method].confirm;
}

// What would make merging now wrong (blocks Confirm) or merely worth knowing.
function renderWarnings() {
  const w = [];
  let block = '';
  if (sync.running || sync.inProgress) block = 'A rebase is in progress in this checkout. Finish or abort it first.';
  else if (info.rewritten || sync.rewritten) block = 'This checkout was rebased but not pushed. Force-push it first, or the old commits are what merges.';
  else if (info.stale) block = 'The PR has commits this checkout has not seen. Pull and review them before merging.';
  else if (state() === 'conflicts') block = `This PR has conflicts with ${baseBranch()}. Resolve them first.`;
  if (block) w.push(`<div class="pr-merge-warn block">${esc(block)}</div>`);
  if (state() === 'unstable') w.push(`<div class="pr-merge-warn">${esc(info.status.reason || 'Some checks are failing or still running.')}</div>`);
  if (info.unpushed > 0 && !info.rewritten) {
    w.push(`<div class="pr-merge-warn">${pluralize(info.unpushed, 'local commit')} in this checkout ${info.unpushed === 1 ? "isn't" : "aren't"} pushed to the PR and won't be part of the merge.</div>`);
  }
  $('#pr-merge-warnings').innerHTML = w.join('');
  const confirm = $('#pr-merge-confirm');
  confirm.disabled = !!block || !!aiJob;
  confirm.title = block || '';
}

function fillCommit() {
  const d = drafts[method] || { title: '', message: '' };
  $('#pr-merge-commit-title').value = d.title;
  $('#pr-merge-commit-body').value = d.message;
}

function saveDraft() {
  if (method === 'rebase') return;
  drafts[method] = { title: $('#pr-merge-commit-title').value, message: $('#pr-merge-commit-body').value };
}

function setPRMergeErr(msg) {
  const el = $('#pr-merge-err');
  if (!el) return;
  el.hidden = !msg;
  el.textContent = msg;
}

async function confirmMerge() {
  const confirm = $('#pr-merge-confirm');
  if (mergeModal()?.hidden || confirm.disabled) return;
  saveDraft();
  const d = drafts[method] || {};
  if (method !== 'rebase' && !d.title?.trim()) {
    setPRMergeErr('The commit needs a title.');
    $('#pr-merge-commit-title').focus();
    return;
  }
  setPRMergeErr('');
  confirm.disabled = true;
  confirm.textContent = 'Merging...';
  try {
    await apiPostJson('/api/pr/merge', { method, title: d.title || '', message: d.message || '' });
  } catch (e) {
    confirm.textContent = METHODS[method].confirm;
    confirm.disabled = false;
    setPRMergeErr(e.message || 'Merge failed');
    refreshMergeStatus();
    return;
  }
  closeDialog();
  showToast('✓', `${METHODS[method].done} into ${baseBranch()}`);
  await ctx.onMerged(); // re-reads the PR, and with it this status
}

// The harness rewrites the message in place from the PR's commits and diff,
// the user's commit instructions from Settings, and the one-off instruction
// typed here. It only fills the fields: nothing merges until Confirm.
async function rewriteWithAI() {
  if (aiJob || method === 'rebase') return;
  saveDraft();
  const btn = $('#pr-merge-ai');
  const cb = $('#pr-merge-ai-checkbox');
  const cbText = $('.pr-merge-checkbox-text');
  if (cb && method === 'squash') cb.checked = true;
  const d = drafts[method] || {};
  setPRMergeErr('');
  let job;
  try {
    job = await apiPostJson('/api/pr/merge-message', {
      method, instruction: $('#pr-merge-ai-instruction').value.trim(), title: d.title || '', message: d.message || '',
    });
  } catch (e) {
    setPRMergeErr(e.message || 'Could not start the harness');
    if (cb && method === 'squash') cb.checked = false;
    return;
  }
  aiJob = job.id;
  const forMethod = method;
  btn.disabled = true;
  renderWarnings();
  const reset = () => {
    aiJob = 0;
    btn.disabled = false;
    btn.textContent = 'Rewrite with AI';
    if (cbText) cbText.textContent = 'Rewrite commit message with AI';
    if (!mergeModal().hidden) renderWarnings();
  };
  const poll = async () => {
    let j;
    try {
      j = await api('/api/agent/job?id=' + job.id);
    } catch (e) {
      j = { running: false, error: e.message };
    }
    if (j.running) {
      const statusText = `Rewriting... (${Math.round((j.ms || 0) / 1000)}s)`;
      btn.textContent = statusText;
      if (cbText && method === 'squash') cbText.textContent = statusText;
      setTimeout(poll, 700);
      return;
    }
    reset();
    if (j.error) {
      setPRMergeErr((j.harness || 'harness') + ': ' + j.error);
      if (cb && forMethod === 'squash') cb.checked = false;
      return;
    }
    const msg = splitMessage(j.stdout || j.log || '');
    if (!msg.title) {
      setPRMergeErr('The harness returned an empty message.');
      if (cb && forMethod === 'squash') cb.checked = false;
      return;
    }
    drafts[forMethod] = msg;
    if (method === forMethod) fillCommit();
    if (cb && forMethod === 'squash') cb.checked = true;
  };
  setTimeout(poll, 400);
}

// First line is the commit title; the rest, past the blank line, its body.
function splitMessage(text) {
  let t = (text || '').trim();
  const fence = t.match(/```[a-zA-Z0-9_-]*\r?\n([\s\S]*?)\r?\n```/) || t.match(/^```([\s\S]*?)```$/);
  if (fence) t = fence[1].trim();
  if ((t.startsWith('"') && t.endsWith('"')) || (t.startsWith("'") && t.endsWith("'"))) {
    t = t.slice(1, -1).trim();
  }
  const [title, ...rest] = t.split('\n');
  return { title: (title || '').trim(), message: rest.join('\n').trim() };
}
