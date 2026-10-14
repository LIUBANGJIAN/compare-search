// txt2html 把一个「每行一个搜索词」的 txt 变成双引擎并行检索面板：
// 面板里输入关键字（支持单个/多个），点击一次即向两个源同时发起检索，
// 多个关键字时用面板上的翻页按钮逐条查看。
//
// 两种用法：
//  1. 把 txt 拖到可执行文件图标上，或双击后把 txt 拖进窗口再回车；
//  2. 直接双击运行（不选 txt），只用页面上的输入框检索。
//
// 配置优先级：真实环境变量 > 同目录 config.env > 内置默认值。
// 部署到容器时全部通过环境变量注入，参见 loadConfig。
package main

import (
	"bufio"
	_ "embed"
	"fmt"
	"html"
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
	// 真实地址请用 SRC1_BASE / SRC2_BASE 环境变量或同目录 config.env 覆盖，
	// 这样仓库里不会留下任何内网拓扑（CI 的脱敏闸门会强制检查这一点）。
	defaultSource1 = "http://192.0.2.20:11510"
	defaultSource2 = "http://192.0.2.20:8787"

	defaultPagePort   = 18899
	defaultProxy1Port = 18898

	// 面板内隐藏站点外壳的 CSS，可按需增删选择器
	hideCSS1 = `header,aside,footer{display:none!important}[data-sidebar="sidebar"],[data-slot="sidebar"],[data-slot="sidebar-gap"],[data-sidebar="footer"],[data-slot="sidebar-footer"]{display:none!important}` // 源1：隐藏顶栏、侧边栏、底部统计条
	hideCSS2 = "header.topbar,#filter-form,.stats,#batchbar{display:none!important}"                                                                                                                              // 源2：隐藏顶栏、筛选条、统计行、批量操作栏
)

// config 汇总全部可调参数，全部可通过环境变量覆盖。
type config struct {
	Source1    string // 源1 基地址
	Source2    string // 源2 基地址
	PagePort   int    // 面板页 + 源2 代理端口
	Proxy1Port int    // 源1 代理端口
	ListenHost string // 监听地址：本机用 127.0.0.1，容器内用 0.0.0.0
	CookieFile string // 源1 登录 Cookie 持久化文件
}

func main() {
	loadEnvFiles()
	cfg := loadConfig()

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

	itemsHTML := ""
	if inputPath != "" {
		if _, statErr := os.Stat(inputPath); os.IsNotExist(statErr) {
			fmt.Printf("\n错误：找不到文件 '%s'\n", inputPath)
			pauseAndExit()
			return
		}
		content, readErr := readListItems(inputPath)
		if readErr != nil {
			fmt.Printf("\n读取文件失败: %v\n", readErr)
			pauseAndExit()
			return
		}
		itemsHTML = content
	}

	pageURL, err := startServers(cfg, itemsHTML)
	if err != nil {
		fmt.Printf("\n启动本地服务失败: %v\n", err)
		pauseAndExit()
		return
	}

	fmt.Println("\n========================================")
	fmt.Printf("  本地服务已启动：%s\n", pageURL)
	fmt.Printf("  源1 代理端口：%d    源2 经面板页同源转发\n", cfg.Proxy1Port)
	fmt.Println("  浏览器将自动打开，保持本窗口开启即可使用")
	fmt.Println("  按 Enter 键停止服务并退出")
	fmt.Println("========================================")

	if runtime.GOOS == "windows" && os.Getenv("NO_BROWSER") == "" {
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", pageURL).Start()
	}

	waitForExit()
}

// printBanner 打印交互模式下的标题。
func printBanner() {
	fmt.Println("========================================")
	fmt.Println("  TXT 转双源检索工具")
	fmt.Println("========================================")
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

func loadConfig() config {
	c := config{
		Source1:    defaultSource1,
		Source2:    defaultSource2,
		PagePort:   defaultPagePort,
		Proxy1Port: defaultProxy1Port,
		ListenHost: "127.0.0.1",
	}
	c.Source1 = envOr("SRC1_BASE", c.Source1)
	c.Source2 = envOr("SRC2_BASE", c.Source2)
	c.PagePort = envPortOr("PAGE_PORT", c.PagePort)
	c.Proxy1Port = envPortOr("PROXY1_PORT", c.Proxy1Port)
	c.ListenHost = envOr("LISTEN_HOST", c.ListenHost)

	c.CookieFile = strings.TrimSpace(os.Getenv("COOKIE_FILE"))
	if c.CookieFile == "" {
		c.CookieFile = "cookie_115.txt"
		if exePath, err := os.Executable(); err == nil {
			c.CookieFile = filepath.Join(filepath.Dir(exePath), "cookie_115.txt")
		}
	}
	return c
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

// readListItems 读取 txt（每行一个搜索词），生成右侧列表的 HTML 片段。
func readListItems(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	var b strings.Builder
	scanner := bufio.NewScanner(file)
	// 放宽单行长度上限，避免超长行直接报错
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	index := 0
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		encoded := url.QueryEscape(text)
		b.WriteString(fmt.Sprintf(
			`<a href="javascript:void(0)" class="data-line" data-id="item-%d" data-q1="/files?q=%s" data-q2="/s?q=%s">%s</a>`+"\n",
			index, encoded, encoded, html.EscapeString(text),
		))
		index++
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if index == 0 {
		return `<div class="list-empty">未从 txt 中读到任何搜索词，可直接使用上方的输入框检索。</div>` + "\n", nil
	}
	return b.String(), nil
}

// ---------------------------------------------------------------------------
// 服务
// ---------------------------------------------------------------------------

// startServers 启动两个本地服务：
//   - 端口 cfg.Proxy1Port：全部反向代理到源1（剥安全头 + 注入隐藏 CSS + 绝对地址改写）
//   - 端口 cfg.PagePort ："/" 返回检索面板页，"/healthz" 健康检查，
//     其余路径反向代理到源2（同样剥头 + 注入 CSS）
func startServers(cfg config, itemsHTML string) (string, error) {
	t1, err := url.Parse(cfg.Source1)
	if err != nil {
		return "", fmt.Errorf("源1 地址不合法: %w", err)
	}
	t2, err := url.Parse(cfg.Source2)
	if err != nil {
		return "", fmt.Errorf("源2 地址不合法: %w", err)
	}

	jar := newCookieJar(cfg.CookieFile)

	// 源1 代理服务
	ln1, err := listenOn(cfg.ListenHost, cfg.Proxy1Port)
	if err != nil {
		return "", fmt.Errorf("监听源1代理端口失败: %w", err)
	}
	mux1 := http.NewServeMux()
	mux1.Handle("/", makeProxy(t1, hideCSS1, jar))
	go func() { _ = http.Serve(ln1, mux1) }()

	// 源2 代理 + 面板页服务
	ln2, err := listenOn(cfg.ListenHost, cfg.PagePort)
	if err != nil {
		_ = ln1.Close()
		return "", fmt.Errorf("监听面板端口失败: %w", err)
	}
	page := buildPage(itemsHTML, portOf(ln1))
	proxy2 := makeProxy(t2, hideCSS2, nil)

	itemsCount := strings.Count(itemsHTML, `class="data-line"`)
	mux2 := http.NewServeMux()
	mux2.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = fmt.Fprintf(w, `{"status":"ok","version":%q,"items":%d,"source1":%q,"source2":%q}`,
			version, itemsCount, cfg.Source1, cfg.Source2)
	})
	mux2.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, page)
			return
		}
		proxy2.ServeHTTP(w, r)
	})
	go func() { _ = http.Serve(ln2, mux2) }()

	host := displayHost(cfg.ListenHost)
	return fmt.Sprintf("http://%s:%d", host, portOf(ln2)), nil
}

// buildPage 把面板模板里的三个占位符填实。
// 顺序有意为之：{ITEMS} 最后替换，用户文本里若含 {SCRIPT}/{P1PORT} 也不会被二次解析。
func buildPage(itemsHTML string, p1Port int) string {
	page := strings.ReplaceAll(panelHTMLRaw, "{P1PORT}", strconv.Itoa(p1Port))
	page = strings.ReplaceAll(page, "{SCRIPT}", panelJSRaw)
	page = strings.ReplaceAll(page, "{ITEMS}", itemsHTML)
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

// makeProxy 创建反向代理：剥掉禁止 iframe 内嵌的安全头；
// 对 HTML/JS/CSS 响应做正文改写（绝对地址转相对），并向 HTML 注入隐藏站点外壳的 CSS；
// jar 非 nil 时自动携带/记录该源的登录 Cookie。
func makeProxy(target *url.URL, hideCSS string, jar *cookieJar) *httputil.ReverseProxy {
	origin := target.Scheme + "://" + target.Host
	// 上游地址可能出现三种形式，按「长模式优先」替换，避免短模式先命中导致长的残留：
	//   http://host（原样）  http:\/\/host（JS 字符串）  http:\\/\\/host（JSON 里再转义一层）
	jsonEscaped := strings.ReplaceAll(origin, "/", `\\/`)
	jsEscaped := strings.ReplaceAll(origin, "/", `\/`)

	proxy := httputil.NewSingleHostReverseProxy(target)
	// 关闭传输层的自动解压：我们要拿到后端的原始字节自行判断，而不是被透明解压后
	// 丢掉 Content-Encoding 头、再对已解压内容做字符串改写。
	proxy.Transport = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}

	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = target.Host
		// 让后端看到的 Origin/Referer 与其真实主机一致，避免源站跨域校验拦截（如下载接口）
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		// 显式声明 identity，让后端返回未压缩内容以便正文改写。
		// 注意：这里不能只是 Del —— net/http 的 Transport 在请求头缺失 Accept-Encoding
		// 时会自动补回 "gzip"，上游仍可能返回压缩体。
		req.Header.Set("Accept-Encoding", "identity")
		// 自动携带持久化的登录 Cookie
		if jar != nil {
			if cv := jar.headerValue(req.Header.Get("Cookie")); cv != "" {
				req.Header.Set("Cookie", cv)
			}
		}
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
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

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		s := string(body)
		s = strings.ReplaceAll(s, jsonEscaped, "")
		s = strings.ReplaceAll(s, jsEscaped, "")
		s = strings.ReplaceAll(s, origin, "")
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
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "代理请求失败: "+err.Error(), http.StatusBadGateway)
	}
	return proxy
}
