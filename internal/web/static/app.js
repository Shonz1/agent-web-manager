/* Agent Web Manager — sandboxes in the sidebar, their sessions attached to a
   terminal. A sandbox is the durable thing; sessions come and go inside it. */
'use strict';

const $ = (id) => document.getElementById(id);

const state = {
  sandboxes: [],
  // What the main pane shows: {kind: 'sandbox'|'session', id} or null.
  sel: null,
  term: null,
  fit: null,
  socket: null,
  listSig: null,
  refit: null,
};

// The one breakpoint the layout turns on, shared by the CSS and the behaviour
// that has to match it: the sidebar is a drawer below this width, not a column.
const narrow = window.matchMedia('(max-width: 720px)');

/* ---------- API ---------- */

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 204) return null;
  const text = await res.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch { data = { error: text }; }
  }
  if (!res.ok) throw new Error((data && data.error) || `${res.status} ${res.statusText}`);
  return data;
}

/* ---------- lookups ---------- */

function findSandbox(id) {
  return state.sandboxes.find((b) => b.id === id) || null;
}

function findSession(id) {
  for (const b of state.sandboxes) {
    const s = (b.sessions || []).find((x) => x.id === id);
    if (s) return s;
  }
  return null;
}

function selectedSandbox() {
  if (!state.sel) return null;
  if (state.sel.kind === 'sandbox') return findSandbox(state.sel.id);
  const s = findSession(state.sel.id);
  return s ? findSandbox(s.sandboxId) : null;
}

function selectedSession() {
  return state.sel && state.sel.kind === 'session' ? findSession(state.sel.id) : null;
}

// Titles are assigned by the manager when a session starts, so every client
// calls the same session the same thing.
function sessionLabel(s) {
  return s.title || s.kind;
}

// A session's second line says what it is doing, which the manager's title
// never can: the name an agent gave its own conversation, or — for a shell,
// which names nothing — the last command typed at its prompt.
function sessionSubtitle(s) {
  return s.aiTitle || s.lastCommand || '';
}

/* A live session's dot carries two things at once: that the process is alive,
   and what it looks like to be doing. Activity is absent for a session that is
   not running — nothing in there is doing anything — so the dot falls back to
   saying only what it said before. */
const ACTIVITY_LABEL = {
  busy: 'working',
  waiting: 'waiting for you',
  idle: 'idle',
};

function dotClass(s) {
  return s.activity ? `dot ${s.status} ${s.activity}` : `dot ${s.status}`;
}

function dotTitle(s) {
  const doing = ACTIVITY_LABEL[s.activity];
  return doing ? `${s.status} · ${doing}` : s.status;
}

// The same thing in words, for the places with room for them. A session with
// no activity to report contributes nothing rather than an empty chip.
function activityBadge(s) {
  const label = ACTIVITY_LABEL[s.activity];
  if (!label) return document.createDocumentFragment();
  const el = document.createElement('span');
  el.className = `badge activity ${s.activity}`;
  el.textContent = label;
  return el;
}

// The two lines are stacked rather than swapped: a session must not rename
// itself out from under whoever is looking for it.
function sessionTextEl(s) {
  const wrap = document.createElement('span');
  wrap.className = 'session-text';

  const name = document.createElement('span');
  name.className = 'name';
  name.textContent = sessionLabel(s);
  wrap.append(name);

  const subtitle = sessionSubtitle(s);
  if (subtitle) {
    const sub = document.createElement('span');
    sub.className = 'session-sub';
    sub.textContent = subtitle;
    sub.title = subtitle;
    wrap.append(sub);
  }
  return wrap;
}

/* ---------- sidebar ---------- */

async function refresh() {
  try {
    const data = await api('GET', '/api/sandboxes');
    state.sandboxes = data.sandboxes || [];
  } catch (err) {
    console.error('list sandboxes:', err);
    return;
  }
  // A selection can vanish under us: another tab deleted the sandbox, or the
  // session was ended.
  if (state.sel && !selectionExists(state.sel)) clearSelection();
  renderList();
  renderMain();
}

// listSignature captures everything renderList draws, so the 5s poll can skip
// rebuilding the DOM (and dropping hover/focus) when nothing changed.
function listSignature() {
  const parts = state.sandboxes.map((b) => [
    b.id, b.name, b.agent, b.status, b.workspace,
    (b.sessions || []).map((s) => `${s.id}:${s.title}:${sessionSubtitle(s)}:${s.status}:${s.activity || ''}`).join(','),
  ].join(' '));
  parts.push(state.sel ? `${state.sel.kind}:${state.sel.id}` : '-');
  return parts.join('|');
}

function renderList(force) {
  const sig = listSignature();
  if (!force && sig === state.listSig) return;
  state.listSig = sig;

  const list = $('sandbox-list');
  list.textContent = '';

  if (state.sandboxes.length === 0) {
    const hint = document.createElement('p');
    hint.className = 'empty-hint';
    hint.textContent = 'No sandboxes yet.';
    list.append(hint);
    return;
  }

  for (const b of state.sandboxes) {
    const group = document.createElement('div');
    group.className = 'sandbox-group';

    const item = document.createElement('button');
    item.type = 'button';
    item.className = 'sandbox-item'
      + (state.sel && state.sel.kind === 'sandbox' && state.sel.id === b.id ? ' active' : '');

    const row = document.createElement('div');
    row.className = 'row';

    const dot = document.createElement('span');
    dot.className = `dot sandbox-${b.status}`;
    dot.title = `sandbox ${b.status}`;

    const name = document.createElement('span');
    name.className = 'name';
    name.textContent = b.name;

    const agent = document.createElement('span');
    agent.className = 'badge subtle';
    agent.textContent = b.agent || 'unknown';

    row.append(dot, name, agent);

    const sub = document.createElement('div');
    sub.className = 'sub';
    sub.textContent = shortPath(b.workspace) || 'no workspace mount';
    sub.title = b.workspace || '';

    item.append(row, sub);
    item.addEventListener('click', () => selectSandbox(b.id));
    group.append(item);

    const sessions = b.sessions || [];
    if (sessions.length) {
      const rows = document.createElement('div');
      rows.className = 'session-rows';
      for (const s of sessions) {
        const srow = document.createElement('button');
        srow.type = 'button';
        srow.className = 'session-row'
          + (state.sel && state.sel.kind === 'session' && state.sel.id === s.id ? ' active' : '');

        const sdot = document.createElement('span');
        sdot.className = dotClass(s);
        sdot.title = dotTitle(s);

        srow.append(sdot, sessionTextEl(s));
        srow.addEventListener('click', () => selectSession(s.id));
        rows.append(srow);
      }
      group.append(rows);
    }

    list.append(group);
  }
}

// shortPath keeps the tail of a path, which is the part that identifies a
// workspace; the full path stays available as a tooltip.
function shortPath(p) {
  if (!p) return '';
  const parts = p.split('/').filter(Boolean);
  if (parts.length <= 2) return p;
  return '…/' + parts.slice(-2).join('/');
}

/* ---------- main pane ---------- */

function renderMain() {
  const settings = !!state.sel && state.sel.kind === 'settings';
  const sandbox = selectedSandbox();
  const session = selectedSession();

  $('empty').hidden = !!sandbox || settings;
  $('settings-panel').hidden = !settings;
  $('sandbox-panel').hidden = settings || !sandbox || !!session;
  $('session-panel').hidden = settings || !session;

  if (sandbox && !session) renderSandboxPanel(sandbox);
  if (session) renderSessionPanel(session, sandbox);
}

function renderSandboxPanel(b) {
  $('sb-name').textContent = b.name;

  const status = $('sb-status');
  status.textContent = b.status;
  status.className = `badge sandbox-${b.status}`;

  const agent = $('sb-agent');
  agent.textContent = b.agent || 'unknown agent';

  const bits = [b.workspace];
  if (b.adopted) bits.push('added from sbx');
  if (b.extraWorkspaces && b.extraWorkspaces.length) bits.push(b.extraWorkspaces.join(' '));
  if (b.publish && b.publish.length) bits.push(`ports ${b.publish.join(', ')}`);
  const meta = bits.filter(Boolean).join('  ·  ');
  $('sb-meta').textContent = meta;
  $('sb-meta').title = meta;

  const sessions = b.sessions || [];
  const gone = b.status === 'missing' && b.adopted;

  $('btn-start-agent').disabled = gone;
  $('btn-start-shell').disabled = gone;
  $('btn-stop-sandbox').disabled = b.status === 'missing';

  const list = $('sb-sessions');
  list.textContent = '';

  if (sessions.length === 0) {
    const li = document.createElement('li');
    li.className = 'empty-hint';
    li.textContent = gone
      ? 'This sandbox is gone from sbx and was not created here, so it cannot be recreated.'
      : 'No sessions running. Start an agent or a shell to get a terminal.';
    list.append(li);
    return;
  }

  for (const s of sessions) {
    const li = document.createElement('li');
    const card = document.createElement('button');
    card.type = 'button';
    card.className = 'session-card';

    const dot = document.createElement('span');
    dot.className = dotClass(s);
    dot.title = dotTitle(s);

    const badge = document.createElement('span');
    badge.className = `badge ${s.status}`;
    badge.textContent = s.status === 'exited' && s.exitCode
      ? `exited (${s.exitCode})`
      : s.status;

    card.append(dot, sessionTextEl(s), activityBadge(s), badge);
    card.addEventListener('click', () => selectSession(s.id));
    li.append(card);
    list.append(li);
  }
}

function renderSessionPanel(s, b) {
  $('term-name').textContent = sessionLabel(s);

  const subtitle = sessionSubtitle(s);
  const sub = $('term-sub');
  sub.textContent = subtitle;
  sub.hidden = !subtitle;

  const status = $('term-status');
  status.textContent = s.status === 'exited' && s.exitCode
    ? `exited (${s.exitCode})`
    : s.status;
  status.className = `badge ${s.status}`;

  const activity = $('term-activity');
  activity.textContent = ACTIVITY_LABEL[s.activity] || '';
  activity.className = `badge activity ${s.activity || ''}`;
  activity.hidden = !s.activity;

  const sandbox = $('term-sandbox');
  sandbox.textContent = b ? `${b.name} · sandbox ${b.status}` : s.sandboxName;

  const bits = [b ? b.agent : '', b ? b.workspace : ''];
  if (s.agentArgs && s.agentArgs.length) bits.push(`-- ${s.agentArgs.join(' ')}`);
  if (s.error) bits.push(`error: ${s.error}`);
  const meta = bits.filter(Boolean).join('  ·  ');
  $('term-meta').textContent = meta;
  $('term-meta').title = meta;

  $('btn-interrupt').disabled = !isLive(s);
  $('btn-restart').disabled = isLive(s);
}

function isLive(s) {
  return s.status === 'running' || s.status === 'starting';
}

/* ---------- routing ---------- */

/* The selection is the URL: /sandboxes/{id} or /sessions/{id}, bare / for
   nothing selected. Reloading the page — which a phone does on its own when
   the tab has been in the background — comes back to the same view, and a
   view can be linked to. The server answers these paths with the index page;
   resolving them is this file's job. */

// Set while the selection is being driven by the URL — an initial load or a
// back/forward — so that applying a route does not write it back as a new one.
let applyingRoute = false;

function routeFor(pathname) {
  if (/^\/settings\/?$/.test(pathname)) return { kind: 'settings' };
  const m = /^\/(sandboxes|sessions)\/([A-Za-z0-9._-]+)\/?$/.exec(pathname);
  if (!m) return null;
  return { kind: m[1] === 'sandboxes' ? 'sandbox' : 'session', id: m[2] };
}

function pathFor(sel) {
  if (!sel) return '/';
  if (sel.kind === 'settings') return '/settings';
  return sel.kind === 'sandbox' ? `/sandboxes/${sel.id}` : `/sessions/${sel.id}`;
}

// Settings is the one selection that is not a thing the manager owns, so it
// cannot go missing the way a sandbox or a session can.
function selectionExists(sel) {
  if (!sel) return false;
  if (sel.kind === 'settings') return true;
  return sel.kind === 'sandbox' ? !!findSandbox(sel.id) : !!findSession(sel.id);
}

// replace is for selections the user did not ask for — a restored route that
// no longer exists, a session that ended — where a Back entry would only lead
// somewhere already gone.
function syncURL(replace) {
  if (applyingRoute) return;
  const url = pathFor(state.sel) + location.search;
  if (url === location.pathname + location.search) return;
  history[replace ? 'replaceState' : 'pushState'](null, '', url);
}

// Points the selection at whatever the URL says. Sandbox ids survive a manager
// restart but session ids do not, so a route can name something that is not
// there any more; the URL is then rewritten to what is actually shown.
function applyRoute() {
  const route = routeFor(location.pathname);
  const exists = selectionExists(route);

  applyingRoute = true;
  try {
    if (!exists) {
      clearSelection();
      renderList();
      renderMain();
    } else if (route.kind === 'settings') {
      selectSettings();
    } else if (route.kind === 'sandbox') {
      selectSandbox(route.id);
    } else {
      selectSession(route.id);
    }
  } finally {
    applyingRoute = false;
  }

  if (!exists) syncURL(true);
}

window.addEventListener('popstate', applyRoute);

/* ---------- selection ---------- */

function clearSelection() {
  state.sel = null;
  closeSocket();
  if (state.term) state.term.reset();
  syncURL(true);
}

function selectSandbox(id) {
  state.sel = { kind: 'sandbox', id };
  closeSocket();
  setNav(false);
  syncURL();
  renderList();
  renderMain();
}

function selectSettings() {
  state.sel = { kind: 'settings' };
  closeSocket();
  setNav(false);
  syncURL();
  renderList();
  renderMain();
  // Read afresh on every visit: another tab, or the environment at the last
  // restart, may have changed it since this one last looked.
  loadTelegramSettings();
}

function selectSession(id) {
  if (state.sel && state.sel.kind === 'session' && state.sel.id === id && state.socket) {
    setNav(false);
    syncURL();
    return;
  }
  state.sel = { kind: 'session', id };
  setNav(false);
  syncURL();

  const term = ensureTerm();
  term.reset();

  renderList();
  renderMain();
  connect(id);
  // Focusing raises the soft keyboard, which would cover half the terminal
  // before there is anything to read. On a phone that is the user's call.
  if (!narrow.matches) term.focus();
}

/* ---------- terminal ---------- */

// A phone is only ~48 columns at the desktop size, which is below what the
// agent TUIs lay out for; a point smaller buys back six or so.
function termFontSize() {
  return narrow.matches ? 12 : 13;
}

function ensureTerm() {
  if (state.term) return state.term;

  const term = new Terminal({
    fontFamily: 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
    fontSize: termFontSize(),
    cursorBlink: true,
    scrollback: 10000,
    allowProposedApi: true,
    theme: {
      background: '#0a0d12',
      foreground: '#e6edf3',
      cursor: '#e6edf3',
      selectionBackground: '#2d4f7c',
    },
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open($('terminal'));
  fit.fit();
  enableTouchScroll($('terminal'));

  term.onData((data) => sendSocket({ type: 'input', data }));

  let lastDims = '';
  const onResize = () => {
    applyTermFont();
    try { fit.fit(); } catch { return; /* container not laid out yet */ }
    const dims = `${term.cols}x${term.rows}`;
    if (dims === lastDims) return;
    lastDims = dims;
    sendSocket({ type: 'resize', cols: term.cols, rows: term.rows });
  };
  window.addEventListener('resize', onResize);

  // The terminal panel is hidden until a session is selected, so its real
  // size is only known once it becomes visible.
  new ResizeObserver(onResize).observe($('terminal'));

  state.term = term;
  state.fit = fit;
  state.refit = onResize;
  return term;
}

// Only sets the option; the fit is left to the caller, which is always the
// resize path — changing the font moves no container, so nothing the
// ResizeObserver watches would trigger one.
function applyTermFont() {
  const term = state.term;
  if (!term) return;
  const size = termFontSize();
  if (term.options.fontSize !== size) term.options.fontSize = size;
}

// xterm scrolls on touch drags itself, but only while the program inside the
// PTY has not turned on mouse reporting — and agent TUIs (claude, codex, tmux)
// all turn it on. Those TUIs also draw into the alternate buffer, where there
// is no scrollback for xterm to move, so a phone is left with nothing that
// scrolls at all. A desktop wheel still works there because xterm forwards it
// to the program, so feed the drag back in as wheel events and let xterm's own
// wheel path pick the destination: a mouse report to the TUI, arrow keys in an
// alternate buffer without mouse reporting, or the scrollback under a shell.
// Handled in the capture phase, before xterm sees the touch: a drag scrolls,
// a tap still falls through as a click for the TUI.
function enableTouchScroll(container) {
  const SLOP = 8; // px of movement before a touch counts as a scroll, not a tap
  let tracking = false;
  let dragging = false;
  let startY = 0;
  let lastX = 0;
  let lastY = 0;
  let lastAt = 0;
  let velocity = 0; // px per frame, carried into the fling after touchend
  let fling = 0;

  const stopFling = () => {
    if (fling) cancelAnimationFrame(fling);
    fling = 0;
  };

  // Mouse reports carry the cell the wheel turned over, so aim the event at the
  // finger — clamped to the screen element xterm measures those coords against.
  const wheelBy = (dy, x, y) => {
    const screen = container.querySelector('.xterm-screen');
    if (!screen) return;
    const r = screen.getBoundingClientRect();
    screen.dispatchEvent(new WheelEvent('wheel', {
      deltaY: dy,
      deltaMode: 0, // pixels; xterm converts to rows against its own cell size
      clientX: Math.min(Math.max(x, r.left), r.right - 1),
      clientY: Math.min(Math.max(y, r.top), r.bottom - 1),
      bubbles: true,
      cancelable: true,
    }));
  };

  container.addEventListener('touchstart', (ev) => {
    stopFling();
    tracking = ev.touches.length === 1;
    dragging = false;
    velocity = 0;
    if (!tracking) return;
    startY = lastY = ev.touches[0].clientY;
    lastX = ev.touches[0].clientX;
    lastAt = ev.timeStamp;
  }, { capture: true, passive: true });

  container.addEventListener('touchmove', (ev) => {
    if (!tracking || ev.touches.length !== 1) return;
    const y = ev.touches[0].clientY;
    if (!dragging && Math.abs(y - startY) < SLOP) return;
    dragging = true;

    // Keep xterm (and the synthetic click the browser would fire at the end of
    // the drag) out of it; the gesture is a scroll now.
    ev.stopPropagation();
    if (ev.cancelable) ev.preventDefault();

    const dy = lastY - y;
    const dt = Math.max(1, ev.timeStamp - lastAt);
    lastX = ev.touches[0].clientX;
    lastY = y;
    lastAt = ev.timeStamp;
    velocity = (dy / dt) * 16;
    wheelBy(dy, lastX, y);
  }, { capture: true, passive: false });

  const end = (ev) => {
    if (dragging) ev.stopPropagation();
    tracking = false;
    dragging = false;
    // Coast, so a flick reaches further back than the finger travelled. Only
    // the decay stops it: with a TUI on the far end there is nothing to read
    // back that says the scroll has hit an end.
    if (Math.abs(velocity) < 1) return;
    const step = () => {
      velocity *= 0.94;
      if (Math.abs(velocity) < 0.5) {
        fling = 0;
        return;
      }
      wheelBy(velocity, lastX, lastY);
      fling = requestAnimationFrame(step);
    };
    fling = requestAnimationFrame(step);
  };
  container.addEventListener('touchend', end, { capture: true, passive: true });
  container.addEventListener('touchcancel', end, { capture: true, passive: true });
}

function sendSocket(msg) {
  const sock = state.socket;
  if (sock && sock.readyState === WebSocket.OPEN) {
    sock.send(JSON.stringify(msg));
  }
}

function closeSocket() {
  if (!state.socket) return;
  state.socket.onclose = null;
  state.socket.close();
  state.socket = null;
}

function connect(id) {
  closeSocket();

  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const sock = new WebSocket(`${proto}//${location.host}/api/sessions/${id}/attach`);
  sock.binaryType = 'arraybuffer';
  state.socket = sock;

  const decoder = new TextDecoder();

  sock.onopen = () => {
    // Match the PTY to the browser's terminal size on attach.
    if (state.term) sendSocket({ type: 'resize', cols: state.term.cols, rows: state.term.rows });
  };

  sock.onmessage = (ev) => {
    if (typeof ev.data === 'string') {
      handleControl(JSON.parse(ev.data));
      return;
    }
    state.term.write(decoder.decode(new Uint8Array(ev.data), { stream: true }));
  };

  sock.onclose = () => {
    if (state.socket === sock) state.socket = null;
  };

  sock.onerror = () => {
    if (state.term) state.term.writeln('\r\n\x1b[31m[connection error]\x1b[0m');
  };
}

function handleControl(msg) {
  if (msg.type === 'status' && msg.session) {
    const b = findSandbox(msg.session.sandboxId);
    if (b) {
      const idx = (b.sessions || []).findIndex((s) => s.id === msg.session.id);
      if (idx >= 0) b.sessions[idx] = msg.session;
    }
    renderList();
    renderMain();
  } else if (msg.type === 'error') {
    state.term.writeln(`\r\n\x1b[31m[${msg.message}]\x1b[0m`);
  }
}

/* ---------- sandbox actions ---------- */

async function withSandbox(fn, confirmText) {
  const b = selectedSandbox();
  if (!b) return;
  if (confirmText && !window.confirm(confirmText)) return;
  try {
    await fn(b);
  } catch (err) {
    window.alert(err.message);
  }
  await refresh();
}

async function withSession(fn, confirmText) {
  const s = selectedSession();
  if (!s) return;
  if (confirmText && !window.confirm(confirmText)) return;
  try {
    await fn(s);
  } catch (err) {
    window.alert(err.message);
  }
  await refresh();
}

$('btn-start-shell').addEventListener('click', () =>
  withSandbox(async (b) => {
    const term = ensureTerm();
    const created = await api('POST', `/api/sandboxes/${b.id}/sessions`,
      { kind: 'shell', cols: term.cols, rows: term.rows });
    await refresh();
    selectSession(created.id);
  }));

$('btn-stop-sandbox').addEventListener('click', () =>
  withSandbox((b) => api('POST', `/api/sandboxes/${b.id}/stop`),
    'Stop this sandbox? Every session in it ends; the sandbox keeps its state and can be started again.'));

$('btn-delete-sandbox').addEventListener('click', () =>
  withSandbox(async (b) => {
    await api('DELETE', `/api/sandboxes/${b.id}`);
    clearSelection();
  }, 'Delete this sandbox permanently, along with everything inside it? This cannot be undone.'));

/* ---------- session actions ---------- */

$('btn-interrupt').addEventListener('click', () =>
  withSession((s) => api('POST', `/api/sessions/${s.id}/interrupt`)));

$('btn-restart').addEventListener('click', () =>
  withSession(async (s) => {
    const term = ensureTerm();
    term.reset();
    await api('POST', `/api/sessions/${s.id}/restart`, { cols: term.cols, rows: term.rows });
    connect(s.id);
  }));

$('btn-close-session').addEventListener('click', () =>
  withSession(async (s) => {
    const sandboxId = s.sandboxId;
    await api('DELETE', `/api/sessions/${s.id}`);
    closeSocket();
    if (state.term) state.term.reset();
    state.sel = { kind: 'sandbox', id: sandboxId };
    // Replaced, not pushed: the entry this leaves behind names a session that
    // no longer exists.
    syncURL(true);
  }, 'End this session? The sandbox and everything else running in it are left alone.'));

$('term-sandbox').addEventListener('click', () => {
  const b = selectedSandbox();
  if (b) selectSandbox(b.id);
});

/* ---------- start-agent dialog ---------- */

const agentDialog = $('agent-dialog');

$('btn-start-agent').addEventListener('click', () => {
  const b = selectedSandbox();
  if (!b) return;
  $('agent-error').hidden = true;
  $('agent-note').textContent =
    `Runs ${b.agent || 'the agent'} in ${b.name}, on ${b.workspace || 'the sandbox workspace'}.`;
  agentDialog.showModal();
  $('s-args').focus();
});

$('agent-cancel').addEventListener('click', () => agentDialog.close());
$('agent-submit').addEventListener('click', submitAgent);

$('agent-form').addEventListener('keydown', (ev) => {
  if (ev.key === 'Enter') {
    ev.preventDefault();
    submitAgent();
  }
});

async function submitAgent() {
  const b = selectedSandbox();
  if (!b) return;

  const raw = $('s-args').value.trim();
  const term = ensureTerm();
  const btn = $('agent-submit');
  btn.disabled = true;
  try {
    const created = await api('POST', `/api/sandboxes/${b.id}/sessions`, {
      kind: 'agent',
      agentArgs: raw ? raw.split(/\s+/) : [],
      cols: term.cols,
      rows: term.rows,
    });
    agentDialog.close();
    $('s-args').value = '';
    await refresh();
    selectSession(created.id);
  } catch (err) {
    const box = $('agent-error');
    box.textContent = err.message;
    box.hidden = false;
  } finally {
    btn.disabled = false;
  }
}

/* ---------- create-sandbox dialog ---------- */

const dialog = $('create-dialog');

$('new-sandbox').addEventListener('click', () => {
  $('create-error').hidden = true;
  applyMode();
  dialog.showModal();
  if (createMode() === 'new') $('f-workspace').focus();
  loadSbxSandboxes();
});

/* The dialog creates a fresh sandbox or takes over one that already exists;
   the two modes share no fields. */

function createMode() {
  const picked = document.querySelector('input[name="source"]:checked');
  return picked ? picked.value : 'new';
}

function applyMode() {
  const existing = createMode() === 'existing';

  $('fields-new').hidden = existing;
  $('fields-existing').hidden = !existing;
  // Extra workspaces and ports are fixed at creation time, so they have
  // nothing to say about a sandbox that already exists.
  $('create-advanced').hidden = existing;

  // Hidden inputs still take part in form validation, so the constraint has
  // to move with the mode.
  $('f-workspace').required = !existing;
  $('f-agent').required = !existing;
  $('f-sandbox').required = existing;

  $('create-title').textContent = existing ? 'Add an existing sandbox' : 'New sandbox';
  $('create-error').hidden = true;
  updateSubmitLabel();
}

// The button says what the click will actually do.
function updateSubmitLabel() {
  $('create-submit').textContent =
    createMode() === 'existing' ? 'Add sandbox' : 'Create sandbox';
}

$('f-sandbox').addEventListener('change', updateSubmitLabel);

for (const radio of document.querySelectorAll('input[name="source"]')) {
  radio.addEventListener('change', () => {
    applyMode();
    if (createMode() === 'existing') loadSbxSandboxes();
  });
}

$('f-sandbox-refresh').addEventListener('click', () => loadSbxSandboxes());

// Opening the dialog and switching to "existing" both ask for the list, so
// loads overlap; only the newest one is allowed to touch the select.
let sandboxLoad = 0;

// Names this manager already has an entry for, from the last list load. They
// cannot be added a second time, and the check has to survive the select being
// bypassed by a keyboard submit.
let addedSbx = new Set();

// Every sandbox sbx knows about is listed, including ones this manager already
// has an entry for — those stay visible but unselectable, so it is clear why
// they are not on offer rather than them simply being missing.
async function loadSbxSandboxes() {
  const sel = $('f-sandbox');
  const hint = $('f-sandbox-hint');
  const previous = sel.value;
  const load = ++sandboxLoad;

  let boxes;
  try {
    const data = await api('GET', '/api/sbx/sandboxes');
    boxes = data.sandboxes || [];
  } catch (err) {
    if (load !== sandboxLoad) return;
    sel.textContent = '';
    sel.disabled = true;
    hint.textContent = `Could not list sandboxes: ${err.message}`;
    return;
  }
  if (load !== sandboxLoad) return;

  addedSbx = new Set(boxes.filter((b) => b.managed).map((b) => b.name));
  sel.textContent = '';

  if (boxes.length === 0) {
    sel.disabled = true;
    hint.textContent = 'sbx reports no sandboxes at all. Create one under “Create one”.';
    return;
  }

  const free = boxes.filter((b) => !b.managed);
  sel.disabled = free.length === 0;
  hint.textContent = free.length === 0
    ? 'Every sandbox sbx knows about is already listed here. Create one under “Create one”.'
    : 'Takes the sandbox as it is — its agent and workspaces come from the sandbox itself.';

  for (const b of boxes) {
    const opt = document.createElement('option');
    opt.value = b.name;
    opt.disabled = b.managed;
    const where = (b.workspaces && b.workspaces.length) ? ` · ${shortPath(b.workspaces[0])}` : '';
    const held = b.managed ? ' · already added' : '';
    opt.textContent = `${b.name} — ${b.agent || 'unknown agent'} (${b.status})${where}${held}`;
    sel.append(opt);
  }

  // A disabled option is still what a select falls back to when it is first
  // in the list, so the selection is pinned to one that can actually be added
  // — and cleared outright when none can.
  sel.value = '';
  if (free.some((b) => b.name === previous)) sel.value = previous;
  else if (free.length > 0) sel.value = free[0].name;
  updateSubmitLabel();
}

$('create-cancel').addEventListener('click', () => dialog.close());

$('create-submit').addEventListener('click', submitCreate);

$('create-form').addEventListener('keydown', (ev) => {
  if (ev.key === 'Enter' && ev.target.tagName !== 'TEXTAREA') {
    ev.preventDefault();
    submitCreate();
  }
});

function lines(id) {
  return $(id).value.split('\n').map((v) => v.trim()).filter(Boolean);
}

async function submitCreate() {
  const form = $('create-form');
  if (!form.reportValidity()) return;

  const existing = createMode() === 'existing';

  // A disabled select is skipped by form validation, so the empty case needs
  // its own check.
  if (existing && !$('f-sandbox').value) {
    const box = $('create-error');
    box.textContent = 'Pick a sandbox to add.';
    box.hidden = false;
    return;
  }

  // The list still shows sandboxes this manager has an entry for, and the
  // server rejects a second one anyway — this says so without the round trip.
  if (existing && addedSbx.has($('f-sandbox').value)) {
    const box = $('create-error');
    box.textContent = 'That sandbox is already listed here.';
    box.hidden = false;
    return;
  }

  const path = existing ? '/api/sandboxes/adopt' : '/api/sandboxes';
  const body = existing ? {
    name: $('f-sandbox').value,
  } : {
    agent: $('f-agent').value,
    workspace: $('f-workspace').value.trim(),
    name: $('f-name').value.trim(),
    extraWorkspaces: lines('f-extra'),
    publish: lines('f-publish'),
  };

  const btn = $('create-submit');
  const label = btn.textContent;
  btn.disabled = true;
  // Creating pulls an agent image the first time, which is minutes, not
  // milliseconds.
  if (!existing) btn.textContent = 'Creating…';
  try {
    const created = await api('POST', path, body);
    dialog.close();
    $('f-name').value = '';
    await refresh();
    selectSandbox(created.id);
  } catch (err) {
    const box = $('create-error');
    box.textContent = err.message;
    box.hidden = false;
  } finally {
    btn.disabled = false;
    btn.textContent = label;
  }
}

/* ---------- folder picker ---------- */

// The page cannot learn a real host path from a native file input, so folders
// are browsed through the manager's own directory listing API.
const browse = {
  dialog: $('browse-dialog'),
  path: null,   // directory currently listed
  onPick: null, // called with the chosen path, and only then
};

// The picker takes a callback rather than returning a promise: dismissing the
// dialog with Esc bypasses every button, and a promise nobody resolves would
// wedge the caller.
function openBrowser(startPath, onPick) {
  browse.onPick = onPick;
  $('browse-error').hidden = true;
  browse.dialog.showModal();
  loadDir(startPath || '');
}

function closeBrowser(picked) {
  const onPick = browse.onPick;
  browse.onPick = null;
  browse.dialog.close();
  if (picked && onPick) onPick(picked);
}

async function loadDir(path) {
  const params = new URLSearchParams({ path });
  if ($('browse-show-hidden').checked) params.set('hidden', '1');

  let data;
  try {
    data = await api('GET', `/api/fs/dirs?${params}`);
  } catch (err) {
    const box = $('browse-error');
    box.textContent = err.message;
    box.hidden = false;
    return;
  }
  $('browse-error').hidden = true;

  browse.path = data.path;
  $('browse-path').value = data.path;
  $('browse-up').disabled = !data.parent;
  $('browse-up').dataset.parent = data.parent || '';
  $('browse-home').dataset.home = data.home || '';

  const list = $('browse-list');
  list.textContent = '';

  if (data.entries.length === 0) {
    const li = document.createElement('li');
    li.className = 'browse-empty';
    li.textContent = 'No sub-folders here.';
    list.append(li);
  }

  for (const entry of data.entries) {
    const li = document.createElement('li');
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'browse-item';
    btn.title = entry.path;

    const name = document.createElement('span');
    name.className = 'name';
    name.textContent = entry.name;
    btn.append(name);

    if (entry.repo) {
      const tag = document.createElement('span');
      tag.className = 'badge subtle';
      tag.textContent = 'git';
      btn.append(tag);
    }

    // Clicking walks into the folder and puts it in the path bar, which is
    // what "Use this folder" selects — so one click both navigates and picks.
    btn.addEventListener('click', () => loadDir(entry.path));

    li.append(btn);
    list.append(li);
  }

  if (data.truncated) {
    const li = document.createElement('li');
    li.className = 'browse-empty';
    li.textContent = 'Too many folders to show; type a path above to narrow it down.';
    list.append(li);
  }

  list.scrollTop = 0;
}

$('browse-up').addEventListener('click', (ev) => loadDir(ev.currentTarget.dataset.parent));
$('browse-home').addEventListener('click', (ev) => loadDir(ev.currentTarget.dataset.home || '~'));
$('browse-show-hidden').addEventListener('change', () => loadDir(browse.path || ''));
$('browse-cancel').addEventListener('click', () => closeBrowser(null));
$('browse-select').addEventListener('click', () => closeBrowser($('browse-path').value.trim()));

$('browse-path').addEventListener('keydown', (ev) => {
  if (ev.key !== 'Enter') return;
  ev.preventDefault();
  loadDir(ev.target.value.trim());
});

$('f-workspace-browse').addEventListener('click', () => {
  openBrowser($('f-workspace').value.trim(), (picked) => {
    $('f-workspace').value = picked;
    $('f-workspace').focus();
  });
});

$('f-extra-browse').addEventListener('click', () => {
  const box = $('f-extra');
  const last = box.value.split('\n').pop().replace(/:ro$/, '').trim();
  openBrowser(last, (picked) => {
    box.value = lines('f-extra').concat(picked).join('\n') + '\n';
    box.focus();
  });
});

/* ---------- notifications ---------- */

/* The manager decides which moments are worth interrupting someone for and
   streams them from /api/events; this only has to show them. Nothing here
   works out for itself when an agent has finished — a page that reimplemented
   that would disagree with the Telegram bot watching the same manager, and
   the two are meant to say the same thing at the same time. */

const NOTIFY_KEY = 'awm.notify';

const notify = {
  enabled: localStorage.getItem(NOTIFY_KEY) === 'on',
  source: null,
  retry: null,
};

const NOTIFY_TEXT = {
  attention: { icon: '⚠️', what: 'needs you' },
  done: { icon: '✅', what: 'finished' },
};

/* The Notification API is only handed to a secure context. The manager is one
   on localhost and is not one over plain HTTP on a LAN address — which is the
   phone case, and so the case where this would have mattered most. Nothing on
   this page can fix that, so the button says so rather than failing mutely. */
function notifySupported() {
  return typeof Notification !== 'undefined';
}

function browserNotifyOn() {
  return notify.enabled && notifySupported() && Notification.permission === 'granted';
}

function renderBrowserNotify() {
  const btn = $('browser-notify-toggle');
  const state = $('browser-notify-state');

  if (!notifySupported()) {
    btn.disabled = true;
    btn.textContent = 'Unavailable';
    state.textContent =
      'This browser only gives a page notifications over HTTPS or on localhost.';
    renderSettingsLink();
    return;
  }
  if (Notification.permission === 'denied') {
    btn.disabled = true;
    btn.textContent = 'Blocked';
    state.textContent =
      'This browser is blocking notifications for this site; allow them in its site settings.';
    renderSettingsLink();
    return;
  }

  const on = browserNotifyOn();
  btn.disabled = false;
  btn.textContent = on ? 'Turn off' : 'Turn on';
  btn.classList.toggle('primary', !on);
  state.textContent = on
    ? 'A desktop notification, while this page is open somewhere.'
    : 'Off. Nothing is shown while this tab is in the background.';
  renderSettingsLink();
}

$('browser-notify-toggle').addEventListener('click', async () => {
  if (!notifySupported()) return;

  if (notify.enabled) {
    notify.enabled = false;
    localStorage.setItem(NOTIFY_KEY, 'off');
    renderBrowserNotify();
    return;
  }

  // Permission can only be asked for from a gesture, which is this click.
  let permission = Notification.permission;
  if (permission === 'default') {
    try {
      permission = await Notification.requestPermission();
    } catch {
      permission = 'denied';
    }
  }
  notify.enabled = permission === 'granted';
  localStorage.setItem(NOTIFY_KEY, notify.enabled ? 'on' : 'off');
  renderBrowserNotify();
});

/* ---------- settings page ---------- */

// What the server last said about Telegram. The token is never part of it.
let telegram = { enabled: false, fromEnv: false, chatId: '', bot: '' };

$('open-settings').addEventListener('click', () => selectSettings());

// The sidebar button carries whether anything is on at all, because a
// notification that is silently not arriving looks exactly like a quiet
// afternoon.
function renderSettingsLink() {
  const btn = $('open-settings');
  const on = browserNotifyOn() || telegram.enabled;
  btn.classList.toggle('on', on);
  btn.textContent = on ? '🔔 Settings' : '⚙ Settings';
  btn.title = on ? 'Notifications are on' : 'Notifications are off';
}

async function loadTelegramSettings() {
  try {
    telegram = await api('GET', '/api/settings/telegram');
  } catch (err) {
    console.error('load telegram settings:', err);
    return;
  }
  renderTelegram();
}

function renderTelegram() {
  const state = $('tg-state');
  const fields = $('tg-fields');
  const token = $('tg-token');

  state.classList.toggle('on', telegram.enabled);

  if (telegram.fromEnv) {
    state.textContent = telegram.chatId
      ? `Set by the environment, sending to chat ${telegram.chatId}. Change AWM_TELEGRAM_TOKEN or AWM_TELEGRAM_CHAT_ID and restart the manager to alter it.`
      : 'Set by the environment.';
  } else if (telegram.enabled) {
    const who = telegram.bot ? `as @${telegram.bot} ` : '';
    const button = telegram.linkBase
      ? `Notifications link back to ${telegram.linkBase}.`
      : 'Notifications have no button: no address to link back to is set.';
    state.textContent = `Connected ${who}to chat ${telegram.chatId}. ${button}`;
  } else {
    state.textContent = 'Not set up. Nothing is sent anywhere.';
  }

  // The environment is deliberately not editable from here: writing a file
  // that the environment would go on shadowing is a setting that appears to
  // work and does nothing.
  for (const input of fields.querySelectorAll('input')) input.disabled = telegram.fromEnv;
  fields.hidden = telegram.fromEnv;

  // The token is never sent back, so it cannot be pre-filled — but it does not
  // have to be typed again either, unless it is the thing being changed.
  token.placeholder = telegram.enabled ? 'Leave empty to keep the current one' : '123456789:AAE…';
  if (!$('tg-chat').value) $('tg-chat').value = telegram.chatId || '';

  // The address this page was reached at is the best guess there is, and it is
  // exactly right when the setting up is done from the device that will be
  // reading the notifications.
  const link = $('tg-link');
  if (!link.value) link.value = telegram.linkBase || '';
  link.placeholder = location.origin;

  $('tg-save').disabled = telegram.fromEnv;
  $('tg-disable').disabled = telegram.fromEnv || !telegram.enabled;
  $('tg-test').disabled = !telegram.enabled;

  renderSettingsLink();
}

function telegramMessage(err, ok) {
  const errBox = $('tg-error');
  const okBox = $('tg-ok');
  errBox.textContent = err || '';
  errBox.hidden = !err;
  okBox.textContent = ok || '';
  okBox.hidden = !ok;
}

// Every one of these talks to Telegram before it answers, so the button has to
// say that something is happening rather than appearing to have been missed.
async function withTelegramButton(id, busyLabel, fn) {
  const btn = $(id);
  const label = btn.textContent;
  btn.disabled = true;
  btn.textContent = busyLabel;
  telegramMessage('', '');
  try {
    await fn();
  } catch (err) {
    telegramMessage(err.message, '');
  } finally {
    btn.textContent = label;
    renderTelegram();
  }
}

$('tg-save').addEventListener('click', () =>
  withTelegramButton('tg-save', 'Connecting…', async () => {
    telegram = await api('PUT', '/api/settings/telegram', {
      token: $('tg-token').value.trim(),
      chatId: $('tg-chat').value.trim(),
      linkBase: $('tg-link').value.trim(),
    });
    // Held no longer than it takes to save it.
    $('tg-token').value = '';
    telegramMessage('', 'Connected. A message has been sent to the chat.');
  }));

$('tg-test').addEventListener('click', () =>
  withTelegramButton('tg-test', 'Sending…', async () => {
    await api('POST', '/api/settings/telegram/test');
    telegramMessage('', 'Test message sent.');
  }));

$('tg-disable').addEventListener('click', () => {
  if (!window.confirm('Turn Telegram notifications off and forget the bot token?')) return;
  return withTelegramButton('tg-disable', 'Turning off…', async () => {
    telegram = await api('DELETE', '/api/settings/telegram');
    $('tg-chat').value = '';
    $('tg-link').value = '';
    telegramMessage('', 'Turned off. The token has been removed from this machine.');
  });
});

/* The stream is connected whether or not notifications are switched on: an
   event is also the earliest anything here learns that a session has moved,
   and the list is otherwise up to five seconds behind. */
function connectEvents() {
  if (notify.source) return;

  const source = new EventSource('/api/events');
  notify.source = source;

  const onEvent = (ev) => {
    let data;
    try { data = JSON.parse(ev.data); } catch { return; }
    showNotification(data);
    refresh();
  };
  for (const kind of Object.keys(NOTIFY_TEXT)) source.addEventListener(kind, onEvent);

  source.onerror = () => {
    // EventSource reconnects on its own after a dropped connection, but gives
    // up for good on an HTTP error — so a manager that was restarting, or a
    // laptop that was asleep, has to be picked back up here.
    if (source.readyState !== EventSource.CLOSED) return;
    source.close();
    if (notify.source === source) notify.source = null;
    clearTimeout(notify.retry);
    notify.retry = setTimeout(connectEvents, 5000);
  };
}

function showNotification(ev) {
  if (!notify.enabled || !notifySupported() || Notification.permission !== 'granted') return;

  // Looking straight at the session is the one case where a notification says
  // nothing the screen has not already said.
  const looking = document.visibilityState === 'visible'
    && state.sel && state.sel.kind === 'session' && state.sel.id === ev.sessionId;
  if (looking) return;

  const text = NOTIFY_TEXT[ev.kind];
  if (!text) return;

  let note;
  try {
    note = new Notification(`${text.icon} ${ev.title} ${text.what}`, {
      body: [ev.detail, ev.sandboxName].filter(Boolean).join('  ·  '),
      // One notification per session: a later one about the same agent
      // replaces the earlier one rather than stacking up behind it.
      tag: `session-${ev.sessionId}`,
      renotify: true,
    });
  } catch {
    // Some browsers only allow this from a service worker, and say so by
    // throwing. There is nothing to fall back to.
    return;
  }

  note.addEventListener('click', async () => {
    window.focus();
    note.close();
    // The notification can outlive what it is about, and the list behind this
    // page may not have caught up either way.
    await refresh();
    if (findSession(ev.sessionId)) selectSession(ev.sessionId);
  });
}

/* ---------- mobile drawer ---------- */

/* Below the breakpoint the sidebar is an overlay, so it has to be opened and
   dismissed explicitly. It stays a plain column above it, where every call
   here is a no-op. */

const menuButtons = document.querySelectorAll('.menu-btn');

function navOpen() {
  return document.body.classList.contains('nav-open');
}

function setNav(open) {
  document.body.classList.toggle('nav-open', open);
  for (const btn of menuButtons) btn.setAttribute('aria-expanded', String(open));
  // Translated off-screen, the drawer is still in the tab order and still
  // reachable by a screen reader; inert is what actually takes it out.
  document.querySelector('.sidebar').inert = narrow.matches && !open;
}

for (const btn of menuButtons) {
  btn.addEventListener('click', () => setNav(!navOpen()));
}

$('scrim').addEventListener('click', () => setNav(false));

document.addEventListener('keydown', (ev) => {
  if (ev.key === 'Escape' && navOpen()) setNav(false);
});

// Rotating a phone, or dragging a desktop window narrow, crosses the
// breakpoint in either direction; the drawer state does not survive it. The
// terminal is refit from here as well as from the window's own resize, since
// a viewport can change without either event being the one that fires.
narrow.addEventListener('change', () => {
  setNav(false);
  if (state.refit) state.refit();
});

/* ---------- terminal key bar ---------- */

// Sent straight to the PTY rather than through xterm: these have no keyboard
// event to synthesise from a tap.
const TERM_KEYS = {
  esc: '\x1b',
  tab: '\t',
  'ctrl-c': '\x03',
  up: '\x1b[A',
  down: '\x1b[B',
  right: '\x1b[C',
  left: '\x1b[D',
};

for (const btn of $('term-keys').querySelectorAll('button')) {
  // Taking focus would dismiss the soft keyboard, and these keys are most
  // useful mid-sentence.
  btn.addEventListener('pointerdown', (ev) => ev.preventDefault());
  btn.addEventListener('click', () => {
    const seq = TERM_KEYS[btn.dataset.key];
    if (seq) sendSocket({ type: 'input', data: seq });
  });
}

/* ---------- bootstrap ---------- */

async function loadAgents() {
  try {
    const { agents } = await api('GET', '/api/agents');
    const sel = $('f-agent');
    for (const a of agents) {
      const opt = document.createElement('option');
      opt.value = a;
      opt.textContent = a;
      sel.append(opt);
    }
    sel.value = agents.includes('claude') ? 'claude' : agents[0];
  } catch (err) {
    console.error('load agents:', err);
  }
}

async function pollHealth() {
  const el = $('health');
  try {
    const h = await api('GET', '/api/health');
    el.className = 'health ' + (h.ok ? 'ok' : 'bad');
    el.title = h.ok ? 'sbx is available' : h.error;
  } catch {
    el.className = 'health bad';
    el.title = 'manager unreachable';
  }
}

(async function init() {
  setNav(false);
  ensureTerm();
  renderBrowserNotify();
  loadTelegramSettings();
  connectEvents();
  await loadAgents();
  await refresh();
  // Only once the sandboxes are in can the URL be resolved to a selection.
  applyRoute();
  await pollHealth();

  // With nothing selected the main pane is a dead end on a phone — the list,
  // and the only button that creates anything, are both behind the drawer.
  if (narrow.matches && !state.sel) setNav(true);

  setInterval(refresh, 5000);
  setInterval(pollHealth, 30000);
})();
