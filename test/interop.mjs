// 跨语言互操作校验：Node(sm-crypto) <-> Go(gmsm)
// 用法：
//   SMCRYPTO_DIR=/path/to/sm-crypto/dist node test/interop.mjs seal  '{"key":"hex","plain":"utf8"}'
//   SMCRYPTO_DIR=/path/to/sm-crypto/dist node test/interop.mjs open  '{"key":"hex","data":"b64"}'
//   SMCRYPTO_DIR=... node test/interop.mjs wrap  '{"pub":"hex","key":"hex"}'   -> SM2 密文 hex
import { createRequire } from 'node:module';
import { webcrypto } from 'node:crypto';

const require = createRequire(import.meta.url);
const dir = process.env.SMCRYPTO_DIR;
if (!dir) {
  console.error('请设置 SMCRYPTO_DIR 指向 sm-crypto/dist');
  process.exit(2);
}
globalThis.sm2 = require(`${dir}/sm2.js`);
globalThis.sm3 = require(`${dir}/sm3.js`);
globalThis.sm4 = require(`${dir}/sm4.js`);
try {
  globalThis.crypto = webcrypto;
} catch {
  // Node 22 起 crypto 是只读 getter，已内置 webcrypto，无需赋值
}

const S = await import('../web/js/secure.js');

const mode = process.argv[2];
const arg = JSON.parse(process.argv[3] || '{}');

if (mode === 'seal') {
  const key = S.hexToBytes(arg.key);
  const out = S.seal(key, S.utf8Bytes(arg.plain));
  process.stdout.write(S.bytesToB64(out));
} else if (mode === 'open') {
  const key = S.hexToBytes(arg.key);
  const out = S.open(key, S.b64ToBytes(arg.data));
  process.stdout.write(S.bytesToUtf8(out));
} else if (mode === 'wrap') {
  process.stdout.write(S.sm2WrapKey(arg.pub, S.hexToBytes(arg.key)));
} else {
  console.error('未知模式：' + mode);
  process.exit(2);
}
