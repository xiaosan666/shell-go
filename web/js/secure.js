// 应用层国密信封：SM2 交换密钥 + SM4-CBC 加密 + HMAC-SM3 认证。
// 依赖全局对象 sm2 / sm3 / sm4（由 vendor 脚本注入）。
// 报文格式：IV(16) | 密文 | MAC(32)，对外用 base64。

const BLOCK = 64; // SM3 分组长度，与 HMAC 规范一致
const IV_LEN = 16;
const MAC_LEN = 32;

const encoder = new TextEncoder();
const decoder = new TextDecoder();

export function hexToBytes(hex) {
  if (hex.length % 2 !== 0) throw new Error('hex 长度非法');
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) {
    out[i] = parseInt(hex.substr(i * 2, 2), 16);
  }
  return out;
}

export function bytesToHex(bytes) {
  let s = '';
  for (let i = 0; i < bytes.length; i++) {
    s += bytes[i].toString(16).padStart(2, '0');
  }
  return s;
}

export function bytesToB64(bytes) {
  let s = '';
  const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) {
    s += String.fromCharCode.apply(null, bytes.subarray(i, i + chunk));
  }
  return btoa(s);
}

export function b64ToBytes(b64) {
  const s = atob(b64);
  const out = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
  return out;
}

export function utf8Bytes(str) {
  return encoder.encode(str);
}

export function bytesToUtf8(bytes) {
  return decoder.decode(bytes);
}

export function randomBytes(n) {
  const b = new Uint8Array(n);
  (globalThis.crypto || globalThis.msCrypto).getRandomValues(b);
  return b;
}

function sm3Hex(bytes) {
  return globalThis.sm3(Array.from(bytes));
}

function concat(...parts) {
  let len = 0;
  for (const p of parts) len += p.length;
  const out = new Uint8Array(len);
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}

// 与 Go 侧 DeriveKeys 完全一致：
// encKey = SM3(K || 0x01)[0:16]，macKey = SM3(K || 0x02)[0:32]
export function deriveKeys(key) {
  const km = new Uint8Array(key.length + 1);
  km.set(key);
  km[key.length] = 0x01;
  const enc = hexToBytes(sm3Hex(km)).slice(0, 16);

  const kk = new Uint8Array(key.length + 1);
  kk.set(key);
  kk[kk.length - 1] = 0x02;
  const mac = hexToBytes(sm3Hex(kk));
  return { enc, mac };
}

function hmacSm3(key, data) {
  let k = key;
  if (k.length > BLOCK) k = hexToBytes(sm3Hex(k));
  const padded = new Uint8Array(BLOCK);
  padded.set(k);
  const ipad = new Uint8Array(BLOCK);
  const opad = new Uint8Array(BLOCK);
  for (let i = 0; i < BLOCK; i++) {
    ipad[i] = padded[i] ^ 0x36;
    opad[i] = padded[i] ^ 0x5c;
  }
  const inner = hexToBytes(sm3Hex(concat(ipad, data)));
  return hexToBytes(sm3Hex(concat(opad, inner)));
}

function timingSafeEqual(a, b) {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a[i] ^ b[i];
  return diff === 0;
}

export function seal(key, plain) {
  const { enc, mac } = deriveKeys(key);
  const iv = randomBytes(IV_LEN);
  const ct = Uint8Array.from(
    globalThis.sm4.encrypt(Array.from(plain), Array.from(enc), {
      mode: 'cbc',
      iv: Array.from(iv),
      output: 'array',
    })
  );
  const body = concat(iv, ct);
  return concat(body, hmacSm3(mac, body));
}

export function open(key, sealed) {
  if (sealed.length < IV_LEN + 16 + MAC_LEN) throw new Error('报文长度非法');
  const { enc, mac } = deriveKeys(key);
  const body = sealed.subarray(0, sealed.length - MAC_LEN);
  const tag = sealed.subarray(sealed.length - MAC_LEN);
  if (!timingSafeEqual(hmacSm3(mac, body), tag)) throw new Error('报文完整性校验失败');
  const iv = body.subarray(0, IV_LEN);
  const ct = body.subarray(IV_LEN);
  const pt = globalThis.sm4.decrypt(Array.from(ct), Array.from(enc), {
    mode: 'cbc',
    iv: Array.from(iv),
    output: 'array',
  });
  return Uint8Array.from(pt);
}

// 用服务端 SM2 公钥加密会话密钥；协议要求补 0x04 前缀（未压缩点）。
export function sm2WrapKey(pubHex, key) {
  let hex = globalThis.sm2.doEncrypt(Array.from(key), pubHex, 1);
  if (!hex.startsWith('04')) hex = '04' + hex;
  return hex;
}

export function sealJSON(key, obj) {
  return bytesToB64(seal(key, utf8Bytes(JSON.stringify(obj))));
}

export function openJSON(key, b64) {
  return JSON.parse(bytesToUtf8(open(key, b64ToBytes(b64))));
}

export function randomHex(n) {
  return bytesToHex(randomBytes(n));
}
