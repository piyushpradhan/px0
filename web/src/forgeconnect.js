// web/src/forgeconnect.js
// The Connect dialog: getting a forge token without leaving px0 or reading
// docs first. It opens wherever a token is missing (the PR bar's "No token"
// note, the git panel's nudge, or any comment, review or merge tried
// without one) and walks through it for the forge and host at hand: the
// page that creates the token, what to pick there, and a field to paste it
// into. The server checks the token against the forge before keeping it
// (/api/forge/connect) and a running review uses it at once -- no restart.
// It knows nothing forge-specific itself: everything comes from
// S.meta.forge, which the server builds from the provider's TokenSetup.
import { $, S, esc, apiPostJson, keyLabel } from './state.js';
import { showToast } from './ui.js';
import { emit } from './bus.js';
import { openSettings } from './settings.js';

const connectModal = () => $('#forge-connect-modal');

const SOURCES = {
  settings: 'a token saved in px0 Settings',
  env: 'an environment variable',
  gh: 'the GitHub CLI (gh)',
  glab: 'the GitLab CLI (glab)',
  session: 'a token used for this session only',
};

let wired = false;

function wireForgeConnect() {
  if (wired) return;
  wired = true;
  const m = connectModal();
  $('#forge-connect-close')?.addEventListener('click', close);
  $('#forge-connect-cancel')?.addEventListener('click', close);
  $('#forge-connect-submit')?.addEventListener('click', submitForgeConnect);
  $('#forge-connect-settings')?.addEventListener('click', e => {
    e.preventDefault();
    const f = S.meta?.forge?.info;
    close();
    if (f) openSettings('ui', f.tokenCategory, f.tokenKey);
  });
  m.addEventListener('click', e => { if (e.target === m) close(); });
  $('#forge-connect-hint').textContent = keyLabel('Enter') + ' to connect, Esc to cancel';
  m.addEventListener('keydown', e => {
    if (e.key === 'Escape') { e.preventDefault(); close(); }
    else if (e.key === 'Enter' && (e.target.tagName === 'INPUT' || e.metaKey || e.ctrlKey)) { e.preventDefault(); submitForgeConnect(); }
  });
}

/**
 * Opens the dialog for this workspace's forge. reason says why it opened
 * ("to post comments"), so the first line answers "why am I seeing this".
 * Falls back to the Settings page when the forge is unknown.
 * @param {string} [reason]
 */
export function openForgeConnect(reason = '') {
  const forge = S.meta?.forge;
  if (!forge || !connectModal()) {
    openSettings('ui', 'GitHub', 'github.token');
    return;
  }
  wireForgeConnect();
  const { info, setup, host, hasToken, source } = forge;
  const pr = !!S.meta?.pr;
  $('#forge-connect-title').textContent = `Connect ${info.label}`;
  $('#forge-connect-host').textContent = host || '';

  let why = hasToken
    ? `px0 is using ${SOURCES[source] || 'a token'} for ${esc(host)}. Paste a new one to replace it.`
    : `${esc(setup.readonly || '')}`;
  if (reason && !hasToken) why = `A ${esc(info.label)} token is needed ${esc(reason)}. ` + why;
  $('#forge-connect-why').innerHTML = why;

  const steps = [
    `Open <a href="${esc(setup.url)}" target="_blank" rel="noopener noreferrer" class="pr-merge-link">${esc(info.label)}'s token page on ${esc(host)}</a>.`,
    ...setup.steps.map(esc),
    'Paste it below.',
  ];
  $('#forge-connect-steps').innerHTML = steps.map(s => `<li>${s}</li>`).join('');

  const extra = setup.extra;
  $('#forge-connect-extra').hidden = !extra;
  if (extra) {
    $('#forge-connect-extra-label').textContent = extra.label;
    $('#forge-connect-extra-input').placeholder = extra.placeholder;
    $('#forge-connect-extra-hint').textContent = extra.hint;
  }

  const alts = [`set ${codeTag(setup.env)}`];
  if (setup.cli) alts.push(`run ${codeTag(setup.cli)}`);
  $('#forge-connect-alt').innerHTML = `Or ${alts.join(' or ')} in your shell, then restart px0.` +
    (setup.hostKey ? ` px0 sends the token only to ${esc(host)}.` : '');
  $('#forge-connect-check').textContent = pr
    ? 'px0 checks the token by reading this pull request with it before saving it.'
    : 'Saved to your px0 settings. It is checked the next time you open a pull request.';

  $('#forge-connect-token').value = '';
  $('#forge-connect-extra-input').value = '';
  setForgeConnectErr('');
  setForgeConnectBusy(false);
  connectModal().hidden = false;
  $('#forge-connect-token').focus();
}

const codeTag = s => '<code>' + esc(s || '') + '</code>';

function close() {
  const m = connectModal();
  if (m) m.hidden = true;
}

function setForgeConnectErr(msg) {
  const el = $('#forge-connect-err');
  el.hidden = !msg;
  el.textContent = msg;
}

function setForgeConnectBusy(busy) {
  const btn = $('#forge-connect-submit');
  btn.disabled = busy;
  btn.textContent = busy ? 'Checking...' : 'Connect';
}

async function submitForgeConnect() {
  const token = $('#forge-connect-token').value.trim();
  if (!token) {
    setForgeConnectErr('Paste the token first.');
    $('#forge-connect-token').focus();
    return;
  }
  setForgeConnectErr('');
  setForgeConnectBusy(true);
  let res;
  try {
    res = await apiPostJson('/api/forge/connect', { token, extra: $('#forge-connect-extra-input').value.trim() });
  } catch (e) {
    setForgeConnectBusy(false);
    setForgeConnectErr(e.message || 'Could not connect');
    return;
  }
  const forge = S.meta.forge;
  const label = forge.info.label;
  forge.hasToken = true;
  forge.source = res.saved ? 'settings' : 'session';
  close();
  let msg = `Connected to ${label}`;
  if (!res.saved) msg += ' for this session (a token for another host is already saved)';
  if (res.validated && !res.writeAccess) msg += '. Read access only: Approve and Merge need push access to this repository';
  showToast('✓', msg, res.validated && !res.writeAccess ? 6000 : 3000);
  emit('forge:connected', res);
}
