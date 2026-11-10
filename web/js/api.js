// 与服务端的加密通信封装：握手 -> 登录 -> 每次请求独立信封
import {
  randomBytes,
  randomHex,
  bytesToHex,
  hexToBytes,
  sm2WrapKey,
  seal,
  open,
  sealJSON,
  openJSON,
  utf8Bytes,
} from './secure.js';

const STORAGE_KEY = 'shellgo.session.v1';

export const session = {
  sid: '',
  key: null,
  token: '',
  user: '',
  transport: 'ws',
  pollMs: 120,
  maxTerms: 8,
  caps: null, // 账号能力集合（角色 + 逐项权限），由登录响应下发
  skew: 0, // 服务端时间 - 本地时间（毫秒）
  exp: 0,
};

export class ApiError extends Error {
  constructor(code, message) {
    super(message);
    this.code = code;
  }
}

function nowTS() {
  return Math.floor((Date.now() + session.skew) / 1000);
}

async function postJSON(path, body) {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
    credentials: 'same-origin',
  });
  return res;
}

export async function handshake() {
  const res = await postJSON('/api/handshake', {});
  if (!res.ok) {
    let msg = '无法连接服务端';
    try {
      const j = await res.json();
      if (j.error) msg = j.error;
    } catch { /* ignore */ }
    throw new ApiError(-1, msg);
  }
  const j = await res.json();
  session.sid = j.sid;
  session.pub = j.pub;
  session.key = randomBytes(16);
  session.skew = j.ts * 1000 - Date.now();
  session.version = j.version;
  return j;
}

export async function login(user, pass) {
  await handshake();
  const k = sm2WrapKey(session.pub, session.key);
  const data = sealJSON(session.key, {
    ts: nowTS(),
    nonce: randomHex(16),
    token: '',
    p: { u: user, p: pass },
  });
  const res = await postJSON('/api/login', { sid: session.sid, k, data });
  const j = await res.json();
  if (j.error) throw new ApiError(-1, j.error);
  const payload = openJSON(session.key, j.data);
  if (payload.code !== 0) throw new ApiError(payload.code, payload.msg || '登录失败');
  const d = payload.data;
  session.token = d.token;
  session.user = d.user;
  session.transport = d.transport || 'ws';
  session.pollMs = d.pollMs || 120;
  session.maxTerms = d.maxTerms || 8;
  session.caps = d.caps || null;
  session.exp = d.expire;
  saveSession();
  return d;
}

export function saveSession() {
  try {
    sessionStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({
        sid: session.sid,
        key: bytesToHex(session.key),
        token: session.token,
        user: session.user,
        transport: session.transport,
        pollMs: session.pollMs,
        maxTerms: session.maxTerms,
        caps: session.caps,
        skew: session.skew,
        exp: session.exp,
      })
    );
  } catch { /* 隐私模式可能禁用存储 */ }
}

export function restoreSession() {
  try {
    const raw = sessionStorage.getItem(STORAGE_KEY);
    if (!raw) return false;
    const s = JSON.parse(raw);
    if (!s.key || !s.token || !s.sid) return false;
    if (s.exp && s.exp * 1000 < Date.now()) return false;
    session.sid = s.sid;
    session.key = hexToBytes(s.key);
    session.token = s.token;
    session.user = s.user;
    session.transport = s.transport || 'ws';
    session.pollMs = s.pollMs || 120;
    session.maxTerms = s.maxTerms || 8;
    session.caps = s.caps || null;
    session.skew = s.skew || 0;
    session.exp = s.exp || 0;
    return true;
  } catch {
    return false;
  }
}

export function clearSession() {
  session.sid = '';
  session.key = null;
  session.token = '';
  session.user = '';
  session.caps = null;
  try {
    sessionStorage.removeItem(STORAGE_KEY);
  } catch { /* ignore */ }
}

export async function api(path, p = null) {
  if (!session.key) throw new ApiError(2, '尚未登录');
  const sealed = sealJSON(session.key, {
    ts: nowTS(),
    nonce: randomHex(16),
    token: session.token,
    p,
  });
  const res = await postJSON(path, { sid: session.sid, data: sealed });
  if (res.status === 401) {
    throw new ApiError(2, '会话已失效，请重新登录');
  }
  let j;
  try {
    j = await res.json();
  } catch {
    throw new ApiError(-1, '服务端返回异常');
  }
  if (j.error) throw new ApiError(-1, j.error);
  const payload = openJSON(session.key, j.data);
  if (payload.code !== 0) throw new ApiError(payload.code, payload.msg || '请求失败');
  return payload.data;
}

// 二进制帧（WebSocket / 轮询）加解密
export function sealFrame(obj) {
  return seal(session.key, utf8Bytes(JSON.stringify(obj)));
}

export function openFrame(buf) {
  const u8 = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  return JSON.parse(new TextDecoder().decode(open(session.key, u8)));
}
