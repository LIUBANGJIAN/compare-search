package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
)

// draft 是「面板上正在编辑的关键字」。
//
// 存在服务端而不是浏览器里：换浏览器、清了缓存、开隐私窗口、甚至换台设备都还在，
// 而且**只有用户点「清空」才会变空** —— 这正是需求要的语义。
type draft struct {
	Text  string `json:"text"`
	Index int    `json:"index"`
	// Src 记录番号来源（由 txt / 引导数据决定）。来源变了旧草稿就作废，
	// 避免换了 txt 文件之后，新内容被旧草稿悄悄盖掉。
	Src string `json:"src,omitempty"`
}

// 草稿是纯文本，给个上限防止有人往里灌大文件。
const maxDraftBytes = 256 << 10

// draftStore 负责草稿的读取与原子落盘。
type draftStore struct {
	mu   sync.Mutex
	path string
	cur  draft
}

func newDraftStore(path string) *draftStore {
	s := &draftStore{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var d draft
	if err := json.Unmarshal(data, &d); err != nil {
		// 文件坏了就当空的，不影响服务启动
		return s
	}
	s.cur = d
	return s
}

func (s *draftStore) pathOf() string { return s.path }

func (s *draftStore) snapshot() draft {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// save 校验并原子写盘，成功后才更新内存值。
func (s *draftStore) save(d draft) error {
	if len(d.Text) > maxDraftBytes {
		return fmt.Errorf("内容过长（%d 字节，上限 %d）", len(d.Text), maxDraftBytes)
	}
	if d.Index < -1 {
		d.Index = -1
	}

	// 空草稿直接落一个空对象，别留一堆空白字符
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败：%w", s.path, err)
	}

	s.mu.Lock()
	s.cur = d
	s.mu.Unlock()
	return nil
}

// makeDraftHandler 让页面把自己的输入存到服务端。
// 前端只在内容真的变化时才发（有防抖），所以这里不用再限流。
func makeDraftHandler(store *draftStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, store.snapshot())

		case http.MethodPost:
			// 与配置接口一致：拒绝浏览器发起的跨站写入
			if !sameOrigin(r) {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "跨站请求被拒绝"})
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, maxDraftBytes+1024))
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "读取请求体失败"})
				return
			}
			var d draft
			if err := json.Unmarshal(body, &d); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON"})
				return
			}
			if err := store.save(d); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "index": store.snapshot().Index})

		default:
			w.Header().Set("Allow", "GET, POST")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET 与 POST"})
		}
	}
}
