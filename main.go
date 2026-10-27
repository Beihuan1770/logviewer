package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed static
var staticFiles embed.FS

// 版本号：源码内定版；构建时可用 -ldflags "-X main.version=v1.0.1" 覆盖
// 页面页脚与 /api/version 都读这一个变量，改版本只动这一行
var version = "v1.0"

// ---------- 配置文件结构（YAML） ----------

type Defaults struct {
	User string `yaml:"user"`
	Port int    `yaml:"port"`
	Dir  string `yaml:"dir"`
}

type HostSpec struct {
	IP    string `yaml:"ip"`
	Label string `yaml:"label"`
	User  string `yaml:"user"`
	Port  int    `yaml:"port"`
	Pass  string `yaml:"pass"`
	Dir   string `yaml:"dir"`
}

type AppSpec struct {
	Label   string            `yaml:"label"`
	File    string            `yaml:"file"`
	Dir     string            `yaml:"dir"`
	Hosts   []string          `yaml:"hosts"`
	Files   map[string]string `yaml:"files"`
	Entries [][2]string       `yaml:"-"` // 块写法按书写顺序保留，允许同一主机出现多次
}

// 允许 `- 应用id: 显示名` 这种单行写法
func (a *AppSpec) UnmarshalYAML(v *yaml.Node) error {
	if v.Kind == yaml.ScalarNode {
		a.Label = strings.TrimSpace(v.Value)
		return nil
	}
	type alias AppSpec
	var raw alias
	if err := v.Decode(&raw); err != nil {
		return err
	}
	*a = AppSpec(raw)
	return nil
}

type rawConfig struct {
	Port         int                   `yaml:"port"`
	TailLines    int                   `yaml:"tail_lines"`
	AllowAnyPath bool                  `yaml:"allow_any_path"`
	PathRoots    []string              `yaml:"path_roots"`
	Defaults     Defaults              `yaml:"defaults"`
	Hosts        []map[string]HostSpec `yaml:"hosts"`
	Apps         yaml.Node             `yaml:"apps"`
}

// apps 支持两种写法：
//  1) 块写法（推荐，与运维手写习惯一致）：
//     apps: |
//     flightcontrol[
//     drone1:/data/projects/drone_logs/flightcontrol.log
//     drone2:/data/projects/drone_logs/flightcontrol.log
//     ]
//  2) 旧版 YAML 列表（- flightcontrol: 飞控），保留兼容
type dslApp struct {
	ID      string
	Label   string
	Entries [][2]string // host, path
}

func parseAppsDSL(text string) ([]dslApp, error) {
	var apps []dslApp
	var cur *dslApp
	lines := strings.Split(text, "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if cur == nil {
			if !strings.HasSuffix(line, "[") {
				return nil, fmt.Errorf("apps 第%d行：应用块必须以 [ 结尾，例如 flightcontrol[", i+1)
			}
			head := strings.TrimSpace(strings.TrimSuffix(line, "["))
			fields := strings.Fields(head)
			if len(fields) == 0 {
				return nil, fmt.Errorf("apps 第%d行：缺少应用名", i+1)
			}
			cur = &dslApp{ID: fields[0], Label: strings.Join(fields[1:], " ")}
			continue
		}
		if line == "]" {
			if len(cur.Entries) == 0 {
				return nil, fmt.Errorf("应用 %s 没有配置任何日志", cur.ID)
			}
			apps = append(apps, *cur)
			cur = nil
			continue
		}
		idx := strings.Index(line, ":")
		if idx <= 0 {
			return nil, fmt.Errorf("apps 第%d行：应为 主机:绝对路径，实际为 %s", i+1, line)
		}
		host := strings.TrimSpace(line[:idx])
		p := strings.TrimSpace(line[idx+1:])
		if !strings.HasPrefix(p, "/") {
			return nil, fmt.Errorf("apps 第%d行：路径必须是绝对路径（以 / 开头），实际为 %s", i+1, p)
		}
		cur.Entries = append(cur.Entries, [2]string{host, p})
	}
	if cur != nil {
		return nil, fmt.Errorf("应用 %s 的块没有以 ] 结束", cur.ID)
	}
	return apps, nil
}

// ---------- 运行时结构 ----------

type Host struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Addr  string `json:"addr"`
	Port  int    `json:"port"`
	User  string `json:"user"`
	Pass  string `json:"-"`
	Dir   string
}

type Source struct {
	HostID    string `json:"host_id"`
	HostLabel string `json:"host_label"`
	Addr      string `json:"addr"`
	Path      string `json:"path"`
}

type App struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Sources []Source `json:"sources"`
}

type Config struct {
	Port         int
	TailLines    int
	AllowAnyPath bool
	PathRoots    []string
	Hosts        []*Host
	Apps         []*App
	hostByID     map[string]*Host
	allowed      map[string][]string
}

// IsAllowed 判断某主机上的路径是否允许读取：
// 开启 allow_any_path 时任意绝对路径都放行；否则只允许配置里声明的主机目录/应用文件，以及 path_roots 下的路径
func (c *Config) IsAllowed(hostID, p string) bool {
	if !path.IsAbs(p) {
		return false
	}
	if _, ok := c.hostByID[hostID]; !ok {
		return false
	}
	if c.AllowAnyPath {
		return true
	}
	p = path.Clean(p)
	for _, root := range c.PathRoots {
		root = path.Clean(root)
		if root == "/" || p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	for _, pre := range c.allowed[hostID] {
		pre = path.Clean(pre)
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

func (c *Config) Host(id string) (*Host, bool) {
	h, ok := c.hostByID[id]
	return h, ok
}

func buildConfig(rc *rawConfig) (*Config, error) {
	cfg := &Config{Port: rc.Port, TailLines: rc.TailLines, AllowAnyPath: rc.AllowAnyPath, PathRoots: rc.PathRoots}
	if cfg.Port == 0 {
		cfg.Port = 23344
	}
	if cfg.TailLines <= 0 {
		cfg.TailLines = 1000
	}

	def := rc.Defaults
	if def.Port == 0 {
		def.Port = 22
	}
	if def.Dir == "" {
		def.Dir = "/data/projects/drone_logs"
	}
	if def.User == "" {
		def.User = "root"
	}

	seen := map[string]bool{}
	for _, item := range rc.Hosts {
		for id, h := range item {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if seen[id] {
				return nil, fmt.Errorf("主机昵称重复: %s", id)
			}
			seen[id] = true
			hh := &Host{ID: id, Label: h.Label, Addr: h.IP, Port: h.Port, User: h.User, Pass: h.Pass, Dir: h.Dir}
			if hh.Addr == "" {
				return nil, fmt.Errorf("主机 %s 缺少 ip", id)
			}
			if hh.Label == "" {
				hh.Label = id
			}
			if hh.Port == 0 {
				hh.Port = def.Port
			}
			if hh.User == "" {
				hh.User = def.User
			}
			if hh.Dir == "" {
				hh.Dir = def.Dir
			}
			cfg.Hosts = append(cfg.Hosts, hh)
		}
	}
	if len(cfg.Hosts) == 0 {
		return nil, fmt.Errorf("未配置任何主机")
	}

	cfg.hostByID = map[string]*Host{}
	for _, h := range cfg.Hosts {
		cfg.hostByID[h.ID] = h
	}

	var appSpecs []map[string]AppSpec
	switch rc.Apps.Kind {
	case 0:
		// 未配置 apps
	case yaml.ScalarNode:
		dsl, err := parseAppsDSL(rc.Apps.Value)
		if err != nil {
			return nil, err
		}
		for _, a := range dsl {
			appSpecs = append(appSpecs, map[string]AppSpec{a.ID: {Label: a.Label, Entries: a.Entries}})
		}
	case yaml.SequenceNode:
		if err := rc.Apps.Decode(&appSpecs); err != nil {
			return nil, fmt.Errorf("解析 apps 失败: %w", err)
		}
	default:
		return nil, fmt.Errorf("apps 配置格式不正确：应为块写法（应用名[ ... ]）")
	}

	appSeen := map[string]bool{}
	for _, item := range appSpecs {
		for id, a := range item {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if appSeen[id] {
				return nil, fmt.Errorf("应用 id 重复: %s", id)
			}
			appSeen[id] = true

			app := &App{ID: id, Label: a.Label}
			if app.Label == "" {
				app.Label = id
			}

			if len(a.Entries) > 0 {
				// 块写法：严格按书写顺序成栏，同一主机可以出现多次（如主日志 + error 日志）
				for _, e := range a.Entries {
					h, ok := cfg.hostByID[e[0]]
					if !ok {
						return nil, fmt.Errorf("应用 %s 引用了未知主机: %s", id, e[0])
					}
					app.Sources = append(app.Sources, Source{
						HostID:    e[0],
						HostLabel: h.Label,
						Addr:      h.Addr,
						Path:      e[1],
					})
				}
			} else {
				hosts := a.Hosts
				if len(hosts) == 0 {
					for _, h := range cfg.Hosts {
						hosts = append(hosts, h.ID)
					}
				}

				dir := a.Dir
				if dir == "" {
					dir = def.Dir
				}
				file := a.File
				if file == "" {
					file = id + ".log"
				}

				for _, hid := range hosts {
					h, ok := cfg.hostByID[hid]
					if !ok {
						return nil, fmt.Errorf("应用 %s 引用了未知主机: %s", id, hid)
					}
					p := ""
					if a.Files != nil {
						p = a.Files[hid]
					}
					if p == "" {
						p = path.Join(dir, file)
					}
					app.Sources = append(app.Sources, Source{
						HostID:    hid,
						HostLabel: h.Label,
						Addr:      h.Addr,
						Path:      p,
					})
				}
			}
			if len(app.Sources) > 0 {
				cfg.Apps = append(cfg.Apps, app)
			}
		}
	}

	cfg.allowed = map[string][]string{}
	for _, h := range cfg.Hosts {
		cfg.allowed[h.ID] = append(cfg.allowed[h.ID], h.Dir)
	}
	for _, a := range cfg.Apps {
		for _, s := range a.Sources {
			cfg.allowed[s.HostID] = append(cfg.allowed[s.HostID], s.Path)
		}
	}
	return cfg, nil
}

// ---------- 旧版 JSON 配置兼容 ----------

type legacyHost struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	LogDir   string `json:"log_dir"`
}

type legacyConfig struct {
	Server struct {
		Port      int `json:"port"`
		TailLines int `json:"tail_lines"`
	} `json:"server"`
	Hosts []legacyHost `json:"hosts"`
}

func loadLegacy(data []byte) (*Config, error) {
	var lc legacyConfig
	if err := json.Unmarshal(data, &lc); err != nil {
		return nil, err
	}
	rc := &rawConfig{
		Port:      lc.Server.Port,
		TailLines: lc.Server.TailLines,
	}
	for _, h := range lc.Hosts {
		id := "host-" + strings.ReplaceAll(h.Host, ".", "-")
		sp := HostSpec{IP: h.Host, Label: h.Name, Port: h.Port, User: h.User, Pass: h.Password, Dir: h.LogDir}
		rc.Hosts = append(rc.Hosts, map[string]HostSpec{id: sp})
	}
	return buildConfig(rc)
}

func loadConfig(p string) (*Config, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	if strings.EqualFold(filepath.Ext(p), ".json") {
		return loadLegacy(data)
	}
	var rc rawConfig
	if err := yaml.Unmarshal(data, &rc); err != nil {
		if cfg, lerr := loadLegacy(data); lerr == nil {
			return cfg, nil
		}
		return nil, fmt.Errorf("解析 YAML 失败: %w", err)
	}
	if len(rc.Hosts) == 0 {
		if cfg, lerr := loadLegacy(data); lerr == nil {
			return cfg, nil
		}
	}
	return buildConfig(&rc)
}

// ---------- HTTP ----------

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径（支持 .yaml / 旧版 .json）")
	checkConfig := flag.Bool("check", false, "只打印解析后的主机与应用清单，不启动服务")
	showVersion := flag.Bool("version", false, "只打印版本号后退出")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	if *checkConfig {
		out, _ := json.MarshalIndent(map[string]interface{}{
			"port":           cfg.Port,
			"tail_lines":     cfg.TailLines,
			"allow_any_path": cfg.AllowAnyPath,
			"path_roots":     cfg.PathRoots,
			"hosts":          cfg.Hosts,
			"apps":           cfg.Apps,
		}, "", "  ")
		fmt.Println(string(out))
		return
	}

	log.Printf("已加载 %d 台主机、%d 个应用", len(cfg.Hosts), len(cfg.Apps))

	pool := NewSSHPool(cfg.Hosts)
	hub := NewWSHub(pool, cfg)

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatalf("内置静态资源异常: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("/ws", hub.HandleWS)

	mux.HandleFunc("/api/hosts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		type hostInfo struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Host   string `json:"host"`
			LogDir string `json:"log_dir"`
		}
		infos := make([]hostInfo, 0, len(cfg.Hosts))
		for _, h := range cfg.Hosts {
			infos = append(infos, hostInfo{ID: h.ID, Name: h.Label, Host: h.Addr, LogDir: h.Dir})
		}
		json.NewEncoder(w).Encode(infos)
	})

	// 版本号：四个页面统一从这里取，避免 HTML 里硬编码
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]string{"version": version})
	})

	mux.HandleFunc("/api/apps", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if cfg.Apps == nil {
			cfg.Apps = []*App{}
		}
		json.NewEncoder(w).Encode(cfg.Apps)
	})

	mux.HandleFunc("/api/stat", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		host := r.URL.Query().Get("host")
		file := r.URL.Query().Get("path")
		if host == "" || file == "" {
			http.Error(w, "缺少 host 或 path 参数", http.StatusBadRequest)
			return
		}
		if _, ok := cfg.Host(host); !ok {
			http.Error(w, "未知主机", http.StatusBadRequest)
			return
		}
		if !cfg.IsAllowed(host, file) {
			http.Error(w, "路径不在允许范围内", http.StatusForbidden)
			return
		}
		st, err := pool.Stat(host, file)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"exists": false, "error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(st)
	})

	mux.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		host := r.URL.Query().Get("host")
		file := r.URL.Query().Get("file")
		if host == "" || file == "" {
			http.Error(w, "缺少 host 或 file 参数", http.StatusBadRequest)
			return
		}
		if decoded, err := url.QueryUnescape(file); err == nil {
			file = decoded
		}
		if _, ok := cfg.Host(host); !ok {
			http.Error(w, "未知主机", http.StatusBadRequest)
			return
		}
		if !cfg.IsAllowed(host, file) {
			http.Error(w, "路径不在允许范围内", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(file)))
		if err := pool.StreamFile(host, file, w); err != nil {
			log.Printf("下载文件失败 [%s] %s: %v", host, file, err)
		}
	})

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("=== 日志追踪系统 %s 已启动: http://0.0.0.0%s ===", version, addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
