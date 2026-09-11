// ---------- 工具 ----------
const $ = id => document.getElementById(id);
const API = '/v0/management/plugins/cpa-usage-manager';
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const pad2 = n => String(n).padStart(2, '0');

function fmtInt(n) {
  n = Number(n) || 0;
  const a = Math.abs(n);
  if (a >= 1e8) return (n / 1e8).toFixed(a >= 1e10 ? 0 : 1) + ' 亿';
  if (a >= 1e4) return (n / 1e4).toFixed(a >= 1e6 ? 0 : 1) + ' 万';
  return n.toLocaleString('zh-CN');
}
// fmtTok Token 计数专用：按 K / M / B 自动升级（<1000 原样显示），整数部分满三位后省去小数；
// 升级阈值取 999.5 的倍数，四舍五入后满千的值（如 999500）直接进位到更大单位（1M 而非 1000K）。
function fmtTok(n) {
  n = Math.round(Number(n) || 0);
  const a = Math.abs(n);
  const dec = s => s.toFixed(Math.abs(s) >= 100 ? 0 : 1).replace(/\.0$/, '');
  if (a >= 999.5e6) return dec(n / 1e9) + 'B';
  if (a >= 999.5e3) return dec(n / 1e6) + 'M';
  if (a >= 999.5) return dec(n / 1e3) + 'K';
  return String(n);
}
// parseTokens 解析 token 数输入，接受 1000 / 1k / 1.5m / 2b 与含千分位逗号的写法。
// 空串返回 null（表示不限），非法输入抛错由调用方转成提示。
function parseTokens(raw) {
  const s = String(raw ?? '').trim().replace(/[,，\s_]/g, '');
  if (!s) return null;
  const m = /^(\d+(?:\.\d+)?)([kKmMbB])?$/.exec(s);
  if (!m) throw new Error('Token 限额格式非法：' + raw + '（可写 500000 或 500k / 1.5m）');
  const mult = { k: 1e3, m: 1e6, b: 1e9 }[(m[2] || '').toLowerCase()] || 1;
  const n = Math.round(parseFloat(m[1]) * mult);
  if (!Number.isFinite(n) || n < 0) throw new Error('Token 限额必须为非负整数');
  if (!Number.isSafeInteger(n)) throw new Error('Token 限额过大');
  return n;
}
function fmtUSD(micro) { return fmtMoney(micro, '$'); }
// fmtMoney 是币种无关的金额格式化；sym 为 '$' 或 '¥'。
function fmtMoney(micro, sym) {
  if (micro === null || micro === undefined) return '不限';
  const v = (Number(micro) || 0) / 1e6, neg = v < 0, a = Math.abs(v);
  let s;
  if (a === 0) s = '0';
  else if (a < 0.01) s = a.toFixed(6);
  else if (a < 1) s = a.toFixed(4);
  else s = a.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  s = s.replace(/(\.\d*?)0+$/, '$1').replace(/\.$/, '');
  return (neg ? '-' + sym : sym) + s;
}
// 价格位恒带 $ 单位（价格表与 models.dev 搜索列表共用）；0 也显示 $0 明确币种。
const fmtPrice = p => fmtUSD(Number(p) || 0);

// ---------- 显示币种 ----------
// dispCur 只影响面板展示（账本与限额口径恒为 USD）；cny 按启动时拉取的
// 汇率折算，汇率未就绪时回退美元。偏好经 ui_disp-cur 跨设备同步。
let dispCur = localStorage.getItem('disp-cur') === 'cny' ? 'cny' : 'usd';
let fxRateCNY = 0;
const fmtCur = micro => {
  if (micro === null || micro === undefined) return '不限';
  if (dispCur !== 'cny' || !fxRateCNY) return fmtUSD(micro);
  return fmtMoney(Math.round(micro * fxRateCNY), '¥');
};
async function loadDispCurRate() {
  try {
    const r = await api('/exchange-rate');
    if (r && r.usd_to_cny_micro) fxRateCNY = r.usd_to_cny_micro / 1e6;
  } catch (_) { /* 汇率失败保持美元显示 */ }
  if (dispCur === 'cny') reloadActive();
}
function fmtSec(ms) {
  ms = Number(ms) || 0;
  if (ms <= 0) return '-';
  if (ms < 1000) return ms + ' ms';
  return (ms / 1000).toFixed(2) + ' s';
}
function fmtBytes(b) {
  b = Number(b) || 0;
  if (b >= 1 << 20) return (b / (1 << 20)).toFixed(1) + ' MB';
  if (b >= 1 << 10) return (b / (1 << 10)).toFixed(1) + ' KB';
  return b + ' B';
}
function fmtDT(ts, withSec) {
  if (!ts) return '-';
  const d = new Date(ts), now = new Date();
  const md = pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
  const t = pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + (withSec ? ':' + pad2(d.getSeconds()) : '');
  return d.getFullYear() === now.getFullYear() ? md + ' ' + t : d.getFullYear() + '-' + md + ' ' + t;
}
function rel(ts) {
  if (!ts) return '从未使用';
  const s = (Date.now() - new Date(ts).getTime()) / 1000;
  if (s < 60) return '刚刚';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  if (s < 86400 * 30) return Math.floor(s / 86400) + ' 天前';
  return fmtDT(ts);
}
function debounce(fn, ms) { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; }
function copyText(text) {
  if (navigator.clipboard && navigator.clipboard.writeText) return navigator.clipboard.writeText(text);
  return new Promise((res, rej) => {
    const ta = document.createElement('textarea');
    ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
    document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy') ? res() : rej(new Error('copy 失败')); } catch (e) { rej(e); }
    ta.remove();
  });
}
function toast(msg, kind) {
  const el = document.createElement('div');
  el.className = 'toast' + (kind ? ' ' + kind : '');
  el.textContent = msg;
  // 模态 dialog 在 top layer，页面其余部分（含全局 #toasts）都被其 ::backdrop
  // 压在模糊层下面——toast 必须挂进当前打开的 dialog 才能露出。容器按需
  // 创建、随 dialog 存活复用；无 dialog 时回退到全局容器。
  const dialogs = document.querySelectorAll('dialog[open]');
  const host = dialogs.length ? dialogs[dialogs.length - 1] : document.body;
  let box = host.querySelector(':scope > .toasts');
  if (!box) {
    box = document.createElement('div');
    box.className = 'toasts';
    box.setAttribute('role', 'status');
    box.setAttribute('aria-live', 'polite');
    host.appendChild(box);
  }
  box.appendChild(el);
  const kill = () => el.remove();
  el.addEventListener('click', kill);
  setTimeout(kill, kind === 'err' ? 6000 : 3500);
}

