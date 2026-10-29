// web/src/stream.js
//
// The tab's one EventSource. Browsers allow six HTTP/1.1 connections per
// host, shared by every tab, and each open EventSource holds one for good.
// Per-feature streams (thread list, open thread, review workflow, stage
// thread) used to add up until no slot was left for ordinary requests, at
// which point every fetch queued forever and the UI went blank. So every
// live feed rides on /api/stream: features say which topics they want and
// subscribe to event names; changing topics reopens the connection, and
// the server resends a snapshot of each topic on every open.

const handlers = new Map(); // event name -> Set<fn(data)>
const threadRefs = new Map(); // thread id -> number of watchers
const topics = { threads: false, review: false };

let es = null;
let wanted = false; // the owner (gitstream) wants the stream open
let lastTrafficAt = Date.now();
let reopenQueued = false;

export const streamTrafficAt = () => lastTrafficAt;
export const streamOpen = () => !!es && es.readyState === EventSource.OPEN;

function markTraffic() {
  lastTrafficAt = Date.now();
}

function dispatch(e) {
  markTraffic();
  const fns = handlers.get(e.type);
  if (!fns || !fns.size) return;
  let data;
  try {
    data = JSON.parse(e.data);
  } catch (_) {
    return; // drop malformed frame
  }
  for (const fn of fns) {
    const fail = err => console.error('stream handler for ' + e.type + ' failed', err);
    try {
      const r = fn(data);
      if (r && typeof r.catch === 'function') r.catch(fail);
    } catch (err) {
      fail(err);
    }
  }
}

// onStream registers fn for an event name and returns an unsubscribe.
export function onStream(name, fn) {
  let fns = handlers.get(name);
  if (!fns) {
    handlers.set(name, (fns = new Set()));
    if (es) es.addEventListener(name, dispatch);
  }
  fns.add(fn);
  return () => fns.delete(fn);
}

export function setStreamTopic(name, on) {
  if (topics[name] === !!on) return;
  topics[name] = !!on;
  queueReopen();
}

// watchThread asks for one thread's events and returns a release function.
export function watchThread(id) {
  if (!id) return () => {};
  threadRefs.set(id, (threadRefs.get(id) || 0) + 1);
  if (threadRefs.get(id) === 1) queueReopen();
  let released = false;
  return () => {
    if (released) return;
    released = true;
    const n = (threadRefs.get(id) || 1) - 1;
    if (n > 0) {
      threadRefs.set(id, n);
      return;
    }
    threadRefs.delete(id);
    queueReopen();
  };
}

function streamUrl() {
  const u = new URL('api/stream', document.baseURI || location.href);
  if (topics.threads) u.searchParams.set('threads', '1');
  if (topics.review) u.searchParams.set('review', '1');
  for (const id of threadRefs.keys()) u.searchParams.append('thread', id);
  return u.href;
}

// Topic changes in one task (close a thread, open another) cost one reopen.
function queueReopen() {
  if (reopenQueued) return;
  reopenQueued = true;
  queueMicrotask(() => {
    reopenQueued = false;
    if (wanted && (!es || es.url !== streamUrl())) {
      closeStream();
      openStream();
    }
  });
}

// openStream connects if not already connected; onError fires when the
// connection drops, after it has been closed.
let onErrorCb = null;
export function openStream(onError) {
  if (onError) onErrorCb = onError;
  wanted = true;
  if (es && es.readyState !== EventSource.CLOSED) return;
  es = new EventSource(streamUrl());
  markTraffic();
  es.onopen = markTraffic;
  for (const name of handlers.keys()) es.addEventListener(name, dispatch);
  es.addEventListener('ping', markTraffic);
  const mine = es;
  es.onerror = () => {
    if (es !== mine) return;
    closeStream();
    wanted = true;
    onErrorCb?.();
  };
}

export function closeStream() {
  wanted = false;
  if (es) {
    es.close();
    es = null;
  }
}
