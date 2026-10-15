package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func eng(name, base, path string) engine {
	return engine{Name: name, Base: base, Path: path}
}

// ---------------------------------------------------------------------------
// 引擎校验
// ---------------------------------------------------------------------------

func TestNormalizeEngine(t *testing.T) {
	cases := []struct {
		name    string
		in      engine
		wantErr bool
		check   func(t *testing.T, out engine)
	}{
		{
			name: "正常值",
			in:   eng("115", "http://192.0.2.20:11510", "/files"),
			check: func(t *testing.T, out engine) {
				if out.Name != "115" || out.Base != "http://192.0.2.20:11510" || out.Path != "/files" {
					t.Errorf("输出不正确：%+v", out)
				}
			},
		},
		{
			name: "地址去尾斜杠与空白",
			in:   eng("  A  ", "  http://192.0.2.20:8787/  ", "  /s  "),
			check: func(t *testing.T, out engine) {
				if out.Name != "A" || out.Base != "http://192.0.2.20:8787" || out.Path != "/s" {
					t.Errorf("未归一化：%+v", out)
				}
			},
		},
		{
			name: "名称留空时给默认名",
			in:   eng("", "http://192.0.2.20:8787", "/s"),
			check: func(t *testing.T, out engine) {
				if out.Name != "搜索引擎 1" {
					t.Errorf("默认名不正确：%q", out.Name)
				}
			},
		},
		{
			name: "路径缺前导斜杠自动补",
			in:   eng("A", "http://192.0.2.20:8787", "s"),
			check: func(t *testing.T, out engine) {
				if out.Path != "/s" {
					t.Errorf("路径未补斜杠：%q", out.Path)
				}
			},
		},
		{
			name: "路径带查询串时剥掉（程序自己追加 ?q=）",
			in:   eng("A", "http://192.0.2.20:8787", "/search?q="),
			check: func(t *testing.T, out engine) {
				if out.Path != "/search" {
					t.Errorf("查询串未剥掉：%q", out.Path)
				}
			},
		},
		{
			name: "路径为空时退化为根路径",
			in:   eng("A", "http://192.0.2.20:8787", ""),
			check: func(t *testing.T, out engine) {
				if out.Path != "/" {
					t.Errorf("路径应为 /：%q", out.Path)
				}
			},
		},
		{name: "地址为空", in: eng("A", "", "/s"), wantErr: true},
		{name: "缺 scheme", in: eng("A", "192.0.2.20:8787", "/s"), wantErr: true},
		{name: "scheme 不支持", in: eng("A", "ftp://x", "/s"), wantErr: true},
		{name: "缺主机", in: eng("A", "http://", "/s"), wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := normalizeEngine(tc.in, 0)
			if tc.wantErr {
				if err == nil {
					t.Errorf("应报错，实际返回 %+v", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外报错：%v", err)
			}
			if tc.check != nil {
				tc.check(t, out)
			}
		})
	}
}

func TestNormalizeEngineRejectsTooLongName(t *testing.T) {
	if _, err := normalizeEngine(eng(strings.Repeat("字", 41), "http://192.0.2.20:1", "/s"), 0); err == nil {
		t.Error("超过 40 字的名称应被拒绝")
	}
	if _, err := normalizeEngine(eng(strings.Repeat("字", 40), "http://192.0.2.20:1", "/s"), 0); err != nil {
		t.Errorf("正好 40 字应当通过：%v", err)
	}
}

func TestNormalizeEnginesLimits(t *testing.T) {
	if _, err := normalizeEngines(nil); err == nil {
		t.Error("空列表应报错")
	}
	many := make([]engine, maxEngines+1)
	for i := range many {
		many[i] = eng("", "http://192.0.2.20:8787", "/s")
	}
	if _, err := normalizeEngines(many); err == nil {
		t.Errorf("超过 %d 个应报错", maxEngines)
	}
	out, err := normalizeEngines([]engine{eng("A", "http://192.0.2.20:1", "/s")})
	if err != nil || len(out) != 1 {
		t.Errorf("单个引擎应通过：%v %v", out, err)
	}
}

// ---------------------------------------------------------------------------
// 配置持久化
// ---------------------------------------------------------------------------

func TestSettingsStoreCreatesFileOnFirstRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "settings.json") // 目录不存在，应自动创建
	fallback := settings{Engines: []engine{eng("A", "http://192.0.2.21:1", "/s")}}

	st, err := newSettingsStore(path, fallback, originEnv)
	if err != nil {
		t.Fatalf("首次启动不应报错：%v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("首次启动应自动生成配置文件：%v", err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "http://192.0.2.21:1") {
		t.Errorf("自动生成的内容不正确：%s", data)
	}
	got, orig := st.snapshot()
	if len(got.Engines) != 1 || got.Engines[0].Base != "http://192.0.2.21:1" {
		t.Errorf("内容不正确：%+v", got)
	}
	if orig != originEnv {
		t.Errorf("首次生成的内容来自环境变量，来源应为 %q，实际 %q", originEnv, orig)
	}

	// 重启后应改为从文件读取
	st2, err := newSettingsStore(path, settings{}, originDefault)
	if err != nil {
		t.Fatal(err)
	}
	got2, orig2 := st2.snapshot()
	if len(got2.Engines) != 1 || got2.Engines[0].Base != "http://192.0.2.21:1" {
		t.Errorf("重启后读回的值不一致：%+v", got2)
	}
	if orig2 != originFile {
		t.Errorf("重启后来源应为 %q，实际 %q", originFile, orig2)
	}
}

func TestSettingsStoreMigratesLegacyFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	legacy := `{"source1":"http://192.0.2.31:1","source2":"http://192.0.2.32:2"}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := newSettingsStore(path, settings{}, originDefault)
	if err != nil {
		t.Fatalf("旧格式应能平滑迁移：%v", err)
	}
	got, _ := st.snapshot()
	if len(got.Engines) != 2 {
		t.Fatalf("旧格式应迁移出 2 个引擎，实际 %d 个", len(got.Engines))
	}
	if got.Engines[0].Base != "http://192.0.2.31:1" || got.Engines[1].Base != "http://192.0.2.32:2" {
		t.Errorf("迁移后的地址不正确：%+v", got.Engines)
	}
	// 迁移出来的引擎也应带上默认的搜索路径，否则检索地址拼不出来
	if got.Engines[0].Path == "" || got.Engines[1].Path == "" {
		t.Errorf("迁移后的引擎缺少搜索路径：%+v", got.Engines)
	}
}

func TestSettingsStoreFallsBackWhenFileBroken(t *testing.T) {
	cases := []string{
		`{ 这不是 json`,
		`{"engines":[{"name":"A","base":"ftp://x","path":"/s"}]}`,
		`{"engines":[]}`,
	}
	for _, body := range cases {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		fallback := settings{Engines: []engine{eng("A", "http://192.0.2.21:1", "/s")}}
		st, err := newSettingsStore(path, fallback, originDefault)
		if err == nil {
			t.Errorf("内容为 %s 时应给出错误提示", body)
		}
		got, orig := st.snapshot()
		if len(got.Engines) != 1 || orig != originDefault {
			t.Errorf("内容为 %s 时应退回 fallback，实际 %+v / %q", body, got, orig)
		}
	}
}

func TestSettingsStoreSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	st, err := newSettingsStore(path, settings{}, originDefault)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.save(settings{Engines: []engine{
		eng(" 甲 ", "http://192.0.2.31:1/", "/s"),
		eng("乙", "http://192.0.2.32:2", "/files"),
	}}); err != nil {
		t.Fatalf("保存失败：%v", err)
	}

	got, orig := st.snapshot()
	if len(got.Engines) != 2 || got.Engines[0].Name != "甲" || got.Engines[0].Base != "http://192.0.2.31:1" {
		t.Errorf("保存后内存值不正确：%+v", got)
	}
	if orig != originFile {
		t.Errorf("保存后来源应为 %q", originFile)
	}

	again, _ := newSettingsStore(path, settings{}, originDefault)
	reloaded, _ := again.snapshot()
	if len(reloaded.Engines) != 2 || reloaded.Engines[1].Base != "http://192.0.2.32:2" {
		t.Errorf("重启后读回的值不一致：%+v", reloaded)
	}
}

func TestSettingsStoreSaveRejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	st, _ := newSettingsStore(path, settings{Engines: []engine{eng("A", "http://192.0.2.21:1", "/s")}}, originDefault)

	if err := st.save(settings{Engines: []engine{eng("A", "ftp://x", "/s")}}); err == nil {
		t.Error("非法地址应被拒绝")
	}
	if err := st.save(settings{Engines: nil}); err == nil {
		t.Error("空引擎列表应被拒绝")
	}
	got, _ := st.snapshot()
	if got.Engines[0].Base != "http://192.0.2.21:1" {
		t.Error("校验失败不应改动现有配置")
	}
}

func TestWriteFileAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := writeFileAtomic(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "hello\n" {
		t.Errorf("内容不正确：%q", data)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("目录里应只剩目标文件，实际 %d 个", len(entries))
	}
}

func TestHidePresets(t *testing.T) {
	list := hidePresets()
	if len(list) < 3 {
		t.Fatalf("预设太少，用户还是得自己写选择器（实际 %d 个）", len(list))
	}
	for _, p := range list {
		if p.Key == "" || p.Label == "" {
			t.Errorf("预设缺少 key 或 label：%+v", p)
		}
	}
	// 「通用」必须存在且有内容 —— 它是新建引擎的默认值
	var common string
	for _, p := range list {
		if p.Key == "common" {
			common = p.CSS
		}
	}
	if strings.TrimSpace(common) == "" {
		t.Error("通用预设不能为空")
	}
	if commonHideCSS() != common {
		t.Errorf("commonHideCSS() 与通用预设不一致：%q vs %q", commonHideCSS(), common)
	}
	// 预设里的 CSS 必须真的包含 display:none，否则等于没隐藏
	for _, p := range list {
		if p.CSS == "" {
			continue
		}
		if !strings.Contains(p.CSS, "display:none") {
			t.Errorf("预设 %q 的 CSS 不像隐藏规则：%q", p.Label, p.CSS)
		}
	}
}

func TestBuildPageCarriesHidePresets(t *testing.T) {
	page := buildPage(pageData{
		Items:       []string{"A"},
		Engines:     []enginePayload{{Name: "X", Base: "http://a", Path: "/s", ProxyPort: 1}},
		HidePresets: hidePresets(),
	})
	if !strings.Contains(page, `"hidePresets"`) {
		t.Error("引导数据里应带上隐藏外壳的预设")
	}
	if !strings.Contains(page, `"key":"common"`) {
		t.Error("引导数据里应包含通用预设")
	}
}

// ---------------------------------------------------------------------------
// 隐藏站点外壳的注入
// ---------------------------------------------------------------------------

func TestHideSelectors(t *testing.T) {
	cases := map[string]string{
		"header,aside,footer{display:none!important}":                 "header,aside,footer",
		"header{display:none!important}aside{display:none!important}": "header,aside",
		`[data-sidebar="sidebar"]{display:none!important}`:            `[data-sidebar="sidebar"]`,
		"   ": "",
		"":    "",
	}
	for in, want := range cases {
		if got := hideSelectors(in); got != want {
			t.Errorf("hideSelectors(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestInjectHidePlacesBlockAtEndOfBody(t *testing.T) {
	page := "<!DOCTYPE html><html><head><style>header{display:flex!important}</style></head>" +
		"<body><header>顶栏</header><main>正文</main></body></html>"
	out := injectHide(page, "header{display:none!important}")

	if !strings.Contains(out, "<style data-local-inject>") {
		t.Fatal("未注入隐藏样式")
	}
	if !strings.Contains(out, "<script data-local-inject>") {
		t.Fatal("未注入行内 !important 兜底脚本")
	}
	// 关键：注入块必须在 </body> 之前、且在文档已有的样式之后 ——
	// 同优先级同时 !important 时后出现的胜出，这样才压得住站点自己的规则
	block := strings.Index(out, "data-local-inject")
	siteStyle := strings.Index(out, "header{display:flex!important}")
	bodyEnd := strings.Index(out, "</body>")
	if !(siteStyle < block && block < bodyEnd) {
		t.Errorf("注入位置不对：站点样式=%d 注入=%d </body>=%d", siteStyle, block, bodyEnd)
	}
	if !strings.HasPrefix(out, "<!DOCTYPE html>") {
		t.Error("不应把 DOCTYPE 挤到后面（会让页面进怪异模式）")
	}
}

func TestInjectHideFallbacks(t *testing.T) {
	// 没有 </body> 时退到 </html> 前
	out := injectHide("<html><body>x</html>", "header{display:none!important}")
	if !strings.Contains(out, "data-local-inject") || !strings.Contains(out, "</html>") {
		t.Errorf("退到 </html> 前失败：%s", out)
	}
	// 两者都没有时直接追加
	out = injectHide("裸片段", "header{display:none!important}")
	if !strings.HasSuffix(out, "</script>") {
		t.Errorf("无 </body>/</html> 时应追加到末尾：%s", out)
	}
	// 空的 hideCSS 不应改动页面
	raw := "<html><body>x</body></html>"
	if got := injectHide(raw, "   "); got != raw {
		t.Errorf("空 hideCSS 不应注入任何东西：%q", got)
	}
}

func TestHideScriptIsSafeAndSelectorsAreJSONEncoded(t *testing.T) {
	script := hideScript(hideSelectors(hideCSS1))
	if !strings.Contains(script, `document.querySelectorAll`) {
		t.Error("脚本应使用 querySelectorAll 兜底")
	}
	if !strings.Contains(script, `'important'`) {
		t.Error("脚本必须用行内 !important（行内 important 优先级最高）")
	}
	// 选择器里的双引号必须被 JSON 转义，否则脚本会语法错误
	if strings.Contains(script, `[data-sidebar="sidebar"]`) {
		t.Error("选择器里的双引号未转义，生成的脚本会坏")
	}
	// 含尖括号的输入要转义，避免提前结束 <script>
	if s := hideScript(`a<script>`); strings.Contains(s, `<script>`) {
		t.Error("脚本内容未转义尖括号")
	}
}

// ---------------------------------------------------------------------------
// 引擎注册表
// ---------------------------------------------------------------------------

func TestWriteFileAtomicReportsPathOnFailure(t *testing.T) {
	// 父路径是一个文件而不是目录 —— 建目录就会失败
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeFileAtomic(filepath.Join(blocker, "settings.json"), []byte("{}"), 0o600)
	if err == nil {
		t.Fatal("父路径是文件时应报错")
	}
	if !strings.Contains(err.Error(), "blocker") {
		t.Errorf("错误信息里应带上出问题的路径：%v", err)
	}
}

func TestSettingsStoreSaveMentionsDataDirOnFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := &settingsStore{
		path: filepath.Join(blocker, "settings.json"),
		cfg:  settings{Engines: []engine{eng("A", "http://192.0.2.21:1", "/s")}},
		orig: originDefault,
	}
	err := st.save(settings{Engines: []engine{eng("A", "http://192.0.2.21:1", "/s")}})
	if err == nil {
		t.Fatal("写入失败时应返回错误")
	}
	if !strings.Contains(err.Error(), "失败") {
		t.Errorf("错误信息应说明是哪一步失败：%v", err)
	}
}

func TestEngineRegistry(t *testing.T) {
	reg := &engineRegistry{}
	ports := make([]int, maxEngines)
	for i := range ports {
		ports[i] = 18898 - i
	}
	reg.setPorts(ports)

	list := []engine{eng("A", "http://192.0.2.21:1", "/s"), eng("B", "http://192.0.2.22:2", "/s")}
	if err := reg.apply(list); err != nil {
		t.Fatal(err)
	}

	rt, ok := reg.at(0)
	if !ok || rt.eng.Name != "A" || rt.port != 18898 {
		t.Errorf("第 1 个引擎不正确：%+v ok=%v", rt, ok)
	}
	if _, ok := reg.at(2); ok {
		t.Error("越界索引不应返回引擎")
	}
	if _, ok := reg.at(-1); ok {
		t.Error("负索引不应返回引擎")
	}

	// 缩到 1 个引擎后，第 2 个位置应变成「未配置」
	if err := reg.apply(list[:1]); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.at(1); ok {
		t.Error("引擎被删掉后该位置不应再返回引擎")
	}
	if rt, _ := reg.at(0); rt.port != 18898 {
		t.Errorf("端口不应变化：%d", rt.port)
	}

	// 再加回来（数量超过启动时的引擎数）：端口池是独立的，新引擎必须拿到端口
	three := append(append([]engine(nil), list...), eng("C", "http://192.0.2.23:3", "/s"))
	if err := reg.apply(three); err != nil {
		t.Fatalf("扩到 3 个引擎应成功：%v", err)
	}
	rt3, ok := reg.at(2)
	if !ok {
		t.Fatal("第 3 个引擎位应可用")
	}
	if rt3.port == 0 {
		t.Error("新增引擎必须拿到代理端口，否则它对应的 iframe 无法加载")
	}
	if rt3.port != 18896 {
		t.Errorf("第 3 个引擎端口应为 18896，实际 %d", rt3.port)
	}
}

func TestEngineRegistryRejectsWhenPortPoolExhausted(t *testing.T) {
	reg := &engineRegistry{}
	reg.setPorts([]int{18898}) // 只留一个端口
	two := []engine{eng("A", "http://192.0.2.21:1", "/s"), eng("B", "http://192.0.2.22:2", "/s")}
	if err := reg.apply(two); err == nil {
		t.Error("端口池不足时应报错，而不是把引擎挂到端口 0 上")
	}
}

// ---------------------------------------------------------------------------
// 面板页拼装
// ---------------------------------------------------------------------------

func TestBuildPageReplacesAllPlaceholders(t *testing.T) {
	page := buildPage(pageData{
		Items: []string{"ABP-123"},
		Engines: []enginePayload{
			{Name: "115 Lite Web", Base: "http://192.0.2.31:1", Path: "/files", ProxyPort: 18898},
		},
		Origin: string(originFile), Version: "test", Query: "ABP-123", MaxEngines: maxEngines,
	})

	for _, ph := range []string{"{SCRIPT}", "{BOOTSTRAP}"} {
		if strings.Contains(page, ph) {
			t.Errorf("占位符 %s 未被替换", ph)
		}
	}
	if !strings.Contains(page, "splitKeywords") {
		t.Error("前端脚本未内联")
	}
	if !strings.Contains(page, `"items":["ABP-123"]`) {
		t.Error("引导数据里未包含番号列表")
	}
	if !strings.Contains(page, `"name":"115 Lite Web"`) || !strings.Contains(page, `"proxyPort":18898`) {
		t.Error("引导数据里未包含引擎信息")
	}
	if !strings.Contains(page, `"q":"ABP-123"`) {
		t.Error("引导数据里未包含 URL 带来的关键字")
	}
}

func TestBuildPageEscapesBootstrapForScriptContext(t *testing.T) {
	nasty := `</script><script>alert(1)</script>`
	page := buildPage(pageData{
		Items:   []string{nasty},
		Engines: []enginePayload{{Name: nasty, Base: "http://a", Path: "/s", ProxyPort: 1}},
	})
	if strings.Contains(page, "</script><script>alert(1)</script>") {
		t.Error("引导数据未做转义，存在脚本注入风险")
	}
	if !strings.Contains(page, `\u003c/script\u003e`) {
		t.Error("尖括号应被转义为 \\u003c 形式")
	}
}

func TestPlaceholderAndNotFoundPages(t *testing.T) {
	if p := placeholderPage(3); !strings.Contains(p, "第 3 个引擎位") {
		t.Errorf("占位页内容不正确：%s", p)
	}
	p := notFoundPage("/evil?x=<script>")
	if strings.Contains(p, "<script>") {
		t.Error("404 页里的路径未转义，存在反射型 XSS")
	}
	if !strings.Contains(p, "/api/search") {
		t.Error("404 页应提示可用路径")
	}
}

// ---------------------------------------------------------------------------
// 端口与监听
// ---------------------------------------------------------------------------

func TestListenOnFallsBackWhenPortTaken(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	used := portOf(taken)

	ln, err := listenOn("127.0.0.1", used)
	if err != nil {
		t.Fatalf("端口被占用时应退化为随机端口，却返回错误：%v", err)
	}
	defer ln.Close()
	if portOf(ln) == used {
		t.Error("未退化为随机端口")
	}
}

func TestDisplayHost(t *testing.T) {
	cases := map[string]string{
		"": "127.0.0.1", "0.0.0.0": "127.0.0.1", "::": "127.0.0.1",
		"192.0.2.5": "192.0.2.5", "nas.local": "nas.local",
	}
	for in, want := range cases {
		if got := displayHost(in); got != want {
			t.Errorf("displayHost(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestLoadDeploymentProxyPortBase(t *testing.T) {
	t.Setenv("PROXY_PORT_BASE", "28898")
	t.Setenv("PROXY1_PORT", "")
	t.Setenv("PAGE_PORT", "28899")
	dep := loadDeployment()
	if dep.ProxyPortBase != 28898 || dep.PagePort != 28899 {
		t.Errorf("端口未被采纳：%+v", dep)
	}

	// 兼容旧变量名
	t.Setenv("PROXY_PORT_BASE", "")
	t.Setenv("PROXY1_PORT", "27898")
	dep = loadDeployment()
	if dep.ProxyPortBase != 27898 {
		t.Errorf("应兼容 PROXY1_PORT，实际 %d", dep.ProxyPortBase)
	}

	// 过小的基址会撞到下限，应退回默认值
	t.Setenv("PROXY_PORT_BASE", "5")
	dep = loadDeployment()
	if dep.ProxyPortBase != defaultProxyPortBase {
		t.Errorf("基址过小时应退回默认值，实际 %d", dep.ProxyPortBase)
	}
}

func TestDefaultEnginesFromEnv(t *testing.T) {
	t.Setenv("SRC1_BASE", "")
	t.Setenv("SRC2_BASE", "")
	list, orig := defaultEngines()
	if len(list) != 2 || orig != originDefault {
		t.Errorf("无环境变量时应给 2 个内置引擎：%+v / %q", list, orig)
	}

	t.Setenv("SRC1_BASE", "http://192.0.2.31:1/")
	t.Setenv("SRC2_BASE", "http://192.0.2.32:2")
	list, orig = defaultEngines()
	if orig != originEnv {
		t.Errorf("来源应为 %q，实际 %q", originEnv, orig)
	}
	if list[0].Base != "http://192.0.2.31:1" || list[1].Base != "http://192.0.2.32:2" {
		t.Errorf("环境变量未被采纳：%+v", list)
	}
	if list[0].Path == "" || list[1].Path == "" {
		t.Errorf("内置引擎应带默认搜索路径：%+v", list)
	}
}

// ---------------------------------------------------------------------------
// 输入解析
// ---------------------------------------------------------------------------

func TestReadList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "list.txt")
	if err := os.WriteFile(path, []byte("  ABP-123  \r\n\r\nSSIS-001\nIPX-777\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := readList(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ABP-123", "SSIS-001", "IPX-777"}
	if len(items) != len(want) {
		t.Fatalf("应读到 %d 条，实际 %d 条：%v", len(want), len(items), items)
	}
	for i := range want {
		if items[i] != want[i] {
			t.Errorf("第 %d 条应为 %q，实际 %q", i, want[i], items[i])
		}
	}
}

func TestReadListEmptyAndMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(path, []byte("\n\n   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err := readList(path)
	if err != nil || len(items) != 0 {
		t.Errorf("空文件应返回空列表，实际 %v / %v", items, err)
	}
	if _, err := readList(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Error("读取不存在的文件应返回错误")
	}
}

// ---------------------------------------------------------------------------
// Cookie
// ---------------------------------------------------------------------------

func TestCookieJarPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookie.txt")
	jar := newCookieJar(path)
	jar.absorb([]*http.Cookie{{Name: "sid", Value: "abc"}, {Name: "uid", Value: "42"}})
	jar.absorb([]*http.Cookie{{Name: "uid", Value: "43"}})

	got := newCookieJar(path).headerValue("")
	if !strings.Contains(got, "sid=abc") || !strings.Contains(got, "uid=43") {
		t.Errorf("Cookie 未正确持久化/重载：%q", got)
	}
}

func TestCookieJarBrowserValueMergedWithLocalWins(t *testing.T) {
	jar := newCookieJar(filepath.Join(t.TempDir(), "cookie.txt"))
	jar.absorb([]*http.Cookie{{Name: "sid", Value: "local"}})
	merged := jar.headerValue("sid=browser; other=1")
	if !strings.Contains(merged, "sid=local") || !strings.Contains(merged, "other=1") {
		t.Errorf("Cookie 合并结果不正确：%q", merged)
	}
}

func TestCookieJarDeletesExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookie.txt")
	jar := newCookieJar(path)
	jar.absorb([]*http.Cookie{{Name: "sid", Value: "abc"}})
	jar.absorb([]*http.Cookie{{Name: "sid", Value: "", MaxAge: -1}})
	if got := jar.headerValue(""); strings.Contains(got, "sid") {
		t.Errorf("Max-Age<0 的 Cookie 应被删除：%q", got)
	}
}

// ---------------------------------------------------------------------------
// 反向代理
// ---------------------------------------------------------------------------

func TestProxyStripsHeadersInjectsCSSAndRewritesURLs(t *testing.T) {
	var upstream *httptest.Server
	var seenOrigin, seenAcceptEncoding, seenCookie string

	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenOrigin = r.Header.Get("Origin")
		seenAcceptEncoding = r.Header.Get("Accept-Encoding")
		seenCookie = r.Header.Get("Cookie")

		jsEscaped := strings.ReplaceAll(upstream.URL, "/", `\/`)
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!DOCTYPE html><html><head><title>mock</title></head><body>`+
			`<a href="`+upstream.URL+`/files?q=x">raw</a>`+
			`<script>var u = "`+jsEscaped+`/files";</script>`+
			`</body></html>`)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	jar := newCookieJar(filepath.Join(t.TempDir(), "cookie.txt"))
	jar.absorb([]*http.Cookie{{Name: "sid", Value: "persisted"}})

	proxy := makeProxy(target, hideCSS2, jar)
	req := httptest.NewRequest(http.MethodGet, "/s?q=x", nil)
	req.Header.Set("Cookie", "browser=1")
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	res := rr.Result()
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	got := string(body)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("代理返回 %d，body=%s", res.StatusCode, got)
	}
	if v := res.Header.Get("X-Frame-Options"); v != "" {
		t.Errorf("X-Frame-Options 未被剥离：%q", v)
	}
	if v := res.Header.Get("Content-Security-Policy"); v != "" {
		t.Errorf("CSP 未被剥离：%q", v)
	}
	if !strings.Contains(got, "<style data-local-inject>") {
		t.Error("未注入隐藏外壳样式")
	}
	if strings.Contains(got, upstream.URL) || strings.Contains(got, `\/`) {
		t.Errorf("响应正文仍含上游绝对地址：\n%s", got)
	}
	if !strings.Contains(got, "/files?q=x") {
		t.Errorf("绝对地址未改写成相对路径：\n%s", got)
	}
	if seenOrigin != upstream.URL {
		t.Errorf("上游收到的 Origin 应为 %q，实际 %q", upstream.URL, seenOrigin)
	}
	if seenAcceptEncoding == "gzip" {
		t.Error("不应向上游声明 gzip：正文改写需要未压缩内容")
	}
	if !strings.Contains(seenCookie, "persisted") || !strings.Contains(seenCookie, "browser=1") {
		t.Errorf("Cookie 未正确合并：%q", seenCookie)
	}
}

func TestProxyUsesPerEngineHideCSS(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><head></head><body>x</body></html>")
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)

	rr := httptest.NewRecorder()
	makeProxy(target, ".only-this-engine{display:none!important}", nil).
		ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	res := rr.Result()
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), ".only-this-engine") {
		t.Error("每个引擎各自的隐藏 CSS 未生效")
	}
}

func TestProxyLeavesCompressedResponseUntouched(t *testing.T) {
	var buf strings.Builder
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte("<html><head></head><body>" + strings.Repeat("x", 512) + "</body></html>"))
	_ = zw.Close()
	gzBody := buf.String()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write([]byte(gzBody))
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	rr := httptest.NewRecorder()
	makeProxy(target, hideCSS2, nil).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))

	res := rr.Result()
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != gzBody {
		t.Error("压缩响应不应被改写（改写会破坏内容）")
	}
}

func TestProxyLeavesBinaryUntouched(t *testing.T) {
	payload := "\x00\x01\x02binary\xff\xfe"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, payload)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	rr := httptest.NewRecorder()
	makeProxy(target, hideCSS2, nil).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/bin", nil))

	res := rr.Result()
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(body) != payload {
		t.Error("二进制响应不应被改写")
	}
}

func TestProxyUpstreamErrorReturnsBadGateway(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:1")
	rr := httptest.NewRecorder()
	makeProxy(target, "", nil).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr.Code != http.StatusBadGateway {
		t.Errorf("上游不可达应返回 502，实际 %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// 配置 API
// ---------------------------------------------------------------------------

func newTestConfigAPI(t *testing.T) (http.HandlerFunc, *settingsStore, *engineRegistry, deployment) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	fallback := settings{Engines: []engine{
		eng("甲", "http://192.0.2.21:1", "/s"),
		eng("乙", "http://192.0.2.22:2", "/files"),
	}}
	st, err := newSettingsStore(path, fallback, originDefault)
	if err != nil {
		t.Fatal(err)
	}
	reg := &engineRegistry{}
	ports := make([]int, maxEngines) // 生产里启动时会一次性建好这么多端口
	for i := range ports {
		ports[i] = 18898 - i
	}
	reg.setPorts(ports)
	if err := reg.apply(fallback.Engines); err != nil {
		t.Fatal(err)
	}
	dep := deployment{PagePort: 18899, ProxyPortBase: 18898}
	return makeConfigHandler(st, reg, dep, func() int { return 7 }), st, reg, dep
}

func TestConfigAPIGet(t *testing.T) {
	h, _, _, _ := newTestConfigAPI(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("GET 应返回 200，实际 %d", rr.Code)
	}
	var got configPayload
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON：%s", rr.Body.String())
	}
	if len(got.Engines) != 2 {
		t.Fatalf("应返回 2 个引擎，实际 %d", len(got.Engines))
	}
	if got.Engines[0].Name != "甲" || got.Engines[0].ProxyPort != 18898 {
		t.Errorf("引擎信息不正确：%+v", got.Engines[0])
	}
	if got.Engines[1].ProxyPort != 18897 {
		t.Errorf("第 2 个引擎端口不正确：%d", got.Engines[1].ProxyPort)
	}
	if got.Items != 7 || got.MaxEngines != maxEngines || got.ConfigFile == "" {
		t.Errorf("附加信息不正确：%+v", got)
	}
}

func TestConfigAPIPostAddsAndRemovesEngines(t *testing.T) {
	h, st, reg, _ := newTestConfigAPI(t)

	body := `{"engines":[` +
		`{"name":"一","base":"http://192.0.2.41:1","path":"/s"},` +
		`{"name":"二","base":"http://192.0.2.42:2","path":"/s"},` +
		`{"name":"三","base":"http://192.0.2.43:3","path":"/s"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	req.Header.Set("Origin", "http://"+req.Host)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("同源 POST 应返回 200，实际 %d：%s", rr.Code, rr.Body.String())
	}

	cur, orig := st.snapshot()
	if len(cur.Engines) != 3 || orig != originFile {
		t.Errorf("配置未更新：%+v / %q", cur, orig)
	}
	if _, ok := reg.at(2); !ok {
		t.Error("第 3 个引擎位的代理应立即可用（无需重启）")
	}
	var got configPayload
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if len(got.Engines) != 3 || got.Engines[2].ProxyPort != 18896 {
		t.Errorf("返回的端口分配不正确：%+v", got.Engines)
	}

	// 再缩回 1 个
	body2 := `{"engines":[{"name":"只留一个","base":"http://192.0.2.41:1","path":"/s"}]}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body2))
	req2.Header.Set("Origin", "http://"+req2.Host)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("缩容应成功，实际 %d", rr2.Code)
	}
	if _, ok := reg.at(1); ok {
		t.Error("删掉的引擎位不应再返回引擎")
	}
}

func TestConfigAPIRejectsCrossSite(t *testing.T) {
	h, st, _, _ := newTestConfigAPI(t)
	body := `{"engines":[{"name":"x","base":"http://192.0.2.41:1","path":"/s"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	req.Header.Set("Origin", "http://evil.example")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("跨站 POST 应返回 403，实际 %d", rr.Code)
	}
	cur, _ := st.snapshot()
	if len(cur.Engines) != 2 {
		t.Error("被拒绝的请求不应改动配置")
	}
}

func TestConfigAPIRejectsCrossSiteRefererWhenOriginMissing(t *testing.T) {
	h, _, _, _ := newTestConfigAPI(t)
	req := httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"engines":[{"name":"x","base":"http://192.0.2.41:1","path":"/s"}]}`))
	req.Header.Set("Referer", "http://evil.example/page")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("来自外站 Referer 的 POST 应返回 403，实际 %d", rr.Code)
	}
}

func TestConfigAPIAllowsClientsWithoutOrigin(t *testing.T) {
	h, _, _, _ := newTestConfigAPI(t)
	req := httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"engines":[{"name":"x","base":"http://192.0.2.41:1","path":"/s"}]}`))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("非浏览器客户端（curl）应放行，实际 %d：%s", rr.Code, rr.Body.String())
	}
}

func TestConfigAPIRejectsBadPayload(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"非法 JSON", `{not json`},
		{"地址非法", `{"engines":[{"name":"a","base":"ftp://x","path":"/s"}]}`},
		{"空列表", `{"engines":[]}`},
		{"名称过长", `{"engines":[{"name":"` + strings.Repeat("字", 41) + `","base":"http://192.0.2.41:1","path":"/s"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, st, _, _ := newTestConfigAPI(t)
			req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("应返回 400，实际 %d：%s", rr.Code, rr.Body.String())
			}
			var payload map[string]string
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil || payload["error"] == "" {
				t.Error("错误响应应包含可读的 error 字段")
			}
			cur, _ := st.snapshot()
			if len(cur.Engines) != 2 {
				t.Error("校验失败不应改动配置")
			}
		})
	}
}

func TestConfigAPIRejectsUnsupportedMethod(t *testing.T) {
	h, _, _, _ := newTestConfigAPI(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/config", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE 应返回 405，实际 %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// /api/search
// ---------------------------------------------------------------------------

func TestSearchAPI(t *testing.T) {
	_, _, reg, _ := newTestConfigAPI(t)
	h := makeSearchAPI(nil, reg, func() int { return 3 })

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=%E4%B8%AD%E6%96%87", nil)
	req.Host = "192.0.2.9:18899"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d：%s", rr.Code, rr.Body.String())
	}
	var got searchPayload
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Keyword != "中文" {
		t.Errorf("关键字不正确：%q", got.Keyword)
	}
	// 主字段必须是「这个网站带关键字」的直达地址 —— 对接方只需要这一个 URL
	wantPage := "http://192.0.2.9:18899/?q=" + url.QueryEscape("中文")
	if got.URL != wantPage {
		t.Errorf("网站直达地址不正确：\n实际 %s\n期望 %s", got.URL, wantPage)
	}
	if len(got.Engines) != 2 {
		t.Fatalf("补充的引擎地址应为 2 条，实际 %d", len(got.Engines))
	}
	want := "http://192.0.2.9:18898/s?q=" + url.QueryEscape("中文")
	if got.Engines[0].URL != want {
		t.Errorf("引擎地址不正确：\n实际 %s\n期望 %s", got.Engines[0].URL, want)
	}
	if got.Engines[0].Name != "甲" {
		t.Errorf("引擎名不正确：%q", got.Engines[0].Name)
	}
	if got.ItemCount != 3 {
		t.Errorf("番号数不正确：%d", got.ItemCount)
	}
}

func TestSearchAPIAcceptsCommonParameterNames(t *testing.T) {
	_, _, reg, _ := newTestConfigAPI(t)
	h := makeSearchAPI(nil, reg, func() int { return 0 })

	for _, key := range []string{"q", "keyword", "kw", "word", "search"} {
		req := httptest.NewRequest(http.MethodGet, "/api/search?"+key+"=abc", nil)
		req.Host = "h:18899"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("参数名 %s 应被接受，实际 %d", key, rr.Code)
			continue
		}
		var got searchPayload
		_ = json.Unmarshal(rr.Body.Bytes(), &got)
		if got.Keyword != "abc" {
			t.Errorf("参数名 %s 未取到关键字：%q", key, got.Keyword)
		}
	}
}

func TestQueryFromRequest(t *testing.T) {
	cases := map[string]string{
		"/?q=abc":             "abc",
		"/?keyword=abc":       "abc",
		"/?kw=abc":            "abc",
		"/?word=abc":          "abc",
		"/?search=abc":        "abc",
		"/?q=%E4%B8%AD":       "中",
		"/?q=%20%20abc%20%20": "abc",
		"/?q=":                "",
		"/":                   "",
		"/?other=1":           "",
		"/?q=&keyword=xyz":    "xyz",
	}
	for target, want := range cases {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if got := queryFromRequest(req); got != want {
			t.Errorf("queryFromRequest(%q) = %q，期望 %q", target, got, want)
		}
	}
}

// newTestAPIOnly 只要一个注册表就够了（用于 /api/search 的用例）。
func newTestAPIOnly(t *testing.T) (*engineRegistry, deployment) {
	t.Helper()
	_, _, reg, dep := newTestConfigAPI(t)
	return reg, dep
}

func TestSearchAPIRejectsMissingKeyword(t *testing.T) {
	_, _, reg, _ := newTestConfigAPI(t)
	h := makeSearchAPI(nil, reg, func() int { return 0 })

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/search", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("缺少 q 应返回 400，实际 %d", rr.Code)
	}

	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodPost, "/api/search?q=x", nil))
	if rr2.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST 应返回 405，实际 %d", rr2.Code)
	}
}

// ---------------------------------------------------------------------------
// 端到端：真起面板端口 + 多个引擎代理端口
// ---------------------------------------------------------------------------

func TestStartServersEndToEnd(t *testing.T) {
	mk := func(marker string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, "<html><head></head><body>"+marker+
				" "+r.URL.Path+" q="+r.URL.Query().Get("q")+"</body></html>")
		}))
	}
	one := mk("UPSTREAM-ONE")
	defer one.Close()
	two := mk("UPSTREAM-TWO")
	defer two.Close()
	fresh := mk("UPSTREAM-FRESH")
	defer fresh.Close()

	tmp := t.TempDir()
	dep := deployment{
		PagePort:      0,
		ProxyPortBase: 28898, // 用一个不常见的基址，避免和本机其它服务抢端口
		ListenHost:    "127.0.0.1",
		CookieFile:    filepath.Join(tmp, "cookie.txt"),
		ConfigFile:    filepath.Join(tmp, "settings.json"),
	}
	store, err := newSettingsStore(dep.ConfigFile, settings{Engines: []engine{
		{Name: "一号", Base: one.URL, Path: "/files"},
		{Name: "二号", Base: two.URL, Path: "/s"},
	}}, originEnv)
	if err != nil {
		t.Fatal(err)
	}

	pageURL, err := startServers(dep, store, []string{"ABP-123", "SSIS-001"})
	if err != nil {
		t.Fatalf("startServers 失败：%v", err)
	}

	// 1) 面板页
	page := httpGet(t, pageURL+"/")
	for _, want := range []string{`id="kwInput"`, `id="searchBtn"`, `id="settingsBtn"`, `id="pager"`,
		`id="prevBtn"`, `id="nextBtn"`, `id="clearBtn"`, `id="listBox"`, `id="codesToggle"`,
		`id="splitter"`, `id="settingsDialog"`, `id="engineList"`, `id="addEngineBtn"`, "ABP-123"} {
		if !strings.Contains(page, want) {
			t.Errorf("面板页缺少 %s", want)
		}
	}
	for _, ph := range []string{"{SCRIPT}", "{BOOTSTRAP}"} {
		if strings.Contains(page, ph) {
			t.Errorf("面板页残留占位符 %s", ph)
		}
	}

	// 2) 健康检查里应能看到两个引擎及各自的代理端口
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Items   int    `json:"items"`
		Engines []struct {
			Name      string `json:"name"`
			Base      string `json:"base"`
			ProxyPort int    `json:"proxyPort"`
		} `json:"engines"`
	}
	if err := json.Unmarshal([]byte(httpGet(t, pageURL+"/healthz")), &health); err != nil {
		t.Fatal(err)
	}
	if health.Status != "ok" || health.Items != 2 || len(health.Engines) != 2 {
		t.Fatalf("/healthz 内容不正确：%+v", health)
	}
	if health.Engines[0].Name != "一号" || health.Engines[1].Name != "二号" {
		t.Errorf("引擎名不正确：%+v", health.Engines)
	}

	// 3) 两个引擎各自的代理端口都能取到内容
	p0 := health.Engines[0].ProxyPort
	p1 := health.Engines[1].ProxyPort
	if got := httpGet(t, "http://127.0.0.1:"+strconv.Itoa(p0)+"/files?q=abc"); !strings.Contains(got, "UPSTREAM-ONE") {
		t.Errorf("引擎1 代理内容不正确：%s", got)
	}
	if got := httpGet(t, "http://127.0.0.1:"+strconv.Itoa(p1)+"/s?q=%E4%B8%AD%E6%96%87"); !strings.Contains(got, "UPSTREAM-TWO") {
		t.Errorf("引擎2 代理内容不正确：%s", got)
	}

	// 4) 未配置的引擎位应给提示页而不是失败。
	// 只有端口基址没被占用（也就是没有触发随机端口回退）时才能算出它的端口。
	if p0 == dep.ProxyPortBase {
		p7 := dep.ProxyPortBase - (maxEngines - 1)
		if got := httpGet(t, "http://127.0.0.1:"+strconv.Itoa(p7)+"/s?q=x"); !strings.Contains(got, "未配置") {
			t.Errorf("空的引擎位应给提示页：%s", got)
		}
	} else {
		t.Logf("端口基址 %d 被占用，已回退到随机端口，跳过空引擎位断言", dep.ProxyPortBase)
	}

	// 5) /api/search：主字段应是「这个网站带关键字」的直达地址
	var sp searchPayload
	if err := json.Unmarshal([]byte(httpGet(t, pageURL+"/api/search?q=abc")), &sp); err != nil {
		t.Fatalf("/api/search 响应不合法：%v", err)
	}
	wantPage := pageURL + "/?q=abc"
	if sp.URL != wantPage {
		t.Errorf("/api/search 的网站直达地址不正确：\n实际 %s\n期望 %s", sp.URL, wantPage)
	}
	if len(sp.Engines) != 2 || !strings.Contains(sp.Engines[0].URL, "/files?q=abc") {
		t.Errorf("/api/search 内容不正确：%+v", sp)
	}
	if sp.Engines[0].URL != "http://127.0.0.1:"+strconv.Itoa(p0)+"/files?q=abc" {
		t.Errorf("/api/search 里的端口不对：%s", sp.Engines[0].URL)
	}

	// 6) 这个网站带关键字的几种写法都要能用
	for _, target := range []string{"/?q=abc", "/search?q=abc", "/?keyword=abc", "/?kw=abc"} {
		page := httpGet(t, pageURL+target)
		if !strings.Contains(page, `id="bootstrap"`) || !strings.Contains(page, `"q":"abc"`) {
			t.Errorf("%s 未把关键字带进面板页", target)
		}
	}
	// 没带关键字的 /search 不是面板页，应给 404
	respSearch, err := http.Get(pageURL + "/search")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, respSearch.Body)
	respSearch.Body.Close()
	if respSearch.StatusCode != http.StatusNotFound {
		t.Errorf("不带关键字的 /search 应返回 404，实际 %d", respSearch.StatusCode)
	}

	// 6) 通过配置接口改第 1 个引擎地址，代理应立即指向新上游
	body := `{"engines":[` +
		`{"name":"一号","base":"` + fresh.URL + `","path":"/files"},` +
		`{"name":"二号","base":"` + two.URL + `","path":"/s"}]}`
	post, _ := http.NewRequest(http.MethodPost, pageURL+"/api/config", strings.NewReader(body))
	post.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatalf("POST /api/config 失败：%v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/config 应返回 200，实际 %d", resp.StatusCode)
	}
	if got := httpGet(t, "http://127.0.0.1:"+strconv.Itoa(p0)+"/files?q=abc"); !strings.Contains(got, "UPSTREAM-FRESH") {
		t.Errorf("改地址后代理未切到新上游：%s", got)
	}

	// 7) 面板端口上的未知路径给 404 提示，而不是转发到某个上游
	respNotFound, err := http.Get(pageURL + "/something")
	if err != nil {
		t.Fatal(err)
	}
	body404, _ := io.ReadAll(respNotFound.Body)
	respNotFound.Body.Close()
	if respNotFound.StatusCode != http.StatusNotFound {
		t.Errorf("未知路径应返回 404，实际 %d", respNotFound.StatusCode)
	}
	if !strings.Contains(string(body404), "/api/search") {
		t.Error("404 页应提示可用路径")
	}

	// 8) 配置已落盘（持久化判据）
	data, err := os.ReadFile(dep.ConfigFile)
	if err != nil {
		t.Fatalf("配置文件未落盘：%v", err)
	}
	if !strings.Contains(string(data), fresh.URL) {
		t.Errorf("配置文件内容不正确：%s", data)
	}
}

func TestPanelCarriesQueryIntoBootstrap(t *testing.T) {
	one := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ONE")
	}))
	defer one.Close()

	tmp := t.TempDir()
	dep := deployment{
		PagePort: 0, ProxyPortBase: 29898, ListenHost: "127.0.0.1",
		CookieFile: filepath.Join(tmp, "c.txt"), ConfigFile: filepath.Join(tmp, "s.json"),
	}
	store, _ := newSettingsStore(dep.ConfigFile, settings{Engines: []engine{
		{Name: "唯一", Base: one.URL, Path: "/s"},
	}}, originDefault)

	pageURL, err := startServers(dep, store, nil)
	if err != nil {
		t.Fatal(err)
	}

	// URL 带上关键字时，引导数据里应带上它，前端据此自动检索
	page := httpGet(t, pageURL+"/?q=%E4%B8%AD%E6%96%87")
	raw := page[strings.Index(page, `id="bootstrap"`):]
	start := strings.Index(raw, ">") + 1
	end := strings.Index(raw, "</script>")
	var payload struct {
		Query   string `json:"q"`
		Engines []struct {
			Name      string `json:"name"`
			ProxyPort int    `json:"proxyPort"`
		} `json:"engines"`
	}
	if err := json.Unmarshal([]byte(raw[start:end]), &payload); err != nil {
		t.Fatalf("引导数据不是合法 JSON：%v", err)
	}
	if payload.Query != "中文" {
		t.Errorf("引导数据未带上 URL 里的关键字：%q", payload.Query)
	}
	if len(payload.Engines) != 1 || payload.Engines[0].ProxyPort == 0 {
		t.Errorf("引导数据里的引擎信息不正确：%+v", payload.Engines)
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

func httpGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败：%v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 %s 响应失败：%v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s 返回 %d：%s", url, resp.StatusCode, string(body))
	}
	return string(body)
}
