package main

import (
	"context"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

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
		// 向上翻页用字节偏移定位，避免每次重扫整个文件
		readEnd   int64
		readLines int
		readPath  string
		// 上一条 tail 何时真正结束，用于开新会话前等待，避免超过 sshd 并发会话上限
		tailDone chan struct{}
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
			// 等上一条 tail 会话真正关闭再开新会话，否则快速切换日志时会撞上 sshd 的并发会话上限
			if tailDone != nil {
				select {
				case <-tailDone:
				case <-time.After(2 * time.Second):
				}
			}

			lines, start, err := h.pool.ReadLastNLinesWithOffset(msg.Host, msg.File, h.cfg.TailLines)
			if err != nil {
				writeJSON(map[string]interface{}{"type": "error", "message": "读取初始化行失败: " + err.Error()})
				continue
			}
			readEnd, readLines, readPath = start, len(lines), msg.File

			writeJSON(map[string]interface{}{
				"type": "init_response",
				"data": map[string]interface{}{
					"lines":       lines,
					"total_lines": len(lines),
					"has_more":    start > 0,
					"end_offset":  start,
				},
			})

			id := atomic.AddInt64(&tailID, 1)
			atomic.StoreInt64(&activeTailID, id)

			ctx, cancel := context.WithCancel(context.Background())
			mu.Lock()
			cancelFunc = cancel
			mu.Unlock()

			lineCh := make(chan string, 200)

			done := make(chan struct{})
			tailDone = done

			go func() {
				defer close(done)
				defer close(lineCh)
				if err := h.pool.TailFile(ctx, msg.Host, msg.File, lineCh); err != nil {
					if ctx.Err() == nil {
						writeJSON(map[string]interface{}{"type": "error", "message": "tail 错误: " + err.Error()})
					}
				}
			}()

			go func() {
				// 100ms 一批推送，避免高频日志一行一条 JSON 造成消息风暴
				ticker := time.NewTicker(100 * time.Millisecond)
				defer ticker.Stop()
				batch := make([]string, 0, 256)
				flush := func() {
					if len(batch) == 0 {
						return
					}
					if err := writeJSON(map[string]interface{}{"type": "new_lines", "data": batch}); err != nil {
						cancel()
					}
					batch = batch[:0]
				}
				for {
					select {
					case line, ok := <-lineCh:
						if !ok {
							flush()
							if atomic.LoadInt64(&activeTailID) == id {
								writeJSON(map[string]interface{}{"type": "status", "status": "stopped", "tailId": id})
							}
							return
						}
						batch = append(batch, line)
						if len(batch) >= 500 {
							flush()
						}
					case <-ticker.C:
						flush()
					}
				}
			}()

			writeJSON(map[string]interface{}{
				"type": "status", "host": msg.Host, "file": msg.File, "status": "tailing", "tailId": id,
			})

		case "load_prev":
			limit := 200
			if msg.Params != nil && msg.Params.Limit > 0 {
				limit = msg.Params.Limit
			}
			if !h.cfg.IsAllowed(msg.Host, msg.File) {
				writeJSON(map[string]interface{}{"type": "error", "message": "路径不在允许范围内"})
				continue
			}
			// 换了文件（或重连后）就重新定位到文件末尾
			if msg.File != readPath {
				lines, start, err := h.pool.ReadLastNLinesWithOffset(msg.Host, msg.File, h.cfg.TailLines)
				if err != nil {
					writeJSON(map[string]interface{}{"type": "error", "message": "读取初始化行失败: " + err.Error()})
					continue
				}
				readEnd, readLines, readPath = start, len(lines), msg.File
			}
			if readEnd <= 0 {
				writeJSON(map[string]interface{}{
					"type": "prev_lines",
					"data": map[string]interface{}{"lines": []string{}, "total_lines": readLines, "has_more": false},
				})
				continue
			}

			lines, newEnd, more, err := h.pool.ReadLinesBefore(msg.Host, msg.File, readEnd, limit)
			if err != nil {
				writeJSON(map[string]interface{}{"type": "error", "message": "加载历史失败: " + err.Error()})
				continue
			}
			readEnd = newEnd
			readLines += len(lines)
			writeJSON(map[string]interface{}{
				"type": "prev_lines",
				"data": map[string]interface{}{
					"lines": lines, "total_lines": readLines, "has_more": more, "end_offset": newEnd,
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
