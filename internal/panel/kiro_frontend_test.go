package panel

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// Kiro 前端接线：app.js 里引用的每个 Kiro 元素 id 都必须在 index.html 里存在（少一个就是
// 「按钮点了没反应」，而 JS 语法检查与顶层冒烟都发现不了）；添加账号 tab、平台筛选 chip、
// 配置分区导航也要齐。
func TestPanelKiroWiring(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$\('((?:kr|btnKr|addTabKr|addFtKr|sKr)[A-Za-z]*)'\)`).FindAllStringSubmatch(string(js), -1) {
		ids[m[1]] = true
	}
	if len(ids) < 20 {
		t.Fatalf("only %d kiro element ids found in app.js; pattern broken?", len(ids))
	}
	for id := range ids {
		if !strings.Contains(string(html), `id="`+id+`"`) {
			t.Errorf("app.js references #%s but index.html has no such element", id)
		}
	}
	for _, s := range []string{`data-tab="krlogin"`, `data-tab="krimport"`, `data-plat="kiro"`, `data-target="cfgSecKiro"`,
		`id="cfgSecKiro"`, `name="krMethod"`, `.realm-tag.kr`, `<option value="kiro">`} {
		if !strings.Contains(string(html), s) {
			t.Errorf("index.html missing %s", s)
		}
	}
}

// 平台判定 / 类型列徽标 / 导入文本合并（多个 JSON 文件包成一个数组才能让 Kiro IDE 的 token 文件
// 与 clientId 文件配对；JSON 与行格式混合直接报错）。
func TestAppJSKiroHelpers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; kiro helper test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const cut = (a, b) => { const i = src.indexOf(a), j = src.indexOf(b); if (i < 0 || j < 0 || j < i) throw new Error('region not found: ' + a); return src.slice(i, j); };
const ctx = { String, RegExp, Number, Error, Array, Object };
vm.createContext(ctx);
vm.runInContext(
  cut('function esc(', 'function ago(') +
  cut('function isQoderAcct(', 'function syncAccPlatChips(') +
  cut('function krMergeImportTexts(', "$('krImportFile').onchange") +
  '\nthis.acctPlatform=acctPlatform; this.accTypeHTML=accTypeHTML; this.krMergeImportTexts=krMergeImportTexts; this.isKiroAcct=isKiroAcct;',
  ctx);
assert.equal(ctx.acctPlatform({ platform: 'kiro' }), 'kiro');
assert.equal(ctx.acctPlatform({ realm: 'kiro' }), 'kiro');
assert.equal(ctx.acctPlatform({ realm: 'qoder-intl' }), 'qoder');
assert.equal(ctx.acctPlatform({ realm: 'global' }), 'workbuddy');
assert.equal(ctx.acctPlatform({}), 'workbuddy');
const kr = ctx.accTypeHTML({ platform: 'kiro', realm: 'kiro', auth_kind: 'BuilderId', plan: 'PRO' });
assert(kr.includes('realm-tag kr') && kr.includes('Builder ID · PRO'), kr);
assert(ctx.accTypeHTML({ platform: 'kiro', auth_kind: 'APIKey', plan: '<b>' }).includes('API Key · &lt;b&gt;'));
assert(ctx.accTypeHTML({ realm: 'qoder-cn' }).includes('realm-tag qd'));
assert(!ctx.accTypeHTML({ realm: 'cn' }).includes(' kr'));
// 超额已开：类型列要有显眼的标记；关着 / 未知时没有。
assert(ctx.accTypeHTML({ platform: 'kiro', auth_kind: 'BuilderId', overage: 'ENABLED' }).includes('realm-tag od'));
assert(!ctx.accTypeHTML({ platform: 'kiro', auth_kind: 'BuilderId', overage: 'DISABLED' }).includes('realm-tag od'));
assert(!ctx.accTypeHTML({ platform: 'kiro', auth_kind: 'BuilderId' }).includes('realm-tag od'));
assert.equal(ctx.krMergeImportTexts(['', '  ']), '');
assert.equal(ctx.krMergeImportTexts(['\ufeff{"a":1}']), '{"a":1}');
assert.equal(ctx.krMergeImportTexts(['{"a":1}', ' [{"b":2}] ']), '[\n{"a":1},\n[{"b":2}]\n]');
assert.equal(ctx.krMergeImportTexts(['', 'ksk_a', 'x@y----p----rt----cid----cs']), 'ksk_a\nx@y----p----rt----cid----cs');
assert.throws(() => ctx.krMergeImportTexts(['{"a":1}', 'ksk_a']), /不能混/);
console.log('OK');`
	f, err := os.CreateTemp(t.TempDir(), "kiro-helpers-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK") {
		t.Fatalf("kiro helpers: %v\n%s", err, out)
	}
}

// 代理池页：app.js 引用的元素 id 在 index.html 里都有；渲染时代理备注 / 测试错误等服务端文本全部转义；
// 配置文件里的代理没有停用 / 删除按钮；账号没走代理时有醒目提示。
func TestAppJSKiroProxyPoolRender(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$\('(kp[A-Za-z]*)'\)`).FindAllStringSubmatch(string(js), -1) {
		ids[m[1]] = true
	}
	if len(ids) < 8 {
		t.Fatalf("only %d proxy-pool element ids found in app.js; pattern broken?", len(ids))
	}
	for id := range ids {
		if !strings.Contains(string(html), `id="`+id+`"`) {
			t.Errorf("app.js references #%s but index.html has no such element", id)
		}
	}
	kc := map[string]bool{}
	for _, m := range regexp.MustCompile(`\$\('(kc[A-Za-z]*)'\)`).FindAllStringSubmatch(string(js), -1) {
		kc[m[1]] = true
	}
	if len(kc) < 20 {
		t.Fatalf("only %d subscription element ids found in app.js; pattern broken?", len(kc))
	}
	for id := range kc {
		if !strings.Contains(string(html), `id="`+id+`"`) {
			t.Errorf("app.js references #%s but index.html has no such element", id)
		}
	}
	for _, s := range []string{`id="view-kiroproxies"`, `data-view="kiroproxies"`, `id="navKiroProxies"`, `id="kpEgBody"`} {
		if !strings.Contains(string(html), s) {
			t.Errorf("index.html missing %s", s)
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; proxy pool render test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const cut = (a, b) => { const i = src.indexOf(a), j = src.indexOf(b); if (i < 0 || j < 0 || j < i) throw new Error('region not found: ' + a); return src.slice(i, j); };
const ctx = { String, Date, Number, Math, Set, KIRO_PROXY_KIND: { account: '账号代理', pool: '代理池', default: '默认代理', direct: '直连' } };
vm.createContext(ctx);
vm.runInContext(cut('function esc(', 'function ago(') + cut('function kpMoved(', 'function kpRender(') +
  cut('let kcData = null', 'function kcRender(') + cut('function kcNodeMatch(', 'function kcRenderNodes(') +
  '\nthis.row=kpRowHTML; this.acc=kpAccHTML; this.test=kpTestHTML; this.direct=kpDirectHTML; this.eg=kpEgRowHTML;' +
  ' this.sub=kcSubHTML; this.node=kcNodeRowHTML; this.match=kcNodeMatch; this.bytes=kcBytes; this.status=kcStatusText;' +
  ' this.check=n => kcChecked.add(n);', ctx);
// 订阅节点行：显示「订阅名 · 节点名」，只能测试，不能停用 / 删除；节点名转义。
const subRow = ctx.row({ id: 'n1', proxy: 'http://127.0.0.1:17890', note: '机场 · <b>US</b>', source: 'sub', enabled: true, active: true, accounts: 1, auth: true });
assert(subRow.includes('订阅') && subRow.includes('&lt;b&gt;US') && !subRow.includes('<b>US') && subRow.includes('data-a="test"'), subRow);
assert(!subRow.includes('data-a="remove"') && !subRow.includes('data-a="toggle"') && !subRow.includes('>认证<'), subRow);
// 平台出口：池里生效的成员才能选；固定的成员不在池里时也要显示出来；错误信息转义。
const pool = [{ id: 'a', proxy: 'http://h:1', source: 'panel', active: true }, { id: 'b', proxy: 'x', source: 'sub', note: '机场 · JP', active: true }, { id: 'c', proxy: 'off', active: false }];
const egOff = ctx.eg({ platform: 'workbuddy', name: 'WorkBuddy', mode: '', direct_cn: true }, pool);
assert(egOff.includes('value="" selected') && egOff.includes('value="pin:a"') && egOff.includes('固定：机场 · JP') && !egOff.includes('pin:c') && egOff.includes('checked'), egOff);
const egPin = ctx.eg({ platform: 'qoder', name: 'Qoder', mode: 'pin', proxy_id: 'gone', direct_cn: false, error: '<x>没有</x>' }, pool);
assert(egPin.includes('value="pin:gone" selected') && egPin.includes('已不在池里') && egPin.includes('&lt;x&gt;') && !egPin.includes(' checked'), egPin);
// 订阅行：流量 / 到期 / 错误，名称与错误都转义。
const sub = ctx.sub({ id: 's"1', name: '<i>机场</i>', host: 'cdn.example', has_url: true, has_content: true, effective_interval_min: 1440,
  updated_at: Math.floor(Date.now() / 1000) - 7200, via: '直连', format: 'clash', size: 77000, selected: 2,
  info: { upload: 1, download: 1023, total: 2048, expire: 1793116800 }, last_error: '<script>x</script>' });
assert(sub.includes('&lt;i&gt;机场') && sub.includes('&lt;script&gt;') && !sub.includes('<script>') && sub.includes('data-id="s&quot;1"'), sub);
assert(sub.includes('1.0 KB / 2.0 KB') && sub.includes('2 小时前') && sub.includes('每 24 小时自动更新') && sub.includes('2 个'), sub);
assert(ctx.sub({ id: 'm', name: 'm', has_url: false, info: { upload: 5, download: 5, total: 10 } }).includes('流量用完'));
assert(!ctx.sub({ id: 'm', name: 'm', has_url: false }).includes('data-a="refresh"'), 'a pasted-only subscription has nothing to refresh');
// 节点行与筛选。
ctx.check('US 001');
const nr = ctx.node({ name: 'US 001', type: 'Trojan', delay: 120, tested: true, port: 17890 });
assert(nr.includes('checked') && nr.includes('120 ms') && nr.includes('127.0.0.1:17890'), nr);
assert(ctx.node({ name: 'gone', missing: true, tested: false }).includes('已不在订阅里'));
assert(ctx.node({ name: 'JP', type: 'Vmess', delay: 0, tested: true }).includes('不通'));
assert(ctx.match({ name: '美国 01', type: 'Trojan' }, '美国 trojan') && !ctx.match({ name: '日本', type: 'Vmess' }, '美国'));
assert.equal(ctx.bytes(162022817792), '151 GB'); assert.equal(ctx.bytes(35760107693), '33.3 GB');
assert(ctx.status({ installed: false, install: { running: true } }).includes('正在下载'));
assert(ctx.status({ installed: true, running: true, version: 'v1', listeners: 3 }).includes('3 个节点代理口'));
assert.equal(ctx.direct(null), '');
const dfail = ctx.direct({ ok: false, error: '<b>i/o timeout</b>', at: 1 });
assert(dfail.includes('连不上 Kiro') && dfail.includes('需要在下面添加') && !dfail.includes('<b>i/o'), dfail);
assert(ctx.direct({ ok: true, country: 'CN', latency_ms: 90, at: 1 }).includes('出口在国内'));
assert(!ctx.direct({ ok: true, country: 'US', latency_ms: 90, at: 1 }).includes('出口在国内'));
const base = { id: 'a"b', proxy: 'http://h:1', note: '', source: 'panel', enabled: true, active: true, down: false, accounts: 0 };
const evil = ctx.row(Object.assign({}, base, { proxy: '<x>', note: '<img src=x onerror=1>', test: { ok: false, error: '<script>1</script>', at: 1 } }));
assert(!evil.includes('<img') && !evil.includes('<x>') && !evil.includes('<script>'), evil);
assert(evil.includes('data-id="a&quot;b"'), evil);
assert(evil.includes('data-a="remove"') && evil.includes('data-a="toggle"') && evil.includes('data-on="0"'));
const cfg = ctx.row(Object.assign({}, base, { source: 'config' }));
assert(!cfg.includes('data-a="remove"') && !cfg.includes('data-a="toggle"') && cfg.includes('data-a="test"'), cfg);
const off = ctx.row(Object.assign({}, base, { enabled: false, active: false }));
assert(off.includes('class="off"') && off.includes('data-on="1"') && off.includes('已停用'), off);
assert(ctx.row(Object.assign({}, base, { down: true })).includes('连不上'));
assert(ctx.test(null).includes('未测试'));
const cn = ctx.test({ ok: true, country: 'CN', ip: '1.1.1.1', latency_ms: 80, at: 1 });
assert(cn.includes('tag bad') && cn.includes('国内出口没有 Claude'), cn);
const us = ctx.test({ ok: true, country: 'US', ip: '1.1.1.1', org: 'AS1 <b>', latency_ms: 80, at: 1 });
assert(us.includes('已连通') && !us.includes('国内出口') && us.includes('AS1 &lt;b&gt;'), us);
assert(ctx.acc({ uid: 'u1', nickname: '', kind: 'none' }).includes('没走代理'));
assert(ctx.acc({ uid: 'u1', nickname: 'n', kind: 'direct' }).includes('直连'));
const pooled = ctx.acc({ uid: 'u1', nickname: 'n', kind: 'pool', proxy: 'http://h:1', down: true });
assert(pooled.includes('代理池') && pooled.includes('http://h:1') && pooled.includes('连不上'), pooled);
console.log('OK');`
	f, err := os.CreateTemp(t.TempDir(), "kiro-proxypool-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK") {
		t.Fatalf("proxy pool render: %v\n%s", err, out)
	}
}

// 超额开关会产生真实费用：用户在确认框点取消时绝不能发出请求；确认文案要带真实的费用数字；
// 订阅不支持时直接拦住；请求体显式带 enabled。
func TestAppJSKiroOverageToggle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; overage toggle test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const assert = require('assert');
const src = fs.readFileSync(process.argv[2], 'utf8');
const i = src.indexOf('const fmtUSD'), j = src.indexOf('/* ── 添加 Kiro 账号：网页登录');
if (i < 0 || j < i) throw new Error('overage region not found');
const calls = [], toasts = [], confirms = [];
let answer = true, state = {};
const ctx = {
  encodeURIComponent, Number, JSON,
  api: async (path, opts) => { calls.push({ path, body: opts && opts.body ? JSON.parse(opts.body) : null }); return state; },
  confirm: msg => { confirms.push(msg); return answer; },
  toast: (m, c) => toasts.push([m, c]),
};
vm.createContext(ctx);
vm.runInContext(src.slice(i, j) + '\nthis.kiroOverageToggle = kiroOverageToggle;', ctx);
(async () => {
  // 关着 → 取消确认：只读了状态，没有任何 POST。
  state = { capable: true, enabled: false, cap_usd: 200, rate_usd: 0.04, current_usd: 1.5 };
  answer = false;
  await ctx.kiroOverageToggle('k/1');
  assert.equal(calls.length, 1); assert.equal(calls[0].body, null);
  assert.equal(calls[0].path, 'kiro/accounts/k%2F1/overage', 'uid must be URL-encoded');
  assert(/\$200\.00/.test(confirms[0]) && /\$0\.04/.test(confirms[0]) && /\$1\.50/.test(confirms[0]) && /真实费用/.test(confirms[0]), confirms[0]);
  assert.equal(toasts.length, 0);
  // 关着 → 确认：POST {enabled:true}。
  calls.length = 0; answer = true;
  await ctx.kiroOverageToggle('k1');
  assert.equal(calls.length, 2); assert.deepEqual(calls[1].body, { enabled: true });
  // 开着 → 确认：POST {enabled:false}，且提示关闭后会冷却。
  calls.length = 0; confirms.length = 0;
  state = { capable: true, enabled: true, cap_usd: 50, rate_usd: 0, current_usd: 0 };
  await ctx.kiroOverageToggle('k1');
  assert.deepEqual(calls[1].body, { enabled: false }); assert(/冷却/.test(confirms[0]));
  // 开着 → 取消：不改。
  calls.length = 0; answer = false;
  await ctx.kiroOverageToggle('k1');
  assert.equal(calls.length, 1);
  // 订阅不支持：不弹确认、不 POST，给出提示。
  calls.length = 0; confirms.length = 0; toasts.length = 0; answer = true;
  state = { capable: false, enabled: false, subscription: 'KIRO FREE' };
  await ctx.kiroOverageToggle('k1');
  assert.equal(calls.length, 1); assert.equal(confirms.length, 0);
  assert.equal(toasts[0][1], 'err'); assert(/KIRO FREE/.test(toasts[0][0]));
  console.log('OK');
})().catch(e => { console.error(e); process.exit(1); });`
	f, err := os.CreateTemp(t.TempDir(), "kiro-overage-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK") {
		t.Fatalf("overage toggle: %v\n%s", err, out)
	}
}
