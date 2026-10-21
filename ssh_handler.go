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

func (p *SSHPool) runCmd(hostID, cmd string) (string, error) {
	client, err := p.getClient(hostID)
	if err != nil {
		return "", err
	}
	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("创建SSH会话失败: %w", err)
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
	client, err := p.getClient(hostID)
	if err != nil {
		return err
	}

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("创建SSH会话失败: %w", err)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return err
	}

	escaped := "'" + strings.ReplaceAll(filePath, "'", "'\\''") + "'"
	// 初始内容由 ReadLastNLines 提供，这里从文件末尾开始跟，避免重复推送已有行
	cmd := fmt.Sprintf("tail -n 0 -F %s 2>&1", escaped)

	if err := session.Start(cmd); err != nil {
		session.Close()
		return fmt.Errorf("启动 tail 失败: %w", err)
	}

	go func() {
		<-ctx.Done()
		session.Close()
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

// ReadLinesSkip 跳过 skip 行后往前读 count 行
func (p *SSHPool) ReadLinesSkip(hostID, filePath string, skip int, count int) ([]string, error) {
	escaped := "'" + strings.ReplaceAll(filePath, "'", "'\\''") + "'"
	out, err := p.runCmd(hostID, fmt.Sprintf("tail -n %d %s 2>/dev/null | head -n %d", skip+count, escaped, count))
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
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
	client, err := p.getClient(hostID)
	if err != nil {
		return err
	}

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
