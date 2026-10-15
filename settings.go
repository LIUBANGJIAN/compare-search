package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// maxEngines 是支持的上游引擎数量上限。
// 每个引擎占用一个独立的代理端口（ProxyPortBase - index）：
// 这样上游页面里的「根相对路径」（/api/x 这种）不需要做任何改写就能正常工作，
// 避开了「把根相对路径补上前缀」这类改写漏项导致页面半坏的风险。
const maxEngines = 8

// engine 是一个可检索的上游站点。
type engine struct {
	Name    string `json:"name"`              // 显示名，同时作为结果面板标题
	Base    string `json:"base"`              // 上游基地址，如 http://nas.local:8787
	Path    string `json:"path"`              // 搜索路径，如 /s；查询串由程序追加
	HideCSS string `json:"hideCss,omitempty"` // 可选：隐藏该站点自身外壳的 CSS
}

// settings 是「可以在页面上修改、并且需要持久化」的那部分配置。
// 部署参数（端口、监听地址、数据目录）不在这里 —— 那些只认环境变量。
type settings struct {
	Engines []engine `json:"engines"`
}

// settingsOrigin 说明当前配置是从哪里来的，用于在页面上如实告知用户。
type settingsOrigin string

const (
	originDefault settingsOrigin = "内置默认值"
	originEnv     settingsOrigin = "环境变量 / config.env"
	originFile    settingsOrigin = "配置文件（页面保存）"
)

// settingsStore 负责配置的读取、校验与原子落盘。
// 优先级：配置文件 > 环境变量 > 内置默认值。
type settingsStore struct {
	mu   sync.RWMutex
	path string
	cfg  settings
	orig settingsOrigin
}

// newSettingsStore 读取磁盘上的配置文件。
// 文件不存在时**立即把当前生效的配置写一份**（这样用户在数据目录里能直接看到它），
// 内容不可用时给出错误提示并退回 fallback。
func newSettingsStore(path string, fallback settings, fallbackOrigin settingsOrigin) (*settingsStore, error) {
	st := &settingsStore{path: path, cfg: fallback, orig: fallbackOrigin}

	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return st, fmt.Errorf("读取配置文件失败: %w", err)
		}
		// 首次运行：把当前生效的配置落一份盘，这样用户在数据目录里能直接看到它。
		// fallback 本身不合法（调用方没给可用引擎）时跳过写盘，不算错误。
		if _, verr := normalizeEngines(fallback.Engines); verr != nil {
			return st, nil
		}
		if werr := st.save(fallback); werr != nil {
			return st, fmt.Errorf("配置文件不存在，且无法自动创建：%w", werr)
		}
		// 首次生成的内容来自环境变量/默认值，来源照实说
		st.mu.Lock()
		st.orig = fallbackOrigin
		st.mu.Unlock()
		return st, nil
	}

	// 兼容早期「只有两个源」的格式
	var probe struct {
		Engines []engine `json:"engines"`
		Source1 string   `json:"source1"`
		Source2 string   `json:"source2"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return st, fmt.Errorf("配置文件 %s 不是合法 JSON，已改用%s", path, fallbackOrigin)
	}
	loaded := probe.Engines
	if len(loaded) == 0 && (probe.Source1 != "" || probe.Source2 != "") {
		loaded = legacyEngines(probe.Source1, probe.Source2)
	}

	normalized, err := normalizeEngines(loaded)
	if err != nil {
		return st, fmt.Errorf("配置文件 %s 内容无效（%v），已改用%s", path, err, fallbackOrigin)
	}

	st.cfg = settings{Engines: normalized}
	st.orig = originFile
	return st, nil
}

// legacyEngines 把旧格式的两个地址转成引擎列表。
func legacyEngines(s1, s2 string) []engine {
	var out []engine
	if strings.TrimSpace(s1) != "" {
		out = append(out, engine{Name: "搜索结果1", Base: s1, Path: "/files", HideCSS: hideCSS1})
	}
	if strings.TrimSpace(s2) != "" {
		out = append(out, engine{Name: "搜索结果2", Base: s2, Path: "/s", HideCSS: hideCSS2})
	}
	return out
}

func (s *settingsStore) snapshot() (settings, settingsOrigin) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg, s.orig
}

func (s *settingsStore) pathOf() string { return s.path }

// save 校验并原子写盘，成功后才更新内存中的当前值。
func (s *settingsStore) save(next settings) error {
	normalized, err := normalizeEngines(next.Engines)
	if err != nil {
		return err
	}
	out := settings{Engines: normalized}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := writeFileAtomic(s.path, data, 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败：%w", s.path, err)
	}

	s.mu.Lock()
	s.cfg = out
	s.orig = originFile
	s.mu.Unlock()
	return nil
}

// normalizeEngines 校验并归一化引擎列表。
func normalizeEngines(list []engine) ([]engine, error) {
	if len(list) == 0 {
		return nil, errors.New("至少要有一个搜索引擎")
	}
	if len(list) > maxEngines {
		return nil, fmt.Errorf("最多支持 %d 个搜索引擎", maxEngines)
	}
	out := make([]engine, 0, len(list))
	for i, e := range list {
		n, err := normalizeEngine(e, i)
		if err != nil {
			return nil, fmt.Errorf("第 %d 个引擎：%w", i+1, err)
		}
		out = append(out, n)
	}
	return out, nil
}

func normalizeEngine(e engine, index int) (engine, error) {
	name := strings.TrimSpace(e.Name)
	if name == "" {
		name = fmt.Sprintf("搜索引擎 %d", index+1)
	}
	if n := len([]rune(name)); n > 40 {
		return engine{}, fmt.Errorf("名称过长（%d 字，上限 40）", n)
	}

	base, err := normalizeSourceURL(e.Base)
	if err != nil {
		return engine{}, fmt.Errorf("地址无效：%w", err)
	}

	path := strings.TrimSpace(e.Path)
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// 允许用户直接粘贴 "/search?q=" 这种带查询串的写法，查询串由程序统一追加
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if path == "" {
		path = "/"
	}

	return engine{
		Name:    name,
		Base:    base,
		Path:    path,
		HideCSS: strings.TrimSpace(e.HideCSS),
	}, nil
}

// normalizeSourceURL 校验并归一化上游地址。
func normalizeSourceURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("地址不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("地址格式无法解析")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("地址必须以 http:// 或 https:// 开头")
	}
	if u.Host == "" {
		return "", errors.New("地址缺少主机名（例如 http://nas.local:8787）")
	}
	return strings.TrimRight(raw, "/"), nil
}

// writeFileAtomic 先写同目录临时文件再 rename，避免写入中断把配置改坏。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// ---------------------------------------------------------------------------
// 运行时引擎注册表
// ---------------------------------------------------------------------------

// engineRuntime 是某个引擎在运行时的形态：配置 + 实际监听端口。
type engineRuntime struct {
	eng  engine
	port int
}

// engineRegistry 保存当前生效的引擎列表，供各代理端口按索引查询。
// 端口池在启动时一次性建好并记住（与引擎列表分开存），
// 这样页面上增删引擎时端口分配是稳定的，也不会出现「新引擎没有端口」。
type engineRegistry struct {
	mu    sync.RWMutex
	ports []int // 索引 → 实际监听端口
	items []engineRuntime
}

// setPorts 记录启动时建好的端口池，必须在 apply 之前调用。
func (r *engineRegistry) setPorts(ports []int) {
	r.mu.Lock()
	r.ports = append([]int(nil), ports...)
	r.mu.Unlock()
}

func (r *engineRegistry) at(index int) (engineRuntime, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if index < 0 || index >= len(r.items) {
		return engineRuntime{}, false
	}
	return r.items[index], true
}

func (r *engineRegistry) snapshot() []engineRuntime {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]engineRuntime, len(r.items))
	copy(out, r.items)
	return out
}

// apply 用新配置重建列表，端口从启动时确定的端口池里取。
func (r *engineRegistry) apply(list []engine) error {
	items := make([]engineRuntime, 0, len(list))
	for i, e := range list {
		if _, err := url.Parse(e.Base); err != nil {
			return fmt.Errorf("第 %d 个引擎地址无法解析：%w", i+1, err)
		}
		port := 0
		if i < len(r.ports) {
			port = r.ports[i]
		}
		if port == 0 {
			return fmt.Errorf("第 %d 个引擎没有可用的代理端口（端口池只有 %d 个）", i+1, len(r.ports))
		}
		items = append(items, engineRuntime{eng: e, port: port})
	}
	r.mu.Lock()
	r.items = items
	r.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------------------
// 配置 API
// ---------------------------------------------------------------------------

type enginePayload struct {
	Name      string `json:"name"`
	Base      string `json:"base"`
	Path      string `json:"path"`
	HideCSS   string `json:"hideCss,omitempty"`
	ProxyPort int    `json:"proxyPort"`
}

type configPayload struct {
	Engines       []enginePayload `json:"engines"`
	Origin        string          `json:"origin"`
	ConfigFile    string          `json:"configFile"`
	Items         int             `json:"items"`
	MaxEngines    int             `json:"maxEngines"`
	ProxyPortBase int             `json:"proxyPortBase"`
}

func buildConfigPayload(store *settingsStore, reg *engineRegistry, dep deployment, itemCount int) configPayload {
	cur, orig := store.snapshot()
	runtimes := reg.snapshot()

	byName := runtimes
	list := make([]enginePayload, 0, len(cur.Engines))
	for i, e := range cur.Engines {
		port := 0
		if i < len(byName) {
			port = byName[i].port
		}
		list = append(list, enginePayload{
			Name: e.Name, Base: e.Base, Path: e.Path, HideCSS: e.HideCSS, ProxyPort: port,
		})
	}
	return configPayload{
		Engines:       list,
		Origin:        string(orig),
		ConfigFile:    store.pathOf(),
		Items:         itemCount,
		MaxEngines:    maxEngines,
		ProxyPortBase: dep.ProxyPortBase,
	}
}

// makeConfigHandler 提供读取与修改搜索引擎的接口。保存成功后立即生效，无需重启。
func makeConfigHandler(store *settingsStore, reg *engineRegistry, dep deployment, itemCount func() int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, buildConfigPayload(store, reg, dep, itemCount()))

		case http.MethodPost:
			// 防 CSRF：浏览器发起的跨站请求必带 Origin/Referer，对不上就拒绝。
			// 非浏览器客户端（curl 等）两者都不带，放行 —— 它们本来就不受同源策略约束。
			if !sameOrigin(r) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "跨站请求被拒绝"})
				return
			}

			body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取请求体失败"})
				return
			}
			var req settings
			if err := json.Unmarshal(body, &req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
				return
			}
			if err := store.save(req); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}

			cur, _ := store.snapshot()
			if err := reg.apply(cur.Engines); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, buildConfigPayload(store, reg, dep, itemCount()))

		default:
			w.Header().Set("Allow", "GET, POST")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET 与 POST"})
		}
	}
}

// ---------------------------------------------------------------------------
// /api/search：给其他程序调用的关键字检索入口
// ---------------------------------------------------------------------------

type searchHit struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type searchPayload struct {
	Keyword   string      `json:"keyword"`
	PageURL   string      `json:"pageUrl"`
	Engines   []searchHit `json:"engines"`
	ItemCount int         `json:"itemCount"`
}

// makeSearchAPI 返回 JSON：把关键字换成各引擎的检索地址，方便脚本/浏览器书签直接调用。
// 返回的 URL 用请求自带的 Host 拼装，因此从局域网其它机器调用时拿到的也是可用地址。
func makeSearchAPI(store *settingsStore, reg *engineRegistry, itemCount func() int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
			return
		}
		keyword := strings.TrimSpace(r.URL.Query().Get("q"))
		if keyword == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少查询参数 q"})
			return
		}

		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}

		hits := make([]searchHit, 0, len(reg.snapshot()))
		for _, rt := range reg.snapshot() {
			if rt.port == 0 {
				continue
			}
			hits = append(hits, searchHit{
				Name: rt.eng.Name,
				URL: "http://" + net.JoinHostPort(host, fmt.Sprint(rt.port)) + rt.eng.Path +
					"?q=" + url.QueryEscape(keyword),
			})
		}

		writeJSON(w, http.StatusOK, searchPayload{
			Keyword:   keyword,
			PageURL:   "http://" + r.Host + "/?q=" + url.QueryEscape(keyword),
			Engines:   hits,
			ItemCount: itemCount(),
		})
	}
}

// sameOrigin 判断请求是否来自本站。
// Origin 缺失或为 null（隐私上下文 / 沙箱 iframe）时退回 Referer；两者都没有则放行。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
