package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type SSHPool struct {
	mu      sync.Mutex
	clients map[string]*ssh.Client
	hosts   map[string]*Host
}

type DirEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	RelPath string `json:"rel_path"`
	Type    string `json:"type"` // "directory" or "file"
	Size    int64  `json:"size"`
	Mtime   string `json:"mtime"`
	MtimeTs int64  `json:"mtime_ts"`
}

func NewSSHPool(hosts []*Host) *SSHPool {
	hostMap := make(map[string]*Host, len(hosts))
	for _, h := range hosts {
		hostMap[h.ID] = h
	}
	return &SSHPool{
		clients: make(map[string]*ssh.Client),
		hosts:   hostMap,
	}
}

// dialClient 新建一条独立的 SSH 连接（不复用连接池）
// 实时追踪单独用一条连接，停止时直接关闭连接即可释放全部通道
func (p *SSHPool) dialClient(hostID string) (*ssh.Client, error) {
	cfg, ok := p.hosts[hostID]
	if !ok {
		return nil, fmt.Errorf("未知主机: %s", hostID)
	}
	sshCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.Password(cfg.Pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	addr := net.JoinHostPort(cfg.Addr, strconv.Itoa(cfg.Port))
	client, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("SSH连接 %s(%s) 失败: %w", cfg.Label, cfg.Addr, err)
	}
	return client, nil
}

func (p *SSHPool) getClient(hostID string) (*ssh.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if client, ok := p.clients[hostID]; ok {
		if _, _, err := client.Conn.SendRequest("keepalive@openssh.com", true, nil); err == nil {
			return client, nil
		}
		client.Close()
		delete(p.clients, hostID)
	}

	cfg, ok := p.hosts[hostID]
	if !ok {
		return nil, fmt.Errorf("未知主机: %s", hostID)
	}

	client, err := p.dialClient(hostID)
	if err != nil {
		return nil, err
	}

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			p.mu.Lock()
			_, alive := p.clients[hostID]
			p.mu.Unlock()
			if !alive {
				return
			}
			if _, _, err := client.Conn.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				client.Close()
				return
			}
		}
	}()

	p.clients[hostID] = client
	log.Printf("SSH 已连接: %s (%s)", cfg.Label, cfg.Addr)
	return client, nil
}

// newSession 打开一个 SSH 会话。sshd 对单连接并发会话数有上限，
// 快速切换日志时可能瞬时超限（administratively prohibited），这里做几次短重试
func (p *SSHPool) newSession(hostID string) (*ssh.Session, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		client, err := p.getClient(hostID)
		if err != nil {
			return nil, err
		}
		session, err := client.NewSession()
		if err == nil {
			return session, nil
		}
		lastErr = err
		log.Printf("SSH 会话创建失败（%s 第%d次）: %v", hostID, attempt+1, err)
		time.Sleep(time.Duration(120*(attempt+1)) * time.Millisecond)
	}
	return nil, fmt.Errorf("创建SSH会话失败: %w", lastErr)
}

func (p *SSHPool) runCmd(hostID, cmd string) (string, error) {
	session, err := p.newSession(hostID)
	if err != nil {
		return "", err
	}
	defer session.Close()

	var stdout, stderr strings.Builder
	session.Stdout = &stdout
	session.Stderr = &stderr
	if err := session.Run(cmd); err != nil {
		return "", fmt.Errorf("%s: %s", err.Error(), stderr.String())
	}
	return stdout.String(), nil
}

// Stat 查询远端路径信息（用于「路径追踪」判断文件是否存在、多大）
type StatResult struct {
	Exists bool   `json:"exists"`
	Size   int64  `json:"size"`
	Mtime  int64  `json:"mtime"`
	Kind   string `json:"kind"`
}

func (p *SSHPool) Stat(hostID, filePath string) (StatResult, error) {
	escaped := "'" + strings.ReplaceAll(filePath, "'", "'\\''") + "'"
	out, err := p.runCmd(hostID, fmt.Sprintf("stat -c '%%s|%%Y|%%F' %s 2>/dev/null", escaped))
	if err != nil {
		return StatResult{}, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return StatResult{Exists: false}, nil
	}
	parts := strings.SplitN(out, "|", 3)
	if len(parts) < 3 {
		return StatResult{Exists: false}, nil
	}
	size, _ := strconv.ParseInt(parts[0], 10, 64)
	mtime, _ := strconv.ParseInt(parts[1], 10, 64)
	return StatResult{Exists: true, Size: size, Mtime: mtime, Kind: parts[2]}, nil
}

// ListDir 列出主机日志根目录下指定相对路径的内容
func (p *SSHPool) ListDir(hostID, relPath string) ([]DirEntry, error) {
	cfg, ok := p.hosts[hostID]
	if !ok {
		return nil, fmt.Errorf("未知主机: %s", hostID)
	}

	baseDir := path.Clean(cfg.Dir)
	absPath := path.Clean(path.Join(baseDir, relPath))
	if absPath != baseDir && !strings.HasPrefix(absPath, baseDir+"/") {
		absPath = baseDir
	}

	escaped := "'" + strings.ReplaceAll(absPath, "'", "'\\''") + "'"
	cmd := fmt.Sprintf(
		"find -L %s -maxdepth 1 -mindepth 1 -printf '%%y|%%T@|%%s|%%f\\n' 2>/dev/null | sort -t'|' -k4",
		escaped,
	)

	out, err := p.runCmd(hostID, cmd)
	if err != nil {
		return []DirEntry{}, nil
	}

	var entries []DirEntry
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 4 {
			continue
		}
		typeChar := parts[0]
		name := parts[3]

		entryType := "file"
		switch typeChar {
		case "d":
			entryType = "directory"
		case "l":
			entryType = "symlink"
		}

		tsFloat, _ := strconv.ParseFloat(parts[1], 64)
		ts := int64(tsFloat)
		size, _ := strconv.ParseInt(parts[2], 10, 64)
		if entryType != "file" {
			size = 0
		}

		abs := path.Join(absPath, name)
		rel := path.Join(relPath, name)

		mtime := ""
		if ts > 0 {
			mtime = time.Unix(ts, 0).Format("2006-01-02 15:04:05")
		}

		entries = append(entries, DirEntry{
			Name: name, Path: abs, RelPath: rel, Type: entryType,
			Size: size, Mtime: mtime, MtimeTs: ts,
		})
	}
	if entries == nil {
		entries = []DirEntry{}
	}
	return entries, nil
}

// TailFile 实时追踪日志文件
func (p *SSHPool) TailFile(ctx context.Context, hostID, filePath string, lineCh chan<- string) error {
	// 实时追踪用一条独立 SSH 连接：停止时直接关闭连接，
	// 由 SSH 库回收全部通道，不依赖远端 tail 进程是否退出
	client, err := p.dialClient(hostID)
	if err != nil {
		return err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("创建SSH会话失败: %w", err)
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}

	escaped := "'" + strings.ReplaceAll(filePath, "'", "'\\''") + "'"
	// 初始内容由 ReadLastNLinesWithOffset 提供，这里从文件末尾开始跟，避免重复推送已有行
	cmd := fmt.Sprintf("tail -n 0 -F %s 2>&1", escaped)

	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("启动 tail 失败: %w", err)
	}

	go func() {
		<-ctx.Done()
		client.Close()
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case lineCh <- scanner.Text():
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("tail 读取中断: %w", err)
	}
	return nil
}

// ReadLastNLines 读取文件末尾 n 行
func (p *SSHPool) ReadLastNLines(hostID, filePath string, n int) ([]string, error) {
	escaped := "'" + strings.ReplaceAll(filePath, "'", "'\\''") + "'"
	out, err := p.runCmd(hostID, fmt.Sprintf("tail -n %d %s 2>/dev/null", n, escaped))
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// ReadLastNLinesWithOffset 读取文件末尾 n 行，并返回这些行的起始字节偏移
// 返回的偏移用于后续按字节窗口向前翻页，避免每翻一页都重扫整个文件
func (p *SSHPool) ReadLastNLinesWithOffset(hostID, filePath string, n int) ([]string, int64, error) {
	escaped := "'" + strings.ReplaceAll(filePath, "'", "'\\''") + "'"
	// 分隔标记不能含 #（shell 注释符），且要足够独特，避免与日志内容混淆
	const sep = "__LOGVIEWER_SIZE__"
	cmd := fmt.Sprintf("stat -c %%s %s 2>/dev/null || echo 0; echo %s; tail -n %d %s 2>/dev/null",
		escaped, sep, n, escaped)
	out, err := p.runCmd(hostID, cmd)
	if err != nil {
		return nil, 0, err
	}
	idx := strings.Index(out, sep)
	if idx < 0 {
		return nil, 0, fmt.Errorf("解析 tail 输出失败")
	}
	size, _ := strconv.ParseInt(strings.TrimSpace(out[:idx]), 10, 64)
	tailText := strings.TrimPrefix(out[idx+len(sep):], "\n")
	start := size - int64(len(tailText))
	if start < 0 {
		start = 0
	}
	return splitLines(tailText), start, nil
}

// readByteRange 读取文件 [start, end) 字节区间
func (p *SSHPool) readByteRange(hostID, filePath string, start, end int64) (string, error) {
	if end <= start {
		return "", nil
	}
	escaped := "'" + strings.ReplaceAll(filePath, "'", "'\\''") + "'"
	return p.runCmd(hostID, fmt.Sprintf("tail -c +%d %s 2>/dev/null | head -c %d", start+1, escaped, end-start))
}

// ReadLinesBefore 从 endOffset 往前读取最多 count 行完整日志。
// 返回读取到的行、最早一行的起始偏移、以及是否还有更早内容。
// 每次只读一个固定大小的字节窗口（不够再翻倍），不再重扫整个文件。
func (p *SSHPool) ReadLinesBefore(hostID, filePath string, endOffset int64, count int) ([]string, int64, bool, error) {
	if endOffset <= 0 || count <= 0 {
		return []string{}, 0, false, nil
	}
	win := int64(64 * 1024)
	const maxWin = 16 << 20
	for {
		start := endOffset - win
		if start < 0 {
			start = 0
		}
		text, err := p.readByteRange(hostID, filePath, start, endOffset)
		if err != nil {
			return nil, 0, false, err
		}
		base := start
		body := text
		if start > 0 {
			// 窗口开头是不完整行，丢掉它，从下一个换行之后开始
			if i := strings.IndexByte(body, '\n'); i >= 0 {
				body = body[i+1:]
				base = start + int64(i) + 1
			} else {
				body = ""
			}
		}
		var parts []string
		if body != "" {
			body = strings.TrimSuffix(body, "\n")
			if body != "" {
				parts = strings.Split(body, "\n")
			}
		}
		if len(parts) < count && start > 0 && win < maxWin {
			win *= 2
			continue
		}
		if len(parts) == 0 {
			newEnd := base
			if start == 0 {
				newEnd = 0
			}
			return []string{}, newEnd, newEnd > 0, nil
		}
		from := 0
		if len(parts) > count {
			from = len(parts) - count
		}
		offset := base
		starts := make([]int64, len(parts))
		for i, line := range parts {
			starts[i] = offset
			offset += int64(len(line)) + 1
		}
		return parts[from:], starts[from], starts[from] > 0, nil
	}
}

func splitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// ReadFile 读取文件内容（tailBytes>0 时只取末尾）
func (p *SSHPool) ReadFile(hostID, filePath string, tailBytes int64) (string, error) {
	escaped := "'" + strings.ReplaceAll(filePath, "'", "'\\''") + "'"
	var cmd string
	if tailBytes > 0 {
		cmd = fmt.Sprintf("tail -c %d %s 2>&1", tailBytes, escaped)
	} else {
		cmd = fmt.Sprintf("cat %s 2>&1", escaped)
	}
	return p.runCmd(hostID, cmd)
}

// StreamFile 将远程文件内容流式写入 writer（用于 HTTP 下载）
func (p *SSHPool) StreamFile(hostID, filePath string, w io.Writer) error {
	session, err := p.newSession(hostID)
	if err != nil {
		return err
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}

	escaped := "'" + strings.ReplaceAll(filePath, "'", "'\\''") + "'"
	cmd := fmt.Sprintf("cat %s 2>/dev/null", escaped)
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("启动 cat 失败: %w", err)
	}

	buf := make([]byte, 64*1024)
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				session.Close()
				return writeErr
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	return session.Wait()
}
