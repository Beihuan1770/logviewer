package main

import (
	"context"
	"log"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type wsIncoming struct {
	Action  string    `json:"action,omitempty"`
	Type    string    `json:"type,omitempty"`
	Host    string    `json:"host,omitempty"`
	File    string    `json:"file,omitempty"`
	RelPath string    `json:"rel_path,omitempty"`
	Params  *wsParams `json:"params,omitempty"`
}

type wsParams struct {
	SkipLines int `json:"skip_lines"`
	Limit     int `json:"limit"`
}

type WSHub struct {
	pool *SSHPool
	cfg  *Config
}

func NewWSHub(pool *SSHPool, cfg *Config) *WSHub {
	return &WSHub{pool: pool, cfg: cfg}
}

func (h *WSHub) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket 升级失败: %v", err)
		return
	}
	defer conn.Close()

	var (
		cancelFunc   context.CancelFunc
		mu           sync.Mutex
		tailID       int64
		activeTailID int64
	)

	writeJSON := func(v interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		return conn.WriteJSON(v)
	}

	stopTail := func() {
		mu.Lock()
		defer mu.Unlock()
		if cancelFunc != nil {
			cancelFunc()
			cancelFunc = nil
		}
	}
	defer stopTail()

	for {
		var msg wsIncoming
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}

		if msg.Action != "" {
			if _, ok := h.cfg.Host(msg.Host); !ok {
				writeJSON(map[string]interface{}{"type": "error", "message": "未知主机: " + msg.Host})
				continue
			}
			switch msg.Action {
			case "list_dir":
				entries, err := h.pool.ListDir(msg.Host, msg.RelPath)
				if err != nil {
					writeJSON(map[string]interface{}{"type": "error", "message": "列出目录失败: " + err.Error()})
					continue
				}
				writeJSON(map[string]interface{}{
					"type": "dir_entries", "host": msg.Host, "relPath": msg.RelPath, "data": entries,
				})

			case "read_file":
				if !h.cfg.IsAllowed(msg.Host, msg.File) {
					writeJSON(map[string]interface{}{"type": "error", "message": "路径不在允许范围内"})
					continue
				}
				content, err := h.pool.ReadFile(msg.Host, msg.File, 1048576)
				if err != nil {
					writeJSON(map[string]interface{}{"type": "error", "message": "读取文件失败: " + err.Error()})
					continue
				}
				writeJSON(map[string]interface{}{
					"type": "file_content", "host": msg.Host, "file": msg.File, "content": content,
				})
			}
			continue
		}

		switch msg.Type {
		case "init":
			if _, ok := h.cfg.Host(msg.Host); !ok {
				writeJSON(map[string]interface{}{"type": "error", "message": "未知主机: " + msg.Host})
				continue
			}
			if !h.cfg.IsAllowed(msg.Host, msg.File) {
				writeJSON(map[string]interface{}{"type": "error", "message": "路径不在允许范围内: " + msg.File})
				continue
			}
			stopTail()

			lines, err := h.pool.ReadLastNLines(msg.Host, msg.File, h.cfg.TailLines)
			if err != nil {
				writeJSON(map[string]interface{}{"type": "error", "message": "读取初始化行失败: " + err.Error()})
				continue
			}

			writeJSON(map[string]interface{}{
				"type": "init_response",
				"data": map[string]interface{}{
					"lines":       lines,
					"total_lines": len(lines),
					"has_more":    len(lines) >= h.cfg.TailLines,
				},
			})

			id := atomic.AddInt64(&tailID, 1)
			atomic.StoreInt64(&activeTailID, id)

			ctx, cancel := context.WithCancel(context.Background())
			mu.Lock()
			cancelFunc = cancel
			mu.Unlock()

			lineCh := make(chan string, 200)

			go func() {
				defer close(lineCh)
				if err := h.pool.TailFile(ctx, msg.Host, msg.File, lineCh); err != nil {
					if ctx.Err() == nil {
						writeJSON(map[string]interface{}{"type": "error", "message": "tail 错误: " + err.Error()})
					}
				}
			}()

			go func() {
				for line := range lineCh {
					if err := writeJSON(map[string]interface{}{"type": "new_line", "data": line}); err != nil {
						cancel()
						return
					}
				}
				if atomic.LoadInt64(&activeTailID) == id {
					writeJSON(map[string]interface{}{"type": "status", "status": "stopped", "tailId": id})
				}
			}()

			writeJSON(map[string]interface{}{
				"type": "status", "host": msg.Host, "file": msg.File, "status": "tailing", "tailId": id,
			})

		case "load_prev":
			skipLines := 0
			limit := 200
			if msg.Params != nil {
				skipLines = msg.Params.SkipLines
				if msg.Params.Limit > 0 {
					limit = msg.Params.Limit
				}
			}
			if skipLines <= 0 {
				writeJSON(map[string]interface{}{
					"type": "prev_lines",
					"data": map[string]interface{}{"lines": []string{}, "total_lines": skipLines, "has_more": false},
				})
				continue
			}
			if !h.cfg.IsAllowed(msg.Host, msg.File) {
				writeJSON(map[string]interface{}{"type": "error", "message": "路径不在允许范围内"})
				continue
			}

			lines, err := h.pool.ReadLinesSkip(msg.Host, msg.File, skipLines, limit)
			if err != nil {
				writeJSON(map[string]interface{}{"type": "error", "message": "加载历史失败: " + err.Error()})
				continue
			}
			writeJSON(map[string]interface{}{
				"type": "prev_lines",
				"data": map[string]interface{}{
					"lines": lines, "total_lines": skipLines + len(lines), "has_more": len(lines) >= limit,
				},
			})

		case "stop":
			stopTail()
			writeJSON(map[string]interface{}{
				"type": "status", "status": "stopped", "tailId": atomic.LoadInt64(&activeTailID),
			})
		}
	}
}
