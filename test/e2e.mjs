// 端到端协议测试：不经浏览器，直接按协议与服务端交互。
// 用法：BASE=http://127.0.0.1:8443 USER=admin PASS=xxx SMCRYPTO_DIR=... node test/e2e.mjs
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const dir = process.env.SMCRYPTO_DIR;
if (!dir) {
  console.error('请设置 SMCRYPTO_DIR');
  process.exit(2);
}
globalThis.sm2 = require(`${dir}/sm2.js`);
globalThis.sm3 = require(`${dir}/sm3.js`);
globalThis.sm4 = require(`${dir}/sm4.js`);

const S = await import('../web/js/secure.js');

const BASE = process.env.BASE || 'http://127.0.0.1:8443';
const USER = process.env.USER_NAME || 'admin';
const PASS = process.env.USER_PASS || 'test1234';
const TRANSPORT = process.env.TRANSPORT || 'ws';

let sid = '';
let key = null;
let token = '';
let skew = 0;

const nowTS = () => Math.floor((Date.now() + skew) / 1000);

async function post(path, body) {
  const res = await fetch(BASE + path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${path} -> ${res.status} ${text}`);
  return JSON.parse(text);
}

async function api(path, p) {
  const data = S.sealJSON(key, { ts: nowTS(), nonce: S.randomHex(16), token, p });
  const j = await post(path, { sid, data });
  const payload = S.openJSON(key, j.data);
  if (payload.code !== 0) throw new Error(`${path} -> ${payload.msg}`);
  return payload.data;
}

function assert(cond, msg) {
  if (!cond) throw new Error('断言失败：' + msg);
}

// loginAs 会把模块级的 sid/key/token 换成新登录的会话，
// 多账号用例靠它切换身份，调完记得切回去。
async function loginAs(user, pass) {
  const hs = await post('/api/handshake', {});
  sid = hs.sid;
  key = S.randomBytes(16);
  skew = hs.ts * 1000 - Date.now();
  const k = S.sm2WrapKey(hs.pub, key);
  const data = S.sealJSON(key, { ts: nowTS(), nonce: S.randomHex(16), token: '', p: { u: user, p: pass } });
  const j = await post('/api/login', { sid, k, data });
  const payload = S.openJSON(key, j.data);
  assert(payload.code === 0, '登录应成功，实际：' + payload.msg);
  token = payload.data.token;
  return payload.data;
}

async function login() {
  const d = await loginAs(USER, PASS);
  console.log('✓ 握手 + 登录成功，transport =', d.transport, 'role =', d.caps && d.caps.role);
}

// expectFail 断言接口必须被拒绝；want 用于确认拒绝原因不是别的问题。
async function expectFail(path, p, want) {
  try {
    await api(path, p);
  } catch (e) {
    if (want && !String(e.message).includes(want)) {
      throw new Error(`${path} 的拒绝原因不符，期望含「${want}」，实际：${e.message}`);
    }
    return;
  }
  throw new Error(`${path} 本应被拒绝，却执行成功了`);
}

async function testWS() {
  const term = await api('/api/term/create', { cols: 100, rows: 30 });
  assert(term.id, '应返回终端 id');
  const { ticket } = await api('/api/term/ticket', { term: term.id });

  const wsUrl = BASE.replace('http', 'ws') + '/ws?ticket=' + encodeURIComponent(ticket);
  const ws = new WebSocket(wsUrl);
  ws.binaryType = 'arraybuffer';

  let out = '';
  const decoder = new TextDecoder();

  await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('等待快照超时')), 8000);
    ws.onmessage = (ev) => {
      const frame = JSON.parse(decoder.decode(S.open(key, new Uint8Array(ev.data))));
      if (frame.t === 's') {
        clearTimeout(timer);
        resolve();
      } else if (frame.t === 'o') {
        out += decoder.decode(S.b64ToBytes(frame.d));
      }
    };
    ws.onerror = () => reject(new Error('WebSocket 错误'));
    ws.onclose = (e) => {
      if (e.code === 1006) reject(new Error('WebSocket 异常关闭'));
    };
  });
  console.log('✓ WebSocket 已连接并收到快照');

  const send = (obj) => ws.send(S.seal(key, S.utf8Bytes(JSON.stringify(obj))));
  send({ t: 'i', d: S.bytesToB64(S.utf8Bytes('echo shell-go-$((6*7))\n')) });

  const deadline = Date.now() + 8000;
  while (Date.now() < deadline && !out.includes('shell-go-42')) {
    await new Promise((r) => setTimeout(r, 60));
  }
  assert(out.includes('shell-go-42'), '应能收到命令输出，实际：' + JSON.stringify(out.slice(-200)));
  console.log('✓ 命令回显正确（PTY + 加密帧）');

  send({ t: 'r', x: 120, y: 40 });
  send({ t: 'p', c: Date.now() });
  await new Promise((r) => setTimeout(r, 400));
  ws.close();

  // 重连应拿到快照，验证会话保活
  await new Promise((r) => setTimeout(r, 200));
  const again = await api('/api/term/snapshot', { term: term.id, max: 65536 });
  const snap = decoder.decode(S.b64ToBytes(again.data));
  assert(snap.includes('shell-go-42'), '重连快照应包含历史输出');
  console.log('✓ 断线后会话保活，快照回放正常');

  await api('/api/term/close', { term: term.id });
  console.log('✓ 终端关闭成功');
}

async function testPoll() {
  const term = await api('/api/term/create', { cols: 90, rows: 24 });
  let cursor = term.cursor || 0;
  let out = '';
  const decoder = new TextDecoder();

  await api('/api/term/input', { term: term.id, data: S.bytesToB64(S.utf8Bytes('echo poll-$((5+5))\n')) });
  const deadline = Date.now() + 8000;
  while (Date.now() < deadline && !out.includes('poll-10')) {
    const d = await api('/api/term/poll', { term: term.id, cursor, wait: 400 });
    if (d.data) out += decoder.decode(S.b64ToBytes(d.data));
    cursor = d.cursor;
  }
  assert(out.includes('poll-10'), '轮询模式应能收到输出');
  console.log('✓ 纯 POST 轮询模式可用');
  await api('/api/term/close', { term: term.id });
}

async function testReplay() {
  const data = S.sealJSON(key, { ts: nowTS() - 9999, nonce: S.randomHex(8), token, p: null });
  let rejected = false;
  try {
    await post('/api/session', { sid, data });
  } catch {
    rejected = true;
  }
  assert(rejected, '过期报文应被拒绝');
  console.log('✓ 时间戳窗口生效，过期报文被拒');
}

async function testFiles() {
  const list = await api('/api/file/list', { path: '' });
  assert(Array.isArray(list.entries), '应返回目录列表');
  console.log('✓ 文件列表可用，根目录：', list.root);

  const content = 'shell-go e2e ' + Date.now();
  const name = `e2e-${Date.now()}.txt`;
  const data = S.bytesToB64(S.utf8Bytes(content));
  const w = await api('/api/file/write', { path: name, offset: 0, total: content.length, data });
  assert(w.done && w.offset === content.length, '写入应完成');

  const r = await api('/api/file/read', { path: name, offset: 0, size: 4096 });
  const text = new TextDecoder().decode(S.b64ToBytes(r.data));
  assert(text === content, '读取内容应与写入一致，实际：' + text);
  console.log('✓ 文件写入 / 读取一致');

  // stat 返回纳秒级 mtime，够做乐观并发校验；顺带暴露续传需要的已有大小
  const st = await api('/api/file/stat', { path: name });
  assert(st.entry && st.entry.size === content.length, 'stat 返回的大小不正确');
  assert(/^\d{19}$/.test(st.version || ''), 'stat 应返回纳秒级版本号字符串，实际：' + st.version);
  console.log('✓ 文件 stat 返回大小与版本号');

  // 在线编辑：正常保存
  const edited = content + ' +编辑';
  const sv = await api('/api/file/save', {
    path: name,
    version: st.version,
    data: S.bytesToB64(S.utf8Bytes(edited)),
  });
  assert(sv.size === S.utf8Bytes(edited).length, '保存后大小不正确：' + sv.size);
  const r2 = await api('/api/file/read', { path: name, offset: 0, size: 4096 });
  assert(new TextDecoder().decode(S.b64ToBytes(r2.data)) === edited, '保存后内容不正确');
  console.log('✓ 在线编辑保存生效（含中文）');

  // 用过期的 mtime 保存必须被拒绝，避免覆盖别人的改动
  let conflict = false;
  try {
    await api('/api/file/save', {
      path: name,
      version: st.version,
      data: S.bytesToB64(S.utf8Bytes('覆盖掉')),
    });
  } catch {
    conflict = true;
  }
  assert(conflict, 'mtime 过期时应拒绝保存');
  console.log('✓ 在线编辑的并发冲突被拒绝');

  const rn = await api('/api/file/rename', { path: name, name: 'e2e-renamed.txt' });
  assert(rn.path === 'e2e-renamed.txt', '重命名路径异常：' + rn.path);
  await api('/api/file/remove', { path: 'e2e-renamed.txt' });
  console.log('✓ 重命名 / 删除正常');

  let blocked = false;
  try {
    await api('/api/file/read', { path: '../../etc/passwd', offset: 0, size: 16 });
  } catch {
    blocked = true;
  }
  assert(blocked, '越权路径应被拒绝');
  console.log('✓ 目录越权被拒绝');
}

// 「打开文件管理定位到终端目录」依赖 /api/term/cwd：
// 验证能读到 shell 的 cwd，并把它映射成相对 files.root 的路径；
// 走出 root 时必须明确告诉前端 inside=false，而不是给一个会被当成越权的相对路径。
async function testTermCwd() {
  const b64 = (t) => S.bytesToB64(S.utf8Bytes(t));
  const list = await api('/api/file/list', { path: '' });
  const root = String(list.root).replace(/\/+$/, '');
  const dir = `e2e-cwd-${Date.now()}`;
  await api('/api/file/mkdir', { path: dir });
  const term = await api('/api/term/create', { cols: 100, rows: 30 });
  try {
    await api('/api/term/input', { term: term.id, data: b64(`cd ${root}/${dir}\n`) });
    let d = null;
    for (let i = 0; i < 50; i++) {
      await new Promise((r) => setTimeout(r, 100));
      d = await api('/api/term/cwd', { term: term.id });
      if (d.inside && d.rel === dir) break;
    }
    assert(d && d.inside === true, '终端目录应判定在 root 内，实际：' + JSON.stringify(d));
    assert(d.rel === dir, '相对路径应为 ' + dir + '，实际：' + d.rel);

    // 走出 root：不能把 / 硬映射成 ""（那是 root 本身），必须 inside=false
    await api('/api/term/input', { term: term.id, data: b64('cd /\n') });
    for (let i = 0; i < 50; i++) {
      await new Promise((r) => setTimeout(r, 100));
      d = await api('/api/term/cwd', { term: term.id });
      if (d.inside === false) break;
    }
    assert(d.inside === false, 'root 之外的 cwd 不应标成 inside，实际：' + JSON.stringify(d));
    assert(d.abs === '/', '仍应返回绝对路径供前端提示，实际：' + d.abs);
    console.log('✓ 终端目录定位：root 内给相对路径，root 外只给绝对路径');
  } finally {
    await api('/api/term/close', { term: term.id });
    await api('/api/file/remove', { path: dir, recursive: true });
  }
}

async function testRecord() {
  const term = await api('/api/term/create', { cols: 100, rows: 30 });
  const marker = 'rec-' + Date.now();
  let cursor = term.cursor || 0;
  let out = '';
  const decoder = new TextDecoder();
  await api('/api/term/input', { term: term.id, data: S.bytesToB64(S.utf8Bytes(`echo ${marker}\n`)) });
  const deadline = Date.now() + 8000;
  while (Date.now() < deadline && !out.includes(marker)) {
    const d = await api('/api/term/poll', { term: term.id, cursor, wait: 300 });
    if (d.data) out += decoder.decode(S.b64ToBytes(d.data));
    cursor = d.cursor;
  }
  await api('/api/term/close', { term: term.id });
  await new Promise((r) => setTimeout(r, 600));

  const list = await api('/api/record/list', { limit: 20 });
  assert(list.enabled, '录制应启用');
  const hit = (list.records || []).find((x) => x.term === term.id);
  assert(hit, '应找到本次会话的录制');
  const got = await api('/api/record/get', { id: hit.id, from: 0, count: 1000 });
  let text = '';
  for (const ev of got.events) text += decoder.decode(S.b64ToBytes(ev.d));
  assert(text.includes(marker), '回放内容应包含命令输出');
  console.log('✓ 会话录制可回放（事件数 ' + got.events.length + '）');
  await api('/api/record/delete', { id: hit.id });
  console.log('✓ 录制删除正常');
}

// 权限分级：只读账号不能开终端/不能写文件，运维账号只看得到自己的录制。
async function testCaps() {
  const viewer = process.env.E2E_VIEWER;
  const ops = process.env.E2E_OPS;
  if (!viewer || !ops) {
    console.log('· 跳过权限用例（未设置 E2E_VIEWER / E2E_OPS）');
    return;
  }
  const pass = process.env.E2E_PASS || PASS;
  const b64 = (t) => S.bytesToB64(S.utf8Bytes(t));

  // 先用管理员身份造一条录制，供越权读取用
  const t = await api('/api/term/create', { cols: 90, rows: 24 });
  await api('/api/term/input', { term: t.id, data: b64('echo perm-marker\n') });
  await new Promise((r) => setTimeout(r, 500));
  await api('/api/term/close', { term: t.id });
  await new Promise((r) => setTimeout(r, 500));
  const mine = (await api('/api/record/list', { limit: 50 })).records.find((x) => x.term === t.id);
  assert(mine, '管理员应能看到自己的录制');

  const saved = { sid, key, token, skew };
  try {
    // ---------- 只读审计账号 ----------
    const d = await loginAs(viewer, pass);
    assert(d.caps && d.caps.role === 'viewer', 'viewer 角色未下发，实际：' + JSON.stringify(d.caps));
    assert(d.caps.shell === false, 'viewer 的 shell 能力应为 false');
    await expectFail('/api/term/create', { cols: 100, rows: 30 }, '没有终端权限');

    const fl = await api('/api/file/list', { path: '' });
    assert(fl.writable === false && fl.deletable === false, 'viewer 的文件列表应标记为不可写不可删');
    await expectFail('/api/file/write', { path: 'viewer.txt', offset: 0, total: 3, data: b64('abc') }, '没有文件写入权限');
    await expectFail('/api/file/remove', { path: 'readme.txt' }, '没有文件删除权限');

    const rlist = await api('/api/record/list', { limit: 5 });
    assert(rlist.scope === 'all', 'viewer 应能查看全部录制，实际 scope=' + rlist.scope);
    const replay = await api('/api/record/get', { id: mine.id, from: 0, count: 10 });
    assert(replay.meta && replay.meta.id === mine.id, 'viewer 应能读取他人录制');
    await expectFail('/api/record/delete', { id: mine.id }, '没有删除录制的权限');
    console.log('✓ 只读账号：无终端权限、文件只读、可查全部录制且不能删除');

    // ---------- 运维账号 ----------
    const d2 = await loginAs(ops, pass);
    assert(d2.caps.role === 'operator' && d2.caps.shell === true, 'operator 应有终端权限');
    const t2 = await api('/api/term/create', { cols: 90, rows: 24 });
    assert(t2.id, 'operator 应能创建终端');
    await api('/api/term/close', { term: t2.id });

    await expectFail('/api/record/get', { id: mine.id, from: 0, count: 10 }, '只能查看自己');
    const own = await api('/api/record/list', { limit: 200 });
    assert(own.scope === 'own', 'operator 的录制范围应是 own，实际 ' + own.scope);
    assert((own.records || []).every((r) => r.user === ops), 'operator 的列表里出现了别人的录制');
    console.log('✓ 运维账号：可开终端、只看得到自己的录制（' + (own.records || []).length + ' 条），越权读取被拒');
  } finally {
    sid = saved.sid;
    key = saved.key;
    token = saved.token;
    skew = saved.skew;
  }
  await api('/api/record/delete', { id: mine.id });
  console.log('✓ 权限用例清理完成');
}

// 未登录不得调用业务接口：握手 + 输错密码就能拿到可解密的通道
// （服务端在验密码之前就装好了会话密钥），所以令牌必须强制校验。
async function testTokenRequired() {
  const hs = await post('/api/handshake', {});
  const key2 = S.randomBytes(16);
  const skew2 = hs.ts * 1000 - Date.now();
  const ts = () => Math.floor((Date.now() + skew2) / 1000);
  const k = S.sm2WrapKey(hs.pub, key2);
  const bad = S.sealJSON(key2, { ts: ts(), nonce: S.randomHex(16), token: '', p: { u: USER, p: 'wrong-' + Date.now() } });
  const j = await post('/api/login', { sid: hs.sid, k, data: bad });
  const loginPayload = S.openJSON(key2, j.data);
  assert(loginPayload.code !== 0, '错误密码不应登录成功');

  // 密码错了，但通道密钥已被服务端接受，此时不带令牌直接调接口
  const sealed = S.sealJSON(key2, { ts: ts(), nonce: S.randomHex(16), token: '', p: { path: '' } });
  const r = await post('/api/file/list', { sid: hs.sid, data: sealed });
  const payload = S.openJSON(key2, r.data);
  assert(payload.code === 2, '未登录调用文件接口应被拒绝（code=2），实际 code=' + payload.code);
  console.log('✓ 未登录调用业务接口被拒（令牌强制校验）');
}

async function main() {
  await login();
  const info = await api('/api/sysinfo');
  assert(info.host, '应返回主机信息');
  console.log('✓ 系统信息：', info.host, info.os);
  await testReplay();
  if (TRANSPORT === 'ws') await testWS();
  await testPoll();
  if (process.env.E2E_FILES === '1') await testFiles();
  if (process.env.E2E_FILES === '1') await testTermCwd();
  if (process.env.E2E_RECORD === '1') await testRecord();
  if (process.env.E2E_PERMS === '1') await testCaps();
  await testTokenRequired();
  await api('/api/logout');
  console.log('\n全部通过');
}

main().catch((e) => {
  console.error('✗ 测试失败：', e.message);
  process.exit(1);
});
