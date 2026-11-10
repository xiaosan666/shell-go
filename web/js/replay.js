// 会话回放：读取服务端录制的输出事件，按原始节奏写入只读终端。
import { api, session } from './api.js';
import { TermView } from './terminal.js';
import { b64ToBytes } from './secure.js';

const $ = (id) => document.getElementById(id);
const PAGE = 500;

const player = {
  view: null,
  events: [],
  idx: 0,
  playing: false,
  raf: 0,
  t0: 0,
  base: 0,
  speed: 1,
  end: 0,
  meta: null,
  loading: false,
};

let inited = false;
let selectedId = '';

export function initReplay() {
  if (inited) return;
  inited = true;
  $('btn-rec').addEventListener('click', open);
  $('rec-close').addEventListener('click', close);
  $('recmodal').addEventListener('mousedown', (e) => {
    if (e.target === $('recmodal')) close();
  });
  $('rec-play').addEventListener('click', () => (player.playing ? pause() : play()));
  $('rec-restart').addEventListener('click', restart);
  $('rec-speed').addEventListener('change', (e) => {
    const next = Number(e.target.value) || 1;
    if (player.playing) {
      // 先把当前虚拟时间固化为新的起点，避免变速时跳帧
      player.base = virtualNow();
      player.t0 = performance.now();
    }
    player.speed = next;
  });
  window.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !$('recmodal').hidden) {
      e.stopPropagation();
      close();
    }
  }, true);
  window.addEventListener('resize', () => {
    if (!$('recmodal').hidden && player.view) player.view.fit();
  });
}

function open() {
  $('recmodal').hidden = false;
  loadList();
}

function close() {
  pause();
  $('recmodal').hidden = true;
}

function fmtTime(ts) {
  const d = new Date(ts * 1000);
  const p = (x) => String(x).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

function fmtDur(ms) {
  if (!ms || ms < 0) ms = 0;
  const s = Math.round(ms / 1000);
  const m = Math.floor(s / 60);
  return `${String(m).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`;
}

function fmtSize(n) {
  if (n < 1024) return `${n} B`;
  const u = ['KB', 'MB', 'GB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${u[i]}`;
}

async function loadList() {
  const list = $('rec-list');
  list.textContent = '';
  const tip = document.createElement('div');
  tip.className = 'rec-tip';
  tip.textContent = '加载中…';
  list.appendChild(tip);
  try {
    const d = await api('/api/record/list', { limit: 200 });
    list.textContent = '';
    if (!d.enabled) {
      tip.textContent = '服务端未开启会话录制';
      list.appendChild(tip);
      return;
    }
    if (!d.records || !d.records.length) {
      tip.textContent = '暂无录制。打开终端后会自动录制。';
      list.appendChild(tip);
      return;
    }
    for (const r of d.records) list.appendChild(item(r));
  } catch (ex) {
    list.textContent = '';
    tip.textContent = '加载失败：' + (ex.message || '未知错误');
    list.appendChild(tip);
  }
}

function item(r) {
  const el = document.createElement('div');
  el.className = `rec-item${r.id === selectedId ? ' sel' : ''}`;
  el.dataset.id = r.id;

  const top = document.createElement('div');
  top.className = 'rec-line';
  const user = document.createElement('span');
  user.className = 'rec-user';
  user.textContent = r.user || '未知用户';
  const size = document.createElement('span');
  size.className = 'rec-size';
  size.textContent = fmtSize(r.size || 0);
  top.append(user, size);

  const mid = document.createElement('div');
  mid.className = 'rec-line dim';
  const time = document.createElement('span');
  time.textContent = fmtTime(r.start);
  const dur = document.createElement('span');
  dur.textContent = r.end ? fmtDur(r.end) : '录制中';
  mid.append(time, dur);

  // 无权删除的账号干脆不显示按钮：看得见点不动比看不到更让人困惑
  const caps = session.caps || {};
  if (caps.recordsDelete === false || (caps.recordsAll === false && r.user !== session.user)) {
    el.append(top, mid);
    el.addEventListener('click', () => select(r));
    return el;
  }

  const del = document.createElement('button');
  del.className = 'rec-del';
  del.textContent = '删除';
  del.addEventListener('click', async (e) => {
    e.stopPropagation();
    if (!confirm(`删除这条录制（${fmtTime(r.start)} · ${r.user}）？不可恢复。`)) return;
    try {
      await api('/api/record/delete', { id: r.id });
      if (selectedId === r.id) resetPlayer();
      loadList();
    } catch (ex) {
      alert('删除失败：' + (ex.message || '未知错误'));
    }
  });
  top.appendChild(del);

  el.append(top, mid);
  el.addEventListener('click', () => select(r));
  return el;
}

async function select(r) {
  selectedId = r.id;
  for (const el of document.querySelectorAll('.rec-item')) {
    el.classList.toggle('sel', el.dataset.id === r.id);
  }
  resetPlayer();
  $('rec-hint').textContent =
    `${r.user || '—'} · ${fmtTime(r.start)} · 原始 ${r.cols}×${r.rows} · ${r.term.slice(0, 8)}`;
  player.loading = true;
  $('rec-play').disabled = true;
  $('rec-restart').disabled = true;

  // 分页拉取全部事件，再一次性回放
  const events = [];
  let from = 0;
  let meta = null;
  try {
    for (;;) {
      const d = await api('/api/record/get', { id: r.id, from, count: PAGE });
      meta = d.meta;
      for (const ev of d.events || []) events.push(ev);
      from = d.next;
      if (d.eof) break;
    }
  } catch (ex) {
    $('rec-hint').textContent = '读取失败：' + (ex.message || '未知错误');
    player.loading = false;
    return;
  }
  player.events = events;
  player.meta = meta;
  player.end = meta.end || (events.length ? events[events.length - 1].t : 0);
  player.loading = false;
  $('rec-play').disabled = !events.length;
  $('rec-restart').disabled = !events.length;
  $('rec-play').textContent = '播放';
  $('rec-time').textContent = `00:00 / ${fmtDur(player.end)}`;
  $('rec-bar').style.width = '0%';
  ensureView();
  restart();
}

function ensureView() {
  $('rec-placeholder').hidden = true;
  if (player.view) return;
  const theme = localStorage.getItem('shellgo.theme') || 'dark';
  player.view = new TermView(theme, 12);
  player.view.mount($('rec-screen'));
  player.view.term.options.disableStdin = true;
  player.view.term.options.cursorBlink = false;
}

function virtualNow() {
  return player.base + (performance.now() - player.t0) * player.speed;
}

function play() {
  if (!player.events.length || player.playing) return;
  if (player.idx >= player.events.length) return restart();
  player.playing = true;
  player.base = player.events[player.idx].t;
  player.t0 = performance.now();
  $('rec-play').textContent = '暂停';
  player.raf = requestAnimationFrame(loop);
}

function pause() {
  if (!player.playing) return;
  player.base = virtualNow();
  player.playing = false;
  cancelAnimationFrame(player.raf);
  $('rec-play').textContent = '播放';
}

function loop() {
  if (!player.playing) return;
  const vt = virtualNow();
  while (player.idx < player.events.length && player.events[player.idx].t <= vt) {
    const ev = player.events[player.idx++];
    if (ev.d) player.view.write(b64ToBytes(ev.d));
  }
  const total = player.end || 1;
  const pct = Math.min(100, (vt / total) * 100);
  $('rec-bar').style.width = `${pct.toFixed(2)}%`;
  $('rec-time').textContent = `${fmtDur(vt)} / ${fmtDur(player.end)}`;

  if (player.idx >= player.events.length) {
    player.playing = false;
    $('rec-play').textContent = '播放';
    $('rec-bar').style.width = '100%';
    $('rec-time').textContent = `${fmtDur(player.end)} / ${fmtDur(player.end)}`;
    return;
  }
  player.raf = requestAnimationFrame(loop);
}

function restart() {
  cancelAnimationFrame(player.raf);
  player.playing = false;
  player.idx = 0;
  player.base = 0;
  $('rec-play').textContent = '播放';
  $('rec-bar').style.width = '0%';
  $('rec-time').textContent = `00:00 / ${fmtDur(player.end)}`;
  if (player.view) {
    player.view.reset();
    player.view.fit();
  }
  play();
}

function resetPlayer() {
  pause();
  player.events = [];
  player.idx = 0;
  player.end = 0;
  $('rec-play').disabled = true;
  $('rec-restart').disabled = true;
  $('rec-bar').style.width = '0%';
  $('rec-time').textContent = '00:00 / 00:00';
  if (player.view) player.view.reset();
}
