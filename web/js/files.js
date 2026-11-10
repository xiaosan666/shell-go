// 文件管理：目录浏览、分块上传下载、新建/重命名/删除。
// 所有请求都走 api()，即加密信封 + POST body。
import { api } from './api.js';
import { b64ToBytes, bytesToB64 } from './secure.js';

const CHUNK = 256 * 1024; // 单块 256KB：加密与 base64 膨胀后仍远小于服务端 4MB 限制

let cur = '';        // 当前目录（相对 root，"" 表示根）
let parent = '';
let entries = [];
let filter = '';
let maxUpload = 200 * 1024 * 1024;
let maxEdit = 1024 * 1024;
let writable = false;
let deletable = false;
let busy = false;
let inited = false;
let selected = null;
// 由 app.js 注入：返回「当前光标所在窗格」的终端 id，没有终端时返回空串。
// 文件管理用它把初始目录定位到那个终端的工作目录。
let termLocator = null;

// 在线编辑状态：path 为空表示没在编辑
const editing = { path: '', name: '', version: '0', dirty: false };

const $ = (id) => document.getElementById(id);

export function setTermLocator(fn) {
  termLocator = fn;
}

export function initFileBrowser() {
  if (inited) return;
  inited = true;

  $('files-close').addEventListener('click', close);
  $('filemodal').addEventListener('mousedown', (e) => {
    if (e.target === $('filemodal')) close();
  });
  $('files-refresh').addEventListener('click', () => load(cur));
  $('files-up').addEventListener('click', () => {
    if (cur === '') return;
    load(parent);
  });
  $('files-upload').addEventListener('click', () => {
    if (!writable) return toast('服务端配置为只读');
    $('files-input').click();
  });
  $('files-input').addEventListener('change', async (e) => {
    const list = Array.from(e.target.files || []);
    e.target.value = '';
    if (list.length) await upload(list);
  });
  $('files-mkdir').addEventListener('click', mkdir);
  $('files-newfile').addEventListener('click', newFile);
  $('files-filter').addEventListener('input', (e) => {
    filter = e.target.value.trim().toLowerCase();
    renderList();
  });

  initEditor();

  window.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape') return;
    if (!$('editmodal').hidden) {
      e.stopPropagation();
      closeEditor();
      return;
    }
    if (!$('filemodal').hidden) {
      e.stopPropagation();
      close();
    }
  }, true);
}

function initEditor() {
  $('edit-close').addEventListener('click', closeEditor);
  $('edit-save').addEventListener('click', saveEditor);
  $('editmodal').addEventListener('mousedown', (e) => {
    if (e.target === $('editmodal')) closeEditor();
  });
  const area = $('edit-area');
  area.addEventListener('input', () => {
    if (!editing.path) return;
    editing.dirty = true;
    $('edit-dirty').hidden = false;
  });
  // 配置文件里 Tab 很有用，别让它跳走焦点
  area.addEventListener('keydown', (e) => {
    if (e.key === 'Tab') {
      e.preventDefault();
      const s = area.selectionStart;
      const t = area.selectionEnd;
      area.value = `${area.value.slice(0, s)}  ${area.value.slice(t)}`;
      area.selectionStart = area.selectionEnd = s + 2;
      editing.dirty = true;
      $('edit-dirty').hidden = false;
      return;
    }
    if ((e.ctrlKey || e.metaKey) && e.code === 'KeyS') {
      e.preventDefault();
      saveEditor();
    }
  });
}

// 打开文件管理：默认定位到当前窗格所在终端的目录，再列目录。
// 没有终端、终端刚关掉、或那个目录不在 files.root 之内时，退回上次浏览的位置——
// 定位只是方便，不该因为它失败就把文件管理挡住。
export async function openFileBrowser() {
  $('filemodal').hidden = false;
  let target = cur;
  const term = termLocator ? termLocator() : '';
  if (term) {
    status('定位到终端目录…');
    try {
      const d = await api('/api/term/cwd', { term });
      if (d.inside) {
        target = d.rel;
      } else if (d.abs) {
        toast(`${d.abs} 不在文件根目录内，已回到上次位置`);
      }
    } catch {
      /* 静默退回上次位置 */
    }
  }
  await load(target);
}

// 变更类操作（上传/新建/改名/删除）的统一入口。
// 串行化 busy、错误落到状态栏、结束后刷新列表。
// 刷新必须发生在 busy 放开之后：load() 开头是 if (busy) return，
// 之前把 load 写在 try 里，等于一次都没刷新过（上传完列表不变就是这个原因）。
async function mutate(label, fn) {
  if (busy) return;
  busy = true;
  let ok = false;
  try {
    await fn();
    ok = true;
  } catch (ex) {
    status(`${label}失败：` + (ex.message || '未知错误'));
  } finally {
    busy = false;
  }
  if (ok && !$('filemodal').hidden) await load(cur);
}

function close() {
  if (busy) {
    toast('正在传输，请稍候');
    return;
  }
  $('filemodal').hidden = true;
}

function toast(msg) {
  const el = document.getElementById('toast');
  if (!el) return;
  el.textContent = msg;
  el.hidden = false;
  el.classList.add('show');
  clearTimeout(toast._t);
  toast._t = setTimeout(() => {
    el.classList.remove('show');
    setTimeout(() => (el.hidden = true), 200);
  }, 1800);
}

function status(msg) {
  $('files-status').textContent = msg;
}

function fmtSize(n) {
  if (n < 1024) return `${n} B`;
  const u = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${u[i]}`;
}

function fmtTime(ts) {
  const d = new Date(ts * 1000);
  const p = (x) => String(x).padStart(2, '0');
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

async function load(path) {
  if (busy) return;
  busy = true;
  status('加载中…');
  try {
    const d = await api('/api/file/list', { path });
    cur = d.path;
    parent = d.parent;
    entries = d.entries || [];
    maxUpload = d.maxUpload || maxUpload;
    writable = !!d.writable;
    deletable = !!d.deletable;
    selected = null;
    $('files-root').textContent = d.root || '';
    $('files-upload').disabled = !writable;
    $('files-mkdir').disabled = !writable;
    $('files-newfile').disabled = !writable;
    $('files-up').disabled = cur === '';
    renderCrumb();
    renderList();
    status('就绪');
    $('files-count').textContent = `${entries.length} 项`;
  } catch (ex) {
    status('加载失败：' + (ex.message || '未知错误'));
  } finally {
    busy = false;
  }
}

function renderCrumb() {
  const box = $('files-crumb');
  box.textContent = '';
  const root = document.createElement('button');
  root.className = 'crumb';
  root.textContent = '/';
  root.title = '根目录';
  root.addEventListener('click', () => load(''));
  box.appendChild(root);

  const segs = cur ? cur.split('/') : [];
  let acc = '';
  for (const s of segs) {
    acc = acc ? `${acc}/${s}` : s;
    const sep = document.createElement('span');
    sep.className = 'crumb-sep';
    sep.textContent = '/';
    box.appendChild(sep);
    const b = document.createElement('button');
    b.className = 'crumb';
    b.textContent = s;
    const target = acc;
    b.addEventListener('click', () => load(target));
    box.appendChild(b);
  }
}

function renderList() {
  const list = $('files-list');
  list.textContent = '';
  const shown = filter ? entries.filter((e) => e.name.toLowerCase().includes(filter)) : entries;
  if (!entries.length) {
    const empty = document.createElement('div');
    empty.className = 'files-empty';
    empty.textContent = '空目录';
    list.appendChild(empty);
    return;
  }
  if (!shown.length) {
    const empty = document.createElement('div');
    empty.className = 'files-empty';
    empty.textContent = `没有匹配「${filter}」的文件`;
    list.appendChild(empty);
    return;
  }
  for (const e of shown) {
    list.appendChild(row(e));
  }
  if (filter) {
    $('files-count').textContent = `${shown.length} / ${entries.length} 项`;
  }
}

function row(e) {
  const el = document.createElement('div');
  el.className = `frow${e.dir ? ' is-dir' : ''}${e.link ? ' is-link' : ''}`;
  el.dataset.path = e.path;

  const icon = document.createElement('span');
  icon.className = 'ficon';
  icon.innerHTML = e.dir
    ? '<svg viewBox="0 0 16 16" width="14" height="14"><path d="M1.8 4.2A1.2 1.2 0 013 3h3.1l1.3 1.6H13a1.2 1.2 0 011.2 1.2v6.2A1.2 1.2 0 0113 13.2H3a1.2 1.2 0 01-1.2-1.2V4.2z" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/></svg>'
    : '<svg viewBox="0 0 16 16" width="14" height="14"><path d="M4 2.4h5l3 3v8.2H4V2.4z" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linejoin="round"/><path d="M9 2.4v3h3" fill="none" stroke="currentColor" stroke-width="1.3"/></svg>';

  const name = document.createElement('span');
  name.className = 'fname';
  name.textContent = e.name;
  if (e.link && e.target) name.title = `→ ${e.target}`;

  const size = document.createElement('span');
  size.className = 'fsize';
  size.textContent = e.dir ? '—' : fmtSize(e.size);

  const time = document.createElement('span');
  time.className = 'ftime';
  time.textContent = fmtTime(e.mtime);

  const acts = document.createElement('span');
  acts.className = 'facts';
  if (!e.dir) acts.appendChild(actBtn('下载', () => download(e)));
  if (!e.dir && writable) acts.appendChild(actBtn('编辑', () => editFile(e)));
  if (writable) acts.appendChild(actBtn('改名', () => rename(e)));
  if (deletable) acts.appendChild(actBtn('删除', () => remove(e), true));

  el.append(icon, name, size, time, acts);

  el.addEventListener('click', () => {
    selected?.classList.remove('sel');
    selected = el;
    el.classList.add('sel');
  });
  el.addEventListener('dblclick', () => {
    if (e.dir) load(e.path);
    else if (writable) editFile(e);
    else download(e);
  });
  return el;
}

function actBtn(text, fn, danger) {
  const b = document.createElement('button');
  b.className = `fact${danger ? ' danger' : ''}`;
  b.textContent = text;
  b.addEventListener('click', (ev) => {
    ev.stopPropagation();
    fn();
  });
  return b;
}

// 单块读失败自动重试：长文件下载到一半网络抖一下，不必从头再来。
async function readChunkRetry(path, offset, size) {
  let lastErr;
  for (let i = 0; i < 3; i++) {
    try {
      return await api('/api/file/read', { path, offset, size });
    } catch (ex) {
      lastErr = ex;
      await new Promise((r) => setTimeout(r, 300 * (i + 1)));
    }
  }
  throw lastErr;
}

async function download(e) {
  if (busy) return;
  if (e.size > 512 * 1024 * 1024) {
    if (!confirm(`文件较大（${fmtSize(e.size)}），需要全部载入浏览器内存，继续？`)) return;
  }
  busy = true;
  try {
    const parts = [];
    let offset = 0;
    for (;;) {
      status(`下载 ${e.name} ${e.size ? Math.floor((offset / e.size) * 100) : 100}%`);
      const d = await readChunkRetry(e.path, offset, CHUNK);
      const bytes = b64ToBytes(d.data);
      if (bytes.length) parts.push(bytes);
      offset = d.offset;
      if (d.eof) break;
    }
    const blob = new Blob(parts, { type: 'application/octet-stream' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = e.name;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 5000);
    status(`已下载 ${e.name}（${fmtSize(offset)}）`);
  } catch (ex) {
    status('下载失败：' + (ex.message || '未知错误'));
  } finally {
    busy = false;
  }
}

// 断点续传：只有「已传部分与本地文件开头完全一致」才从断点继续，
// 否则同名不同文件会拼出一个坏文件。只比对前 32KB，成本可以忽略。
async function resumeOffset(f, target, have) {
  const head = Math.min(32 * 1024, have);
  try {
    const d = await api('/api/file/read', { path: target, offset: 0, size: head });
    const remote = b64ToBytes(d.data);
    if (!remote.length) return 0;
    const local = new Uint8Array(await f.slice(0, remote.length).arrayBuffer());
    if (local.length !== remote.length) return 0;
    for (let i = 0; i < local.length; i++) {
      if (local[i] !== remote[i]) return 0;
    }
    return have;
  } catch {
    return 0;
  }
}

async function upload(files) {
  let done = 0;
  await mutate('上传', async () => {
    for (const f of files) {
      if (f.size > maxUpload) {
        status(`${f.name} 超过单文件上限 ${fmtSize(maxUpload)}，已跳过`);
        continue;
      }
      const target = cur ? `${cur}/${f.name}` : f.name;
      let existing = null;
      try {
        const st = await api('/api/file/stat', { path: target });
        if (st.entry && !st.entry.dir) existing = st.entry;
      } catch { /* 不存在就是新文件 */ }

      let offset = 0;
      if (existing && existing.size > 0 && existing.size < f.size) {
        status(`检查 ${f.name} 能否续传…`);
        offset = await resumeOffset(f, target, existing.size);
        if (offset > 0) {
          status(`从 ${fmtSize(offset)} 处续传 ${f.name}`);
        }
      }
      if (existing && offset === 0) {
        if (!confirm(`当前目录已存在 ${f.name}（${fmtSize(existing.size)}），覆盖？`)) continue;
      }
      if (f.size === 0) {
        await api('/api/file/write', { path: target, offset: 0, total: 0, data: '' });
        status(`已创建空文件 ${f.name}`);
        done++;
        continue;
      }
      while (offset < f.size) {
        status(`上传 ${f.name} ${Math.floor((offset / f.size) * 100)}%`);
        const buf = new Uint8Array(await f.slice(offset, offset + CHUNK).arrayBuffer());
        const d = await api('/api/file/write', {
          path: target,
          offset,
          total: f.size,
          data: bytesToB64(buf),
        });
        if (!d.offset || d.offset <= offset) throw new Error('服务端未推进写入偏移');
        offset = d.offset;
      }
      done++;
      status(`已上传 ${f.name}（${fmtSize(f.size)}）`);
    }
  });
  // 刷新之后的 status 会被「就绪」覆盖，这里用 toast 说明这次确实传上去了
  if (done) toast(`已上传 ${done} 个文件`);
}

async function mkdir() {
  if (busy) return;
  const name = prompt('新建目录名称');
  if (!name || !name.trim()) return;
  const target = cur ? `${cur}/${name.trim()}` : name.trim();
  await mutate('新建', async () => {
    await api('/api/file/mkdir', { path: target });
  });
}

async function rename(e) {
  if (busy) return;
  const name = prompt('新的名称', e.name);
  if (name === null || !name.trim() || name === e.name) return;
  await mutate('重命名', async () => {
    await api('/api/file/rename', { path: e.path, name: name.trim() });
  });
}

async function remove(e) {
  if (busy) return;
  const tip = e.dir
    ? `确定删除目录 ${e.name} 及其全部内容？此操作不可恢复。`
    : `确定删除文件 ${e.name}？此操作不可恢复。`;
  if (!confirm(tip)) return;
  await mutate('删除', async () => {
    await api('/api/file/remove', { path: e.path, recursive: e.dir });
  });
}

// ---------------- 在线编辑 ----------------

const textEnc = new TextEncoder();
const textDec = new TextDecoder();

function newFile() {
  if (!writable) return toast('服务端配置为只读');
  const name = prompt('新建文件名（创建后可直接编辑内容）');
  if (!name || !name.trim()) return;
  const clean = name.trim();
  if (clean.includes('/') || clean.includes('\\')) {
    toast('文件名不能包含路径分隔符');
    return;
  }
  const target = cur ? `${cur}/${clean}` : clean;
  openEditor(target, clean, '0', '');
}

async function editFile(e) {
  if (busy) return;
  busy = true;
  status(`打开 ${e.name}…`);
  try {
    const st = await api('/api/file/stat', { path: e.path });
    if (st.maxEdit) maxEdit = st.maxEdit;
    if (st.entry.dir) throw new Error('目标是目录');
    if (st.entry.size > maxEdit) {
      status(`${e.name} 超过在线编辑上限 ${fmtSize(maxEdit)}，请下载后编辑`);
      return;
    }
    // ReadChunk 单次最多返回 1MB，超过就说明文件已超出编辑范围
    const want = Math.max(65536, st.entry.size);
    const d = await api('/api/file/read', { path: e.path, offset: 0, size: Math.min(want, 1 << 20) });
    if (!d.eof) {
      status(`${e.name} 太大，无法在网页内编辑`);
      return;
    }
    const bytes = b64ToBytes(d.data);
    if (bytes.includes(0)) {
      status(`${e.name} 看起来是二进制文件，请下载后用本地编辑器打开`);
      return;
    }
    openEditor(e.path, e.name, st.version || '0', textDec.decode(bytes));
    status('就绪');
  } catch (ex) {
    status('打开失败：' + (ex.message || '未知错误'));
  } finally {
    busy = false;
  }
}

function openEditor(path, name, version, content) {
  editing.path = path;
  editing.name = name;
  editing.version = version || '0';
  editing.dirty = false;
  $('edit-title').textContent = editing.version === '0' ? '新建' : '编辑';
  $('edit-path').textContent = path;
  $('edit-dirty').hidden = true;
  $('edit-status').textContent = 'Ctrl+S 保存 · Esc 关闭 · Tab 缩进两格';
  $('edit-meta').textContent = fmtSize(textEnc.encode(content).length);
  const area = $('edit-area');
  area.value = content;
  area.selectionStart = area.selectionEnd = 0;
  area.scrollTop = 0;
  $('editmodal').hidden = false;
  area.focus();
}

async function saveEditor() {
  if (!editing.path || busy) return;
  const btn = $('edit-save');
  btn.disabled = true;
  busy = true;
  $('edit-status').textContent = '保存中…';
  let ok = false;
  let failMsg = '';
  try {
    const data = bytesToB64(textEnc.encode($('edit-area').value));
    const d = await api('/api/file/save', { path: editing.path, version: editing.version, data });
    editing.version = d.version || '0';
    editing.dirty = false;
    $('edit-dirty').hidden = true;
    $('edit-meta').textContent = fmtSize(d.size);
    $('edit-status').textContent = `已保存 ${new Date().toLocaleTimeString()}`;
    ok = true;
  } catch (ex) {
    failMsg = ex.message || '未知错误';
    $('edit-status').textContent = '保存失败：' + failMsg;
  } finally {
    btn.disabled = false;
    busy = false; // 先放开，load() 才会真的刷新
  }
  if (ok && !$('filemodal').hidden) await load(cur);
}

function closeEditor() {
  if (editing.path && editing.dirty) {
    if (!confirm('有未保存的修改，确定关闭？')) return;
  }
  editing.path = '';
  editing.dirty = false;
  $('editmodal').hidden = true;
}
