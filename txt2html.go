// txt2html 把一个「每行一个番号」的 txt 变成双引擎并行检索面板：
// 面板输入框里一行填一个番号（可一次填多个），点一次检索即向两个源同时发起查询；
// 多个番号会列在右侧番号栏里，点击任意一条即可查看该条的搜索结果，也可用翻页按钮顺序切换。
//
// 两种用法：
//  1. 把 txt 拖到可执行文件图标上，或双击后把 txt 拖进窗口再回车；
//  2. 直接双击运行（不选 txt），只用页面上的输入框检索。
//
// 配置分两类：
//   - 部署参数（端口、监听地址、数据目录）只认环境变量；
//   - 上游地址可在页面上修改并持久化到配置文件
//     （优先级：配置文件 > 环境变量/config.env > 内置默认值）。
package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/panel.html
var panelHTMLRaw string

//go:embed web/panel.js
var panelJSRaw string

// version 由构建时注入：-ldflags "-X main.version=<tag>"，默认 dev。
var version = "dev"

const (
	// 默认上游地址用 RFC 5737 文档专用地址（192.0.2.0/24），只作占位。
	// 真实地址请在页面上填写，或用 SRC1_BASE / SRC2_BASE 环境变量 / config.env 提供。
	defaultSource1 = "http://192.0.2.20:11510"
	defaultSource2 = "http://192.0.2.20:8787"

	defaultPagePort   = 18899
	defaultProxy1Port = 18898

	// 面板内隐藏站点外壳的 CSS，可按需增删选择器
	hideCSS1 = `header,aside,footer{display:none!important}[data-sidebar="sidebar"],[data-slot="sidebar"],[data-slot="sidebar-gap"],[data-sidebar="footer"],[data-slot="sidebar-footer"]{display:none!important}` // 源1：隐藏顶栏、侧边栏、底部统计条
	hideCSS2 = "header.topbar,#filter-form,.stats,#batchbar{display:none!important}"                                                                                                                              // 源2：隐藏顶栏、筛选条、统计行、批量操作栏
)

// deployment 是只能通过部署参数指定的部分（页面上改不了）。
type deployment struct {
	PagePort   int
	Proxy1Port int
	ListenHost string
	CookieFile string
	ConfigFile string
}

func main() {
	loadEnvFiles()
	dep := loadDeployment()

	fallback, fallbackOrigin := sourceDefaults()
	store, err := newSettingsStore(dep.ConfigFile, fallback, fallbackOrigin)
	if err != nil {
		fmt.Printf("警告：%v\n", err)
	}
	cur, _ := store.snapshot()

	var inputPath string
	switch {
	case len(os.Args) > 1:
		inputPath = os.Args[1]
	case isStdinTTY() && os.Getenv("NO_PROMPT") == "":
		printBanner()
		fmt.Print("\n请将 txt 文件拖入此窗口中，然后按回车键（直接回车可跳过，改用页面上的输入框检索）：")
		reader := bufio.NewReader(os.Stdin)
		input, err := reader.ReadString('\n')
		if err != nil && input == "" {
			// stdin 立刻 EOF（容器、后台运行、重定向自 /dev/null）：
			// 没有可读输入，继续以空列表启动服务，而不是报错退出。
			fmt.Println()
		} else {
			inputPath = input
		}
	default:
		printBanner()
	}

	inputPath = strings.Trim(strings.TrimSpace(inputPath), `"'`)

	var items []string
	if inputPath != "" {
		if _, statErr := os.Stat(inputPath); os.IsNotExist(statErr) {
			fmt.Printf("\n错误：找不到文件 '%s'\n", inputPath)
			pauseAndExit()
			return
		}
		items, err = readList(inputPath)
		if err != nil {
			fmt.Printf("\n读取文件失败: %v\n", err)
			pauseAndExit()
			return
		}
	}

	pageURL, err := startServers(dep, store, items)
	if err != nil {
		fmt.Printf("\n启动本地服务失败: %v\n", err)
		pauseAndExit()
		return
	}

	fmt.Println("\n========================================")
	fmt.Printf("  本地服务已启动：%s\n", pageURL)
	fmt.Printf("  源1 代理端口：%d    源2 经面板页同源转发\n", dep.Proxy1Port)
	fmt.Printf("  源1 地址：%s\n", cur.Source1)
	fmt.Printf("  源2 地址：%s\n", cur.Source2)
	fmt.Printf("  配置文件：%s\n", store.pathOf())
	fmt.Println("  上游地址可在页面「设置」里修改，保存后立即生效并持久化")
	fmt.Println("  按 Enter 键停止服务并退出")
	fmt.Println("========================================")

	if runtime.GOOS == "windows" && os.Getenv("NO_BROWSER") == "" {
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", pageURL).Start()
	}

	waitForExit()
}

func printBanner() {
	fmt.Println("========================================")
	fmt.Println("  TXT 转双源检索工具")
	fmt.Println("========================================")
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

func loadDeployment() deployment {
	return deployment{
		PagePort:   envPortOr("PAGE_PORT", defaultPagePort),
		Proxy1Port: envPortOr("PROXY1_PORT", defaultProxy1Port),
		ListenHost: envOr("LISTEN_HOST", "127.0.0.1"),
		CookieFile: defaultDataFile("COOKIE_FILE", "cookie_115.txt"),
		ConfigFile: defaultDataFile("CONFIG_FILE", "settings.json"),
	}
}

// defaultDataFile 解析数据文件路径：环境变量优先，否则放在可执行文件同目录。
func defaultDataFile(envKey, name string) string {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	if exePath, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exePath), name)
	}
	return name
}

// sourceDefaults 返回上游地址的初值，以及它来自环境变量还是内置默认值。
func sourceDefaults() (settings, settingsOrigin) {
	s1 := strings.TrimSpace(os.Getenv("SRC1_BASE"))
	s2 := strings.TrimSpace(os.Getenv("SRC2_BASE"))
	if s1 == "" && s2 == "" {
		return settings{Source1: defaultSource1, Source2: defaultSource2}, originDefault
	}
	if s1 == "" {
		s1 = defaultSource1
	}
	if s2 == "" {
		s2 = defaultSource2
	}
	return settings{Source1: strings.TrimRight(s1, "/"), Source2: strings.TrimRight(s2, "/")}, originEnv
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envPortOr(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 65535 {
		fmt.Printf("警告：%s=%q 不是合法端口，改用默认值 %d\n", key, v, fallback)
		return fallback
	}
	return n
}

// loadEnvFiles 依次读取「可执行文件同目录」与「当前工作目录」下的 config.env，
// 把 KEY=VALUE 写进进程环境，作为环境变量的补充。
// 注意：真实环境变量优先级更高 —— 已经存在的键不会被文件覆盖。
func loadEnvFiles() {
	seen := map[string]bool{}
	for _, p := range []string{envFilePath(), "config.env"} {
		abs, err := filepath.Abs(p)
		if err != nil {
			abs = p
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		applyEnvFile(p)
	}
}

func envFilePath() string {
	exePath, err := os.Executable()
	if err != nil {
		return "config.env"
	}
	return filepath.Join(filepath.Dir(exePath), "config.env")
}

func applyEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	loaded := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.Trim(strings.TrimSpace(kv[1]), `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, val); err == nil {
			loaded++
		}
	}
	if loaded > 0 {
		fmt.Printf("已从 %s 载入 %d 项配置\n", path, loaded)
	}
}

// isStdinTTY 判断标准输入是否连着一个可交互的字符设备。
// 这是个启发式判断：/dev/null 与 Windows 的 NUL 设备同样是字符设备，
// 所以容器/后台场景下可能误判为 true —— 因此调用方还必须能容忍
// 「提示已打印但读不到任何输入」，见 main 中对 EOF 的处理，以及 NO_PROMPT 开关。
func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// ---------------------------------------------------------------------------
// 输入解析
// ---------------------------------------------------------------------------

// readList 读取 txt（一行一个番号），返回去掉空白后的非空行。
func readList(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var items []string
	scanner := bufio.NewScanner(file)
	// 放宽单行长度上限，避免超长行直接报错
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		if text := strings.TrimSpace(scanner.Text()); text != "" {
			items = append(items, text)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// 服务
// ---------------------------------------------------------------------------

// pageData 是注入面板页的引导数据：番号列表与当前上游地址都从这里下发，
// 前端不再依赖服务端渲染的列表 HTML。
type pageData struct {
	Items   []string `json:"items"`
	Source1 string   `json:"source1"`
	Source2 string   `json:"source2"`
	Origin  string   `json:"origin"`
	Version string   `json:"version"`
	P1Port  int      `json:"p1Port"`
}

// startServers 启动两个本地服务：
//   - 端口 dep.Proxy1Port：全部反向代理到源1（剥安全头 + 注入隐藏 CSS + 绝对地址改写）
//   - 端口 dep.PagePort ："/" 面板页，"/healthz" 健康检查，"/api/config" 配置接口，
//     其余路径反向代理到源2（同样剥头 + 注入 CSS）
func startServers(dep deployment, store *settingsStore, items []string) (string, error) {
	cur, _ := store.snapshot()

	t1, err := newProxyTarget(cur.Source1)
	if err != nil {
		return "", fmt.Errorf("源1 地址不合法: %w", err)
	}
	t2, err := newProxyTarget(cur.Source2)
	if err != nil {
		return "", fmt.Errorf("源2 地址不合法: %w", err)
	}

	jar := newCookieJar(dep.CookieFile)

	// 源1 代理服务
	ln1, err := listenOn(dep.ListenHost, dep.Proxy1Port)
	if err != nil {
		return "", fmt.Errorf("监听源1代理端口失败: %w", err)
	}
	mux1 := http.NewServeMux()
	mux1.Handle("/", makeProxy(t1.get, hideCSS1, jar))
	go func() { _ = http.Serve(ln1, mux1) }()

	// 源2 代理 + 面板页服务
	ln2, err := listenOn(dep.ListenHost, dep.PagePort)
	if err != nil {
		_ = ln1.Close()
		return "", fmt.Errorf("监听面板端口失败: %w", err)
	}

	p1Port := portOf(ln1)
	proxy2 := makeProxy(t2.get, hideCSS2, nil)
	itemCount := func() int { return len(items) }

	mux2 := http.NewServeMux()
	mux2.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		s, orig := store.snapshot()
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"version": version,
			"items":   len(items),
			"source1": s.Source1,
			"source2": s.Source2,
			"origin":  string(orig),
		})
	})
	mux2.HandleFunc("/api/config", makeConfigHandler(store, t1, t2, itemCount))
	mux2.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			s, orig := store.snapshot()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = io.WriteString(w, buildPage(pageData{
				Items:   items,
				Source1: s.Source1,
				Source2: s.Source2,
				Origin:  string(orig),
				Version: version,
				P1Port:  p1Port,
			}))
			return
		}
		proxy2.ServeHTTP(w, r)
	})
	go func() { _ = http.Serve(ln2, mux2) }()

	host := displayHost(dep.ListenHost)
	return fmt.Sprintf("http://%s:%d", host, portOf(ln2)), nil
}

// buildPage 把面板模板里的占位符填实。
// 顺序有意为之：{BOOTSTRAP} 最后替换，用户数据里若含其它占位符也不会被二次解析。
func buildPage(data pageData) string {
	raw, err := json.Marshal(data)
	if err != nil {
		raw = []byte(`{}`)
	}
	page := strings.ReplaceAll(panelHTMLRaw, "{P1PORT}", strconv.Itoa(data.P1Port))
	page = strings.ReplaceAll(page, "{SCRIPT}", panelJSRaw)
	page = strings.ReplaceAll(page, "{BOOTSTRAP}", string(raw))
	return page
}

func portOf(ln net.Listener) int {
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// displayHost 把通配监听地址换成浏览器可直接访问的回环地址。
func displayHost(host string) string {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	}
	return host
}

// listenOn 优先监听指定端口；端口被占用时退化为随机端口，保证服务仍可用。
func listenOn(host string, preferred int) (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(preferred)))
	if err == nil {
		return ln, nil
	}
	fallback, ferr := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if ferr != nil {
		return nil, err
	}
	fmt.Printf("警告：端口 %d 不可用（%v），已改用 %d\n", preferred, err, portOf(fallback))
	return fallback, nil
}

// waitForExit 常驻等待退出：交互式场景等用户按 Enter，容器场景等 SIGINT/SIGTERM。
// stdin 不可用时不会再误判为「用户已按回车」而秒退。
func waitForExit() {
	stdinCh := make(chan struct{})
	go func() {
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err == nil {
			close(stdinCh)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case <-stdinCh:
	case <-sigCh:
		fmt.Println("\n收到退出信号，服务已停止")
	}
}

func pauseAndExit() {
	fmt.Println("\n按 Enter 键退出...")
	if !isStdinTTY() {
		return
	}
	bufReader := bufio.NewReader(os.Stdin)
	_, _ = bufReader.ReadString('\n')
}

// ---------------------------------------------------------------------------
// Cookie
// ---------------------------------------------------------------------------

// cookieJar 保存某个源的登录 Cookie 并持久化到本地文件。
// 面板走本地代理后与浏览器里原站的登录态互相隔离（cookie 按域隔离），
// 有了它：面板里登录一次，代理记住会话，之后不再需要登录。
type cookieJar struct {
	mu   sync.Mutex
	jar  map[string]string
	path string
}

func newCookieJar(path string) *cookieJar {
	j := &cookieJar{jar: map[string]string{}, path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return j
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || !strings.Contains(line, "=") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		j.jar[kv[0]] = kv[1]
	}
	return j
}

// headerValue 合并浏览器自带 Cookie 与本地存储的 Cookie（本地存储优先）。
func (j *cookieJar) headerValue(browser string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	merged := map[string]string{}
	if browser != "" {
		for _, part := range strings.Split(browser, ";") {
			part = strings.TrimSpace(part)
			if part == "" || !strings.Contains(part, "=") {
				continue
			}
			kv := strings.SplitN(part, "=", 2)
			merged[kv[0]] = kv[1]
		}
	}
	for k, v := range j.jar {
		merged[k] = v
	}
	pairs := make([]string, 0, len(merged))
	for k, v := range merged {
		pairs = append(pairs, k+"="+v)
	}
	return strings.Join(pairs, "; ")
}

// absorb 吸收响应的 Set-Cookie，更新并持久化到文件。
func (j *cookieJar) absorb(cookies []*http.Cookie) {
	if len(cookies) == 0 {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range cookies {
		// 服务端要求删除的 Cookie（Max-Age<0 或 Expires 已过期）
		if c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(time.Now())) {
			delete(j.jar, c.Name)
			continue
		}
		j.jar[c.Name] = c.Value
	}
	var b strings.Builder
	for k, v := range j.jar {
		b.WriteString(k + "=" + v + "\n")
	}
	_ = os.WriteFile(j.path, []byte(b.String()), 0600)
}

// ---------------------------------------------------------------------------
// 反向代理
// ---------------------------------------------------------------------------

// proxyTarget 允许在运行时热切换上游地址，配置保存后无需重启即可生效。
type proxyTarget struct {
	mu sync.RWMutex
	u  *url.URL
}

func newProxyTarget(raw string) (*proxyTarget, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	return &proxyTarget{u: u}, nil
}

func (p *proxyTarget) set(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.u = u
	p.mu.Unlock()
	return nil
}

func (p *proxyTarget) get() *url.URL {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.u
}

// rewriter 缓存某个上游地址的三种书写形式，正文改写时按「长模式优先」替换：
//   - 原样：        http://host
//   - JS 字符串：   http:\/\/host
//   - JSON 再转义： http:\\/\\/host
type rewriter struct {
	origin  string
	jsonEsc string
	jsEsc   string
}

type rewriterKey struct{}

func newRewriter(target *url.URL) *rewriter {
	origin := target.Scheme + "://" + target.Host
	return &rewriter{
		origin:  origin,
		jsonEsc: strings.ReplaceAll(origin, "/", `\\/`),
		jsEsc:   strings.ReplaceAll(origin, "/", `\/`),
	}
}

// makeProxy 创建反向代理：剥掉禁止 iframe 内嵌的安全头；
// 对 HTML/JS/CSS 响应做正文改写（绝对地址转相对），并向 HTML 注入隐藏站点外壳的 CSS；
// jar 非 nil 时自动携带/记录该源的登录 Cookie。
// 目标地址每次请求都从 getTarget 现取，因此支持热切换。
func makeProxy(getTarget func() *url.URL, hideCSS string, jar *cookieJar) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		// 关闭传输层的自动解压：我们要拿到后端的原始字节自行判断，
		// 而不是被透明解压后丢掉 Content-Encoding 头、再对已解压内容做字符串改写。
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableCompression:    true,
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			target := getTarget()
			rw := newRewriter(target)

			pr.SetURL(target)
			// SetURL 会把 Host 清空，这里恢复成上游主机名（源站常据此做校验）
			pr.Out.Host = target.Host
			pr.SetXForwarded()

			// 让后端看到的 Origin/Referer 与其真实主机一致，避免源站跨域校验拦截（如下载接口）
			pr.Out.Header.Set("Origin", rw.origin)
			pr.Out.Header.Set("Referer", rw.origin+"/")
			// 显式声明 identity，让后端返回未压缩内容以便正文改写。
			// 注意：这里不能只是 Del —— net/http 的 Transport 在请求头缺失 Accept-Encoding
			// 时会自动补回 "gzip"，上游仍可能返回压缩体。
			pr.Out.Header.Set("Accept-Encoding", "identity")

			// 自动携带持久化的登录 Cookie
			if jar != nil {
				if cv := jar.headerValue(pr.Out.Header.Get("Cookie")); cv != "" {
					pr.Out.Header.Set("Cookie", cv)
				}
			}

			// 把本次使用的改写器传给 ModifyResponse
			pr.Out = pr.Out.WithContext(context.WithValue(pr.Out.Context(), rewriterKey{}, rw))
		},
		ModifyResponse: func(resp *http.Response) error {
			// 记录该源的登录 Cookie（登录一次后持久化，无需重复登录）
			if jar != nil {
				jar.absorb(resp.Cookies())
			}
			h := resp.Header
			h.Del("X-Frame-Options")
			h.Del("Content-Security-Policy")
			h.Del("Content-Security-Policy-Report-Only")

			// 上游若仍返回压缩体（中间层强制压缩等），字符串改写会把内容改坏，直接原样放行
			if ce := h.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
				return nil
			}

			ct := h.Get("Content-Type")
			rewriteBody := strings.Contains(ct, "text/html") ||
				strings.Contains(ct, "javascript") ||
				strings.Contains(ct, "text/css")
			inject := strings.Contains(ct, "text/html") && hideCSS != ""
			if !rewriteBody && !inject {
				return nil
			}
			if resp.Request != nil && resp.Request.Method == http.MethodHead {
				return nil
			}

			rw, _ := resp.Request.Context().Value(rewriterKey{}).(*rewriter)
			if rw == nil {
				return nil
			}

			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return err
			}
			s := string(body)
			s = strings.ReplaceAll(s, rw.jsonEsc, "")
			s = strings.ReplaceAll(s, rw.jsEsc, "")
			s = strings.ReplaceAll(s, rw.origin, "")
			if inject {
				style := "<style data-local-inject>" + hideCSS + "</style>"
				if i := strings.Index(s, "</head>"); i >= 0 {
					s = s[:i] + style + s[i:]
				} else {
					s = style + s
				}
			}
			h.Del("Content-Length")
			h.Del("Content-Range")
			resp.Body = io.NopCloser(strings.NewReader(s))
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "代理请求失败: "+err.Error(), http.StatusBadGateway)
		},
	}
}
