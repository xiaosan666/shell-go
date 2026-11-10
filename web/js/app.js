import { session, login, restoreSession, clearSession, api, ApiError, sealFrame, openFrame } from './api.js';
import { TermView, THEMES } from './terminal.js';
import { initFileBrowser, openFileBrowser, setTermLocator } from './files.js';
import { initReplay } from './replay.js';
import { b64ToBytes, bytesToB64 } from './secure.js';

const $ = (id) => document.getElementById(id);
const enc = new TextEncoder();

// 一个标签（tab）可以切成多个窗格（pane），每个窗格是独立的终端会话。
// 会话（session）承载终端视图与传输通道，标签只负责布局与标题。
// 1 个窗格单屏；2 个窗格左右/上下；3~4 个窗格自动切成四宫格。
const MAX_PANES = 4;

// 角色中文名，仅用于界面展示；真正的权限判定在服务端。
const ROLE_LABEL = { admin: '管理员', operator: '运维', viewer: '只读审计' };

const state = {
  tabs: [],
  active: null, // 当前标签
  activeSession: null, // 当前窗格里的会话
  theme: localStorage.getItem('shellgo.theme') || 'dark',
  fontSize: Number(localStorage.getItem('shellgo.font') || 14),
  seq: 1,
  loggingOut: false,
};

// ---------- 登录 ----------

function bindLogin() {
  const form = $('login-form');
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const btn = $('login-btn');
    const err = $('login-err');
    const user = $('login-user').value.trim();
    const pass = $('login-pass').value;
    if (!user || !pass) return;
    err.hidden = true;
    btn.disabled = true;
    btn.textContent = '正在建立加密通道…';
    try {
      const d = await login(user, pass);
      document.title = `${d.title || 'Shell Go'} · ${user}`;
      showApp();
      await openInitialTabs();
    } catch (ex) {
      err.textContent = ex.message || '登录失败';
      err.hidden = false;
      $('login-pass').value = '';
      $('login-pass').focus();
    } finally {
      btn.disabled = false;
      btn.textContent = '登录';
    }
  });
}

function showLogin(message) {
  state.tabs.forEach((t) => destroyTab(t, true));
  state.tabs = [];
  state.active = null;
  state.activeSession = null;
  $('app').hidden = true;
  $('login-view').hidden = false;
  $('login-pass').value = '';
  $('login-user').focus();
  if (message) {
    $('login-err').textContent = message;
    $('login-err').hidden = false;
  }
}

function showApp() {
  $('login-view').hidden = true;
  $('app').hidden = false;
  $('st-user').textContent = session.user || '—';
  applyCaps();
  loadSysInfo();
  if (!state.clockTimer) {
    const tick = () => {
      $('st-clock').textContent = new Date().toLocaleTimeString('zh-CN', { hour12: false });
    };
    tick();
    state.clockTimer = setInterval(tick, 1000);
  }
}

// applyCaps 按账号能力调整界面：没有的入口直接禁掉，
// 免得用户点了才收到「权限不足」。服务端仍会独立校验，这里只是不让人白点。
function applyCaps() {
  const caps = session.caps || {};
  state.caps = caps;
  const badge = $('st-role');
  if (badge) {
    badge.textContent = ROLE_LABEL[caps.role] || caps.role || '—';
    badge.dataset.role = caps.role || '';
  }
  const noShell = caps.shell === false;
  for (const id of ['tab-new', 'btn-split', 'btn-quad']) {
    const el = $(id);
    if (!el) continue;
    el.disabled = noShell;
    if (noShell) el.title = '当前账号没有终端权限';
  }
  const files = $('btn-files');
  if (files && caps.filesRead === false) {
    files.disabled = true;
    files.title = '当前账号没有文件浏览权限';
  }
  if (noShell) {
    // 没有终端就没有连接，状态栏别一直显示「连接中」
    const conn = $('st-conn');
    if (conn) conn.textContent = '只读';
    const lat = $('st-lat');
    if (lat) lat.textContent = '—';
    $('terminals').classList.add('no-shell');
  } else {
    $('terminals').classList.remove('no-shell');
  }
}

// 只读审计账号没有任何终端，主区域给一句说明而不是空白。
function showNoShellNotice() {
  const host = $('terminals');
  host.textContent = '';
  const box = document.createElement('div');
  box.className = 'empty-notice';
  const h = document.createElement('h2');
  h.textContent = '当前账号没有终端权限';
  const p1 = document.createElement('p');
  p1.textContent = `角色：${ROLE_LABEL[session.caps?.role] || '只读'}`;
  const p2 = document.createElement('p');
  p2.textContent = '可以浏览文件与会话回放；需要执行命令请联系管理员调整账号角色。';
  box.append(h, p1, p2);
  host.appendChild(box);
}

async function loadSysInfo() {
  try {
    const d = await api('/api/sysinfo');
    state.sys = d;
    $('st-host').textContent = d.host || '—';
  } catch { /* 忽略 */ }
}

// ---------- 标签与窗格 ----------

async function openInitialTabs() {
  if (session.caps && session.caps.shell === false) {
    showNoShellNotice();
    return;
  }
  try {
    const d = await api('/api/term/list');
    const alive = (d.terms || []).filter((t) => !t.closed);
    if (alive.length) {
      for (const t of alive) {
        const tab = buildTab();
        attachSession(tab, mkSession(tab, { id: t.id, cursor: 0 }), true);
        state.tabs.push(tab);
      }
      focusTab(state.tabs[0]);
      return;
    }
  } catch { /* 忽略，直接新建 */ }
  await newTab();
}

async function newTab() {
  const { cols, rows } = estimateDims(1);
  try {
    const d = await api('/api/term/create', { cols, rows });
    const tab = buildTab();
    attachSession(tab, mkSession(tab, d), true);
    state.tabs.push(tab);
    focusTab(tab);
  } catch (ex) {
    toast(ex.message || '创建终端失败');
  }
}

// 按窗格数量估算尺寸：分屏后每个窗格只占一部分宽/高。
// 估小了会导致首屏换行错乱，估大了会被服务端按实际容器纠正，所以宁可略小。
function estimateDims(panes = 1, layout = 'row') {
  const host = $('terminals');
  const w = host.clientWidth || 1000;
  const h = host.clientHeight || 600;
  let cw = w;
  let ch = h;
  if (layout === 'row') {
    cw = w / panes;
  } else if (layout === 'col') {
    ch = h / panes;
  } else if (layout === 'grid') {
    cw = w / 2;
    ch = h / 2;
  }
  return {
    cols: Math.max(40, Math.floor(cw / 8.6)),
    rows: Math.max(12, Math.floor(ch / 18)),
  };
}

function buildTab() {
  const key = `t${state.seq++}`;
  const tab = {
    key,
    title: '',
    sessions: [],
    activeSession: null,
    layout: 'single',
    el: null,
    paneEl: null,
  };

  const tabEl = document.createElement('div');
  tabEl.className = 'tab';
  tabEl.dataset.key = key;
  tabEl.innerHTML =
    '<span class="tab-dot"></span><span class="tab-title"></span>' +
    '<button class="tab-close" title="关闭 (Ctrl+Shift+W)">' +
    '<svg viewBox="0 0 16 16" width="10" height="10"><path d="M3 3l10 10M13 3L3 13" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>' +
    '</button>';
  tabEl.addEventListener('mousedown', (e) => {
    if (e.button === 0 && !e.target.closest('.tab-close')) focusTab(tab);
    if (e.button === 1) {
      e.preventDefault();
      closeTab(tab);
    }
  });
  tabEl.querySelector('.tab-close').addEventListener('click', (e) => {
    e.stopPropagation();
    closeTab(tab);
  });
  tabEl.addEventListener('dblclick', () => renameTab(tab));

  const paneEl = document.createElement('div');
  paneEl.className = 'tabpane';
  paneEl.dataset.key = key;
  paneEl.dataset.layout = 'single';

  $('tablist').appendChild(tabEl);
  $('terminals').appendChild(paneEl);
  tab.el = tabEl;
  tab.paneEl = paneEl;
  setTabTitle(tab, '');
  return tab;
}

function setTabTitle(tab, title) {
  tab.title = title || tab.title;
  const idx = state.tabs.indexOf(tab) + 1;
  const text = tab.title || `终端 ${idx > 0 ? idx : state.tabs.length + 1}`;
  tab.el.querySelector('.tab-title').textContent = text;
}

function mkSession(tab, info) {
  const s = {
    key: `s${state.seq++}`,
    tab,
    id: info.id,
    snapshot: info.snapshot || '',
    cursor: info.cursor || 0,
    ws: null,
    closed: false,
    latency: 0,
    retry: 0,
    inputQueue: [],
    inputTimer: null,
    resizeTimer: null,
    pingSent: 0,
    ready: false,
    visible: false,
    buf: null,
    bufSize: 0,
    view: null,
    el: null,
  };
  return s;
}

// 建立窗格 DOM、终端视图并接入传输通道。
function attachSession(tab, s, activate) {
  const pane = document.createElement('div');
  pane.className = 'pane';
  pane.dataset.key = s.key;
  const host = document.createElement('div');
  host.className = 'term-host';
  pane.appendChild(host);
  tab.paneEl.appendChild(pane);

  const view = new TermView(state.theme, state.fontSize);
  view.onData = (data) => sendInput(s, data);
  view.onResize = () => scheduleResize(s);
  view.onTitle = (t) => {
    // 分屏时两个窗格都会上报标题（vim/top 会设 OSC 标题），保持标签名稳定不互抢
    if (!t || tab.sessions.length > 1) return;
    setTabTitle(tab, t);
  };
  s.view = view;
  s.el = pane;
  view.mount(host);

  pane.addEventListener('mousedown', () => focusSession(s, false));

  tab.sessions.push(s);
  if (tab.sessions.length === 1) tab.activeSession = s;
  updateLayout(tab);

  if (s.snapshot) {
    pushOutput(s, b64ToBytes(s.snapshot));
    s.snapshot = '';
  }
  if (activate) focusSession(s);
  connect(s);
  return s;
}

// 窗格数量决定布局：1 单屏，2 保持用户选的方向，3 及以上切四宫格。
// 这个函数是唯一决定 layout 的地方，避免各处各写一套判断而互相打架。
function layoutFor(n, prefer) {
  if (n <= 1) return 'single';
  if (n === 2) return prefer === 'col' ? 'col' : 'row';
  return 'grid';
}

function updateLayout(tab) {
  const n = tab.sessions.length;
  tab.layout = layoutFor(n, tab.layout);
  tab.paneEl.dataset.layout = tab.layout;
  tab.paneEl.dataset.panes = String(n);
  tab.paneEl.classList.toggle('multi', n > 1);
  for (const s of tab.sessions) {
    s.el.classList.toggle('active', s === tab.activeSession);
  }
  fitTab(tab);
}

// 布局变化后每个窗格都要重新测量。
// 只 fit 当前窗格的话，其他窗格会保持分屏前的尺寸，表现为宫格里文字被裁掉或大片留白。
function fitTab(tab) {
  if (!tab || state.active !== tab) return;
  requestAnimationFrame(() => {
    for (const s of tab.sessions) {
      if (!s.el || s.el.clientWidth === 0) continue;
      s.view.fit();
      s.ready = true;
      flushOutput(s);
      sendResizeNow(s);
    }
  });
}

function focusTab(tab) {
  if (!tab) return;
  state.active = tab;
  for (const t of state.tabs) {
    const on = t === tab;
    t.el.classList.toggle('active', on);
    t.paneEl.classList.toggle('active', on);
    if (!on) {
      // 非当前标签的窗格要去掉高亮：它们虽然 display:none，
      // 但残留的 .active 会让「当前窗格」的判断和自动化测试都对准隐藏元素。
      for (const s of t.sessions) {
        s.visible = false;
        s.el.classList.remove('active');
      }
    }
  }
  focusSession(tab.activeSession || tab.sessions[0], false);
  // 切回标签时，其他窗格可能积压了缓冲输出，一并补写并校正尺寸
  fitTab(tab);
}

function focusSession(s, doFocus = true) {
  if (!s) return;
  const tab = s.tab;
  if (state.active !== tab) {
    state.active = tab;
    for (const t of state.tabs) {
      const on = t === tab;
      t.el.classList.toggle('active', on);
      t.paneEl.classList.toggle('active', on);
      if (!on) {
        for (const x of t.sessions) {
          x.visible = false;
          x.el.classList.remove('active');
        }
      }
    }
  }
  state.activeSession = s;
  tab.activeSession = s;
  for (const x of tab.sessions) {
    x.el.classList.toggle('active', x === s);
    if (x !== s) x.visible = false;
  }
  requestAnimationFrame(() => {
    s.view.fit();
    s.ready = true;
    s.visible = true;
    flushOutput(s);
    if (doFocus) s.view.focus();
    sendResizeNow(s);
  });
}

// 把一个标签再切一个窗格出来，新窗格另起一个终端会话。
// dir 是「2 个窗格时」的方向偏好；切到 3 个及以上时布局自动变四宫格。
async function splitTab(tab, dir) {
  if (!tab) tab = state.active;
  if (!tab) return;
  if (tab.sessions.length >= MAX_PANES) {
    toast(`一个标签最多 ${MAX_PANES} 个窗格`);
    return;
  }
  const next = tab.sessions.length + 1;
  const layout = next >= 3 ? 'grid' : dir;
  const { cols, rows } = estimateDims(next, layout);
  try {
    const d = await api('/api/term/create', { cols, rows });
    tab.layout = layout;
    const s = attachSession(tab, mkSession(tab, d), false);
    updateLayout(tab);
    focusSession(s);
  } catch (ex) {
    toast(ex.message || '创建窗格失败');
  }
}

// 一键四宫格：补齐到 4 个窗格。逐个创建，中途失败就停在当前数量，不留半成品。
async function splitQuad(tab) {
  if (!tab) tab = state.active;
  if (!tab) return;
  if (tab.sessions.length >= MAX_PANES) {
    toast('已经是四宫格了');
    return;
  }
  while (tab.sessions.length < MAX_PANES) {
    const before = tab.sessions.length;
    await splitTab(tab, 'row');
    if (tab.sessions.length === before) break; // 创建失败，splitTab 已提示
  }
  const last = tab.sessions[tab.sessions.length - 1];
  if (last) focusSession(last);
}

async function closeSession(s) {
  if (!s) return;
  const tab = s.tab;
  destroySession(s);
  try {
    await api('/api/term/close', { term: s.id });
  } catch { /* 可能已经结束 */ }
  tab.sessions = tab.sessions.filter((x) => x !== s);
  if (tab.sessions.length === 0) {
    await closeTab(tab);
    return;
  }
  tab.layout = layoutFor(tab.sessions.length, tab.layout);
  if (tab.activeSession === s) tab.activeSession = tab.sessions[0];
  updateLayout(tab);
  focusTab(tab);
}

async function closeTab(tab) {
  if (!tab) return;
  // 先把会话列表取出来：destroyTab 会清空 tab.sessions，之后就拿不到 id 去关服务端终端了
  const sessions = [...tab.sessions];
  destroyTab(tab, false);
  for (const s of sessions) {
    try {
      await api('/api/term/close', { term: s.id });
    } catch { /* 可能已经结束 */ }
  }
  const idx = state.tabs.indexOf(tab);
  state.tabs = state.tabs.filter((t) => t !== tab);
  if (state.active === tab) {
    state.active = null;
    state.activeSession = null;
    const next = state.tabs[Math.max(0, idx - 1)];
    if (next) focusTab(next);
  }
  if (state.tabs.length === 0) {
    await newTab();
  }
}

function destroySession(s) {
  s.closed = true;
  s.visible = false;
  clearTimeout(s.inputTimer);
  clearTimeout(s.resizeTimer);
  stopPing(s);
  if (s.ws) {
    try {
      s.ws.onclose = null;
      s.ws.close();
    } catch { /* ignore */ }
    s.ws = null;
  }
  if (s.view) s.view.dispose();
  s.el?.remove();
  s.buf = null;
  s.bufSize = 0;
}

function destroyTab(tab, silent) {
  for (const s of tab.sessions) destroySession(s);
  tab.sessions = [];
  tab.el?.remove();
  tab.paneEl?.remove();
  if (!silent) {
    tab.el = null;
    tab.paneEl = null;
  }
}

async function renameTab(tab) {
  const name = prompt('标签名称', tab.title);
  if (name === null) return;
  const title = name.trim() || 'shell';
  setTabTitle(tab, title);
  const s = tab.activeSession || tab.sessions[0];
  if (!s) return;
  try {
    await api('/api/term/rename', { term: s.id, title });
  } catch { /* ignore */ }
}

// ---------- 传输层：WebSocket ----------

// 未激活（隐藏）的窗格不直接写入 xterm：容器尺寸为 0 会让换行错乱，先缓冲等激活再冲刷。
function pushOutput(s, bytes) {
  if (!bytes || !bytes.length) return;
  // 分屏后非焦点窗格同样是「看得见」的，输出必须直接写进去；
  // 只有真正不可见（隐藏标签、宽度为 0）时才缓冲，等切回来再补写。
  const shown = s.ready && s.el && s.el.clientWidth > 0;
  if (shown) {
    s.view.write(bytes);
    return;
  }
  if (!s.buf) {
    s.buf = [];
    s.bufSize = 0;
  }
  s.buf.push(bytes);
  s.bufSize += bytes.length;
  // 防止长期后台窗格占满内存：超出 1MB 丢弃最早的数据
  while (s.bufSize > 1 << 20 && s.buf.length > 1) {
    s.bufSize -= s.buf.shift().length;
  }
}

function flushOutput(s) {
  if (!s.buf || !s.buf.length) return;
  const chunks = s.buf;
  s.buf = [];
  s.bufSize = 0;
  for (const c of chunks) s.view.write(c);
}

async function connect(s) {
  s.retry = 0;
  if (session.transport === 'poll') {
    s.transport = 'poll';
    setConn('poll');
    pollLoop(s);
    return;
  }
  s.transport = 'ws';
  await connectWS(s);
}

async function connectWS(s) {
  if (s.closed) return;
  setConn('connecting');
  let ticket;
  try {
    const d = await api('/api/term/ticket', { term: s.id });
    ticket = d.ticket;
  } catch (ex) {
    if (ex.code === 2) return forceLogout();
    scheduleReconnect(s);
    return;
  }
  const proto = location.protocol === 'https:' ? 'wss' : 'ws';
  const ws = new WebSocket(`${proto}://${location.host}/ws?ticket=${encodeURIComponent(ticket)}`);
  ws.binaryType = 'arraybuffer';
  s.ws = ws;

  ws.onopen = () => {
    s.retry = 0;
    setConn('online');
    sendResizeNow(s);
    pingNow(s);
    startPing(s);
  };
  ws.onmessage = (ev) => {
    let f;
    try {
      f = openFrame(ev.data);
    } catch {
      return;
    }
    switch (f.t) {
      case 's':
        s.view.reset();
        s.buf = null;
        s.bufSize = 0;
        if (f.d) pushOutput(s, b64ToBytes(f.d));
        s.cursor = f.c || 0;
        break;
      case 'o':
        if (f.d) pushOutput(s, b64ToBytes(f.d));
        break;
      case 'e':
        s.closed = true;
        s.view.writeText('\r\n\x1b[38;5;244m[会话已结束，可关闭此窗格]\x1b[0m\r\n');
        break;
      case 'q':
        if (s.pingSent) s.latency = Date.now() - s.pingSent;
        setConn('online');
        break;
      case 'p':
        break;
    }
  };
  ws.onclose = (ev) => {
    stopPing(s);
    if (s.closed || state.loggingOut) return;
    if (ev.code === 1000 || ev.code === 1008) {
      setConn('offline');
      return;
    }
    setConn('offline');
    if (ev.code === 1013) {
      s.retry = 0; // overflow：立即重连并重新取快照
    }
    scheduleReconnect(s);
  };
  ws.onerror = () => setConn('offline');
}

function scheduleReconnect(s) {
  if (s.closed || state.loggingOut) return;
  s.retry = Math.min(s.retry + 1, 6);
  const delay = Math.min(500 * s.retry, 4000);
  setTimeout(() => {
    if (!s.closed && (!s.ws || s.ws.readyState > 1)) connectWS(s);
  }, delay);
}

function pingNow(s) {
  if (s.ws && s.ws.readyState === 1) {
    s.pingSent = Date.now();
    s.ws.send(sealFrame({ t: 'p', c: Date.now() }));
  }
}

function startPing(s) {
  stopPing(s);
  s.pingTimer = setInterval(() => {
    if (s.ws && s.ws.readyState === 1) {
      s.pingSent = Date.now();
      s.ws.send(sealFrame({ t: 'p', c: Date.now() }));
    }
  }, 15000);
}

function stopPing(s) {
  if (s.pingTimer) {
    clearInterval(s.pingTimer);
    s.pingTimer = null;
  }
}

function sendInput(s, data) {
  if (!data || !s) return;
  if (s.ws && s.ws.readyState === 1) {
    s.ws.send(sealFrame({ t: 'i', d: bytesToB64(enc.encode(data)) }));
    return;
  }
  if (s.transport === 'poll') {
    s.inputQueue.push(data);
    if (!s.inputTimer) {
      s.inputTimer = setTimeout(() => flushInput(s), 20);
    }
  }
}

async function flushInput(s) {
  s.inputTimer = null;
  if (!s.inputQueue.length) return;
  const data = s.inputQueue.join('');
  s.inputQueue = [];
  try {
    await api('/api/term/input', { term: s.id, data: bytesToB64(enc.encode(data)) });
  } catch { /* 下次轮询会发现终端已关闭 */ }
}

function scheduleResize(s) {
  clearTimeout(s.resizeTimer);
  s.resizeTimer = setTimeout(() => sendResizeNow(s), 180);
}

function sendResizeNow(s) {
  if (!s || s.closed || !s.visible) return;
  const { cols, rows } = s.view.dims();
  if (cols < 20 || rows < 5) return;
  if (s.ws && s.ws.readyState === 1) {
    s.ws.send(sealFrame({ t: 'r', x: cols, y: rows }));
  } else if (s.transport === 'poll') {
    api('/api/term/resize', { term: s.id, cols, rows }).catch(() => {});
  }
}

// ---------- 传输层：纯 POST 轮询 ----------

async function pollLoop(s) {
  let errors = 0;
  while (!s.closed) {
    const t0 = performance.now();
    try {
      const d = await api('/api/term/poll', { term: s.id, cursor: s.cursor, wait: 600 });
      errors = 0;
      s.latency = Math.round(performance.now() - t0);
      setConn('online');
      if (d.reset) {
        s.view.reset();
        s.buf = null;
        s.bufSize = 0;
      }
      if (d.data) pushOutput(s, b64ToBytes(d.data));
      s.cursor = d.cursor;
      if (d.closed) {
        s.closed = true;
        s.view.writeText('\r\n\x1b[38;5;244m[会话已结束]\x1b[0m\r\n');
        return;
      }
    } catch (ex) {
      if (ex.code === 2) return forceLogout();
      errors += 1;
      setConn('offline');
      if (errors > 30) return;
      await sleep(Math.min(300 * errors, 3000));
    }
  }
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

// ---------- 状态栏 / 提示 ----------

let connTimer = null;
function setConn(status) {
  const el = $('st-conn');
  if (!el) return;
  const map = {
    online: ['已连接', 'ok'],
    connecting: ['连接中', 'wait'],
    poll: ['轮询模式', 'wait'],
    offline: ['连接断开，重连中', 'bad'],
  };
  const [text, cls] = map[status] || map.offline;
  el.textContent = text;
  el.className = `conn ${cls}`;
  clearInterval(connTimer);
  if (status === 'online') {
    connTimer = setInterval(() => {
      const s = state.activeSession;
      $('st-lat').textContent = s && s.latency ? `${s.latency}ms` : '—';
    }, 1000);
  } else {
    $('st-lat').textContent = '—';
  }
}

let toastTimer = null;
function toast(msg) {
  const el = $('toast');
  el.textContent = msg;
  el.hidden = false;
  el.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => {
    el.classList.remove('show');
    setTimeout(() => (el.hidden = true), 200);
  }, 1800);
}

// ---------- 剪贴板 ----------

async function copySelection() {
  const s = state.activeSession;
  if (!s) return;
  const sel = s.view.term.getSelection();
  if (!sel) {
    toast('没有选中内容');
    return;
  }
  try {
    await navigator.clipboard.writeText(sel);
    toast('已复制');
  } catch {
    const ta = document.createElement('textarea');
    ta.value = sel;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try {
      document.execCommand('copy');
      toast('已复制');
    } catch {
      toast('复制失败');
    }
    ta.remove();
  }
}

async function pasteText() {
  const s = state.activeSession;
  if (!s) return;
  try {
    const text = await navigator.clipboard.readText();
    if (text) sendInput(s, text);
  } catch {
    showPasteBox();
  }
}

function showPasteBox() {
  const box = $('pastebox');
  box.hidden = false;
  const ta = $('paste-area');
  ta.value = '';
  ta.focus();
}

function hidePasteBox() {
  $('pastebox').hidden = true;
}

// ---------- 搜索 ----------

function toggleSearch(show) {
  const bar = $('searchbar');
  const visible = show === undefined ? bar.hidden : show;
  bar.hidden = !visible;
  if (visible) {
    $('search-input').focus();
    $('search-input').select();
  } else if (state.activeSession) {
    state.activeSession.view.focus();
  }
}

function runSearch(dir) {
  const s = state.activeSession;
  if (!s) return;
  const term = $('search-input').value;
  if (!term) return;
  if (!s.view.search(term, dir)) toast('未找到匹配内容');
}

// ---------- 右键菜单 ----------

function bindContextMenu() {
  const menu = $('ctxmenu');
  $('terminals').addEventListener('contextmenu', (e) => {
    e.preventDefault();
    // 右键哪个窗格就聚焦哪个，再弹菜单
    const pane = e.target.closest('.pane');
    if (pane) {
      const tab = state.active;
      const s = tab?.sessions.find((x) => x.el === pane);
      if (s) focusSession(s, false);
    }
    const hasSel = state.activeSession && state.activeSession.view.term.getSelection();
    menu.querySelector('[data-act="copy"]').disabled = !hasSel;
    menu.style.left = `${Math.min(e.clientX, window.innerWidth - 180)}px`;
    menu.style.top = `${Math.min(e.clientY, window.innerHeight - 220)}px`;
    menu.hidden = false;
  });
  document.addEventListener('mousedown', (e) => {
    if (!e.target.closest('#ctxmenu')) menu.hidden = true;
  });
  menu.addEventListener('click', (e) => {
    const btn = e.target.closest('button');
    if (!btn) return;
    menu.hidden = true;
    const act = btn.dataset.act;
    const s = state.activeSession;
    if (!s) return;
    if (act === 'copy') copySelection();
    if (act === 'paste') pasteText();
    if (act === 'clear') s.view.clear();
    if (act === 'selectall') s.view.term.selectAll();
    if (act === 'search') toggleSearch(true);
    if (act === 'close') closeSession(s);
  });
}

// ---------- 快捷键 ----------

// 在标签内切换窗格：Ctrl+Shift+方向键
function cyclePane(dir) {
  const tab = state.active;
  if (!tab || tab.sessions.length < 2) return;
  const i = tab.sessions.indexOf(tab.activeSession);
  const n = tab.sessions.length;
  const next = (i + (dir === 'prev' ? -1 : 1) + n) % n;
  focusSession(tab.sessions[next]);
}

// 按几何方向找邻居窗格。四宫格下「上一个/下一个」是按数组顺序走，
// 方向键按位置走更符合直觉，所以两套都留着。
function focusPaneDir(dir) {
  const tab = state.active;
  if (!tab || tab.sessions.length < 2) return;
  const cur = tab.activeSession.el.getBoundingClientRect();
  const cx = cur.left + cur.width / 2;
  const cy = cur.top + cur.height / 2;
  let best = null;
  let bestDist = Infinity;
  for (const s of tab.sessions) {
    if (s === tab.activeSession) continue;
    const r = s.el.getBoundingClientRect();
    const dx = r.left + r.width / 2 - cx;
    const dy = r.top + r.height / 2 - cy;
    const ok =
      dir === 'left' ? dx < -1 : dir === 'right' ? dx > 1 : dir === 'up' ? dy < -1 : dy > 1;
    if (!ok) continue;
    const d = Math.abs(dx) + Math.abs(dy);
    if (d < bestDist) {
      bestDist = d;
      best = s;
    }
  }
  if (best) focusSession(best);
}

function bindKeys() {
  window.addEventListener('keydown', (e) => {
    const mod = e.ctrlKey || e.metaKey;
    if (mod && e.shiftKey && e.code === 'KeyT') {
      e.preventDefault();
      newTab();
      return;
    }
    if (mod && e.shiftKey && e.code === 'KeyD') {
      e.preventDefault();
      splitTab(state.active, 'row');
      return;
    }
    if (mod && e.shiftKey && e.code === 'KeyS') {
      e.preventDefault();
      splitTab(state.active, 'col');
      return;
    }
    if (mod && e.shiftKey && e.code === 'KeyG') {
      e.preventDefault();
      splitQuad(state.active);
      return;
    }
    if (mod && e.shiftKey && e.code.startsWith('Arrow')) {
      e.preventDefault();
      const map = {
        ArrowLeft: 'left',
        ArrowRight: 'right',
        ArrowUp: 'up',
        ArrowDown: 'down',
      };
      focusPaneDir(map[e.code]);
      return;
    }
    if (mod && e.shiftKey && e.code === 'KeyW') {
      e.preventDefault();
      // 有多个窗格时先关窗格，只剩一个再关标签
      const tab = state.active;
      if (tab && tab.sessions.length > 1) closeSession(tab.activeSession);
      else closeTab(tab);
      return;
    }
    if (mod && e.shiftKey && (e.code === 'BracketRight' || e.code === 'BracketLeft')) {
      e.preventDefault();
      cyclePane(e.code === 'BracketLeft' ? 'prev' : 'next');
      return;
    }
    if (mod && e.code === 'Tab') {
      e.preventDefault();
      const i = state.tabs.indexOf(state.active);
      const next = (i + (e.shiftKey ? -1 : 1) + state.tabs.length) % state.tabs.length;
      focusTab(state.tabs[next]);
      return;
    }
    if (mod && e.shiftKey && e.code === 'KeyE') {
      e.preventDefault();
      openFileBrowser();
      return;
    }
    if (mod && e.code === 'KeyF') {
      e.preventDefault();
      toggleSearch(true);
      return;
    }
    if (mod && e.shiftKey && e.code === 'KeyC') {
      e.preventDefault();
      copySelection();
      return;
    }
    if (mod && e.shiftKey && e.code === 'KeyV') {
      e.preventDefault();
      pasteText();
      return;
    }
    if (mod && (e.code === 'Equal' || e.code === 'NumpadAdd')) {
      e.preventDefault();
      setFontSize(state.fontSize + 1);
      return;
    }
    if (mod && (e.code === 'Minus' || e.code === 'NumpadSubtract')) {
      e.preventDefault();
      setFontSize(state.fontSize - 1);
    }
  }, true);

  window.addEventListener('resize', () => {
    for (const t of state.tabs) for (const s of t.sessions) s.view.fit();
    if (state.activeSession) scheduleResize(state.activeSession);
  });
}

function setFontSize(size) {
  state.fontSize = Math.max(9, Math.min(28, size));
  localStorage.setItem('shellgo.font', String(state.fontSize));
  for (const t of state.tabs) for (const s of t.sessions) s.view.setFontSize(state.fontSize);
}

function setTheme(name) {
  state.theme = name;
  localStorage.setItem('shellgo.theme', name);
  document.body.dataset.theme = name;
  document.body.style.setProperty('--term-bg', (THEMES[name] || THEMES.dark).background);
  for (const t of state.tabs) for (const s of t.sessions) s.view.setTheme(name);
}

// ---------- 退出 ----------

async function forceLogout() {
  state.loggingOut = true;
  try {
    await api('/api/logout');
  } catch { /* ignore */ }
  doLogout();
}

function doLogout() {
  state.loggingOut = false;
  clearSession();
  showLogin('已退出登录');
}

// ---------- 启动 ----------

async function boot() {
  setTheme(state.theme);
  bindLogin();
  bindKeys();
  bindContextMenu();
  initFileBrowser();
  // 文件管理打开时定位到「光标所在窗格」的终端目录：
  // activeSession 就是最近一次获得焦点的窗格，切换窗格/标签都会更新它。
  setTermLocator(() => (state.activeSession && state.activeSession.id) || '');
  initReplay();
  $('login-user').focus();

  $('tab-new').addEventListener('click', newTab);
  $('btn-logout').addEventListener('click', doLogout);
  $('btn-search').addEventListener('click', () => toggleSearch());
  $('btn-files').addEventListener('click', openFileBrowser);
  $('btn-split').addEventListener('click', () => splitTab(state.active, 'row'));
  $('btn-quad').addEventListener('click', () => splitQuad(state.active));
  $('btn-theme').addEventListener('click', () => setTheme(state.theme === 'dark' ? 'light' : 'dark'));
  $('search-input').addEventListener('input', () => runSearch('next'));
  $('search-input').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      runSearch(e.shiftKey ? 'prev' : 'next');
    }
    if (e.key === 'Escape') toggleSearch(false);
  });
  $('search-next').addEventListener('click', () => runSearch('next'));
  $('search-prev').addEventListener('click', () => runSearch('prev'));
  $('search-close').addEventListener('click', () => toggleSearch(false));

  $('paste-cancel').addEventListener('click', hidePasteBox);
  $('paste-ok').addEventListener('click', () => {
    const text = $('paste-area').value;
    hidePasteBox();
    if (text && state.activeSession) sendInput(state.activeSession, text);
  });

  if (restoreSession()) {
    try {
      const d = await api('/api/session');
      // 角色可能被管理员改过，恢复会话时以服务端为准
      if (d && d.caps) session.caps = d.caps;
      document.title = `${session.user} · Shell Go`;
      showApp();
      await openInitialTabs();
      return;
    } catch {
      clearSession();
    }
  }
  showLogin();
}

// 调试钩子：便于自动化测试读取内部状态（不影响功能）
window.__state = state;
window.__api = api;

boot();
