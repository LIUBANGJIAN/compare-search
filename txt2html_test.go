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
// 配置
// ---------------------------------------------------------------------------

func TestLoadConfigDefaults(t *testing.T) {
	for _, k := range []string{"SRC1_BASE", "SRC2_BASE", "PAGE_PORT", "PROXY1_PORT", "LISTEN_HOST", "COOKIE_FILE"} {
		t.Setenv(k, "")
	}

	cfg := loadConfig()

	if cfg.Source1 != defaultSource1 {
		t.Errorf("Source1 默认值错误：%q", cfg.Source1)
	}
	if cfg.Source2 != defaultSource2 {
		t.Errorf("Source2 默认值错误：%q", cfg.Source2)
	}
	if cfg.PagePort != defaultPagePort {
		t.Errorf("PagePort 默认值错误：%d", cfg.PagePort)
	}
	if cfg.Proxy1Port != defaultProxy1Port {
		t.Errorf("Proxy1Port 默认值错误：%d", cfg.Proxy1Port)
	}
	if cfg.ListenHost != "127.0.0.1" {
		t.Errorf("ListenHost 默认值应为 127.0.0.1，实际 %q", cfg.ListenHost)
	}
	if !strings.HasSuffix(cfg.CookieFile, "cookie_115.txt") {
		t.Errorf("CookieFile 默认值应以 cookie_115.txt 结尾，实际 %q", cfg.CookieFile)
	}
}

func TestLoadConfigEnvOverride(t *testing.T) {
	// 用 RFC 5737 文档专用地址，避免在仓库里留下任何私网拓扑
	t.Setenv("SRC1_BASE", "http://192.0.2.11:1234")
	t.Setenv("SRC2_BASE", "http://192.0.2.12:5678")
	t.Setenv("PAGE_PORT", "28099")
	t.Setenv("PROXY1_PORT", "28098")
	t.Setenv("LISTEN_HOST", "0.0.0.0")
	t.Setenv("COOKIE_FILE", "  /data/c.txt  ")

	cfg := loadConfig()

	if cfg.Source1 != "http://192.0.2.11:1234" {
		t.Errorf("SRC1_BASE 未被采纳：%q", cfg.Source1)
	}
	if cfg.Source2 != "http://192.0.2.12:5678" {
		t.Errorf("SRC2_BASE 未被采纳：%q", cfg.Source2)
	}
	if cfg.PagePort != 28099 || cfg.Proxy1Port != 28098 {
		t.Errorf("端口未被采纳：%d / %d", cfg.PagePort, cfg.Proxy1Port)
	}
	if cfg.ListenHost != "0.0.0.0" {
		t.Errorf("LISTEN_HOST 未被采纳：%q", cfg.ListenHost)
	}
	if cfg.CookieFile != "/data/c.txt" {
		t.Errorf("COOKIE_FILE 未去空白：%q", cfg.CookieFile)
	}
}

func TestEnvPortOrRejectsInvalid(t *testing.T) {
	cases := []struct {
		value string
		want  int
	}{
		{"", 18899},
		{"0", 18899},
		{"-1", 18899},
		{"65536", 18899},
		{"abc", 18899},
		{"1", 1},
		{"65535", 65535},
		{" 8080 ", 8080},
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
	t.Setenv("T2H_FROM_FILE", "")
	os.Unsetenv("T2H_FROM_FILE")
	t.Setenv("T2H_QUOTED", "")
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

func TestReadListItems(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "list.txt")
	// 含空行、前后空格、HTML 特殊字符、中文
	content := "  ABP-123  \n\nSSIS-001\n<script>alert(1)</script>\n中文 词\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	html, err := readListItems(path)
	if err != nil {
		t.Fatalf("readListItems 失败：%v", err)
	}

	if n := strings.Count(html, `class="data-line"`); n != 4 {
		t.Errorf("应生成 4 条列表项，实际 %d：\n%s", n, html)
	}
	if !strings.Contains(html, `data-id="item-0"`) || !strings.Contains(html, `data-id="item-3"`) {
		t.Error("data-id 序号不正确")
	}
	if !strings.Contains(html, `data-q1="/files?q=ABP-123"`) {
		t.Error("data-q1 前缀或取值不正确")
	}
	if !strings.Contains(html, `data-q2="/s?q=ABP-123"`) {
		t.Error("data-q2 前缀或取值不正确")
	}
	if strings.Contains(html, "<script>") {
		t.Error("原始 HTML 未被转义，存在 XSS 风险")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("HTML 特殊字符应被转义为实体")
	}
	if !strings.Contains(html, `q=%E4%B8%AD%E6%96%87+%E8%AF%8D`) &&
		!strings.Contains(html, `q=%E4%B8%AD%E6%96%87%20%E8%AF%8D`) {
		t.Errorf("中文关键字未被 URL 编码：\n%s", html)
	}
}

func TestReadListItemsEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, []byte("\n\n   \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	html, err := readListItems(path)
	if err != nil {
		t.Fatalf("readListItems 失败：%v", err)
	}
	if strings.Contains(html, `class="data-line"`) {
		t.Error("空文件不应生成列表项")
	}
	if !strings.Contains(html, "list-empty") {
		t.Error("空文件应给出占位提示")
	}
}

func TestReadListItemsMissingFile(t *testing.T) {
	if _, err := readListItems(filepath.Join(t.TempDir(), "nope.txt")); err == nil {
		t.Error("读取不存在的文件应返回错误")
	}
}

// ---------------------------------------------------------------------------
// 面板页拼装
// ---------------------------------------------------------------------------

func TestBuildPageReplacesAllPlaceholders(t *testing.T) {
	items := "<a class=\"data-line\" data-id=\"item-0\">ABP-123</a>\n"
	page := buildPage(items, 18898)

	for _, ph := range []string{"{P1PORT}", "{SCRIPT}", "{ITEMS}"} {
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
	if !strings.Contains(page, items) {
		t.Error("列表片段未注入")
	}
}

func TestBuildPageDoesNotReprocessItemsContent(t *testing.T) {
	// 用户数据里若含占位符字面量，不应被二次解析
	items := "<a class=\"data-line\" data-id=\"item-0\">{SCRIPT}</a>\n"
	page := buildPage(items, 1)

	if !strings.Contains(page, `<a class="data-line" data-id="item-0">{SCRIPT}</a>`) {
		t.Error("列表内容被二次解析，占位符替换顺序有误")
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
	if portOf(ln) == 0 {
		t.Error("退化后的端口不应为 0")
	}
}

func TestDisplayHost(t *testing.T) {
	cases := map[string]string{
		"":            "127.0.0.1",
		"0.0.0.0":     "127.0.0.1",
		"::":          "127.0.0.1",
		"203.0.113.5": "203.0.113.5",
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
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "sid") {
		t.Error("删除后的状态未落盘")
	}
}

func TestCookieJarEmptyBrowserValue(t *testing.T) {
	jar := newCookieJar(filepath.Join(t.TempDir(), "cookie.txt"))
	if got := jar.headerValue(""); got != "" {
		t.Errorf("空状态下应返回空串，实际 %q", got)
	}
	if got := jar.headerValue(";;; bad ; =x; a=1"); !strings.Contains(got, "a=1") {
		t.Errorf("非法片段应被忽略，实际 %q", got)
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
	if !strings.Contains(got, hideCSS2) {
		t.Error("注入的样式内容不正确")
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
	if seenCookie == "" || !strings.Contains(seenCookie, "persisted") {
		t.Errorf("持久化的 Cookie 未被带上：%q", seenCookie)
	}
	if !strings.Contains(seenCookie, "browser=1") {
		t.Errorf("浏览器自带 Cookie 应一并带上：%q", seenCookie)
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
	proxy := makeProxy(target, hideCSS2, nil)

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))

	res := rr.Result()
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()

	if string(body) != gzBody {
		t.Error("压缩响应不应被改写（改写会破坏内容）")
	}
	if strings.Contains(string(body), "data-local-inject") {
		t.Error("压缩响应不应注入样式")
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
	proxy := makeProxy(target, hideCSS2, nil)

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
	// 指向一个必然连不上的地址
	target, _ := url.Parse("http://127.0.0.1:1")
	proxy := makeProxy(target, "", nil)

	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rr.Code != http.StatusBadGateway {
		t.Errorf("上游不可达应返回 502，实际 %d", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// 端到端：真起两个端口，走一遍代理链路
// ---------------------------------------------------------------------------

func TestStartServersEndToEnd(t *testing.T) {
	source1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><head></head><body>UPSTREAM-ONE "+r.URL.Path+"</body></html>")
	}))
	defer source1.Close()

	source2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html><head></head><body>UPSTREAM-TWO "+r.URL.Query().Get("q")+"</body></html>")
	}))
	defer source2.Close()

	cfg := config{
		Source1:    source1.URL,
		Source2:    source2.URL,
		PagePort:   0,
		Proxy1Port: 0,
		ListenHost: "127.0.0.1",
		CookieFile: filepath.Join(t.TempDir(), "cookie.txt"),
	}

	itemsHTML, err := readListItems(writeTempFile(t, "ABP-123\nSSIS-001\n"))
	if err != nil {
		t.Fatal(err)
	}

	pageURL, err := startServers(cfg, itemsHTML)
	if err != nil {
		t.Fatalf("startServers 失败：%v", err)
	}

	// 1) 面板页
	page := httpGet(t, pageURL+"/")
	for _, want := range []string{`id="kwInput"`, `id="searchBtn"`, `id="pager"`, `id="prevBtn"`,
		`id="nextBtn"`, `id="frame1"`, `id="frame2"`, `id="clearBtn"`, "ABP-123"} {
		if !strings.Contains(page, want) {
			t.Errorf("面板页缺少 %s", want)
		}
	}
	for _, ph := range []string{"{P1PORT}", "{SCRIPT}", "{ITEMS}"} {
		if strings.Contains(page, ph) {
			t.Errorf("面板页残留占位符 %s", ph)
		}
	}

	// 从面板页取出源1 代理端口
	m := regexp.MustCompile(`data-p1-port="(\d+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("面板页未注入源1 端口")
	}
	p1Port := m[1]

	// 2) 健康检查
	health := httpGet(t, pageURL+"/healthz")
	var payload struct {
		Status  string `json:"status"`
		Version string `json:"version"`
		Items   int    `json:"items"`
	}
	if err := json.Unmarshal([]byte(health), &payload); err != nil {
		t.Fatalf("/healthz 不是合法 JSON：%s (%v)", health, err)
	}
	if payload.Status != "ok" {
		t.Errorf("/healthz status 应为 ok，实际 %q", payload.Status)
	}
	if payload.Version == "" {
		t.Error("/healthz 应报告版本号")
	}
	if payload.Items != 2 {
		t.Errorf("/healthz items 应为 2，实际 %d", payload.Items)
	}

	// 3) 源2 代理（面板页同源路径）
	got2 := httpGet(t, pageURL+"/s?q=%E4%B8%AD%E6%96%87")
	if !strings.Contains(got2, "UPSTREAM-TWO") {
		t.Errorf("源2 代理未取到上游内容：%s", got2)
	}
	if !strings.Contains(got2, "中文") {
		t.Errorf("源2 代理未透传查询参数：%s", got2)
	}
	if !strings.Contains(got2, "data-local-inject") {
		t.Error("源2 未注入隐藏样式")
	}

	// 4) 源1 代理（独立端口）
	got1 := httpGet(t, "http://127.0.0.1:"+p1Port+"/files?q=abc")
	if !strings.Contains(got1, "UPSTREAM-ONE") {
		t.Errorf("源1 代理未取到上游内容：%s", got1)
	}
	if !strings.Contains(got1, "data-local-inject") {
		t.Error("源1 未注入隐藏样式")
	}
	if !strings.Contains(got1, hideCSS1) {
		t.Error("源1 注入的样式内容不正确")
	}

	// 5) 源1 的响应头应已剥离 X-Frame-Options
	resp, err := http.Get("http://127.0.0.1:" + p1Port + "/files?q=abc")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if v := resp.Header.Get("X-Frame-Options"); v != "" {
		t.Errorf("源1 代理未剥离 X-Frame-Options：%q", v)
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "list.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

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
