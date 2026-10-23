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
