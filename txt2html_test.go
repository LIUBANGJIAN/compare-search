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
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 配置：地址校验
// ---------------------------------------------------------------------------

func TestNormalizeSourceURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"http://192.0.2.20:11510", "http://192.0.2.20:11510", false},
		{"  http://192.0.2.20:11510  ", "http://192.0.2.20:11510", false},
		{"https://example.com", "https://example.com", false},
		{"http://example.com/", "http://example.com", false},
		{"http://example.com/sub/", "http://example.com/sub", false},
		{"", "", true},
		{"   ", "", true},
		{"192.0.2.20:11510", "", true},  // 缺 scheme
		{"ftp://example.com", "", true}, // scheme 不支持
		{"http://", "", true},           // 缺主机
		{"://bad", "", true},            // 无法解析
	}
	for _, tc := range cases {
		got, err := normalizeSourceURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeSourceURL(%q) 应报错，实际返回 %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeSourceURL(%q) 意外报错：%v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeSourceURL(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 配置：持久化
// ---------------------------------------------------------------------------

func TestSettingsStoreFallsBackWhenFileMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	fallback := settings{Source1: "http://192.0.2.20:11510", Source2: "http://192.0.2.20:8787"}

	st, err := newSettingsStore(path, fallback, originEnv)
	if err != nil {
		t.Fatalf("文件不存在时不应报错：%v", err)
	}
	got, orig := st.snapshot()
	if got != fallback {
		t.Errorf("应使用 fallback，实际 %+v", got)
	}
	if orig != originEnv {
		t.Errorf("来源应为 %q，实际 %q", originEnv, orig)
	}
}

func TestSettingsStoreLoadsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	body := `{"source1":"http://192.0.2.31:1","source2":"http://192.0.2.32:2"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := newSettingsStore(path, settings{}, originDefault)
	if err != nil {
		t.Fatalf("读取配置失败：%v", err)
	}
	got, orig := st.snapshot()
	if got.Source1 != "http://192.0.2.31:1" || got.Source2 != "http://192.0.2.32:2" {
		t.Errorf("未加载文件里的地址：%+v", got)
	}
	if orig != originFile {
		t.Errorf("来源应为 %q，实际 %q", originFile, orig)
	}
}

func TestSettingsStoreFallsBackWhenFileBroken(t *testing.T) {
	for _, body := range []string{
		`{ 这不是 json`,
		`{"source1":"ftp://x","source2":"http://y"}`,
		`{"source1":"","source2":"http://y"}`,
	} {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		fallback := settings{Source1: "http://192.0.2.20:11510", Source2: "http://192.0.2.20:8787"}
		st, err := newSettingsStore(path, fallback, originDefault)
		if err == nil {
			t.Errorf("内容为 %s 时应返回错误提示", body)
		}
		got, orig := st.snapshot()
		if got != fallback || orig != originDefault {
			t.Errorf("内容为 %s 时应退回 fallback，实际 %+v / %q", body, got, orig)
		}
	}
}

func TestSettingsStoreSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "settings.json") // 目录不存在，应自动创建
	st, err := newSettingsStore(path, settings{}, originDefault)
	if err != nil {
		t.Fatal(err)
	}

	// 带尾部斜杠与空白，保存时应被归一化
	if err := st.save(settings{Source1: " http://192.0.2.31:1/ ", Source2: "http://192.0.2.32:2"}); err != nil {
		t.Fatalf("保存失败：%v", err)
	}

	got, orig := st.snapshot()
	if got.Source1 != "http://192.0.2.31:1" || got.Source2 != "http://192.0.2.32:2" {
		t.Errorf("保存后内存值不正确：%+v", got)
	}
	if orig != originFile {
		t.Errorf("保存后来源应为 %q，实际 %q", originFile, orig)
	}

	// 重新加载（模拟重启）应拿到同样的值 —— 这就是「持久化」的判据
	again, err := newSettingsStore(path, settings{}, originDefault)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, _ := again.snapshot()
	if reloaded != got {
		t.Errorf("重启后读回的值不一致：%+v vs %+v", reloaded, got)
	}
}

func TestSettingsStoreSaveRejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	st, _ := newSettingsStore(path, settings{}, originDefault)

	if err := st.save(settings{Source1: "ftp://x", Source2: "http://ok.example"}); err == nil {
		t.Error("非法源1 地址应被拒绝")
	}
	if err := st.save(settings{Source1: "http://ok.example", Source2: ""}); err == nil {
		t.Error("空源2 地址应被拒绝")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("校验失败时不应写文件")
	}
}

func TestWriteFileAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := writeFileAtomic(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello\n" {
		t.Errorf("内容不正确：%q", data)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("目录里应只剩目标文件，实际 %d 个", len(entries))
	}
}

// ---------------------------------------------------------------------------
// 环境变量与部署参数
// ---------------------------------------------------------------------------

func TestSourceDefaults(t *testing.T) {
	t.Setenv("SRC1_BASE", "")
	t.Setenv("SRC2_BASE", "")
	got, orig := sourceDefaults()
	if got.Source1 != defaultSource1 || got.Source2 != defaultSource2 {
		t.Errorf("无环境变量时应返回内置默认值，实际 %+v", got)
	}
	if orig != originDefault {
		t.Errorf("来源应为 %q，实际 %q", originDefault, orig)
	}

	t.Setenv("SRC1_BASE", "http://192.0.2.31:1/")
	t.Setenv("SRC2_BASE", "")
	got, orig = sourceDefaults()
	if got.Source1 != "http://192.0.2.31:1" {
		t.Errorf("SRC1_BASE 未被采纳或未去尾斜杠：%q", got.Source1)
	}
	if got.Source2 != defaultSource2 {
		t.Errorf("未设置的源应保留默认值：%q", got.Source2)
	}
	if orig != originEnv {
		t.Errorf("来源应为 %q，实际 %q", originEnv, orig)
	}
}

func TestLoadDeployment(t *testing.T) {
	t.Setenv("PAGE_PORT", "28099")
	t.Setenv("PROXY1_PORT", "28098")
	t.Setenv("LISTEN_HOST", "0.0.0.0")
	t.Setenv("COOKIE_FILE", "  /data/c.txt  ")
	t.Setenv("CONFIG_FILE", "/data/settings.json")

	dep := loadDeployment()
	if dep.PagePort != 28099 || dep.Proxy1Port != 28098 {
		t.Errorf("端口未被采纳：%d / %d", dep.PagePort, dep.Proxy1Port)
	}
	if dep.ListenHost != "0.0.0.0" {
		t.Errorf("LISTEN_HOST 未被采纳：%q", dep.ListenHost)
	}
	if dep.CookieFile != "/data/c.txt" {
		t.Errorf("COOKIE_FILE 未去空白：%q", dep.CookieFile)
	}
	if dep.ConfigFile != "/data/settings.json" {
		t.Errorf("CONFIG_FILE 未被采纳：%q", dep.ConfigFile)
	}
}

func TestEnvPortOrRejectsInvalid(t *testing.T) {
	cases := []struct {
		value string
		want  int
	}{
		{"", 18899}, {"0", 18899}, {"-1", 18899}, {"65536", 18899},
		{"abc", 18899}, {"1", 1}, {"65535", 65535}, {" 8080 ", 8080},
	}
	for _, tc := range cases {
		t.Setenv("PAGE_PORT", tc.value)
		if got := envPortOr("PAGE_PORT", 18899); got != tc.want {
			t.Errorf("envPortOr(%q) = %d，期望 %d", tc.value, got, tc.want)
		}
	}
}

func TestApplyEnvFileRealEnvWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.env")
	content := strings.Join([]string{
		"# 注释行",
		"",
		"T2H_FROM_FILE=来自文件",
		"T2H_QUOTED=\"带引号的值\"",
		"T2H_ALREADY_SET=文件值",
		"没有等号的行",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("T2H_ALREADY_SET", "环境变量值")
	os.Unsetenv("T2H_FROM_FILE")
	os.Unsetenv("T2H_QUOTED")
	defer os.Unsetenv("T2H_FROM_FILE")
	defer os.Unsetenv("T2H_QUOTED")

	applyEnvFile(path)

	if got := os.Getenv("T2H_FROM_FILE"); got != "来自文件" {
		t.Errorf("应载入文件值，实际 %q", got)
	}
	if got := os.Getenv("T2H_QUOTED"); got != "带引号的值" {
		t.Errorf("引号应被剥离，实际 %q", got)
	}
	if got := os.Getenv("T2H_ALREADY_SET"); got != "环境变量值" {
		t.Errorf("真实环境变量应优先，实际 %q", got)
	}
}

func TestApplyEnvFileMissingFileIsNoop(t *testing.T) {
	applyEnvFile(filepath.Join(t.TempDir(), "不存在.env"))
}

// ---------------------------------------------------------------------------
// 输入解析
// ---------------------------------------------------------------------------

func TestReadList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "list.txt")
	content := "  ABP-123  \r\n\r\nSSIS-001\nIPX-777\n\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	items, err := readList(path)
	if err != nil {
		t.Fatalf("readList 失败：%v", err)
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
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("空文件应返回空列表，实际 %v", items)
	}

	if _, err := readList(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Error("读取不存在的文件应返回错误")
	}
}

// ---------------------------------------------------------------------------
// 面板页拼装
// ---------------------------------------------------------------------------

func TestBuildPageReplacesAllPlaceholders(t *testing.T) {
	page := buildPage(pageData{
		Items:   []string{"ABP-123"},
		Source1: "http://192.0.2.31:1",
		Source2: "http://192.0.2.32:2",
		Origin:  string(originFile),
		Version: "test",
		P1Port:  18898,
	})

	for _, ph := range []string{"{P1PORT}", "{SCRIPT}", "{BOOTSTRAP}"} {
		if strings.Contains(page, ph) {
			t.Errorf("占位符 %s 未被替换", ph)
		}
	}
	if !strings.Contains(page, `data-p1-port="18898"`) {
		t.Error("源1 端口未注入 body 的 data-p1-port")
	}
	if !strings.Contains(page, "splitKeywords") {
		t.Error("前端脚本未内联")
	}
	if !strings.Contains(page, `"items":["ABP-123"]`) {
		t.Error("引导数据里未包含番号列表")
	}
	if !strings.Contains(page, `"source1":"http://192.0.2.31:1"`) {
		t.Error("引导数据里未包含源1 地址")
	}
}

func TestBuildPageEscapesBootstrapForScriptContext(t *testing.T) {
	// 番号文本里若含 </script>，直接拼进 <script> 会提前结束脚本块，形成注入
	nasty := `</script><script>alert(1)</script>`
	page := buildPage(pageData{
		Items:   []string{nasty},
		P1Port:  1,
		Source1: "http://a",
		Source2: "http://b",
	})

	if strings.Contains(page, "</script><script>alert(1)</script>") {
		t.Error("引导数据未做转义，存在脚本注入风险")
	}
	if !strings.Contains(page, `\u003c/script\u003e`) {
		t.Error("尖括号应被转义为 \\u003c 形式")
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
		"":          "127.0.0.1",
		"0.0.0.0":   "127.0.0.1",
		"::":        "127.0.0.1",
		"192.0.2.5": "192.0.2.5",
		"nas.local": "nas.local",
	}
	for in, want := range cases {
		if got := displayHost(in); got != want {
			t.Errorf("displayHost(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Cookie
// ---------------------------------------------------------------------------

func TestCookieJarPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cookie.txt")
	jar := newCookieJar(path)

	jar.absorb([]*http.Cookie{
		{Name: "sid", Value: "abc"},
		{Name: "uid", Value: "42"},
	})
	jar.absorb([]*http.Cookie{{Name: "uid", Value: "43"}})

	reloaded := newCookieJar(path)
	got := reloaded.headerValue("")
	if !strings.Contains(got, "sid=abc") || !strings.Contains(got, "uid=43") {
		t.Errorf("Cookie 未正确持久化/重载：%q", got)
	}
}

func TestCookieJarBrowserValueMergedWithLocalWins(t *testing.T) {
	jar := newCookieJar(filepath.Join(t.TempDir(), "cookie.txt"))
	jar.absorb([]*http.Cookie{{Name: "sid", Value: "local"}})

	merged := jar.headerValue("sid=browser; other=1")
	if !strings.Contains(merged, "sid=local") {
		t.Errorf("本地 Cookie 应覆盖浏览器同名 Cookie：%q", merged)
	}
	if !strings.Contains(merged, "other=1") {
		t.Errorf("浏览器独有 Cookie 应保留：%q", merged)
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
	var seenOrigin, seenReferer, seenAcceptEncoding, seenCookie string

	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenOrigin = r.Header.Get("Origin")
		seenReferer = r.Header.Get("Referer")
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

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar := newCookieJar(filepath.Join(t.TempDir(), "cookie.txt"))
	jar.absorb([]*http.Cookie{{Name: "sid", Value: "persisted"}})

	proxy := makeProxy(func() *url.URL { return target }, hideCSS2, jar)

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
	if strings.Contains(got, upstream.URL) {
		t.Errorf("响应正文仍含上游绝对地址：\n%s", got)
	}
	if strings.Contains(got, `\/`) {
		t.Errorf("响应正文仍含 JS 转义形式的上游地址：\n%s", got)
	}
	if !strings.Contains(got, "/files?q=x") {
		t.Errorf("绝对地址未改写成相对路径：\n%s", got)
	}
	if seenOrigin != upstream.URL {
		t.Errorf("上游收到的 Origin 应为 %q，实际 %q", upstream.URL, seenOrigin)
	}
	if seenReferer != upstream.URL+"/" {
		t.Errorf("上游收到的 Referer 应为 %q，实际 %q", upstream.URL+"/", seenReferer)
	}
	if seenAcceptEncoding == "gzip" {
		t.Error("不应向上游声明 gzip：正文改写需要未压缩内容")
	}
	if !strings.Contains(seenCookie, "persisted") || !strings.Contains(seenCookie, "browser=1") {
		t.Errorf("Cookie 未正确合并：%q", seenCookie)
	}
}

func TestProxyHotSwapTarget(t *testing.T) {
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "UPSTREAM-A")
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "UPSTREAM-B")
	}))
	defer upB.Close()

	targetA, _ := url.Parse(upA.URL)
	targetB, _ := url.Parse(upB.URL)
	cur := targetA

	proxy := makeProxy(func() *url.URL { return cur }, "", nil)

	call := func() string {
		rr := httptest.NewRecorder()
		proxy.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
		res := rr.Result()
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return string(body)
	}

	if got := call(); got != "UPSTREAM-A" {
		t.Fatalf("首次请求应打到上游 A，实际 %q", got)
	}
	cur = targetB // 模拟配置保存后的热切换
	if got := call(); got != "UPSTREAM-B" {
		t.Errorf("切换后请求应打到上游 B，实际 %q", got)
	}
}

func TestProxyTargetSetAndGet(t *testing.T) {
	pt, err := newProxyTarget("http://192.0.2.31:1")
	if err != nil {
		t.Fatal(err)
	}
	if got := pt.get().Host; got != "192.0.2.31:1" {
		t.Errorf("初始 host 不正确：%q", got)
	}
	if err := pt.set("http://192.0.2.32:2/"); err != nil {
		t.Fatal(err)
	}
	if got := pt.get().Host; got != "192.0.2.32:2" {
		t.Errorf("切换后 host 不正确：%q", got)
	}
	if err := pt.set("::bad::"); err == nil {
		t.Error("非法地址应返回错误")
	}
	if got := pt.get().Host; got != "192.0.2.32:2" {
		t.Errorf("非法地址不应改变现状，实际 %q", got)
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
	proxy := makeProxy(func() *url.URL { return target }, hideCSS2, nil)

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))

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
	proxy := makeProxy(func() *url.URL { return target }, hideCSS2, nil)

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/bin", nil))

	res := rr.Result()
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if string(body) != payload {
		t.Error("二进制响应不应被改写")
	}
}

func TestProxyUpstreamErrorReturnsBadGateway(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:1")
	proxy := makeProxy(func() *url.URL { return target }, "", nil)

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rr.Code != http.StatusBadGateway {
		t.Errorf("上游不可达应返回 502，实际 %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// 配置 API
// ---------------------------------------------------------------------------

func newTestConfigAPI(t *testing.T) (http.HandlerFunc, *settingsStore, *proxyTarget, *proxyTarget) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	st, err := newSettingsStore(path, settings{Source1: "http://192.0.2.20:11510", Source2: "http://192.0.2.20:8787"}, originDefault)
	if err != nil {
		t.Fatal(err)
	}
	t1, _ := newProxyTarget("http://192.0.2.20:11510")
	t2, _ := newProxyTarget("http://192.0.2.20:8787")
	return makeConfigHandler(st, t1, t2, func() int { return 7 }), st, t1, t2
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
	if got.Source1 != "http://192.0.2.20:11510" || got.Source2 != "http://192.0.2.20:8787" {
		t.Errorf("返回的地址不正确：%+v", got)
	}
	if got.Origin != string(originDefault) {
		t.Errorf("返回的来源不正确：%q", got.Origin)
	}
	if got.Items != 7 {
		t.Errorf("返回的番号数不正确：%d", got.Items)
	}
	if got.ConfigFile == "" {
		t.Error("应返回配置文件路径")
	}
}

func TestConfigAPIPostWithHostOrigin(t *testing.T) {
	h, st, t1, t2 := newTestConfigAPI(t)

	body := `{"source1":"http://192.0.2.41:1","source2":"http://192.0.2.42:2"}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://"+req.Host)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("同源 POST 应返回 200，实际 %d：%s", rr.Code, rr.Body.String())
	}
	cur, orig := st.snapshot()
	if cur.Source1 != "http://192.0.2.41:1" || cur.Source2 != "http://192.0.2.42:2" {
		t.Errorf("配置未更新：%+v", cur)
	}
	if orig != originFile {
		t.Errorf("来源应变为 %q，实际 %q", originFile, orig)
	}
	// 热切换：代理目标应同步跟上
	if t1.get().Host != "192.0.2.41:1" {
		t.Errorf("源1 代理目标未热切换：%q", t1.get().Host)
	}
	if t2.get().Host != "192.0.2.42:2" {
		t.Errorf("源2 代理目标未热切换：%q", t2.get().Host)
	}
}

func TestConfigAPIRejectsCorssSite(t *testing.T) {
	h, st, _, _ := newTestConfigAPI(t)

	body := `{"source1":"http://192.0.2.41:1","source2":"http://192.0.2.42:2"}`
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("跨站 POST 应返回 403，实际 %d", rr.Code)
	}
	cur, _ := st.snapshot()
	if cur.Source1 != "http://192.0.2.20:11510" {
		t.Error("被拒绝的请求不应改动配置")
	}
}

func TestConfigAPIRejectsCrossSiteRefererWhenOriginMissing(t *testing.T) {
	h, _, _, _ := newTestConfigAPI(t)

	req := httptest.NewRequest(http.MethodPost, "/api/config",
		strings.NewReader(`{"source1":"http://192.0.2.41:1","source2":"http://192.0.2.42:2"}`))
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
		strings.NewReader(`{"source1":"http://192.0.2.41:1","source2":"http://192.0.2.42:2"}`))

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
		code int
	}{
		{"非法 JSON", `{not json`, http.StatusBadRequest},
		{"源1 地址非法", `{"source1":"ftp://x","source2":"http://ok.example"}`, http.StatusBadRequest},
		{"源2 为空", `{"source1":"http://ok.example","source2":""}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, st, _, _ := newTestConfigAPI(t)
			req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(tc.body))
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != tc.code {
				t.Fatalf("应返回 %d，实际 %d：%s", tc.code, rr.Code, rr.Body.String())
			}
			var payload map[string]string
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil || payload["error"] == "" {
				t.Error("错误响应应包含可读的 error 字段")
			}
			cur, _ := st.snapshot()
			if cur.Source1 != "http://192.0.2.20:11510" {
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
	if allow := rr.Header().Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "POST") {
		t.Errorf("Allow 头不正确：%q", allow)
	}
}

// ---------------------------------------------------------------------------
// 端到端：真起两个端口，走一遍代理链路与配置接口
// ---------------------------------------------------------------------------

func TestStartServersEndToEnd(t *testing.T) {
	mk := func(marker string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// 回显解码后的查询串，便于断言代理是否原样透传了参数
			_, _ = io.WriteString(w, "<html><head></head><body>"+marker+
				" "+r.URL.Path+" q="+r.URL.Query().Get("q")+"</body></html>")
		}))
	}
	source1 := mk("UPSTREAM-ONE")
	defer source1.Close()
	source2 := mk("UPSTREAM-TWO")
	defer source2.Close()

	tmp := t.TempDir()
	dep := deployment{
		PagePort:   0,
		Proxy1Port: 0,
		ListenHost: "127.0.0.1",
		CookieFile: filepath.Join(tmp, "cookie.txt"),
		ConfigFile: filepath.Join(tmp, "settings.json"),
	}
	store, err := newSettingsStore(dep.ConfigFile, settings{Source1: source1.URL, Source2: source2.URL}, originEnv)
	if err != nil {
		t.Fatal(err)
	}

	items := []string{"ABP-123", "SSIS-001"}
	pageURL, err := startServers(dep, store, items)
	if err != nil {
		t.Fatalf("startServers 失败：%v", err)
	}

	// 1) 面板页
	page := httpGet(t, pageURL+"/")
	for _, want := range []string{`id="kwInput"`, `id="searchBtn"`, `id="settingsBtn"`, `id="pager"`,
		`id="prevBtn"`, `id="nextBtn"`, `id="frame1"`, `id="frame2"`, `id="clearBtn"`,
		`id="listBox"`, `id="codesToggle"`, `id="splitter"`, `id="settingsDialog"`, "ABP-123"} {
		if !strings.Contains(page, want) {
			t.Errorf("面板页缺少 %s", want)
		}
	}
	for _, ph := range []string{"{P1PORT}", "{SCRIPT}", "{BOOTSTRAP}"} {
		if strings.Contains(page, ph) {
			t.Errorf("面板页残留占位符 %s", ph)
		}
	}

	m := regexp.MustCompile(`data-p1-port="(\d+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("面板页未注入源1 端口")
	}
	p1Port := m[1]

	// 2) 健康检查
	var health struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Items   int    `json:"items"`
		Source1 string `json:"source1"`
	}
	if err := json.Unmarshal([]byte(httpGet(t, pageURL+"/healthz")), &health); err != nil {
		t.Fatal(err)
	}
	if health.Status != "ok" || health.Version == "" || health.Items != 2 {
		t.Errorf("/healthz 内容不正确：%+v", health)
	}
	if health.Source1 != source1.URL {
		t.Errorf("/healthz 未反映当前源1 地址：%q", health.Source1)
	}

	// 3) 源2 代理（面板页同源路径）
	got2 := httpGet(t, pageURL+"/s?q=%E4%B8%AD%E6%96%87")
	if !strings.Contains(got2, "UPSTREAM-TWO") || !strings.Contains(got2, "中文") {
		t.Errorf("源2 代理内容不正确：%s", got2)
	}
	if !strings.Contains(got2, "data-local-inject") {
		t.Error("源2 未注入隐藏样式")
	}

	// 4) 源1 代理（独立端口）
	got1 := httpGet(t, "http://127.0.0.1:"+p1Port+"/files?q=abc")
	if !strings.Contains(got1, "UPSTREAM-ONE") {
		t.Errorf("源1 代理内容不正确：%s", got1)
	}
	if !strings.Contains(got1, hideCSS1) {
		t.Error("源1 注入的样式内容不正确")
	}

	// 5) 配置接口：改地址后代理应立刻指向新上游
	upNew := mk("UPSTREAM-NEW")
	defer upNew.Close()
	body := `{"source1":"` + upNew.URL + `","source2":"` + source2.URL + `"}`
	req, _ := http.NewRequest(http.MethodPost, pageURL+"/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/config 失败：%v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/config 应返回 200，实际 %d", resp.StatusCode)
	}

	got1b := httpGet(t, "http://127.0.0.1:"+p1Port+"/files?q=abc")
	if !strings.Contains(got1b, "UPSTREAM-NEW") {
		t.Errorf("保存后源1 代理未切到新上游：%s", got1b)
	}

	// 6) 配置已落盘（持久化判据）
	data, err := os.ReadFile(dep.ConfigFile)
	if err != nil {
		t.Fatalf("配置文件未落盘：%v", err)
	}
	if !strings.Contains(string(data), upNew.URL) {
		t.Errorf("配置文件内容不正确：%s", data)
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
