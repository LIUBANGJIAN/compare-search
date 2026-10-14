package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// settings 是「可以在页面上修改、并且需要持久化」的那部分配置。
// 部署参数（端口、监听地址、数据目录）不在这里 —— 那些只认环境变量。
type settings struct {
	Source1 string `json:"source1"`
	Source2 string `json:"source2"`
}

// settingsOrigin 说明当前地址是从哪里来的，用于在页面上如实告知用户。
type settingsOrigin string

const (
	originDefault settingsOrigin = "内置默认值"
	originEnv     settingsOrigin = "环境变量 / config.env"
	originFile    settingsOrigin = "配置文件（页面保存）"
)

// settingsStore 负责配置的读取、校验与原子落盘。
// 优先级：配置文件 > 环境变量 > 内置默认值。
// 也就是说环境变量只在「还没有配置文件」时作为初值，之后以页面保存的为准。
type settingsStore struct {
	mu   sync.RWMutex
	path string
	cfg  settings
	orig settingsOrigin
}

// newSettingsStore 读取磁盘上的配置文件；文件缺失时静默退回 fallback。
// 文件存在但内容不可用时返回错误，同时仍然可用（退回 fallback）。
func newSettingsStore(path string, fallback settings, fallbackOrigin settingsOrigin) (*settingsStore, error) {
	st := &settingsStore{path: path, cfg: fallback, orig: fallbackOrigin}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return st, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var loaded settings
	if err := json.Unmarshal(data, &loaded); err != nil {
		return st, fmt.Errorf("配置文件 %s 不是合法 JSON，已改用%s", path, fallbackOrigin)
	}
	s1, err1 := normalizeSourceURL(loaded.Source1)
	s2, err2 := normalizeSourceURL(loaded.Source2)
	if err1 != nil || err2 != nil {
		return st, fmt.Errorf("配置文件 %s 里的地址无效，已改用%s", path, fallbackOrigin)
	}

	st.cfg = settings{Source1: s1, Source2: s2}
	st.orig = originFile
	return st, nil
}

func (s *settingsStore) snapshot() (settings, settingsOrigin) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg, s.orig
}

func (s *settingsStore) pathOf() string { return s.path }

// save 校验并原子写盘，成功后才更新内存中的当前值。
func (s *settingsStore) save(next settings) error {
	s1, err := normalizeSourceURL(next.Source1)
	if err != nil {
		return fmt.Errorf("源1 地址无效：%w", err)
	}
	s2, err := normalizeSourceURL(next.Source2)
	if err != nil {
		return fmt.Errorf("源2 地址无效：%w", err)
	}
	normalized := settings{Source1: s1, Source2: s2}

	data, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := writeFileAtomic(s.path, data, 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败：%w", s.path, err)
	}

	s.mu.Lock()
	s.cfg = normalized
	s.orig = originFile
	s.mu.Unlock()
	return nil
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
// 配置 API
// ---------------------------------------------------------------------------

type configPayload struct {
	Source1    string `json:"source1"`
	Source2    string `json:"source2"`
	Origin     string `json:"origin"`
	ConfigFile string `json:"configFile"`
	Items      int    `json:"items"`
}

// makeConfigHandler 提供读取与修改上游地址的接口。
// 保存成功后立即热切换两个代理的转发目标，无需重启。
func makeConfigHandler(store *settingsStore, t1, t2 *proxyTarget, itemCount func() int) http.HandlerFunc {
	respond := func(w http.ResponseWriter, status int) {
		cur, orig := store.snapshot()
		writeJSON(w, status, configPayload{
			Source1:    cur.Source1,
			Source2:    cur.Source2,
			Origin:     string(orig),
			ConfigFile: store.pathOf(),
			Items:      itemCount(),
		})
	}

	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			respond(w, http.StatusOK)

		case http.MethodPost:
			// 防 CSRF：浏览器发起的跨站请求必带 Origin/Referer，对不上就拒绝。
			// 非浏览器客户端（curl 等）两者都不带，放行 —— 它们本来就不受同源策略约束。
			if !sameOrigin(r) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "跨站请求被拒绝"})
				return
			}

			body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
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
			if err := t1.set(cur.Source1); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "源1 地址切换失败：" + err.Error()})
				return
			}
			if err := t2.set(cur.Source2); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "源2 地址切换失败：" + err.Error()})
				return
			}
			respond(w, http.StatusOK)

		default:
			w.Header().Set("Allow", "GET, POST")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET 与 POST"})
		}
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
