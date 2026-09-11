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
// savePref 面板偏好双写：localStorage 即时生效，ui_ 前缀键经 /preferences
// 同步到服务器（fire-and-forget，失败不影响本地体验）；多设备登录后由
// syncPrefsFromServer 以服务器值回灌本地（见 showApp）。
const savePref = (k, v) => {
  localStorage.setItem(k, v);
  post('/preferences', { ['ui_' + k]: v }).catch(() => {});
};
// downloadFile 下载导出接口返回的二进制流。文件名优先取响应头
// Content-Disposition（服务端已带 kind 与时间范围标记），无则用 fallback。
async function downloadFile(path, body, fallbackName) {
  const r = await api(path, { method: 'POST', body: JSON.stringify(body) });
  const blob = await r.blob();
  const m = (r.headers.get('Content-Disposition') || '').match(/filename="([^"]+)"/);
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = m ? m[1] : fallbackName;
  a.click();
  URL.revokeObjectURL(a.href);
}

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
    savePref('console-range', rangeState.id);
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
  $('sheet-copy').hidden = !$('sheet-note').textContent;
  const ok = $('sheet-ok');
  const cancel = $('sheet-cancel');
  ok.textContent = o.okText || '确定';
  ok.className = 'btn' + (o.danger ? ' danger' : ' primary');
  ok.disabled = false;
  // 信息展示类弹窗（无 onOk）：主按钮本身就是「关闭」，再摆一个取消
  // 就是两个按钮做同一件事，只留主按钮。
  cancel.hidden = !o.onOk;
  sheetOk = o.onOk || null;
  sheet.classList.remove('closing');
  sheet.showModal();
  if (!o.noFocus) {
    const first = $('sheet-body').querySelector('input,select,textarea');
    if (first) first.focus();
  }
}
function animateCloseSheet() {
  if (!sheet.open || sheet.classList.contains('closing')) return;
  sheet.classList.add('closing');
  setTimeout(() => {
    if (!sheet.classList.contains('closing')) return; // 动画期间被重新打开
    sheet.classList.remove('closing');
    sheet.close();
  }, 150);
}
$('sheet-x').addEventListener('click', () => animateCloseSheet());
$('sheet-cancel').addEventListener('click', () => animateCloseSheet());
sheet.addEventListener('cancel', e => { e.preventDefault(); animateCloseSheet(); });
$('sheet-form').addEventListener('submit', e => { e.preventDefault(); $('sheet-ok').click(); });
$('sheet-copy').addEventListener('click', () => {
  const text = $('sheet-note').textContent;
  if (!text) return;
  copyText(text).then(() => toast('已复制', 'ok')).catch(e => toast(e.message, 'err'));
});
$('sheet-ok').addEventListener('click', async () => {
  if (!sheetOk) { animateCloseSheet(); return; }
  const btn = $('sheet-ok');
  btn.disabled = true;
  try {
    const stay = await sheetOk();
    if (stay === false) return; // onOk 已接管界面（如展示明文），保持打开
    animateCloseSheet();
  } catch (e) {
    toast(e.message, 'err');
    // 报错同时落到底部信息栏并亮出复制按钮，方便用户拷贝完整报错。
    $('sheet-note').textContent = e.message;
    $('sheet-copy').hidden = false;
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
// infoTip 生成一个 ⓘ 徽标，鼠标悬浮/键盘聚焦显示说明。
//
// 面板里有若干术语对中文读者并不自明（「缓存写」「缓存读」「额度口径」等），
// 它们背后是上游计费口径的差异，靠标题文字讲不清也不该占版面。用原生 title
// 承载完整说明：零依赖、可访问性由浏览器保证、移动端长按亦可见。
function infoTip(text) {
  return '<span class="info" tabindex="0" role="img" aria-label="说明：' + esc(text) + '"'
    + ' title="' + esc(text) + '">i</span>';
}
// labelWithTip 给字段标签追加 ⓘ 说明。
function labelWithTip(label, tip) {
  return esc(label) + infoTip(tip);
}
// TIPS 集中收拢术语解释，避免同一说明在多处漂移。
const TIPS = {
  cacheRead: '缓存读：命中上游提示词缓存的输入 token，单价通常远低于普通输入。'
    + '两种上游口径已归一——Claude 的 cache_read_tokens 独立于输入，'
    + 'OpenAI/Gemini 的 cached_tokens 含在输入内，取较大者避免重复计费。',
  cacheWrite: '缓存写：为建立提示词缓存而写入的 token，只在首次或缓存失效时产生，'
    + '单价通常高于普通输入（Claude 约为 1.25 倍）。后续命中即按「缓存读」计费。',
  input: '输入：本次请求发送给模型的提示词 token（已扣除命中缓存的部分）。',
  output: '输出：模型生成的 token。推理（thinking）token 也并入此项按输出价计费，'
    + '因此无需单独设置推理价。',
  tokenLimit: 'Token 限额与金额限额并列生效，任一触顶即拒绝请求。'
    + '统计口径为计费四类合计（输入＋输出＋缓存读＋缓存写），与费用同一口径。'
    + '混合模型时价差可达数十倍，用 token 约束用量比金额更精确。留空为不限。',
  moneyLimit: '按实际结算金额扣减，跨周期自动归零。留空为不限。',
  callerScope: '归属 caller 共享：额度与同一 caller 下的其他 Key 合并计算。'
    + '独立计额：本 Key 单独一份额度，不受同伴影响。',
  accountingMode: '缓存口径：inclusive 表示上游把缓存命中计入了输入总数（OpenAI/Gemini），'
    + 'exclusive 表示缓存命中独立于输入（Claude）。default 按上游字段自动判断。',
  billingMode: 'token 按 token 计价；per_image 按张计价（图像模型）；free 恒为免费。',
  priority: '同一模型命中多条规则时，优先级数值大的先生效；相同优先级按 id 升序。',
  matchKind: 'exact 完全匹配模型名；glob 支持 * 与 ? 通配；regexp 为正则匹配。',
};
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

// 显示币种选择框（仅影响展示；账本与额度口径恒为 USD）。
const dispCurSel = new Select('disp-cur', [
  { value: 'usd', label: '美元（USD）' },
  { value: 'cny', label: '人民币（CNY）' },
], v => {
  dispCur = v;
  localStorage.setItem('disp-cur', v);
  savePref('disp-cur', v);
  if (v === 'cny' && !fxRateCNY) loadDispCurRate();
  else reloadActive();
}, { value: dispCur, head: '金额显示币种' });

// ---------- 显示设置弹层（齿轮） ----------
// 币种/自动刷新从顶栏收纳进齿轮弹层；刷新与退出仍是一步可达的图标按钮。
function closeSettingsPop() {
  $('settings-pop').hidden = true;
  $('settings-btn').setAttribute('aria-expanded', 'false');
}
$('settings-btn').addEventListener('click', () => {
  const pop = $('settings-pop');
  pop.hidden = !pop.hidden;
  $('settings-btn').setAttribute('aria-expanded', String(!pop.hidden));
});
document.addEventListener('click', e => {
  if (!$('settings-pop').hidden && !e.target.closest('.tb-settings')) closeSettingsPop();
});

// ---------- 顶栏自动刷新 ----------
// 按用户自定义间隔重载当前页签的数据加载器；页面隐藏或未登录时跳过。
// 替代此前请求明细面板里的固定 30s 开关（ui_req-auto 偏好作废，不再读取）。
let autoRefreshTimer = null;
function parseAutoRefreshSecs() {
  const n = parseInt($('auto-refresh-secs').value, 10);
  return isFinite(n) ? Math.min(86400, Math.max(5, n)) : 0;
}
function setupAutoRefresh() {
  if (autoRefreshTimer) { clearInterval(autoRefreshTimer); autoRefreshTimer = null; }
  const on = $('auto-refresh').checked;
  $('set-secs-row').classList.toggle('off', !on);
  if (!on) return;
  const secs = parseAutoRefreshSecs();
  if (!secs) return;
  autoRefreshTimer = setInterval(() => {
    if (document.hidden || $('app').hidden) return;
    reloadActive();
  }, secs * 1000);
}
$('auto-refresh').addEventListener('change', () => {
  localStorage.setItem('auto-refresh', $('auto-refresh').checked ? '1' : '0');
  savePref('auto-refresh', $('auto-refresh').checked ? '1' : '0');
  setupAutoRefresh();
});
$('auto-refresh-secs').addEventListener('change', () => {
  const secs = parseAutoRefreshSecs();
  if (!secs) return;
  $('auto-refresh-secs').value = String(secs);
  localStorage.setItem('auto-refresh-secs', String(secs));
  savePref('auto-refresh-secs', String(secs));
  if ($('auto-refresh').checked) setupAutoRefresh();
});

