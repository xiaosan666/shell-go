// 滑动拼图验证码组件。
// 流程：取图（服务端画背景与缺口） → 用户拖动对齐 → 上报位移校验 → 拿到一次性票据。
// 票据交给登录接口核销，服务端据此确认「人和密码」都到位。
import { fetchCaptcha, verifyCaptcha } from './api.js';

const SHAKE_MS = 640;
const MIN_DRAG = 4; // CSS 像素，低于这个值当成误触

export function createCaptcha() {
  const root = document.getElementById('captcha');
  if (!root) return null;

  const stage = document.getElementById('cap-stage');
  const bg = document.getElementById('cap-bg');
  const piece = document.getElementById('cap-piece');
  const reloadBtn = document.getElementById('cap-reload');
  const doneMask = document.getElementById('cap-done');
  const track = document.getElementById('cap-track');
  const fill = document.getElementById('cap-fill');
  const hint = document.getElementById('cap-hint');
  const handle = document.getElementById('cap-handle');

  const st = {
    ch: null, // 当前验证码素材
    ticket: '', // 验证通过后的票据
    scale: 1, // CSS 像素 / 图片像素（容器被缩放时不为 1）
    max: 0, // 最大可拖动距离（CSS 像素）
    x: 0, // 当前位移（CSS 像素）
    drag: false,
    busy: false,
    loading: false,
    off: false, // 服务端关闭了验证码
    startX: 0,
  };

  // ---------- 位置 ----------

  function applyX(cssX) {
    st.x = Math.max(0, Math.min(cssX, st.max));
    piece.style.left = `${st.x}px`;
    handle.style.left = `${st.x}px`;
    fill.style.width = `${st.x}px`;
    if (st.max > 0) {
      handle.setAttribute('aria-valuenow', String(Math.round((st.x / st.max) * 100)));
    }
  }

  // layout 把拼图块按实际渲染宽度缩放到位，容器被拉伸也不会错位。
  function layout() {
    const ch = st.ch;
    if (!ch) return;
    const shown = stage.clientWidth || ch.w;
    const k = shown / ch.w;
    st.scale = k;
    piece.style.width = `${ch.pw * k}px`;
    piece.style.height = `${ch.ph * k}px`;
    piece.style.top = `${ch.py * k}px`;
    st.max = Math.max(0, (ch.w - ch.pw) * k);
    applyX(0);
  }

  // ---------- 状态 ----------

  function clearState() {
    st.ticket = '';
    st.ch = null;
    for (const c of ['ok', 'err', 'warn', 'loading']) track.classList.remove(c);
    doneMask.hidden = true;
    handle.style.opacity = '';
    handle.classList.add('snap');
    applyX(0);
  }

  async function load() {
    if (st.off) return;
    clearState();
    st.loading = true;
    track.classList.add('loading');
    hint.textContent = '验证码加载中…';
    piece.style.visibility = 'hidden';
    try {
      const ch = await fetchCaptcha();
      if (ch && ch.disabled) {
        st.off = true;
        root.hidden = true;
        return;
      }
      st.ch = ch;
      bg.src = ch.bg;
      piece.src = ch.piece;
      piece.style.visibility = 'visible';
      layout();
      hint.textContent = '按住滑块，拖动完成拼图';
    } catch (e) {
      hint.textContent = (e && e.message) || '验证码加载失败，点击右上角刷新';
    } finally {
      st.loading = false;
      track.classList.remove('loading');
    }
  }

  async function submit() {
    if (!st.ch || st.busy) return;
    st.busy = true;
    track.classList.add('loading');
    hint.textContent = '正在校验…';
    // 上报的是「图片像素」：容器实际宽度未必等于图片宽度，得按比例换算回去
    const x = Math.round(st.x / st.scale);
    try {
      const r = await verifyCaptcha(st.ch.cid, x);
      st.ticket = (r && r.ticket) || '';
      track.classList.add('ok');
      doneMask.hidden = false;
      hint.textContent = '验证通过';
      // 终态：把手退场、进度铺满，免得文字压在滑块边上显得挤
      handle.style.opacity = '0';
      fill.style.width = '100%';
    } catch (e) {
      track.classList.add('err');
      hint.textContent = (e && e.message) || '验证失败，请重试';
      setTimeout(() => {
        track.classList.remove('err');
        load(); // 一次一图，失败就得换
      }, SHAKE_MS);
    } finally {
      st.busy = false;
      track.classList.remove('loading');
    }
  }

  // ---------- 拖动 ----------

  handle.addEventListener('pointerdown', (e) => {
    if (st.off || st.busy || st.ticket || !st.ch || e.button !== 0) return;
    e.preventDefault();
    st.drag = true;
    st.startX = e.clientX;
    handle.classList.remove('snap');
    try {
      handle.setPointerCapture(e.pointerId);
    } catch { /* 某些环境不支持捕获，不影响拖动 */ }
  });

  handle.addEventListener('pointermove', (e) => {
    if (!st.drag) return;
    e.preventDefault();
    applyX(e.clientX - st.startX);
  });

  const endDrag = () => {
    if (!st.drag) return;
    st.drag = false;
    if (st.x < MIN_DRAG) {
      handle.classList.add('snap');
      applyX(0);
      return;
    }
    submit();
  };

  handle.addEventListener('pointerup', endDrag);
  handle.addEventListener('pointercancel', endDrag);

  // 指针捕获可能不可用，兜底在 window 上收尾，避免快速拖出滑块后卡住
  window.addEventListener('pointermove', (e) => {
    if (!st.drag) return;
    applyX(e.clientX - st.startX);
  });
  window.addEventListener('pointerup', endDrag);
  window.addEventListener('pointercancel', endDrag);

  // 键盘可达：方向键微调，回车提交
  handle.addEventListener('keydown', (e) => {
    if (st.off || st.busy || st.ticket || !st.ch) return;
    const step = e.shiftKey ? 10 : 2;
    if (e.key === 'ArrowRight') {
      e.preventDefault();
      handle.classList.remove('snap');
      applyX(st.x + step);
    } else if (e.key === 'ArrowLeft') {
      e.preventDefault();
      handle.classList.remove('snap');
      applyX(st.x - step);
    } else if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      if (st.x >= MIN_DRAG) submit();
    }
  });

  reloadBtn.addEventListener('click', () => {
    if (st.busy || st.loading) return;
    load();
  });

  window.addEventListener('resize', () => {
    if (st.ch && !st.ticket) layout();
  });

  return {
    // ticket 返回票据，登录时带上；没做验证时为空串
    ticket: () => st.ticket,
    // disabled 表示服务端关掉了验证码，登录表单不要拦人
    disabled: () => st.off,
    load,
    // reset 清掉验证结果并换一张新图（登录失败后调用）
    reset() {
      clearState();
      return load();
    },
    // nudge 在用户没做验证就提交时提示一下
    nudge() {
      if (st.off || st.ticket) return;
      hint.textContent = '请先拖动滑块完成拼图';
      track.classList.add('warn');
      setTimeout(() => track.classList.remove('warn'), 1200);
      handle.focus();
    },
  };
}
