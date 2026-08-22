/* CPA 用量管理 · 控制台逻辑
   单文件、零依赖；密钥仅存 sessionStorage，全部数据经宿主管理 API 加载。 */
(function () {
'use strict';

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
function fmtUSD(micro) {
  if (micro === null || micro === undefined) return '不限';
  const v = (Number(micro) || 0) / 1e6, neg = v < 0, a = Math.abs(v);
  let s;
  if (a === 0) s = '0';
  else if (a < 0.01) s = a.toFixed(6);
  else if (a < 1) s = a.toFixed(4);
  else s = a.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  s = s.replace(/(\.\d*?)0+$/, '$1').replace(/\.$/, '');
  return (neg ? '-$' : '$') + s;
}
const fmtPrice = p => (Number(p) || 0) === 0 ? '0' : fmtUSD(p);
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
  $('toasts').appendChild(el);
  const kill = () => el.remove();
  el.addEventListener('click', kill);
  setTimeout(kill, kind === 'err' ? 6000 : 3500);
}

// ---------- 下拉组件 ----------
// 自建 listbox 替代原生 select / datalist：原生 select 的弹出列表无法跨浏览器统一样式，
// datalist 在 Firefox 只显示 value 不显示 label（密钥筛选会只见 kid 不见标签）。
//
// 弹层必须挂 body 且 position:fixed —— .panel{overflow:hidden} 会裁掉面板内的绝对定位弹层。
const CARET = '<svg class="sel-caret" viewBox="0 0 24 24" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg>';
const TICK = '<svg class="so-tick" viewBox="0 0 24 24" aria-hidden="true"><path d="m5 13 4 4L19 7"/></svg>';
const BOXTICK = '<span class="so-box"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m5 13 4 4L19 7"/></svg></span>';
let openSel = null; // 当前展开的下拉，全局只允许一个

function closeAnySel(focusBack) {
  if (!openSel) return;
  const s = openSel;
  openSel = null;
  s.pop.remove();
  s.trigger.setAttribute('aria-expanded', 'false');
  if (focusBack) s.trigger.focus();
}
document.addEventListener('click', e => {
  if (openSel && !e.target.closest('.sel-pop') && !e.target.closest('.sel,.combo')) closeAnySel(false);
});
window.addEventListener('resize', () => closeAnySel(false));
// 滚动时重定位（弹层是 fixed，不随容器滚动）
window.addEventListener('scroll', () => { if (openSel) openSel.place(); }, true);

// placePop 把弹层定位到触发器下方；下方空间不足时向上翻转。
function placePop(pop, trigger) {
  const r = trigger.getBoundingClientRect();
  pop.style.visibility = 'hidden';
  pop.style.left = '0px';
  pop.style.top = '0px';
  const ph = pop.offsetHeight, pw = pop.offsetWidth;
  const below = window.innerHeight - r.bottom - 8;
  const flip = below < ph && r.top > below;
  pop.style.top = (flip ? Math.max(8, r.top - ph - 6) : r.bottom + 6) + 'px';
  pop.style.left = Math.max(8, Math.min(window.innerWidth - pw - 8, r.left)) + 'px';
  pop.style.minWidth = Math.max(r.width, 180) + 'px';
  pop.style.visibility = '';
}

// Select 单选下拉。opts: [{value,label,sub}]；onChange(value) 在选择后调用。
// 用法与原生 select 贴近：sel.value 读写当前值。
function Select(mountId, opts, onChange, o = {}) {
  const mount = $(mountId);
  const label = mount.dataset.label || '';
  let value = o.value !== undefined ? o.value : (opts[0] ? opts[0].value : '');
  let items = opts.slice();

  mount.innerHTML = '<button type="button" class="btn sel-btn" aria-haspopup="listbox" aria-expanded="false"'
    + (label ? ' aria-label="' + esc(label) + '"' : '') + '><span class="sel-text"></span>' + CARET + '</button>';
  const trigger = mount.querySelector('.sel-btn');
  const cur = () => items.find(x => x.value === value);

  function paint() {
    const c = cur();
    trigger.querySelector('.sel-text').textContent = c ? c.label : (o.placeholder || '请选择');
    // 「全部…」这类空值不算激活，避免筛选器默认态就高亮
    trigger.dataset.active = String(!!value);
  }

  function open() {
    if (openSel && openSel.trigger === trigger) { closeAnySel(true); return; }
    closeAnySel(false);
    const pop = document.createElement('div');
    pop.className = 'sel-pop';
    pop.setAttribute('role', 'listbox');
    if (label) pop.setAttribute('aria-label', label);
    pop.innerHTML = (o.head ? '<div class="pop-head">' + esc(o.head) + '</div>' : '')
      + items.map((x, i) => '<button type="button" class="sel-opt" role="option" data-i="' + i + '"'
        + ' aria-selected="' + String(x.value === value) + '">'
        + '<span class="so-main"><span class="so-name">' + esc(x.label) + '</span>'
        + (x.sub ? '<span class="so-sub">' + esc(x.sub) + '</span>' : '') + '</span>' + TICK + '</button>').join('');
    document.body.appendChild(pop);
    trigger.setAttribute('aria-expanded', 'true');
    const place = () => placePop(pop, trigger);
    place();
    openSel = { trigger, pop, place };

    const optEls = [...pop.querySelectorAll('.sel-opt')];
    let ci = Math.max(0, items.findIndex(x => x.value === value));
    const cursor = i => {
      ci = (i + optEls.length) % optEls.length;
      optEls.forEach((el, k) => el.dataset.cursor = String(k === ci));
      optEls[ci].scrollIntoView({ block: 'nearest' });
    };
    if (optEls.length) cursor(ci);
    const pick = i => {
      value = items[i].value;
      paint();
      closeAnySel(true);
      if (onChange) onChange(value);
    };
    pop.addEventListener('click', e => {
      const b = e.target.closest('.sel-opt');
      if (b) pick(+b.dataset.i);
    });
    pop.addEventListener('mousemove', e => {
      const b = e.target.closest('.sel-opt');
      if (b) cursor(+b.dataset.i);
    });
    // 键盘处理挂在触发器的常驻监听上（见下），这里只登记当次的处理函数，
    // 避免每次展开都往触发器上再加一个监听器。
    trigger._selKey = e => {
      if (e.key === 'Escape') { e.preventDefault(); closeAnySel(true); return; }
      if (e.key === 'ArrowDown') { e.preventDefault(); cursor(ci + 1); return; }
      if (e.key === 'ArrowUp') { e.preventDefault(); cursor(ci - 1); return; }
      if (e.key === 'Home') { e.preventDefault(); cursor(0); return; }
      if (e.key === 'End') { e.preventDefault(); cursor(optEls.length - 1); return; }
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); if (optEls.length) pick(ci); return; }
      if (e.key.length === 1) { // 首字符跳转
        const ch = e.key.toLowerCase();
        const from = items.findIndex((x, k) => k > ci && x.label.toLowerCase().startsWith(ch));
        const idx = from >= 0 ? from : items.findIndex(x => x.label.toLowerCase().startsWith(ch));
        if (idx >= 0) cursor(idx);
      }
    };
  }

  trigger.addEventListener('click', open);
  trigger.addEventListener('keydown', e => {
    const isOpen = trigger.getAttribute('aria-expanded') === 'true';
    if (isOpen && trigger._selKey) { trigger._selKey(e); return; }
    if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); }
  });
  paint();
  return {
    get value() { return value; },
    set value(v) { value = v; paint(); },
    setOptions(next) { items = next.slice(); paint(); },
  };
}

// MultiSelect 多选下拉（列偏好）。onChange(Set) 在每次勾选后调用。
function MultiSelect(mountId, opts, selected, onChange, o = {}) {
  const mount = $(mountId);
  const label = mount.dataset.label || '';
  const sel = new Set(selected);
  mount.innerHTML = '<button type="button" class="btn sel-btn" aria-haspopup="listbox" aria-expanded="false"'
    + (label ? ' aria-label="' + esc(label) + '"' : '') + '>'
    + (o.icon || '') + '<span class="sel-text"></span>' + CARET + '</button>';
  const trigger = mount.querySelector('.sel-btn');
  const paint = () => { trigger.querySelector('.sel-text').textContent = (o.text || '列') + ' ' + sel.size; };

  function open() {
    if (openSel && openSel.trigger === trigger) { closeAnySel(true); return; }
    closeAnySel(false);
    const pop = document.createElement('div');
    pop.className = 'sel-pop';
    pop.setAttribute('role', 'listbox');
    pop.setAttribute('aria-multiselectable', 'true');
    if (label) pop.setAttribute('aria-label', label);
    const render = () => {
      pop.innerHTML = (o.head ? '<div class="pop-head">' + esc(o.head) + '</div>' : '')
        + opts.map((x, i) => '<button type="button" class="sel-opt multi" role="option" data-i="' + i + '"'
          + ' aria-selected="' + String(sel.has(x.value)) + '"' + (x.fixed ? ' disabled' : '') + '>'
          + BOXTICK + '<span class="so-main"><span class="so-name">' + esc(x.label) + '</span></span></button>').join('')
        + '<div class="sel-foot"><button type="button" class="btn small" data-act="reset">恢复默认</button></div>';
    };
    render();
    document.body.appendChild(pop);
    trigger.setAttribute('aria-expanded', 'true');
    const place = () => placePop(pop, trigger);
    place();
    openSel = { trigger, pop, place };
    pop.addEventListener('click', e => {
      if (e.target.closest('[data-act="reset"]')) {
        sel.clear();
        (o.defaults || []).forEach(v => sel.add(v));
        render(); paint(); if (onChange) onChange(sel);
        return;
      }
      const b = e.target.closest('.sel-opt');
      if (!b || b.disabled) return;
      const v = opts[+b.dataset.i].value;
      if (sel.has(v)) { if (sel.size > 1) sel.delete(v); } else sel.add(v);
      render(); paint();
      if (onChange) onChange(sel);
    });
    trigger._selKey = e => { if (e.key === 'Escape') { e.preventDefault(); closeAnySel(true); } };
  }
  trigger.addEventListener('click', open);
  trigger.addEventListener('keydown', e => {
    if (trigger.getAttribute('aria-expanded') === 'true' && trigger._selKey) { trigger._selKey(e); return; }
    if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); }
  });
  paint();
  return { get selected() { return sel; } };
}

// Combo 可输入组合框：保留手动输入，同时给出候选下拉（替代 datalist）。
function Combo(mountId, onChange) {
  const mount = $(mountId);
  const label = mount.dataset.label || '';
  let items = [];
  mount.innerHTML = '<input type="text" spellcheck="false" placeholder="'
    + esc(mount.dataset.placeholder || '') + '"' + (label ? ' aria-label="' + esc(label) + '"' : '')
    + ' role="combobox" aria-expanded="false" aria-autocomplete="list" autocomplete="off">'
    + '<button type="button" tabindex="-1" aria-label="展开候选"><svg viewBox="0 0 24 24"><path d="m6 9 6 6 6-6"/></svg></button>';
  const input = mount.querySelector('input');
  const btn = mount.querySelector('button');

  function open(filterText) {
    closeAnySel(false);
    const q = (filterText || '').trim().toLowerCase();
    const list = q
      ? items.filter(x => x.label.toLowerCase().includes(q) || (x.sub || '').toLowerCase().includes(q))
      : items.slice();
    const show = list.slice(0, 60);
    const pop = document.createElement('div');
    pop.className = 'sel-pop';
    pop.setAttribute('role', 'listbox');
    pop.innerHTML = show.length
      ? show.map((x, i) => '<button type="button" class="sel-opt" role="option" data-i="' + i + '">'
        + '<span class="so-main"><span class="so-name">' + esc(x.label) + '</span>'
        + (x.sub ? '<span class="so-sub">' + esc(x.sub) + '</span>' : '') + '</span></button>').join('')
      : '<div class="sel-empty">无匹配候选</div>';
    document.body.appendChild(pop);
    input.setAttribute('aria-expanded', 'true');
    const place = () => placePop(pop, mount);
    place();
    openSel = { trigger: input, pop, place };
    let ci = -1;
    const optEls = [...pop.querySelectorAll('.sel-opt')];
    const cursor = i => {
      ci = (i + optEls.length) % optEls.length;
      optEls.forEach((el, k) => el.dataset.cursor = String(k === ci));
      optEls[ci].scrollIntoView({ block: 'nearest' });
    };
    const pick = i => {
      // 值取 sub（kid）优先，没有则取 label：密钥筛选要提交 kid，模型筛选提交模型名
      input.value = show[i].value;
      closeAnySel(false);
      input.focus();
      mount.dataset.active = String(!!input.value);
      if (onChange) onChange(input.value);
    };
    pop.addEventListener('click', e => {
      const b = e.target.closest('.sel-opt');
      if (b) pick(+b.dataset.i);
    });
    pop.addEventListener('mousemove', e => {
      const b = e.target.closest('.sel-opt');
      if (b) cursor(+b.dataset.i);
    });
    input._onKey = e => {
      if (e.key === 'Escape') { e.preventDefault(); closeAnySel(false); return; }
      if (e.key === 'ArrowDown') { e.preventDefault(); if (optEls.length) cursor(ci + 1); return; }
      if (e.key === 'ArrowUp') { e.preventDefault(); if (optEls.length) cursor(ci - 1); return; }
      if (e.key === 'Enter' && ci >= 0) { e.preventDefault(); pick(ci); }
    };
  }
  input.addEventListener('keydown', e => {
    if (openSel && openSel.trigger === input && input._onKey) { input._onKey(e); return; }
    if (e.key === 'ArrowDown') { e.preventDefault(); open(input.value); }
  });
  input.addEventListener('blur', () => { input.setAttribute('aria-expanded', 'false'); });
  btn.addEventListener('click', () => {
    if (openSel && openSel.trigger === input) { closeAnySel(false); return; }
    open('');
    input.focus();
  });
  input.addEventListener('input', debounce(() => {
    mount.dataset.active = String(!!input.value.trim());
    if (onChange) onChange(input.value.trim());
  }, 400));
  return {
    get value() { return input.value.trim(); },
    setOptions(next) { items = next.slice(); },
  };
}

// ---------- 会话与 API ----------
let key = sessionStorage.getItem('cpa-management-key') || '';

async function api(path, opts = {}) {
  const isRaw = opts.body instanceof Blob || opts.body instanceof File;
  const r = await fetch(API + path, {
    method: opts.method || 'GET',
    headers: Object.assign(
      { Authorization: 'Bearer ' + key },
      opts.body !== undefined && !isRaw ? { 'Content-Type': 'application/json' } : {},
      opts.headers || {}
    ),
    body: opts.body,
  });
  if (r.status === 401) { logout(); throw new Error('管理密钥无效或已失效'); }
  if (!r.ok) {
    let msg = 'HTTP ' + r.status;
    try { const j = await r.json(); if (j.error) msg = j.error; } catch (_) { /* 非 JSON 错误体 */ }
    throw new Error(withStorageHint(msg));
  }
  const ct = r.headers.get('Content-Type') || '';
  return ct.includes('json') ? r.json() : r;
}

// withStorageHint 为存储层错误补上处置建议。
//
// 后端在重试耗尽后已经带上中文成因（见 store.transientCause），此时不再追加，
// 避免同一句话出现两遍；只有透出的是原始英文 SQLite 错误时才由前端补一句。
function withStorageHint(msg) {
  if (/malformed|SQLITE_CORRUPT|not a database/i.test(msg))
    return msg + ' —— 数据库文件已损坏，重试无效：请在「系统」页备份后重建数据库';
  if (/数据目录|已重试|检查 data_dir/.test(msg)) return msg;
  if (/database is locked|SQLITE_BUSY|SQLITE_LOCKED/i.test(msg))
    return msg + ' —— 数据库被其他进程占用；检查是否有第二个宿主实例共用同一 data_dir';
  if (/unable to open database file/i.test(msg))
    return msg + ' —— 无法打开数据库或临时文件；检查 data_dir 权限、磁盘剩余空间与 TEMP 目录';
  if (/disk I\/O error|SQLITE_IOERR/i.test(msg))
    return msg + ' —— 数据目录可能被杀毒软件/同步盘占用，或磁盘空间异常；建议把 data_dir 加入杀毒排除列表并移出同步盘';
  return msg;
}
const post = (path, body) => api(path, { method: 'POST', body: JSON.stringify(body) });

function logout() {
  sessionStorage.removeItem('cpa-management-key');
  key = '';
  $('app').hidden = true;
  $('gate').hidden = false;
  $('gate-key').focus();
}

// ---------- 全局缓存 ----------
const S = { fx: null, stats: null };

// ---------- 主题 ----------
// 无手动切换：始终跟随宿主页面（嵌入时镜像 management.html 的深浅色：
// class / data-theme / 背景亮度三重探测，MutationObserver 实时跟随）；
// 独立打开时回退系统偏好。
function parentDoc() {
  try { return window.parent && window.parent !== window ? window.parent.document : null; }
  catch (_) { return null; } // 跨域 iframe
}
function detectParentDark() {
  const doc = parentDoc();
  if (!doc) return null;
  const els = [doc.documentElement, doc.body];
  for (const el of els) {
    if (!el) continue;
    const dt = el.getAttribute('data-theme') || el.getAttribute('data-color-mode') || '';
    if (/dark|night|black/i.test(dt)) return true;
    if (/light|day|white/i.test(dt)) return false;
    if (el.classList.contains('dark') || el.classList.contains('dark-mode') || el.classList.contains('theme-dark')) return true;
    if (el.classList.contains('light') || el.classList.contains('light-mode') || el.classList.contains('theme-light')) return false;
  }
  for (const el of els) {
    if (!el) continue;
    const bg = getComputedStyle(el).backgroundColor;
    const m = bg && bg.match(/rgba?\((\d+)[,\s]+(\d+)[,\s]+(\d+)/);
    if (m && !/^\s*rgba?\(\s*\d+\s*,\s*\d+\s*,\s*\d+\s*,\s*0\s*\)/.test(bg)) {
      const lum = (0.2126 * +m[1] + 0.7152 * +m[2] + 0.0722 * +m[3]) / 255;
      return lum < 0.45;
    }
  }
  return null;
}
function applyTheme() {
  let dark = detectParentDark();
  if (dark === null) dark = !!(window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.dataset.theme = dark ? 'dark' : 'light';
}
(function watchParentTheme() {
  const doc = parentDoc();
  if (!doc) return;
  const re = debounce(applyTheme, 60);
  const obs = new MutationObserver(re);
  const opts = { attributes: true, attributeFilter: ['class', 'style', 'data-theme', 'data-color-mode'] };
  if (doc.documentElement) obs.observe(doc.documentElement, opts);
  if (doc.body) obs.observe(doc.body, opts);
  window.addEventListener('focus', re);
})();

// ---------- 时间范围 ----------
const PRESETS = [
  { id: 'today', label: '今天' },
  { id: '5h', label: '近 5 小时' },
  { id: '24h', label: '近 24 小时' },
  { id: '7d', label: '近 7 天' },
  { id: '30d', label: '近 30 天' },
  { id: 'month', label: '本月' },
  { id: 'all', label: '全部时间' },
];
const rangeState = { id: localStorage.getItem('console-range') || '7d', from: null, to: null };

function computeRange() {
  const now = new Date();
  const day = 864e5;
  switch (rangeState.id) {
    case 'today': { const d = new Date(now); d.setHours(0, 0, 0, 0); return { from: d, to: null }; }
    case '5h': return { from: new Date(now - 5 * 36e5), to: null };
    case '24h': return { from: new Date(now - day), to: null };
    case '7d': return { from: new Date(now - 7 * day), to: null };
    case '30d': return { from: new Date(now - 30 * day), to: null };
    case 'month': { const d = new Date(now.getFullYear(), now.getMonth(), 1); return { from: d, to: null }; }
    case 'custom': return { from: rangeState.from, to: rangeState.to };
    default: return { from: null, to: null };
  }
}
function rangeParams() {
  const { from, to } = computeRange();
  const p = {};
  if (from) p.from = from.toISOString();
  if (to) p.to = to.toISOString();
  return p;
}
function rangeLabel() {
  if (rangeState.id === 'custom') {
    const f = x => x ? x.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '…';
    return f(rangeState.from) + ' → ' + f(rangeState.to);
  }
  return (PRESETS.find(p => p.id === rangeState.id) || PRESETS[3]).label;
}
function toLocalInput(d) {
  if (!d) return '';
  return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate())
    + 'T' + pad2(d.getHours()) + ':' + pad2(d.getMinutes());
}
(function initRange() {
  if (!PRESETS.some(p => p.id === rangeState.id)) rangeState.id = '7d';
  $('range-presets').innerHTML = PRESETS.map(p =>
    '<button type="button" class="pop-item" data-id="' + p.id + '"><span>' + p.label + '</span></button>').join('');
  $('range-presets').addEventListener('click', e => {
    const b = e.target.closest('.pop-item');
    if (!b) return;
    rangeState.id = b.dataset.id;
    localStorage.setItem('console-range', rangeState.id);
    closeRangePop();
    renderRangeUI();
    reloadActive();
  });
  $('range-btn').addEventListener('click', () => {
    const pop = $('range-pop');
    pop.hidden = !pop.hidden;
    $('range-btn').setAttribute('aria-expanded', String(!pop.hidden));
    if (!pop.hidden) {
      const { from, to } = computeRange();
      $('range-from').value = toLocalInput(from);
      $('range-to').value = toLocalInput(to);
    }
  });
  document.addEventListener('click', e => {
    if (!$('range-pop').hidden && !e.target.closest('.range')) closeRangePop();
  });
  $('range-apply').addEventListener('click', () => {
    const f = $('range-from').value ? new Date($('range-from').value) : null;
    const t = $('range-to').value ? new Date($('range-to').value) : null;
    if (f && t && f > t) { toast('起始时间晚于结束时间', 'err'); return; }
    rangeState.id = 'custom';
    rangeState.from = f; rangeState.to = t;
    localStorage.setItem('console-range', 'custom');
    closeRangePop();
    renderRangeUI();
    reloadActive();
  });
  renderRangeUI();
})();
function closeRangePop() {
  $('range-pop').hidden = true;
  $('range-btn').setAttribute('aria-expanded', 'false');
}
function renderRangeUI() {
  $('range-label').textContent = rangeLabel();
  [...$('range-presets').children].forEach(el =>
    el.setAttribute('aria-current', el.dataset.id === rangeState.id ? 'true' : 'false'));
}

// ---------- 抽屉弹窗 ----------
const sheet = $('sheet');
let sheetOk = null;
function openSheet(o) {
  $('sheet-title').textContent = o.title;
  $('sheet-body').innerHTML = o.body || '';
  $('sheet-note').textContent = o.note || '';
  const ok = $('sheet-ok');
  const cancel = $('sheet-cancel');
  ok.textContent = o.okText || '确定';
  ok.className = 'btn' + (o.danger ? ' danger' : ' primary');
  ok.disabled = false;
  // 信息展示类弹窗（无 onOk）：主按钮本身就是「关闭」，再摆一个取消
  // 就是两个按钮做同一件事，只留主按钮。
  cancel.hidden = !o.onOk;
  sheetOk = o.onOk || null;
  sheet.showModal();
  if (!o.noFocus) {
    const first = $('sheet-body').querySelector('input,select,textarea');
    if (first) first.focus();
  }
}
$('sheet-x').addEventListener('click', () => sheet.close());
$('sheet-cancel').addEventListener('click', () => sheet.close());
$('sheet-form').addEventListener('submit', e => { e.preventDefault(); $('sheet-ok').click(); });
$('sheet-ok').addEventListener('click', async () => {
  if (!sheetOk) { sheet.close(); return; }
  const btn = $('sheet-ok');
  btn.disabled = true;
  try {
    const stay = await sheetOk();
    if (stay === false) return; // onOk 已接管界面（如展示明文），保持打开
    sheet.close();
  } catch (e) {
    toast(e.message, 'err');
    btn.disabled = false;
  }
});
function staySheet(okText) {
  const btn = $('sheet-ok');
  btn.textContent = okText || '完成';
  btn.disabled = false;
  $('sheet-cancel').hidden = true; // 结果态只剩一个关闭按钮
  sheetOk = null; // 下一次点击直接关闭
}
function fieldRow(label, inner, cls) {
  return '<label class="field ' + (cls || '') + '"><span class="field-label">' + label + '</span>' + inner + '</label>';
}
function fact(name, value) {
  return '<dl class="fact"><dt>' + esc(name) + '</dt><dd>' + esc(value) + '</dd></dl>';
}
function secretBlock(plain) {
  return '<div class="secret"><code id="secret-code">' + esc(plain) + '</code>'
    + '<div class="btn-row"><button type="button" class="btn small" id="secret-copy">复制</button>'
    + '<span class="secret-warn">明文仅此一次展示，关闭后无法找回。</span></div></div>';
}
function wireSecretCopy() {
  const b = $('secret-copy');
  if (b) b.addEventListener('click', async () => {
    try { await copyText($('secret-code').textContent); b.textContent = '已复制'; setTimeout(() => b.textContent = '复制', 1500); }
    catch (e) { toast('复制失败，请手动选择文本', 'err'); }
  });
}
function confirmSheet(title, note, action) {
  openSheet({
    title, danger: true, okText: '确认执行',
    body: '<p>' + esc(note) + '</p>',
    onOk: async () => { await action(); toast('已完成', 'ok'); },
  });
}

// ---------- 页签调度 ----------
const loaders = {};
let activeTab = 'overview';
function switchTab(name) {
  activeTab = name;
  // roving tabindex：选中项才进 Tab 序，其余用方向键在 tablist 内移动（ARIA tabs 模式）
  document.querySelectorAll('.tab').forEach(t => {
    const on = t.dataset.tab === name;
    t.setAttribute('aria-selected', String(on));
    t.tabIndex = on ? 0 : -1;
  });
  document.querySelectorAll('.view').forEach(v => { v.hidden = v.id !== 'view-' + name; });
  reloadActive();
}
function reloadActive() {
  const fn = loaders[activeTab];
  if (fn) fn().catch(e => toast(e.message, 'err'));
}
document.querySelectorAll('.tab').forEach(t => t.addEventListener('click', () => switchTab(t.dataset.tab)));
$('tabs').addEventListener('keydown', e => {
  const tabs = [...document.querySelectorAll('.tab')];
  const i = tabs.findIndex(t => t.dataset.tab === activeTab);
  if (i < 0) return;
  let to = -1;
  if (e.key === 'ArrowRight') to = (i + 1) % tabs.length;
  else if (e.key === 'ArrowLeft') to = (i - 1 + tabs.length) % tabs.length;
  else if (e.key === 'Home') to = 0;
  else if (e.key === 'End') to = tabs.length - 1;
  if (to < 0) return;
  e.preventDefault();
  switchTab(tabs[to].dataset.tab);
  tabs[to].focus();
});
// Escape 统一关闭浮层：先关下拉，再关时间范围弹层（原先范围弹层无法用键盘关闭）
document.addEventListener('keydown', e => {
  if (e.key !== 'Escape') return;
  if (openSel) { closeAnySel(true); return; }
  if (!$('range-pop').hidden) {
    closeRangePop();
    $('range-btn').focus();
  }
});
$('refresh-btn').addEventListener('click', reloadActive);
$('logout-btn').addEventListener('click', logout);
function stamp() { $('stamp').textContent = '更新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false }); }

// ---------- 概览 ----------
const GRAINS = [
  { value: 'minute', label: '按分钟' },
  { value: 'hour', label: '按小时' },
  { value: 'day', label: '按天' },
  { value: 'week', label: '按周' },
  { value: 'month', label: '按月' },
];
const trend = { points: [], off: new Set(), grainManual: false, view: 'chart' };

// Token 口径：上游 total 缺失（0）时按计费四类合计兜底；
// 缓存命中取「Claude 口径读写」与「OpenAI/Gemini 口径 cached」的较大者，避免双计。
function effTokens(r) {
  const t = Number(r.total_tokens) || 0;
  if (t > 0) return t;
  return (Number(r.input_tokens) || 0) + (Number(r.output_tokens) || 0)
    + (Number(r.cache_read_tokens) || 0) + (Number(r.cache_creation_tokens) || 0);
}
function cacheHit(r) {
  return Math.max((Number(r.cache_read_tokens) || 0) + (Number(r.cache_creation_tokens) || 0),
    Number(r.cached_tokens) || 0);
}
// cacheReadOf 统一两种上游口径的「缓存读」：Claude 的 cache_read 独立于输入，
// OpenAI/Gemini 的 cached 含在输入内。取较大者，避免双计，与缓存命中率同口径。
function cacheReadOf(r) {
  return Math.max(Number(r.cache_read_tokens) || 0, Number(r.cached_tokens) || 0);
}

loaders.overview = async () => {
  if (!trend.grainManual) trendGrainSel.value = autoGrain();
  const p = rangeParams();
  const [dimModel, dimKey, points, costs] = await Promise.all([
    api('/usage/dimension?' + new URLSearchParams({ dimension: 'model', limit: '200', ...p })),
    api('/usage/dimension?' + new URLSearchParams({ dimension: 'key_id', limit: '100', ...p })),
    api('/trends?' + new URLSearchParams({ grain: trendGrainSel.value, ...p })),
    api('/costs?' + new URLSearchParams(p)),
  ]);
  if (!S.fx) { S.fx = await api('/exchange-rate').catch(() => null); }
  trend.points = Array.isArray(points) ? points : [];
  ovCache.models = dimModel.rows || [];
  ovCache.keys = dimKey.rows || [];
  renderReadouts(dimModel.total || {}, costs);
  renderModels(ovCache.models);
  renderKeySpend(ovCache.keys);
  renderTrend();
  stamp();
};

function readout(label, value, sub, alarm) {
  return '<div class="readout' + (alarm ? ' alarm' : '') + '"><div class="readout-top">'
    + '<span class="readout-label">' + label + '</span></div>'
    + '<div class="readout-value">' + value + '</div>'
    + (sub ? '<div class="readout-sub">' + sub + '</div>' : '') + '</div>';
}
// cacheHitRate 缓存命中率 = 命中 token / (输入 + 缓存读 + 缓存写)。
// OpenAI 口径的 cached_tokens 已含在输入内，Claude 口径的 cache_read 独立，分母对两者均成立。
function cacheHitRate(t) {
  const denom = (+t.input_tokens || 0) + (+t.cache_read_tokens || 0) + (+t.cache_creation_tokens || 0);
  return denom > 0 ? cacheHit(t) / denom * 100 : -1;
}
function renderReadouts(total, costs) {
  const failRate = total.requests ? (total.failures / total.requests * 100).toFixed(1) + '%' : '0%';
  const cover = costs.requests ? Math.round(costs.priced_requests / costs.requests * 100) : 0;
  const rate = S.fx;
  const cny = rate && total.cost_micro_usd
    ? ' ≈ ¥' + fmtUSD(Math.round(total.cost_micro_usd * rate.usd_to_cny_micro / 1e6)).slice(1) : '';
  const hitPct = cacheHitRate(total);
  $('ov-readouts').innerHTML =
    readout('请求总数', fmtInt(total.requests),
      '失败 <b>' + fmtInt(total.failures) + '</b> · 失败率 ' + failRate,
      total.requests > 0 && total.failures / total.requests > 0.05)
    + readout('总消耗 Token', effTokens(total).toLocaleString('zh-CN'),
      '输入 <b>' + fmtTok(total.input_tokens) + '</b> · 输出 <b>' + fmtTok(total.output_tokens)
      + '</b> · 缓存命中 <b>' + fmtTok(cacheHit(total)) + '</b>')
    + readout('总费用', fmtUSD(total.cost_micro_usd),
      '计价覆盖 <b>' + cover + '%</b>' + esc(cny))
    + readout('缓存命中率', hitPct < 0 ? '—' : hitPct.toFixed(1) + '%',
      '读 <b>' + fmtTok(cacheReadOf(total)) + '</b> · 写 <b>' + fmtTok(total.cache_creation_tokens)
      + '</b>' + ((total.cached_tokens || 0) > (total.cache_read_tokens || 0) + (total.cache_creation_tokens || 0)
        ? ' · 含上游缓存口径' : ''));
}

function barList(rows, max, nameText, nameHtml, valFn, valText, color, denom) {
  if (!rows.length) return '<div class="empty"><p class="empty-title">暂无数据</p><p class="empty-hint">所选时间范围内没有请求记录</p></div>';
  // 占比分母必须是**全量**合计，不能用 rows 之和：rows 已被 slice 成前 N 项，
  // 用它做分母会把尾部的量摊到头部，份额被系统性夸大（且头部之和恒为 100%）。
  // 调用方传入截断前的 denom；缺省时退回行内之和。
  //
  // 注：概览两卡的数据源分别带 limit=200（模型）/100（密钥），维度多于该上限时
  // denom 是「前 200/100 项之和」而非绝对全量，占比会有极小偏高 —— 这两个上限
  // 远高于实际维度基数，且副标题会显示「共 N 项」，可据此判断是否触顶。
  const sum = denom !== undefined && denom > 0
    ? denom
    : rows.reduce((a, r) => a + Math.max(0, Number(valFn(r)) || 0), 0);
  return rows.map(r => {
    const v = Math.max(0, Number(valFn(r)) || 0);
    const pct = max > 0 ? (v / max * 100) : 0;
    const share = sum > 0 ? (v / sum * 100) : 0;
    return '<div class="bar-cell" title="' + esc(nameText(r)) + '">'
      + '<div class="bar-top"><span class="bar-name">' + nameHtml(r) + '</span>'
      + '<span class="bar-pct"><span class="bp-share">' + share.toFixed(share < 10 ? 1 : 0)
      + '%</span><span class="bp-val">' + valText(r) + '</span></span></div>'
      + '<div class="bar-line' + (color === 'trace' ? ' trace' : '') + '">'
      + '<span style="width:' + pct.toFixed(1) + '%"></span></div></div>';
  }).join('');
}
// ---------- 概览占比卡：指标切换（费用 / Token / 请求） ----------
const ovCache = { models: [], keys: [] };
const ovMetric = {
  models: localStorage.getItem('ov-models-metric') || 'tokens',
  keys: localStorage.getItem('ov-keys-metric') || 'cost',
};
function metricVal(r, m) {
  if (m === 'cost') return Number(r.cost_micro_usd) || 0;
  if (m === 'requests') return Number(r.requests) || 0;
  return effTokens(r);
}
function metricText(v, m) {
  return m === 'cost' ? fmtUSD(v) : m === 'tokens' ? fmtTok(v) : fmtInt(v);
}
const METRIC_SUBS = { tokens: '按 Token 计量', cost: '按费用计量', requests: '按请求次数计量' };
function bindMetricSeg(id, key, apply) {
  const seg = $(id);
  seg.querySelectorAll('button').forEach(b =>
    b.classList.toggle('on', b.dataset.m === ovMetric[key]));
  seg.addEventListener('click', e => {
    const b = e.target.closest('button[data-m]');
    if (!b || b.dataset.m === ovMetric[key]) return;
    ovMetric[key] = b.dataset.m;
    localStorage.setItem(id, b.dataset.m);
    seg.querySelectorAll('button').forEach(x => x.classList.toggle('on', x === b));
    apply();
  });
}
bindMetricSeg('ov-models-metric', 'models', () => renderModels(ovCache.models));
bindMetricSeg('ov-keys-metric', 'keys', () => renderKeySpend(ovCache.keys));

function renderModels(rows) {
  const m = ovMetric.models;
  const top = rows.slice().sort((a, b) => metricVal(b, m) - metricVal(a, m)).slice(0, 10);
  const max = top.length ? metricVal(top[0], m) : 0;
  // 分母取全量（未截断）合计，占比才是真实份额
  const denom = rows.reduce((a, r) => a + Math.max(0, metricVal(r, m)), 0);
  $('ov-models-sub').textContent = METRIC_SUBS[m]
    + (rows.length > top.length ? '，取前 10（共 ' + rows.length + ' 项）' : '，共 ' + rows.length + ' 项');
  $('ov-models').innerHTML = barList(top, max,
    r => r.value || '(空)', r => esc(r.value || '(空)'),
    r => metricVal(r, m), r => metricText(metricVal(r, m), m), null, denom);
}
function renderKeySpend(rows) {
  const m = ovMetric.keys;
  const withKey = rows.filter(r => r.value);
  const top = withKey.slice().sort((a, b) => metricVal(b, m) - metricVal(a, m)).slice(0, 8);
  const max = top.length ? metricVal(top[0], m) : 0;
  const denom = withKey.reduce((a, r) => a + Math.max(0, metricVal(r, m)), 0);
  $('ov-keys-sub').textContent = METRIC_SUBS[m]
    + (withKey.length > top.length ? '，取前 8（共 ' + withKey.length + ' 枚）' : '，共 ' + withKey.length + ' 枚');
  $('ov-keys').innerHTML = barList(top, max,
    r => (keyLabelOf(r.value) || '(无标签)') + ' · ' + r.value,
    r => '<span class="bar-name-main">' + esc(keyLabelOf(r.value) || '(无标签)') + '</span>'
      + '<span class="bar-kid mono">' + esc(r.value) + '</span>',
    r => metricVal(r, m), r => metricText(metricVal(r, m), m), 'trace', denom);
}

// ---------- 趋势图（内联 SVG 堆叠面积）----------
function niceMax(v) {
  if (v <= 0) return 1;
  const exp = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (v <= m * exp) return m * exp;
  return 10 * exp;
}
function bucketLabel(ts, grain) {
  const d = new Date(ts);
  if (grain === 'month') return d.getFullYear() + '-' + pad2(d.getMonth() + 1);
  if (grain === 'day' || grain === 'week')
    return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
  return pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' + pad2(d.getHours()) + ':' + pad2(d.getMinutes());
}
function bucketTick(ts, grain) {
  const d = new Date(ts);
  if (grain === 'month') return d.getFullYear() + '-' + pad2(d.getMonth() + 1);
  if (grain === 'day' || grain === 'week') return pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
  return pad2(d.getHours()) + ':' + pad2(d.getMinutes());
}
function cssVar(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }

// 按时间跨度自动选粒度（用户手动选过则不再覆盖）。
function autoGrain() {
  const { from } = computeRange();
  const span = from ? Date.now() - from.getTime() : Infinity;
  if (span < 3 * 36e5) return 'minute';
  if (span < 96 * 36e5) return 'hour';
  if (span < 120 * 864e5) return 'day';
  return 'month';
}
// 与服务端口径一致的桶对齐：分/时/日按 UTC 整除，周为 UTC 周一，月为 UTC 月初。
function bucketStepMs(grain) {
  return { minute: 6e4, hour: 36e5, day: 864e5, week: 6048e5, month: 0 }[grain] || 0;
}
function alignBucket(ms, grain) {
  if (grain === 'week') {
    const d = new Date(ms);
    const u = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate());
    const dow = (new Date(u).getUTCDay() + 6) % 7;
    return u - dow * 864e5;
  }
  if (grain === 'month') {
    const d = new Date(ms);
    return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1);
  }
  const step = bucketStepMs(grain);
  return Math.floor(ms / step) * step;
}
// 在所选时间范围内补零值桶，让面积图连续；桶数超上限时退回原始点。
function fillTrendPoints(raw, grain) {
  const pts = raw.slice().sort((a, b) => new Date(a.bucket) - new Date(b.bucket));
  if (!pts.length) return pts;
  const step = bucketStepMs(grain);
  const { from, to } = computeRange();
  let start = from ? alignBucket(from.getTime(), grain) : alignBucket(new Date(pts[0].bucket).getTime(), grain);
  let end = to ? Math.min(to.getTime(), Date.now()) : Date.now();
  end = alignBucket(end, grain);
  if (!step && grain === 'month') {
    // 按自然月推进
    const sd = new Date(start), ed = new Date(end);
    const months = (ed.getUTCFullYear() - sd.getUTCFullYear()) * 12 + ed.getUTCMonth() - sd.getUTCMonth();
    if (months > 2000) return pts;
  } else if (step && (end - start) / step > 2000) {
    return pts;
  }
  const byKey = new Map(pts.map(p => [alignBucket(new Date(p.bucket).getTime(), grain), p]));
  const out = [];
  const zero = () => ({
    requests: 0, failures: 0, input_tokens: 0, output_tokens: 0, cached_tokens: 0,
    cache_read_tokens: 0, cache_creation_tokens: 0, total_tokens: 0, cost_micro_usd: 0,
  });
  if (grain === 'month') {
    const cur = new Date(start);
    while (cur.getTime() <= end) {
      const k = cur.getTime();
      out.push(Object.assign({ bucket: new Date(k).toISOString() }, byKey.get(k) || zero()));
      cur.setUTCMonth(cur.getUTCMonth() + 1);
    }
  } else {
    for (let t = start; t <= end; t += step)
      out.push(Object.assign({ bucket: new Date(t).toISOString() }, byKey.get(t) || zero()));
  }
  return out.length ? out : pts;
}

// downsampleTrend 把过密的桶按相邻 k 个合并，避免柱状图 DOM 爆炸（仅影响显示）。
function downsampleTrend(pts, maxN) {
  if (pts.length <= maxN) return pts;
  const k = Math.ceil(pts.length / maxN);
  const keys = ['requests', 'failures', 'input_tokens', 'output_tokens', 'cached_tokens',
    'cache_read_tokens', 'cache_creation_tokens', 'total_tokens', 'cost_micro_usd'];
  const out = [];
  for (let i = 0; i < pts.length; i += k) {
    const g = { bucket: pts[i].bucket };
    for (const key of keys) g[key] = 0;
    for (let j = i; j < Math.min(i + k, pts.length); j++)
      for (const key of keys) g[key] += +pts[j][key] || 0;
    out.push(g);
  }
  return out;
}

function trendSeries() {
  const metric = trendMetricSel.value;
  // 「成功/失败」是状态语义（good/bad），保留状态色；其余是身份语义，用图表专用系列色。
  // 次级编码：失败恒在栈顶 + 段间 2px 间隙 + 图例常在，不依赖单一色相区分。
  if (metric === 'requests') return [
    { key: 'ok', label: '成功', color: cssVar('--live'), val: p => Math.max(0, p.requests - p.failures) },
    { key: 'fail', label: '失败', color: cssVar('--alarm'), val: p => p.failures },
  ];
  if (metric === 'cost') return [
    { key: 'cost', label: '费用', color: cssVar('--series-1'), val: p => p.cost_micro_usd, money: true },
  ];
  return [
    { key: 'input', label: '输入', color: cssVar('--series-1'), tok: true, val: p => p.input_tokens },
    { key: 'output', label: '输出', color: cssVar('--series-2'), tok: true, val: p => p.output_tokens },
    { key: 'cache-read', label: '缓存读', color: cssVar('--series-3'), tok: true, val: p => cacheReadOf(p) },
    { key: 'cache-creation', label: '缓存写', color: cssVar('--series-4'), tok: true, val: p => p.cache_creation_tokens || 0 },
  ];
}
function renderLegend() {
  // 单系列不需要图例（标题已说明画的是什么），≥2 系列图例常在。
  const defs = trendSeries();
  $('trend-legend').innerHTML = defs.length < 2 ? '' : defs.map(d =>
    '<button type="button" class="legend-item" data-key="' + d.key + '" aria-pressed="'
    + String(!trend.off.has(d.key)) + '"><span class="swatch" style="background:' + d.color + '"></span>'
    + d.label + '</button>').join('');
}
$('trend-legend').addEventListener('click', e => {
  const b = e.target.closest('.legend-item');
  if (!b) return;
  const k = b.dataset.key;
  if (trend.off.has(k)) trend.off.delete(k); else trend.off.add(k);
  if (trend.off.size === trendSeries().length) trend.off.delete(k);
  renderTrend();
});

function renderTrend() {
  const grain = trendGrainSel.value;
  const filled = fillTrendPoints(trend.points, grain);
  const pts = downsampleTrend(filled, 1200);
  const box = $('trend-chart');
  renderLegend();
  const grainText = (GRAINS.find(g => g.value === grain) || {}).label || '';
  $('trend-sub').textContent = '按' + grainText.replace('按', '')
    + '堆叠 · ' + rangeLabel() + ' · ' + pts.length + ' 个桶'
    + (pts.length < filled.length ? '（过密已合并显示）' : '');
  const defs = trendSeries().map(d => Object.assign(d, { on: !trend.off.has(d.key) }));
  trend.view === 'table' ? renderTrendTable(pts, defs, grain) : $('trend-table').hidden = true;
  if (trend.view === 'table') { box.hidden = true; return; }
  box.hidden = false;
  if (!pts.length) {
    box.innerHTML = '<div class="empty" style="height:100%"><p class="empty-title">暂无趋势数据</p>'
      + '<p class="empty-hint">所选时间范围与粒度下没有聚合记录</p></div>';
    return;
  }
  // 用像素级 width/height 而非 preserveAspectRatio=none，避免坐标轴文字被拉伸。
  const W = Math.max(320, Math.floor(box.clientWidth || 800));
  const H = Math.max(240, Math.floor(box.clientHeight || 300));
  const padL = 56, padR = 14, padT = 14, padB = 26;
  const iw = W - padL - padR, ih = H - padT - padB;
  const stacks = pts.map(p => defs.reduce((acc, d) => d.on ? acc + Math.max(0, d.val(p)) : acc, 0));
  const ymax = niceMax(Math.max(...stacks, 1));
  const y = v => padT + ih - (v / ymax) * ih;
  const n = pts.length;
  const slot = iw / n;                 // 每个桶的槽宽
  // 柱宽上限 24px：槽内余量留白，不把槽填满。
  const barW = Math.max(2, Math.min(24, slot * 0.7));
  const cx = i => padL + slot * i + slot / 2;

  let grid = '', labels = '';
  const isMoney = defs.some(d => d.money);
  const isTokens = defs.some(d => d.tok);
  const fmtAxis = v => isMoney ? fmtUSD(v) : isTokens ? fmtTok(v) : fmtInt(v);
  for (let g = 0; g <= 4; g++) {
    const gv = ymax * g / 4, gy = y(gv);
    grid += '<line class="gridline" x1="' + padL + '" y1="' + gy.toFixed(1) + '" x2="' + (W - padR) + '" y2="' + gy.toFixed(1) + '"/>';
    labels += '<text class="axis-text" x="' + (padL - 8) + '" y="' + (gy + 3.5).toFixed(1) + '" text-anchor="end">'
      + fmtAxis(gv) + '</text>';
  }
  const tickStep = Math.max(1, Math.ceil(n / Math.floor(iw / 64)));
  let lastTickX = -1e9;
  for (let i = 0; i < n; i++) {
    const tx = cx(i);
    const isLast = i === n - 1;
    if ((i % tickStep !== 0 && !isLast) || tx - lastTickX < 40) continue;
    lastTickX = tx;
    labels += '<text class="axis-text" x="' + tx.toFixed(1) + '" y="' + (H - 8) + '" text-anchor="middle">'
      + esc(bucketTick(pts[i].bucket, grain)) + '</text>';
  }

  // 堆叠柱：自下而上逐系列叠加。
  // 段间留 2px 表面色间隙（用间隙分隔，不画描边）：间隙统一开在每段的**顶边**，
  // 这样最底段仍然坐在基线上（柱体从单一基线生长，基线端方角）。
  // 高度不足以让出间隙的薄段照原高绘制、不加间隙 —— 宁可少一条分隔，
  // 也不能把数据段整段丢掉或抬高（精确值另有 tooltip 与表格视图承载）。
  const GAP = 2, R = 4;
  const on = defs.filter(d => d.on);
  let bars = '', running = new Array(n).fill(0);
  // 先定位每根柱子最上面的可见段，供圆角判定
  const topSeg = new Array(n).fill(-1);
  for (let i = 0; i < n; i++)
    for (let k = 0; k < on.length; k++)
      if (Math.max(0, on[k].val(pts[i])) > 0) topSeg[i] = k;
  for (let k = 0; k < on.length; k++) {
    const d = on[k];
    for (let i = 0; i < n; i++) {
      const v = Math.max(0, d.val(pts[i]));
      if (v <= 0) continue;
      const yBase = y(running[i]);
      const yTop = y(running[i] + v);
      running[i] += v;
      const rawH = yBase - yTop;
      if (rawH <= 0) continue;
      const isTop = k === topSeg[i];
      const gap = (!isTop && rawH > GAP + 1) ? GAP : 0;
      // 非零段至少画 1px：亚像素高度等于画了个看不见的东西，
      // 「有但极小」比「看不到」更诚实（精确值在 tooltip 与表格视图里）。
      const h = Math.max(1, rawH - gap);
      const yDraw = yTop + gap;
      const x = cx(i) - barW / 2;
      bars += isTop
        ? '<path d="' + topRoundedBar(x, yDraw, barW, h, R) + '" fill="' + d.color + '"/>'
        : '<rect x="' + x.toFixed(1) + '" y="' + yDraw.toFixed(1) + '" width="' + barW.toFixed(1)
          + '" height="' + h.toFixed(1) + '" fill="' + d.color + '"/>';
    }
  }
  // 悬停/聚焦热区（置于柱体之下、网格之上）。tabindex 让键盘也能读到数值。
  let hover = '';
  for (let i = 0; i < n; i++)
    hover += '<rect class="bar-hover" tabindex="0" role="button" data-i="' + i + '"'
      + ' aria-label="' + esc(bucketLabel(pts[i].bucket, grain) + '，合计 ' + fmtAxis(stacks[i])) + '"'
      + ' x="' + (padL + slot * i).toFixed(1) + '" y="' + padT + '" width="' + slot.toFixed(1)
      + '" height="' + ih + '"/>';

  box.innerHTML = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H
    + '" role="img" aria-label="用量趋势图">' + grid + hover + bars + labels + '</svg>'
    + '<div class="chart-tip" id="trend-tip" hidden></div>';

  const svg = box.querySelector('svg'), tip = $('trend-tip');
  let hotIdx = -1;
  function setHot(i) {
    if (i === hotIdx) return;
    hotIdx = i;
    svg.querySelectorAll('.bar-hover').forEach(r =>
      r.classList.toggle('on', +r.dataset.i === i));
  }
  // showTip 鼠标与键盘共用：tooltip 上挂在柱顶，空间不足时下翻。
  function showTip(idx) {
    setHot(idx);
    const p = pts[idx];
    let rows = defs.filter(d => d.on).map(d =>
      '<div class="tip-row"><span class="swatch" style="background:' + d.color + '"></span><span>' + d.label
      + '</span><b>' + (d.money ? fmtUSD(d.val(p)) : d.tok ? fmtTok(d.val(p)) : fmtInt(d.val(p))) + '</b></div>').join('');
    rows += '<div class="tip-row tip-total"><span></span><span>合计</span><b>' + fmtAxis(stacks[idx]) + '</b></div>';
    tip.innerHTML = '<div class="tip-head">' + esc(bucketLabel(p.bucket, grain)) + '</div>' + rows;
    tip.hidden = false;
    const rect = svg.getBoundingClientRect();
    const sx = cx(idx) / W * rect.width;
    const topPx = y(stacks[idx]) / H * rect.height;
    const wantAbove = topPx - tip.offsetHeight - 8 >= 0;
    tip.classList.toggle('below', !wantAbove);
    tip.style.left = Math.max(90, Math.min(rect.width - 90, sx)) + 'px';
    tip.style.top = (wantAbove ? topPx - 8 : topPx + 8) + 'px';
  }
  function hideTip() { tip.hidden = true; setHot(-1); }
  svg.addEventListener('mousemove', ev => {
    const rect = svg.getBoundingClientRect();
    const sx = (ev.clientX - rect.left) * (W / rect.width);
    let idx = Math.floor((sx - padL) / slot);
    showTip(Math.max(0, Math.min(n - 1, idx)));
  });
  svg.addEventListener('mouseleave', hideTip);
  // 键盘：Tab 进入热区即出 tooltip，左右键在桶之间移动
  svg.addEventListener('focusin', e => {
    const r = e.target.closest('.bar-hover');
    if (r) showTip(+r.dataset.i);
  });
  svg.addEventListener('focusout', e => {
    if (!svg.contains(e.relatedTarget)) hideTip();
  });
  svg.addEventListener('keydown', e => {
    const r = e.target.closest('.bar-hover');
    if (!r) return;
    const i = +r.dataset.i;
    const to = e.key === 'ArrowRight' ? i + 1 : e.key === 'ArrowLeft' ? i - 1 : -1;
    if (to < 0 || to >= n) return;
    e.preventDefault();
    svg.querySelector('.bar-hover[data-i="' + to + '"]').focus();
  });
}
// topRoundedBar 顶端圆角、基线方角的柱体路径。
function topRoundedBar(x, y, w, h, r) {
  const R = Math.max(0, Math.min(r, w / 2, h));
  return 'M' + x.toFixed(1) + ' ' + (y + h).toFixed(1)
    + 'V' + (y + R).toFixed(1)
    + 'a' + R.toFixed(1) + ' ' + R.toFixed(1) + ' 0 0 1 ' + R.toFixed(1) + ' -' + R.toFixed(1)
    + 'h' + (w - 2 * R).toFixed(1)
    + 'a' + R.toFixed(1) + ' ' + R.toFixed(1) + ' 0 0 1 ' + R.toFixed(1) + ' ' + R.toFixed(1)
    + 'V' + (y + h).toFixed(1) + 'Z';
}
// renderTrendTable 图表的表格孪生视图：数值不再只能靠悬停读取。
function renderTrendTable(pts, defs, grain) {
  const host = $('trend-table');
  host.hidden = false;
  const on = defs.filter(d => d.on);
  const fmtOf = d => v => d.money ? fmtUSD(v) : d.tok ? fmtTok(v) : fmtInt(v);
  if (!pts.length) {
    host.innerHTML = '<div class="empty"><p class="empty-title">暂无趋势数据</p>'
      + '<p class="empty-hint">所选时间范围与粒度下没有聚合记录</p></div>';
    return;
  }
  const rows = pts.slice().reverse();
  host.innerHTML = '<table class="data"><thead><tr><th>时间桶</th>'
    + on.map(d => '<th class="num">' + esc(d.label) + '</th>').join('')
    + '<th class="num">合计</th></tr></thead><tbody>'
    + rows.map(p => {
      const total = on.reduce((a, d) => a + Math.max(0, d.val(p)), 0);
      const f = on.length ? fmtOf(on[0]) : fmtInt;
      return '<tr><td class="cell-mono">' + esc(bucketLabel(p.bucket, grain)) + '</td>'
        + on.map(d => '<td class="num">' + fmtOf(d)(Math.max(0, d.val(p))) + '</td>').join('')
        + '<td class="num"><b>' + f(total) + '</b></td></tr>';
    }).join('')
    + '</tbody></table>';
}
const trendMetricSel = new Select('trend-metric', [
  { value: 'tokens', label: 'Token' },
  { value: 'requests', label: '请求' },
  { value: 'cost', label: '费用' },
], () => { trend.off.clear(); renderTrend(); }, { value: 'tokens', head: '趋势指标' });
const trendGrainSel = new Select('trend-grain', GRAINS,
  () => { trend.grainManual = true; reloadActive(); }, { value: 'day', head: '聚合粒度' });
// 图表 / 表格切换：表格孪生视图让数值不必依赖悬停
$('trend-view').addEventListener('click', e => {
  const b = e.target.closest('button[data-v]');
  if (!b || b.dataset.v === trend.view) return;
  trend.view = b.dataset.v;
  $('trend-view').querySelectorAll('button').forEach(x => x.classList.toggle('on', x === b));
  renderTrend();
});
window.addEventListener('resize', debounce(() => { if (activeTab === 'overview') renderTrend(); }, 200));

// ---------- 密钥 ----------
const keysView = { cache: [], filtered: [], page: 0, size: 20, search: '', caller: '', status: '' };
// keyLabelOf 由 kid 查密钥标签；无标签或缓存未热时返回空串，由调用方决定回落值。
function keyLabelOf(kid) {
  const k = keysView.cache.find(x => x.kid === kid);
  return k && k.label ? k.label : '';
}

loaders.keys = async () => { await refreshKeys(); };
async function refreshKeys() {
  const q = new URLSearchParams({ limit: '1000' });
  if (keysView.search) q.set('search', keysView.search);
  if (keysView.caller) q.set('caller_id', keysView.caller);
  const r = await api('/keys?' + q);
  keysView.cache = r.items || [];
  applyKeyFilter();
  stamp();
}
function keyStatus(k) {
  if (k.revoked_at) return 'revoked';
  if (!k.enabled) return 'disabled';
  if (k.expires_at && new Date(k.expires_at) <= new Date()) return 'expired';
  return 'active';
}
const STATUS_META = {
  active: { label: '启用中', pill: 'live' },
  disabled: { label: '已禁用', pill: 'warn' },
  revoked: { label: '已撤销', pill: 'alarm' },
  expired: { label: '已过期', pill: '' },
};
function applyKeyFilter() {
  keysView.filtered = keysView.status
    ? keysView.cache.filter(k => keyStatus(k) === keysView.status)
    : keysView.cache.slice();
  const pages = Math.max(1, Math.ceil(keysView.filtered.length / keysView.size));
  if (keysView.page >= pages) keysView.page = pages - 1;
  renderKeys();
  updateBadges();
}
function cycleKeysNow(d = new Date()) {
  const y = d.getUTCFullYear(), m = d.getUTCMonth(), day = d.getUTCDate();
  const u = new Date(Date.UTC(y, m, day));
  const dow = (u.getUTCDay() + 6) % 7;
  const thu = new Date(u); thu.setUTCDate(u.getUTCDate() - dow + 3);
  const jan4 = new Date(Date.UTC(thu.getUTCFullYear(), 0, 4));
  const jdow = (jan4.getUTCDay() + 6) % 7;
  jan4.setUTCDate(jan4.getUTCDate() - jdow + 3);
  const week = 1 + Math.round((thu - jan4) / (7 * 864e5));
  return {
    daily: u.toISOString().slice(0, 10),
    weekly: String(thu.getUTCFullYear()).padStart(4, '0') + '-W' + pad2(week),
    monthly: u.toISOString().slice(0, 7),
  };
}
function todaySpent(k) {
  return k.daily_cycle_key === cycleKeysNow().daily ? k.daily_spent_micro_usd : 0;
}
function meterHTML(spent, limit) {
  if (!limit || limit <= 0) return '<span class="pill mono">不限</span>';
  const pct = Math.min(100, spent / limit * 100);
  const state = pct >= 95 ? 'alarm' : pct >= 80 ? 'warn' : '';
  return '<div class="meter slim" data-state="' + state + '">'
    + '<div class="meter-track"><div class="meter-fill" style="width:' + pct.toFixed(1) + '%"></div></div>'
    + '<div class="meter-readout"><span>' + pct.toFixed(0) + '%</span><b>'
    + fmtUSD(spent) + ' / ' + fmtUSD(limit) + '</b></div></div>';
}
function remainMeter(name, limit, remain) {
  const used = limit ? limit - Math.max(0, remain ?? 0) : 0;
  const pct = limit ? Math.min(100, used / limit * 100) : 0;
  const state = !limit ? 'idle' : pct >= 95 ? 'alarm' : pct >= 80 ? 'warn' : '';
  return '<div class="meter slim" data-state="' + state + '">'
    + '<div class="meter-track"><div class="meter-fill" style="width:' + pct.toFixed(1) + '%"></div></div>'
    + '<div class="meter-readout"><span>' + name + '</span><b>'
    + (limit ? '余 ' + fmtUSD(Math.max(0, remain ?? 0)) : '不限') + '</b></div></div>';
}
function renderKeys() {
  const list = keysView.filtered;
  const totalAll = keysView.cache.length;
  $('key-count').textContent = '共 ' + totalAll + ' 枚'
    + (list.length !== totalAll ? ' · 筛选后 ' + list.length + ' 枚' : '');
  const start = keysView.page * keysView.size;
  const rows = list.slice(start, start + keysView.size);
  $('key-rows').innerHTML = rows.map(k => {
    const meta = STATUS_META[keyStatus(k)];
    const conc = k.max_concurrent_requests > 0 ? '≤ ' + k.max_concurrent_requests : '不限';
    return '<tr class="row" data-kid="' + esc(k.kid) + '" aria-expanded="false">'
      + '<td><div class="cell-key"><span class="label" title="' + esc(k.label || '') + '">'
      + (k.label ? esc(k.label) : '<i>无标签</i>') + '</span>'
      + '<span class="kid">' + esc(k.kid) + '</span></div></td>'
      + '<td class="cell-mono">' + esc(k.caller_id) + '</td>'
      + '<td><span class="pill ' + meta.pill + '">' + meta.label + '</span></td>'
      + '<td class="w-meter">' + meterHTML(k.spent_micro_usd, k.quota_micro_usd) + '</td>'
      + '<td class="num">' + fmtUSD(k.spent_micro_usd) + '</td>'
      + '<td class="num">' + fmtUSD(todaySpent(k)) + '</td>'
      + '<td class="num">' + conc + '</td>'
      + '<td class="cell-dim">' + esc(rel(k.last_used_at)) + '</td>'
      + '<td class="w-chev"><svg class="chev" viewBox="0 0 24 24"><path d="m9 6 6 6-6 6"/></svg></td></tr>';
  }).join('')
    || '<tr><td colspan="9"><div class="empty"><p class="empty-title">没有匹配的密钥</p>'
    + '<p class="empty-hint">调整筛选条件，或点击右上角「签发密钥」</p></div></td></tr>';

  const pages = Math.max(1, Math.ceil(list.length / keysView.size));
  $('key-pager').innerHTML = '<span class="mono">第 ' + (keysView.page + 1) + ' / ' + pages + ' 页</span>'
    + '<span class="grow"></span>'
    + '<button type="button" class="btn small" id="key-prev"' + (keysView.page <= 0 ? ' disabled' : '') + '>上一页</button>'
    + '<button type="button" class="btn small" id="key-next"'
    + ((keysView.page + 1) * keysView.size >= list.length ? ' disabled' : '') + '>下一页</button>';
  const prev = $('key-prev'), next = $('key-next');
  if (prev) prev.onclick = () => { keysView.page--; renderKeys(); };
  if (next) next.onclick = () => { keysView.page++; renderKeys(); };
}

$('key-rows').addEventListener('click', async e => {
  const tr = e.target.closest('tr.row');
  if (!tr) return;
  const open = tr.getAttribute('aria-expanded') === 'true';
  const detail = tr.nextElementSibling;
  if (detail && detail.classList.contains('detail')) {
    tr.setAttribute('aria-expanded', 'false');
    detail.remove();
    if (open) return;
  }
  tr.setAttribute('aria-expanded', 'true');
  const kid = tr.dataset.kid;
  const k = keysView.cache.find(x => x.kid === kid);
  if (!k) return;
  const st = keyStatus(k);
  const dtr = document.createElement('tr');
  dtr.className = 'detail';
  dtr.innerHTML = '<td colspan="9"><div class="detail-grid"><div class="detail-facts">'
    + fact('指纹', k.fingerprint || '-')
    + fact('principal', k.principal || '-')
    + fact('额度口径', k.caller_scope === 'key' ? '独立计额' : '归属 caller')
    + fact('过期时间', k.expires_at ? fmtDT(k.expires_at) : '永不')
    + fact('创建于', fmtDT(k.created_at))
    + fact('更新于', fmtDT(k.updated_at))
    + fact('可用模型', (k.allowed_models && k.allowed_models.length) ? k.allowed_models.join(', ') : '不限制')
    + fact('周期计数', (k.daily_cycle_key || '-') + ' / ' + (k.weekly_cycle_key || '-') + ' / ' + (k.monthly_cycle_key || '-'))
    + '</div><div class="meter-grid" id="kd-meters"><p class="note">余额核算中…</p></div>'
    + '<div class="btn-row">'
    + '<button type="button" class="btn small" data-act="edit">编辑</button>'
    + '<button type="button" class="btn small" data-act="rotate">轮换</button>'
    + '<button type="button" class="btn small" data-act="reveal">查看明文</button>'
    + (st !== 'revoked' ? '<button type="button" class="btn small danger" data-act="revoke">撤销</button>' : '')
    + '<button type="button" class="btn small danger" data-act="delete">删除</button>'
    + '</div></div></td>';
  tr.after(dtr);
  dtr.querySelector('[data-act="edit"]').onclick = () => editKeySheet(k);
  dtr.querySelector('[data-act="rotate"]').onclick = () => rotateSheet(kid);
  dtr.querySelector('[data-act="reveal"]').onclick = () => revealSheet(kid);
  const rv = dtr.querySelector('[data-act="revoke"]');
  if (rv) rv.onclick = () => confirmSheet('撤销密钥 ' + kid,
    '撤销不可逆，该 Key 将立即无法通过鉴权。历史用量保留。',
    () => post('/keys/revoke', { kid, actor: 'console' }).then(refreshKeys));
  dtr.querySelector('[data-act="delete"]').onclick = () => confirmSheet('删除密钥 ' + kid,
    '永久删除该 Key（历史用量保留）。操作不可逆。',
    () => post('/keys/delete', { kid, actor: 'console' }).then(refreshKeys));

  try {
    const b = await api('/balance?key_id=' + encodeURIComponent(kid));
    const box = $('kd-meters');
    if (!box) return;
    box.innerHTML =
      remainMeter('总额度', k.quota_micro_usd, b.total)
      + remainMeter('今日', k.daily_micro_usd, b.daily)
      + remainMeter('本周', k.weekly_micro_usd, b.weekly)
      + remainMeter('本月', k.monthly_micro_usd, b.monthly)
      + '<p class="note">在途预占 ' + fmtUSD(b.held) + ' · 并发 ' + b.concurrent
      + (k.max_concurrent_requests > 0 ? ' / ' + k.max_concurrent_requests : '')
      + ' · 当前周期 ' + cycleKeysNow().daily + '</p>';
  } catch (e) { /* 余额核算失败不打断详情 */ }
});

$('key-search').addEventListener('input', debounce(() => {
  keysView.search = $('key-search').value.trim();
  keysView.page = 0;
  refreshKeys().catch(e => toast(e.message, 'err'));
}, 350));
$('key-search').addEventListener('keydown', e => { if (e.key === 'Enter') e.preventDefault(); });
const keyStatusSel = new Select('key-status', [
  { value: '', label: '全部状态' },
  { value: 'active', label: '启用中' },
  { value: 'disabled', label: '已禁用' },
  { value: 'revoked', label: '已撤销' },
  { value: 'expired', label: '已过期' },
], v => {
  keysView.status = v;
  keysView.page = 0;
  applyKeyFilter();
}, { value: '', head: '按状态过滤' });
const keyCallerSel = new Select('key-caller', [{ value: '', label: '全部 caller' }], v => {
  keysView.caller = v;
  keysView.page = 0;
  refreshKeys().catch(e => toast(e.message, 'err'));
}, { value: '', head: '按 caller 过滤' });
// callersCache 供签发弹窗重建 caller 下拉（原先克隆 select 的 innerHTML，改组件后必须走数据源）
let callersCache = [];
async function loadCallers() {
  try {
    const r = await api('/callers');
    callersCache = r.items || [];
    keyCallerSel.setOptions([{ value: '', label: '全部 caller' }].concat(
      callersCache.map(c => ({ value: c.id, label: (c.display_name || c.id) + (c.enabled ? '' : '（停用）') }))));
  } catch (e) { /* caller 下拉失败不阻塞 */ }
}
// callerOptionsHTML 给弹窗内的原生 <select> 用（弹窗表单保持原生控件）
function callerOptionsHTML() {
  return '<option value="">默认 caller</option>' + callersCache.map(c =>
    '<option value="' + esc(c.id) + '">' + esc(c.display_name || c.id)
    + (c.enabled ? '' : '（停用）') + '</option>').join('');
}

// 签发
$('key-issue-btn').addEventListener('click', () => {
  openSheet({
    title: '签发插件密钥',
    okText: '签发',
    body: '<div class="form-grid">'
      + fieldRow('标签', '<input id="f-label" placeholder="如：张三的测试 Key">')
      + fieldRow('principal', '<input id="f-principal" placeholder="可选，属主标识">')
      + fieldRow('caller', '<select id="f-caller">' + callerOptionsHTML() + '</select>')
      + fieldRow('额度口径', '<select id="f-scope"><option value="caller">归属 caller 共享</option><option value="key">独立计额</option></select>')
      + fieldRow('过期时间', '<input id="f-expires" type="datetime-local">')
      + fieldRow('最大并发', '<input id="f-conc" type="number" min="0" placeholder="0 为不限">')
      + fieldRow('总额度（USD）', '<input id="f-quota" inputmode="decimal" placeholder="留空为不限">')
      + fieldRow('日限额（USD）', '<input id="f-daily" inputmode="decimal" placeholder="留空为不限">')
      + fieldRow('周限额（USD）', '<input id="f-weekly" inputmode="decimal" placeholder="留空为不限">')
      + fieldRow('月限额（USD）', '<input id="f-monthly" inputmode="decimal" placeholder="留空为不限">')
      + fieldRow('可用模型', '<textarea id="f-models" placeholder="逗号或换行分隔，支持 * 通配；留空不限制"></textarea>', 'wide')
      + '</div>',
    note: '明文只在签发结果里出现一次。',
    onOk: async () => {
      const num = id => { const v = $(id).value.trim(); return v ? Math.round(parseFloat(v) * 1e6) : null; };
      const models = $('f-models').value.split(/[\n,，]/).map(s => s.trim()).filter(Boolean);
      const expires = $('f-expires').value ? new Date($('f-expires').value).toISOString() : null;
      const r = await post('/keys/issue', {
        label: $('f-label').value.trim(),
        principal: $('f-principal').value.trim(),
        caller_id: $('f-caller').value || 'default',
        caller_scope: $('f-scope').value,
        quota_micro_usd: num('f-quota'),
        daily_micro_usd: num('f-daily'),
        weekly_micro_usd: num('f-weekly'),
        monthly_micro_usd: num('f-monthly'),
        max_concurrent_requests: parseInt($('f-conc').value, 10) || 0,
        allowed_models: models,
        expires_at: expires,
        actor: 'console',
      });
      $('sheet-title').textContent = '密钥已签发 · ' + r.KID;
      $('sheet-body').innerHTML = secretBlock(r.Key)
        + '<p class="note">指纹 ' + esc(r.Fingerprint) + '</p>';
      $('sheet-note').textContent = '';
      wireSecretCopy();
      staySheet('完成');
      refreshKeys().catch(() => {});
      return false;
    },
  });
});

// 编辑
function editKeySheet(k) {
  const MONEY_FIELDS = [
    ['e-quota', 'quota_micro_usd', '总额度（USD）'],
    ['e-daily', 'daily_micro_usd', '日限额（USD）'],
    ['e-weekly', 'weekly_micro_usd', '周限额（USD）'],
    ['e-monthly', 'monthly_micro_usd', '月限额（USD）'],
  ];
  openSheet({
    title: '编辑密钥 ' + k.kid,
    okText: '保存',
    body: '<div class="form-grid">'
      + fieldRow('标签', '<input id="e-label" value="' + esc(k.label || '') + '">')
      + fieldRow('启用', '<select id="e-enabled"><option value="">保持不变</option>'
        + '<option value="true"' + (k.enabled ? ' selected' : '') + '>是</option>'
        + '<option value="false"' + (!k.enabled ? ' selected' : '') + '>否</option></select>')
      + fieldRow('过期时间', '<input id="e-expires" type="datetime-local" value="'
        + (k.expires_at ? toLocalInput(new Date(k.expires_at)) : '') + '">')
      + fieldRow('最大并发', '<input id="e-conc" type="number" min="0" value="' + (k.max_concurrent_requests || 0) + '">')
      + MONEY_FIELDS.map(([id, , label]) =>
        fieldRow(label, '<input id="' + id + '" inputmode="decimal" placeholder="留空不改，输入 null 清空">')).join('')
      + fieldRow('可用模型', '<textarea id="e-models" placeholder="留空清空清单；输入 null 表示不修改">'
        + esc((k.allowed_models || []).join(', ')) + '</textarea>', 'wide')
      + '</div>',
    note: '金额字段留空表示不修改；输入 null 表示清除限制。模型清单留空表示不限制。',
    onOk: async () => {
      const body = { kid: k.kid, actor: 'console' };
      const label = $('e-label').value.trim();
      if (label !== (k.label || '')) body.label = label;
      if ($('e-enabled').value !== '') body.enabled = $('e-enabled').value === 'true';
      const exp = $('e-expires').value;
      if (exp) body.expires_at = new Date(exp).toISOString();
      else if (k.expires_at) body.expires_at = null;
      const conc = $('e-conc').value;
      if (conc !== '' && (parseInt(conc, 10) || 0) !== k.max_concurrent_requests)
        body.max_concurrent_requests = parseInt(conc, 10) || 0;
      for (const [id, field] of MONEY_FIELDS) {
        const v = $(id).value.trim();
        if (v === '') continue;
        if (v.toLowerCase() === 'null') { body[field] = null; continue; }
        const m = Math.round(parseFloat(v) * 1e6);
        const cur = k[field];
        if (cur === null || cur === undefined || m !== cur) body[field] = m;
      }
      const modelsRaw = $('e-models').value.trim();
      const modelsCur = (k.allowed_models || []).join(', ');
      if (modelsRaw.toLowerCase() === 'null') { /* 保持不变 */ }
      else if (modelsRaw !== modelsCur)
        body.allowed_models = modelsRaw ? modelsRaw.split(/[\n,，]/).map(s => s.trim()).filter(Boolean) : [];
      await post('/keys/update', body);
      toast('密钥已更新', 'ok');
      refreshKeys().catch(() => {});
    },
  });
}
function rotateSheet(kid) {
  openSheet({
    title: '轮换 ' + kid, danger: true, okText: '轮换',
    body: '<p>旧 Key 立即失效，并生成新明文（仅展示一次）。</p>',
    onOk: async () => {
      const r = await post('/keys/rotate', { kid, actor: 'console' });
      $('sheet-title').textContent = '已轮换 · ' + r.KID;
      $('sheet-body').innerHTML = secretBlock(r.Key);
      wireSecretCopy();
      staySheet('完成');
      refreshKeys().catch(() => {});
      return false;
    },
  });
}
function revealSheet(kid) {
  openSheet({
    title: '查看明文 ' + kid, okText: '解密',
    body: '<p>解密该 Key 的明文用于配置客户端。</p>',
    onOk: async () => {
      const r = await post('/keys/reveal', { kid, actor: 'console' });
      $('sheet-title').textContent = '明文 · ' + kid;
      $('sheet-body').innerHTML = secretBlock(r.key);
      wireSecretCopy();
      staySheet('关闭');
      return false;
    },
  });
}

// ---------- 用量 ----------
const REQ_SIZES = [20, 50, 100];
const reqView = {
  page: 0, size: +(localStorage.getItem('req-size') || 20) || 20,
  sort: 'ts', order: 'desc', model: '', keyId: '', result: '',
};

// 请求明细列定义。
// 默认列刻意收进视口宽度内：13 列独立排布时表格 1322px > 容器 1184px，
// 叠加外层 max-height 的竖向滚动后一个卡片里两个方向都要拖。
// 计量三列与延迟两列合并成复合单元格，其余低频列改为按需开启。
//
// sort 只填后端 ListRequests 白名单里真实存在的键（ts/cost/tokens/latency/model）：
// 其余列不给排序 affordance，避免「看起来能点但点了没反应」。
const REQ_COLS = [
  { id: 'ts', label: '时间', sort: 'ts', fixed: true, cell: x => '<td class="cell-mono">' + fmtDT(x.ts, true) + '</td>' },
  {
    id: 'key', label: '密钥', cell: x => '<td class="cell-mono" title="' + esc(x.key_id || '') + '">'
      + esc(keyLabelOf(x.key_id) || x.key_id || '-') + '</td>',
  },
  {
    id: 'model', label: '模型', sort: 'model', cell: x => '<td class="cell-mono cell-clip" title="' + esc(x.model) + '">'
      + esc(x.model || '-') + '</td>',
  },
  { id: 'provider', label: '提供方', off: true, cell: x => '<td class="cell-dim">' + esc(x.provider || '-') + '</td>' },
  {
    id: 'result', label: '结果', cell: x => '<td><span class="pill '
      + (x.result === 'ok' ? 'live' : x.result === 'blocked' ? 'warn' : 'alarm') + '">' + esc(x.result) + '</span></td>',
  },
  {
    id: 'toks', label: '输入 / 输出 / 缓存读', num: true,
    cell: x => '<td class="num"><span class="cell-toks" title="输入 ' + esc(fmtTok(x.input_tokens))
      + ' · 输出 ' + esc(fmtTok(x.output_tokens)) + ' · 缓存读 ' + esc(fmtTok(cacheReadOf(x))) + '">'
      + '<span class="tk">' + fmtTok(x.input_tokens) + '</span><span class="sep">/</span>'
      + '<span class="tk out">' + fmtTok(x.output_tokens) + '</span><span class="sep">/</span>'
      + '<span class="tk cr">' + fmtTok(cacheReadOf(x)) + '</span></span></td>',
  },
  { id: 'tokens', label: '总 Token', sort: 'tokens', num: true, cell: x => '<td class="num">' + reqTokenCell(x) + '</td>' },
  { id: 'cost', label: '费用', sort: 'cost', num: true, cell: x => '<td class="num">' + fmtUSD(x.cost_micro_usd) + '</td>' },
  {
    // 排序键指向 latency（总延迟）。旧版把「首字」表头标成 data-sort="latency"，
    // 而 latency 在后端映射到 latency_ms，点「首字」实际按总延迟排 —— 表头与行为不一致。
    id: 'lat', label: '延迟 首字→总', sort: 'latency', num: true,
    cell: x => '<td class="num"><span class="cell-lat"><span class="ttft">' + fmtSec(x.ttft_ms)
      + '</span><span class="arrow">→</span><span>' + fmtSec(x.latency_ms) + '</span></span></td>',
  },
  { id: 'tps', label: 'TPS', num: true, off: true, cell: x => '<td class="num">' + fmtTPS(x.tps_milli) + '</td>' },
  { id: 'reasoning', label: '推理', num: true, off: true, cell: x => '<td class="num">' + fmtTok(x.reasoning_tokens) + '</td>' },
  { id: 'tier', label: '档位', off: true, cell: x => '<td class="cell-dim">' + esc(x.tier || '-') + '</td>' },
];
const REQ_COLS_DEFAULT = REQ_COLS.filter(c => !c.off).map(c => c.id);
function loadReqCols() {
  try {
    const saved = JSON.parse(localStorage.getItem('req-cols') || 'null');
    if (Array.isArray(saved) && saved.length) {
      const valid = saved.filter(id => REQ_COLS.some(c => c.id === id));
      if (valid.length) return new Set(valid.concat(REQ_COLS.filter(c => c.fixed).map(c => c.id)));
    }
  } catch (_) { /* 偏好损坏则回默认 */ }
  return new Set(REQ_COLS_DEFAULT);
}
let reqCols = loadReqCols();
const activeReqCols = () => REQ_COLS.filter(c => reqCols.has(c.id));
// reqTokenCell 总 Token 单元格：上游未返回用量但已按预占扣费时给出显式标记。
function reqTokenCell(x) {
  const noUsage = !(+x.input_tokens || 0) && !(+x.output_tokens || 0)
    && !(+x.cache_read_tokens || 0) && !(+x.cache_creation_tokens || 0);
  return noUsage && (+x.cost_micro_usd || 0) > 0
    ? '<span class="pill warn mono" title="上游未返回用量，费用按预占估算扣费">未捕获</span>'
    : '<b>' + fmtTok(effTokens(x)) + '</b>';
}

// fillReqSuggestions 填充模型/密钥筛选框的联想候选（自建组合框，标签与 kid 同时可见）。
function fillReqSuggestions() {
  api('/usage/dimension?' + new URLSearchParams({ dimension: 'model', limit: '200' }))
    .then(r => reqModelCombo.setOptions((r.rows || []).filter(x => x.value)
      .map(x => ({ value: x.value, label: x.value }))))
    .catch(() => {});
  // 值提交 kid，但标签同时显示 —— datalist 在 Firefox 只显示 value，标签会丢
  reqKeyCombo.setOptions(keysView.cache.map(k => ({
    value: k.kid, label: k.label || '(无标签)', sub: k.kid,
  })));
}

loaders.usage = async () => {
  await Promise.all([loadDim(), loadCosts()]);
  await loadRequests();
  fillReqSuggestions();
  stamp();
};
const DIMS = [
  { value: 'model', label: '模型' },
  { value: 'provider', label: '提供方' },
  { value: 'source', label: '来源' },
  { value: 'auth_type', label: '认证类型' },
  { value: 'auth_label', label: '认证账号' },
  { value: 'result', label: '结果' },
  { value: 'key_id', label: '密钥' },
  { value: 'caller_id', label: 'caller' },
];
async function loadDim() {
  const dim = dimSel.value;
  const r = await api('/usage/dimension?' + new URLSearchParams({ dimension: dim, ...rangeParams() }));
  // 费用相同时（如全部未计价）按请求数降序，保证视觉排序稳定。
  const rows = (r.rows || []).slice()
    .sort((a, b) => b.cost_micro_usd - a.cost_micro_usd || (b.requests || 0) - (a.requests || 0));
  if (!rows.length) {
    $('dim-body').innerHTML = '<div class="empty"><p class="empty-title">暂无数据</p>'
      + '<p class="empty-hint">所选时间范围内没有请求记录</p></div>';
    return;
  }
  // 占比 = 本行请求数 / 所有分组请求数之和，各行之和为 100%（四舍五入误差除外）。
  //
  // 曾用「最大行请求数」作分母（v0.3.0 为修 >100% 而引入），那算的是「相对最大值的
  // 比例」而不是占比：最大行恒显示 100%，且各行相加远超 100%（result 维度下
  // ok 100% + error 8% = 108%，provider 维度累计 263%），与列名和常识都不符。
  //
  // 分母用行内之和而非 r.total.requests —— 服务端的 total 是**只对返回行**累加的
  // （accumulate 在扫描循环里调用），带 limit 时它等于返回行之和、不是全量总数，
  // 因此两者在这里等价；本函数不传 limit，拿到的就是全部分组。
  // 条长仍按最大值归一，保证最长条填满、短条之间仍可比。
  const denom = rows.reduce((a, row) => a + (Number(row.requests) || 0), 0);
  const maxReq = Math.max(1, ...rows.map(row => Number(row.requests) || 0));
  const shareOf = row => denom > 0 ? (Number(row.requests) || 0) / denom * 100 : 0;
  // 密钥维度的分组值是 kid，显示标签更可读（与请求表/概览/详情弹窗同口径），
  // kid 保留在 title 里。其余维度分组值本身就是可读文本。
  const nameOf = row => {
    const v = row.value || '';
    if (dim === 'key_id' && v) return keyLabelOf(v) || v;
    return v || '(空)';
  };
  const titleOf = row => {
    const v = row.value || '';
    if (dim === 'key_id' && v) {
      const label = keyLabelOf(v);
      return label ? label + ' · ' + v : v;
    }
    return v || '(空)';
  };
  $('dim-body').innerHTML = '<div class="table-wrap"><table class="data"><thead><tr>'
    + '<th class="w-grow">' + esc((DIMS.find(d => d.value === dim) || {}).label || dim) + '</th>'
    + '<th class="num">请求</th><th class="num">失败</th><th class="num">Token</th><th class="num">费用</th>'
    + '<th class="num">平均延迟</th><th class="num">TPS</th></tr></thead><tbody>'
    + rows.map(row => {
      const share = shareOf(row);
      return '<tr>'
      + '<td><div class="bar-cell" title="' + esc(titleOf(row)) + '"><div class="bar-top"><span class="bar-name">'
      + esc(nameOf(row)) + '</span><span class="bar-pct">'
      + (share < 10 ? share.toFixed(1) : share.toFixed(0)) + '%</span></div>'
      + '<div class="bar-line"><span style="width:'
      + ((Number(row.requests) || 0) / maxReq * 100).toFixed(1) + '%"></span></div></div></td>'
      + '<td class="num">' + fmtInt(row.requests) + '</td>'
      + '<td class="num">' + (row.failures ? '<span class="pill alarm mono">' + fmtInt(row.failures) + '</span>' : '0') + '</td>'
      + '<td class="num">' + fmtTok(effTokens(row)) + '</td>'
      + '<td class="num">' + fmtUSD(row.cost_micro_usd) + '</td>'
      + '<td class="num">' + fmtSec(row.latency_avg_ms) + '</td>'
      + '<td class="num">' + fmtTPS(row.tps_avg_milli) + '</td></tr>';
    }).join('')
    + '</tbody></table></div>';
}
// fmtTPS 展示 TPS：超过 3000 token/s 视为宿主缓冲整转产生的坏测量
// （与后端落库上限同口径），v0.3.0 之前入库的历史脏行在展示层一并隐藏。
const maxPlausibleTPS = 3000;
const fmtTPS = milli => milli > 0 && milli / 1000 <= maxPlausibleTPS ? (milli / 1000).toFixed(1) : '-';

function kv(name, value) { return '<div class="kv-row"><dt>' + name + '</dt><dd>' + value + '</dd></div>'; }
async function loadCosts() {
  const costs = await api('/costs?' + new URLSearchParams(rangeParams()));
  const cover = costs.requests ? Math.round(costs.priced_requests / costs.requests * 100) : 0;
  const rate = S.fx;
  const cny = rate && costs.cost_micro_usd
    ? '¥' + fmtUSD(Math.round(costs.cost_micro_usd * rate.usd_to_cny_micro / 1e6)).slice(1) : '-';
  $('cost-body').innerHTML = '<div class="kv">'
    + kv('请求总数', fmtInt(costs.requests))
    + kv('已计价请求', fmtInt(costs.priced_requests))
    + kv('价格覆盖率', '<span class="pill ' + (cover >= 90 ? 'live' : cover >= 60 ? 'warn' : 'alarm') + ' mono">' + cover + '%</span>')
    + kv('总费用', '<b class="mono">' + fmtUSD(costs.cost_micro_usd) + '</b>')
    + kv('约合人民币', cny)
    + '</div><p class="note" style="margin-top:10px">未命中价格的请求不计费用；汇率来源 '
    + esc(rate ? rate.source + (rate.fallback ? '（兜底）' : '') : '未知') + '。</p>';
}
async function loadRequests() {
  const q = new URLSearchParams({
    limit: String(reqView.size), offset: String(reqView.page * reqView.size),
    sort: reqView.sort, order: reqView.order, ...rangeParams(),
  });
  if (reqView.model) q.set('model', reqView.model);
  if (reqView.keyId) q.set('key_id', reqView.keyId);
  if (reqView.result) q.set('result', reqView.result);
  const r = await api('/requests?' + q);
  const items = r.items || [], total = r.total || 0;
  const pages = Math.max(1, Math.ceil(total / reqView.size));
  const cols = activeReqCols();
  $('req-count').textContent = '共 ' + fmtInt(total) + ' 条 · 第 ' + (reqView.page + 1) + ' / ' + pages + ' 页';
  // 表头随列偏好重建；只有后端支持的排序键才带 .sort
  $('req-head').innerHTML = cols.map(c => '<th' + (c.num ? ' class="num' + (c.sort ? ' sort' : '') + '"'
    : (c.sort ? ' class="sort"' : '')) + (c.sort ? ' data-sort="' + c.sort + '"' : '')
    + (c.sort && reqView.sort === c.sort ? ' data-dir="' + reqView.order + '"' : '')
    + '>' + esc(c.label) + '</th>').join('');
  $('req-rows').innerHTML = items.map(x =>
    '<tr class="row" data-id="' + esc(x.id) + '">' + cols.map(c => c.cell(x)).join('') + '</tr>').join('')
    || '<tr><td colspan="' + cols.length + '"><div class="empty"><p class="empty-title">没有匹配的请求</p>'
    + '<p class="empty-hint">调整筛选条件或时间范围</p></div></td></tr>';
  $('req-rows').dataset.items = JSON.stringify(items);
  // 分页：75 页时只有上/下一页不够，补每页条数与跳页
  $('req-pager').innerHTML = '<span class="jump">每页'
    + REQ_SIZES.map(s => '<button type="button" class="btn small" data-size="' + s + '"'
      + (s === reqView.size ? ' disabled' : '') + '>' + s + '</button>').join('') + '</span>'
    + '<span class="grow"></span>'
    + '<button type="button" class="btn small" id="req-prev"' + (reqView.page <= 0 ? ' disabled' : '') + '>上一页</button>'
    + '<span class="jump"><input type="number" id="req-jump" min="1" max="' + pages + '" value="' + (reqView.page + 1)
    + '" aria-label="跳转到页码"><span class="mono">/ ' + pages + '</span></span>'
    + '<button type="button" class="btn small" id="req-next"'
    + ((reqView.page + 1) * reqView.size >= total ? ' disabled' : '') + '>下一页</button>';
  const go = () => loadRequests().catch(e => toast(e.message, 'err'));
  const prev = $('req-prev'), next = $('req-next'), jump = $('req-jump');
  if (prev) prev.onclick = () => { reqView.page--; go(); };
  if (next) next.onclick = () => { reqView.page++; go(); };
  $('req-pager').querySelectorAll('[data-size]').forEach(b => b.onclick = () => {
    reqView.size = +b.dataset.size;
    localStorage.setItem('req-size', String(reqView.size));
    reqView.page = 0;
    go();
  });
  if (jump) {
    const apply = () => {
      const p = Math.max(1, Math.min(pages, parseInt(jump.value, 10) || 1));
      if (p - 1 === reqView.page) { jump.value = String(p); return; }
      reqView.page = p - 1;
      go();
    };
    jump.onchange = apply;
    jump.onkeydown = e => { if (e.key === 'Enter') { e.preventDefault(); apply(); } };
  }
}
$('req-table').querySelector('thead').addEventListener('click', e => {
  const th = e.target.closest('th.sort');
  if (!th) return;
  const keyName = th.dataset.sort;
  if (reqView.sort === keyName) reqView.order = reqView.order === 'desc' ? 'asc' : 'desc';
  else { reqView.sort = keyName; reqView.order = 'desc'; }
  reqView.page = 0;
  loadRequests().catch(err => toast(err.message, 'err'));
});
$('req-rows').addEventListener('click', e => {
  const tr = e.target.closest('tr.row');
  if (!tr) return;
  let items = [];
  try { items = JSON.parse($('req-rows').dataset.items || '[]'); } catch (_) { /* 忽略 */ }
  const x = items.find(i => i.id === tr.dataset.id);
  if (!x) return;
  openSheet({
    title: '请求明细 · ' + fmtDT(x.ts, true),
    okText: '关闭', noFocus: true,
    body: '<div class="detail-facts">'
      + fact('模型', x.model || '-') + fact('提供方', x.provider || '-')
      + fact('来源', x.source || '-') + fact('结果', x.result)
      + fact('密钥', x.key_id ? (keyLabelOf(x.key_id) || x.key_id) : '-') + fact('caller', x.caller_id || '-')
      + fact('认证账号', x.auth_label || x.auth_id || '-') + fact('认证类型', x.auth_type || '-')
      + fact('档位', x.tier || '-') + fact('思考强度', x.thinking_intensity || '-')
      + fact('输入 Token', fmtTok(x.input_tokens)) + fact('输出 Token', fmtTok(x.output_tokens))
      + fact('推理 Token', fmtTok(x.reasoning_tokens)) + fact('缓存读', fmtTok(cacheReadOf(x)))
      + fact('缓存写', fmtTok(x.cache_creation_tokens)) + fact('总 Token', fmtTok(effTokens(x)))
      + (!(+x.input_tokens || 0) && !(+x.output_tokens || 0) && (+x.cost_micro_usd || 0) > 0
        ? fact('用量捕获', '上游未返回用量，费用按预占估算扣费') : '')
      + fact('首字延迟', fmtSec(x.ttft_ms))
      + fact('生成耗时', fmtSec(x.generation_ms))
      + fact('TPS', fmtTPS(x.tps_milli))
      + fact('总延迟', fmtSec(x.latency_ms))
      + fact('费用', fmtUSD(x.cost_micro_usd)) + fact('命中计价', x.priced ? '是' : '否')
      + (x.reservation_id ? fact('预占 ID', x.reservation_id) : '')
      + '</div>',
  });
});
const reqFilterChanged = () => {
  reqView.model = reqModelCombo.value;
  reqView.keyId = reqKeyCombo.value;
  reqView.result = reqResultSel.value;
  reqView.page = 0;
  loadRequests().catch(e => toast(e.message, 'err'));
};
const reqModelCombo = new Combo('req-model', reqFilterChanged);
const reqKeyCombo = new Combo('req-key', reqFilterChanged);
const reqResultSel = new Select('req-result', [
  { value: '', label: '全部结果' },
  { value: 'ok', label: '成功 ok' },
  { value: 'error', label: '失败 error' },
], reqFilterChanged, { value: '', head: '按结果过滤' });
const dimSel = new Select('dim', DIMS, () => loadDim().catch(e => toast(e.message, 'err')),
  { value: 'model', head: '聚合维度' });
// 列偏好：默认列收进视口，低频列按需开启（偏好存 localStorage）
new MultiSelect('req-cols', REQ_COLS.map(c => ({ value: c.id, label: c.label, fixed: c.fixed })),
  reqCols, sel => {
    reqCols = new Set(sel);
    localStorage.setItem('req-cols', JSON.stringify([...sel]));
    loadRequests().catch(e => toast(e.message, 'err'));
  }, { text: '列', head: '显示列', defaults: REQ_COLS_DEFAULT });
$('req-export').addEventListener('click', async () => {
  try {
    const r = await api('/export/csv', {
      method: 'POST',
      body: JSON.stringify({
        kind: 'requests', limit: 50000,
        filter: Object.assign({ model: reqView.model, key_id: reqView.keyId, result: reqView.result }, rangeParams()),
      }),
    });
    const blob = await r.blob();
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'cpa-usage-manager-requests.csv';
    a.click();
    URL.revokeObjectURL(a.href);
    toast('CSV 已导出', 'ok');
  } catch (e) { toast(e.message, 'err'); }
});

// ---------- 价格 ----------
const pricingCache = { items: [] };
loaders.pricing = async () => {
  const [r, fx] = await Promise.all([api('/pricing'), api('/exchange-rate')]);
  S.fx = fx;
  $('fx-info').textContent = fx && fx.usd_to_cny_micro
    ? 'USD→CNY ' + (fx.usd_to_cny_micro / 1e6).toFixed(4) + ' · ' + fx.source + (fx.fallback ? '（兜底）' : '') : '';
  const items = (r.items || []).slice().sort((a, b) => b.priority - a.priority || a.id - b.id);
  pricingCache.items = items;
  $('pricing-rows').innerHTML = items.map(p => '<tr>'
    + '<td class="num cell-mono">' + p.priority + '</td>'
    + '<td><span class="pill signal mono">' + esc(p.match_kind) + '</span></td>'
    + '<td class="cell-mono w-grow" title="' + esc(p.pattern) + '">' + esc(p.pattern) + '</td>'
    + '<td><span class="pill ' + (p.enabled ? 'live' : '') + '">' + (p.enabled ? '启用' : '停用') + '</span></td>'
    + '<td class="num">' + fmtPrice(p.price_input) + '</td>'
    + '<td class="num">' + fmtPrice(p.price_output) + '</td>'
    + '<td class="num">' + fmtPrice(p.price_cache_read) + '</td>'
    + '<td class="num">' + fmtPrice(p.price_cache_creation) + '</td>'
    + '<td class="cell-dim">' + esc(p.source === 'models_dev' ? 'models.dev' : '手动') + '</td>'
    + '<td class="w-act"><button type="button" class="btn small" data-edit="' + p.id + '">编辑</button>'
    + '<button type="button" class="btn small danger" data-id="' + p.id + '">删除</button></td></tr>').join('')
    || '<tr><td colspan="10"><div class="empty"><p class="empty-title">还没有计价规则</p>'
    + '<p class="empty-hint">新增规则，或在上方搜索 models.dev 后按条添加</p></div></td></tr>';
  stamp();
};
$('pricing-rows').addEventListener('click', e => {
  const del = e.target.closest('button[data-id]');
  if (!del) return;
  confirmSheet('删除计价规则 #' + del.dataset.id,
    '删除后相关模型将回落到兜底规则（通常免费）。',
    async () => {
      await post('/pricing/delete', { id: parseInt(del.dataset.id, 10), actor: 'console' });
      loaders.pricing().catch(() => {});
    });
});
// micro-USD/百万 token → 弹窗里的 $/M 数值文本。
const priceInputVal = v => String((Number(v) || 0) / 1e6);
function pricingFormBody(p) {
  const sel = (kind, cur) => '<option value="' + kind + '"' + (kind === cur ? ' selected' : '') + '>' + kind + '</option>';
  const kindSel = p
    ? ['exact', 'glob', 'regexp'].map(k => sel(k, p.match_kind)).join('')
    : '<option value="exact">exact 完全匹配</option><option value="glob" selected>glob 通配</option><option value="regexp">regexp 正则</option>';
  return '<div class="form-grid">'
    + fieldRow('匹配方式', '<select id="p-kind">' + kindSel + '</select>')
    + fieldRow('优先级', '<input id="p-priority" type="number" value="' + (p ? p.priority : 100) + '">')
    + fieldRow('模式', '<input id="p-pattern" value="' + (p ? esc(p.pattern) : '') + '" placeholder="如 gpt-* 或 claude-sonnet-4" spellcheck="false">')
    + fieldRow('输入价 $/M', '<input id="p-in" inputmode="decimal" value="' + (p ? priceInputVal(p.price_input) : '') + '" placeholder="0">')
    + fieldRow('输出价 $/M', '<input id="p-out" inputmode="decimal" value="' + (p ? priceInputVal(p.price_output) : '') + '" placeholder="0">')
    + fieldRow('推理价 $/M', '<input id="p-reasoning" inputmode="decimal" value="' + (p ? priceInputVal(p.price_reasoning) : '') + '" placeholder="0">')
    + fieldRow('缓存价 $/M', '<input id="p-cached" inputmode="decimal" value="' + (p ? priceInputVal(p.price_cached) : '') + '" placeholder="0">')
    + fieldRow('缓存读 $/M', '<input id="p-cache-read" inputmode="decimal" value="' + (p ? priceInputVal(p.price_cache_read) : '') + '" placeholder="0">')
    + fieldRow('缓存写 $/M', '<input id="p-cache-create" inputmode="decimal" value="' + (p ? priceInputVal(p.price_cache_creation) : '') + '" placeholder="0">')
    + fieldRow('状态', '<select id="p-enabled"><option value="true"' + (!p || p.enabled ? ' selected' : '') + '>启用</option>'
      + '<option value="false"' + (p && !p.enabled ? ' selected' : '') + '>停用</option></select>')
    + '</div>';
}
function pricingSubmit() {
  const num = id => Math.round((parseFloat($(id).value) || 0) * 1e6);
  return {
    match_kind: $('p-kind').value,
    pattern: $('p-pattern').value.trim() || '*',
    priority: parseInt($('p-priority').value, 10) || 0,
    enabled: $('p-enabled').value === 'true',
    price_input: num('p-in'), price_output: num('p-out'),
    price_reasoning: num('p-reasoning'), price_cached: num('p-cached'),
    price_cache_read: num('p-cache-read'), price_cache_creation: num('p-cache-create'),
    accounting_mode: 'default', billing_mode: 'token', per_image_micro_usd: 0,
    source: 'manual',
  };
}
$('pricing-add').addEventListener('click', () => {
  openSheet({
    title: '新增计价规则', okText: '保存',
    body: pricingFormBody(null),
    note: '单价为每百万 Token 的美元金额；同匹配方式同模式重复添加将覆盖原规则。',
    onOk: async () => {
      await post('/pricing', pricingSubmit());
      toast('规则已保存', 'ok');
      loaders.pricing().catch(() => {});
    },
  });
});
$('pricing-rows').addEventListener('click', e => {
  const b = e.target.closest('button[data-edit]');
  if (!b) return;
  const p = pricingCache.items.find(x => x.id === parseInt(b.dataset.edit, 10));
  if (!p) return;
  openSheet({
    title: '编辑计价规则 #' + p.id, okText: '保存',
    body: pricingFormBody(p),
    note: '修改匹配方式或模式会按新键生效；若新键已存在，两条将合并为一条。计价口径与来源保持原值。',
    onOk: async () => {
      const body = pricingSubmit();
      body.id = p.id;
      // 口径字段不在表单里，保留原值避免把按张计价 / models.dev 规则改坏。
      body.accounting_mode = p.accounting_mode;
      body.billing_mode = p.billing_mode;
      body.per_image_micro_usd = p.per_image_micro_usd;
      body.source = p.source;
      body.models_dev_id = p.models_dev_id;
      await post('/pricing', body);
      toast('规则已更新', 'ok');
      loaders.pricing().catch(() => {});
    },
  });
});
// models.dev 搜索后按条添加：目录在服务端缓存 10 分钟，避免整本同步的瞬时 IO。
async function pricingSearchRun() {
  const q = $('pricing-search-input').value.trim();
  if (!q) { toast('先输入模型关键词', 'err'); return; }
  const box = $('pricing-search-results');
  box.hidden = false;
  box.innerHTML = pricingSearchHead() + '<p class="note" style="padding:10px 14px">正在搜索 models.dev…</p>';
  try {
    const list = await api('/pricing/search?' + new URLSearchParams({ q, limit: '20' }));
    box.dataset.items = JSON.stringify(list);
    box.innerHTML = pricingSearchHead()
      + (list.length
        ? '<div class="search-list">' + list.map((c, i) =>
          '<div class="search-item"><div class="si-main">'
          + '<span class="si-name">' + esc(c.name || c.model_id) + '</span>'
          + '<span class="si-id mono">' + esc(c.provider_id) + '/' + esc(c.model_id) + '</span></div>'
          + '<span class="si-price mono">入 ' + fmtPrice(c.price_input) + ' · 出 ' + fmtPrice(c.price_output) + '</span>'
          + '<button type="button" class="btn small primary" data-si="' + i + '">添加</button></div>').join('') + '</div>'
        : '<p class="note" style="padding:10px 14px">没有匹配的模型，换个关键词试试。</p>');
  } catch (e) { box.innerHTML = pricingSearchHead() + '<p class="note" style="padding:10px 14px">' + esc(e.message) + '</p>'; }
}
// pricingSearchHead 搜索结果面板的标题栏，带关闭按钮（结果面板本身没有原生收起入口）。
function pricingSearchHead() {
  return '<div class="search-head"><span>models.dev 搜索结果</span>'
    + '<button type="button" class="btn small" data-search-close>关闭</button></div>';
}
$('pricing-search-results').addEventListener('click', e => {
  if (!e.target.closest('[data-search-close]')) return;
  $('pricing-search-results').hidden = true;
});
$('pricing-search-btn').addEventListener('click', pricingSearchRun);
$('pricing-search-input').addEventListener('keydown', e => {
  if (e.key === 'Enter') { e.preventDefault(); pricingSearchRun(); }
});
$('pricing-reset').addEventListener('click', () => {
  openSheet({
    title: '清空计价规则',
    okText: '清空', danger: true,
    body: '<p>将删除全部自定义计价规则，只保留全模型免费兜底规则（glob:*）。'
      + '密钥额度与用量数据不受影响。此操作不可撤销，确定继续吗？</p>',
    onOk: async () => {
      const r = await post('/pricing/reset', { actor: 'console' });
      toast('已清空 ' + (r.deleted || 0) + ' 条计价规则', 'ok');
      loaders.pricing().catch(() => {});
    },
  });
});
$('pricing-search-results').addEventListener('click', async e => {
  const b = e.target.closest('button[data-si]');
  if (!b) return;
  let list = [];
  try { list = JSON.parse($('pricing-search-results').dataset.items || '[]'); } catch (_) { /* 忽略 */ }
  const c = list[+b.dataset.si];
  if (!c) return;
  b.disabled = true;
  b.textContent = '已添加';
  try {
    await post('/pricing', {
      match_kind: 'exact', pattern: c.pattern, priority: 100, enabled: true,
      price_input: c.price_input, price_output: c.price_output,
      price_reasoning: c.price_reasoning, price_cached: c.price_cached,
      price_cache_read: c.price_cache_read, price_cache_creation: c.price_cache_creation,
      accounting_mode: 'default', billing_mode: 'token', per_image_micro_usd: 0,
      source: c.source, models_dev_id: c.models_dev_id,
    });
    toast('已添加 ' + c.model_id, 'ok');
    loaders.pricing().catch(() => {});
  } catch (err) {
    b.disabled = false;
    b.textContent = '添加';
    toast(err.message, 'err');
  }
});
$('fx-refresh').addEventListener('click', async () => {
  try {
    S.fx = await post('/exchange-rate');
    toast('汇率已刷新', 'ok');
    loaders.pricing().catch(() => {});
  } catch (e) { toast(e.message, 'err'); }
});

// ---------- 认证额度 ----------
loaders.auth = async () => {
  const r = await api('/auth-quotas');
  const items = r.items || [];
  $('auth-body').innerHTML = items.length
    ? '<div class="auth-grid">' + items.map(a => {
      const pill = a.status === 'ok' ? 'live' : a.status === 'error' ? 'alarm' : '';
      const snap = a.snapshot ? Object.entries(a.snapshot)
        .filter(([, v]) => v !== null && typeof v !== 'object')
        .map(([k, v]) => kv(esc(k), esc(String(v)))).join('') : '';
      const wins = (a.windows || []).map(w => '<div class="kv-row"><dt>'
        + '<span class="pill mono">' + esc(w.window_id) + '</span></dt>'
        + '<dd>观测 ' + fmtInt(w.observed) + ' · 增量 ' + fmtInt(w.delta) + '</dd></div>').join('');
      return '<div class="auth-card"><div class="auth-card-top">'
        + '<span class="pill ' + pill + '">' + esc(a.status || 'unknown') + '</span>'
        + '<div class="who"><span class="prov">' + esc(a.provider) + '</span>'
        + '<span class="aid">' + esc(a.auth_id) + '</span></div></div>'
        + (snap ? '<div class="kv">' + snap + '</div>' : '')
        + (wins ? '<div class="kv">' + wins + '</div>' : '')
        + '<p class="note">抓取于 ' + (a.fetched_at ? fmtDT(a.fetched_at, true) : '-') + '</p></div>';
    }).join('') + '</div>'
    : '<div class="empty"><p class="empty-title">暂无上游认证额度</p>'
    + '<p class="empty-hint">宿主侧产生 OAuth 用量观测后会出现在这里</p></div>';
  stamp();
};

// ---------- 系统 ----------
loaders.system = async () => {
  const h = await api('/health');
  const s = h.stats || {};
  S.stats = s;
  $('sys-readouts').innerHTML =
    readout('数据库文件', fmtBytes(s.file_bytes),
      'schema v' + s.schema_version + ' · ' + (s.writable ? '可写' : '只读'), !s.writable)
    + readout('请求明细', fmtInt(s.requests), '逐请求记录')
    + readout('分钟聚合', fmtInt(s.rollups), '趋势与维度查询的数据源')
    + readout('在途预占', fmtInt(s.held_reservations), '未结算的额度预占', s.held_reservations > 0)
    + readout('密钥', fmtInt(s.keys), '含已撤销 / 过期')
    + readout('Caller', fmtInt(s.callers), '归属记录')
    + readout('计价规则', fmtInt(s.pricing_rules), 'manual + models.dev')
    + readout('存储重试', fmtInt(s.io_retries || 0),
      s.io_retries > 0 ? '瞬时 I/O 故障已自动重试；持续增长请把 data_dir 移出杀毒/同步盘' : '本次运行未出现瞬时 I/O 故障',
      s.io_retries > 0);
  $('db-note').textContent = s.writable
    ? '备份为单文件 SQLite 快照；恢复前服务端会做一致性检查。'
    : '当前实例处于只读模式（可能存在跨进程写者），备份可用，恢复不可用。';
  updateBadges();
  stamp();
};
$('backup-btn').addEventListener('click', async () => {
  try {
    const r = await api('/backup');
    const blob = await r.blob();
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'cpa-usage-manager-backup.db';
    a.click();
    URL.revokeObjectURL(a.href);
    toast('备份已下载（' + fmtBytes(blob.size) + '）', 'ok');
  } catch (e) { toast(e.message, 'err'); }
});
$('restore-file').addEventListener('change', () => {
  const f = $('restore-file').files[0];
  $('restore-name').textContent = f ? f.name + '（' + fmtBytes(f.size) + '）' : '选择备份文件…';
});
$('restore-btn').addEventListener('click', () => {
  const f = $('restore-file').files[0];
  if (!f) { toast('请先选择备份文件', 'err'); return; }
  openSheet({
    title: '整库恢复', danger: true, okText: '确认恢复',
    body: '<p>将用 <b>' + esc(f.name) + '</b> 替换当前数据库的全部内容。'
      + '若备份来自其他机器，必须同时迁移 <span class="mono">data_dir/key-peppers</span>，否则密文无法解密。</p>',
    onOk: async () => {
      const res = await fetch(API + '/restore?actor=console', {
        method: 'POST',
        headers: { Authorization: 'Bearer ' + key, 'X-Confirm-Restore': 'replace' },
        body: f,
      });
      if (!res.ok) {
        let msg = 'HTTP ' + res.status;
        try { const j = await res.json(); if (j.error) msg = j.error; } catch (_) { /* 忽略 */ }
        throw new Error(msg);
      }
      const j = await res.json();
      const rows = j.tables ? Object.values(j.tables).reduce((a, b) => a + b, 0) : 0;
      toast('恢复完成：' + fmtBytes(j.bytes || f.size) + ' · ' + rows + ' 行', 'ok');
      loaders.system().catch(() => {});
    },
  });
});
function maintainRun(vacuum) {
  return async () => {
    try {
      const r = await post('/maintain', { vacuum, actor: 'console' });
      const g = k => r[k] !== undefined ? r[k] : r[k.charAt(0).toUpperCase() + k.slice(1)];
      $('maintain-note').textContent = '上次结果：清理请求 ' + fmtInt(g('requests'))
        + ' · 聚合 ' + fmtInt(g('rollups')) + ' · 预占 ' + fmtInt(g('reservations'))
        + ' · 合并重复 ' + fmtInt(g('deduped'))
        + (vacuum ? ' · 已 VACUUM' : '');
      toast(vacuum ? '清理并 VACUUM 完成' : '清理完成', 'ok');
      loaders.system().catch(() => {});
    } catch (e) { toast(e.message, 'err'); }
  };
}
$('maintain-btn').addEventListener('click', maintainRun(false));
$('vacuum-btn').addEventListener('click', maintainRun(true));
$('dedupe-btn').addEventListener('click', async () => {
  const btn = $('dedupe-btn');
  btn.disabled = true;
  try {
    const r = await post('/dedupe', { actor: 'console' });
    const n = r.merged || 0;
    $('maintain-note').textContent = n > 0
      ? '上次对账：合并了 ' + fmtInt(n) + ' 条重复请求行，费用口径不变（保留执行器行的结算金额）。'
      : '上次对账：未发现重复请求行。';
    toast(n > 0 ? '对账完成：合并 ' + fmtInt(n) + ' 条' : '对账完成：无重复', 'ok');
    loaders.system().catch(() => {});
  } catch (e) { toast(e.message, 'err'); } finally { btn.disabled = false; }
});
$('reset-confirm').addEventListener('input', () => {
  $('reset-btn').disabled = $('reset-confirm').value !== 'reset';
});
$('reset-btn').addEventListener('click', () => {
  openSheet({
    title: '重置统计', danger: true, okText: '确认重置',
    body: '<p>将清空逐请求明细、分钟聚合、已终结预占与密钥周期计数器。'
      + '<b>密钥与计价规则保留</b>。</p>',
    onOk: async () => {
      const r = await post('/reset', { confirm: 'reset', actor: 'console' });
      toast('重置完成：请求 ' + fmtInt(r.requests) + ' · 聚合 ' + fmtInt(r.rollups), 'ok');
      $('reset-confirm').value = '';
      $('reset-btn').disabled = true;
      loaders.system().catch(() => {});
    },
  });
});

// ---------- 徽标 ----------
function updateBadges() {
  const bad = keysView.cache.filter(k => keyStatus(k) !== 'active').length;
  const dot = document.querySelector('[data-badge="keys"]');
  if (dot) { dot.hidden = bad === 0; dot.textContent = bad > 99 ? '99+' : String(bad); }
  const sys = document.querySelector('[data-badge="system"]');
  if (sys) sys.hidden = !(S.stats && S.stats.writable === false);
}

// ---------- 登录门 ----------
$('gate-form').addEventListener('submit', async e => {
  e.preventDefault();
  const btn = $('gate-submit');
  const k = $('gate-key').value.trim();
  if (!k) return;
  btn.disabled = true;
  btn.textContent = '验证中…';
  $('gate-error').hidden = true;
  const saved = key;
  key = k;
  try {
    await api('/health');
    sessionStorage.setItem('cpa-management-key', k);
    $('gate-key').value = '';
    showApp();
  } catch (err) {
    key = saved;
    $('gate-error').textContent = err.message === '管理密钥无效或已失效' ? '管理密钥不正确' : err.message;
    $('gate-error').hidden = false;
  } finally {
    btn.disabled = false;
    btn.textContent = '进入面板';
  }
});

function showApp() {
  $('gate').hidden = true;
  $('app').hidden = false;
  loadCallers();
  refreshKeys().catch(() => {}); // 预热徽标
  api('/health').then(h => { S.stats = h.stats; updateBadges(); }).catch(() => {});
  switchTab('overview');
}

// ---------- 启动 ----------
applyTheme();
if (key) showApp();
else { $('gate').hidden = false; $('gate-key').focus(); }
})();
