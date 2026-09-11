// 面板脚本的 node 侧校验：①整份拼接结果可编译（等价 node --check）；
// ②纯函数行为断言（fmtInt / fmtTok / fmtUSD / fmtCur / esc 等此前零测试）。
// 无第三方依赖，`node scripts/js-tests.mjs` 即可运行；CI 与 go test 并行把
// 前端逻辑钉住。段文件清单与 internal/web/web.go 的 jsParts 保持同步。
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const jsDir = join(root, 'internal', 'web', 'js');
const parts = [
  '00-head.js', '10-utils.js', '20-components.js', '30-overview.js',
  '40-keys.js', '50-usage.js', '60-pricing.js', '70-system.js',
  '80-misc.js', '99-boot.js',
];

let failures = 0;
const check = (name, cond, detail) => {
  if (cond) return;
  failures++;
  console.error(`FAIL ${name}${detail ? '：' + detail : ''}`);
};

// ① 语法：整份拼接结果必须可编译（IIFE 开合在编译期即校验配平）。
const full = parts.map(p => readFileSync(join(jsDir, p), 'utf8')).join('');
new vm.Script(full, { filename: 'console.js' });

// ② 行为：加载工具段（顶层只有 localStorage 触碰），垫片后断言纯函数。
const sandbox = { localStorage: { getItem: () => null }, console };
vm.createContext(sandbox);
vm.runInContext(readFileSync(join(jsDir, '10-utils.js'), 'utf8'), sandbox, { filename: '10-utils.js' });
const run = expr => vm.runInContext(expr, sandbox);

// fmtInt：万 / 亿自动升级（非 token 数字）。
check('fmtInt 万', run(`fmtInt(12345)`) === '1.2 万');
check('fmtInt 亿', run(`fmtInt(2.5e8)`) === '2.5 亿');
check('fmtInt 小数值原样', run(`fmtInt(1234)`) === '1,234');
check('fmtInt 非法输入归零', run(`fmtInt(undefined)`) === '0');

// fmtTok：K/M/B 升级，阈值取 999.5 的倍数（999500 进位为 1M 而非 1000K）；
// dec 会剥掉尾随 .0，所以 1.0M 显示为 1M。
check('fmtTok <1000 原样', run(`fmtTok(999)`) === '999');
check('fmtTok K', run(`fmtTok(12345)`) === '12.3K');
check('fmtTok 999500 进位 M', run(`fmtTok(999500)`) === '1M');
check('fmtTok B', run(`fmtTok(1.5e9)`) === '1.5B');
check('fmtTok 0', run(`fmtTok(0)`) === '0');

// esc：HTML 转义覆盖五个实体。
check('esc <', run(`esc('<')`) === '&lt;');
check('esc >', run(`esc('>')`) === '&gt;');
check('esc &', run(`esc('&')`) === '&amp;');
check('esc 双引号', run(`esc('"')`) === '&quot;');
check('esc 单引号', run(`esc("'")`) === '&#39;');

// fmtSec / fmtBytes。
check('fmtSec 秒', run(`fmtSec(1500)`) === '1.50 s');
check('fmtSec 非正值', run(`fmtSec(0)`) === '-');
check('fmtBytes MB', run(`fmtBytes(2 * 1024 * 1024)`) === '2.0 MB');
check('fmtBytes B', run(`fmtBytes(512)`) === '512 B');

// fmtCur：默认美元；cny 且汇率就绪时折算（垫片 getItem 返回 null → usd）。
check('fmtCur usd 回退', run(`fmtCur(1234567)`) === '$1.23');
check('fmtCur null 不限', run(`fmtCur(null)`) === '不限');

if (failures > 0) {
  console.error(`\n${failures} 项断言失败`);
  process.exit(1);
}
console.log('js-tests: all passed');
