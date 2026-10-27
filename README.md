# logviewer · 日志追踪系统

一个用 SSH 直连服务器的轻量日志查看工具。浏览器打开一个页面就能实时追踪多台主机上的日志——不装 agent、不做日志收集、日志不落盘，部署只需要一个可执行文件加一份配置文件。

> 面向内网使用：**没有登录鉴权**，主机密码以明文写在 `config.yaml` 里。不要把它暴露到公网。

## 适用场景

- 内网几十台机器，看日志不想一台台 `ssh` + `tail`
- 同一个服务跑在多台机器上，需要并排对照着看
- 临时想看某个日志文件，不想为此部署 ELK / Loki
- 日志有几百 MB，只想看尾部，需要时再往上翻

## 工作原理

```
浏览器  ── WebSocket ──  logviewer(Go)  ── SSH ──  目标主机
                                                tail -F / find / stat / cat
```

- 后端为每台主机维持一个 SSH 连接（30 秒 keepalive），每次读日志再开独立会话
- 首次打开用 `tail -n N` 一次性取回末尾内容，之后用 `tail -n 0 -F` 只推新增行，不会重复
- 每行经 WebSocket 推给浏览器；前端按 150ms 批量渲染，并限制 DOM 节点数和内存行数
- 四个页面用 `go:embed` 内嵌进二进制，所以部署只有一个可执行文件

## 功能

- **三种看法**
  - *主机日志*：按主机浏览日志目录，点文件即可实时追踪
  - *应用日志*：按 `apps` 配置把同一应用在多台主机上的日志并排分栏
  - *路径追踪*：输入任意绝对路径即开即看，最近访问会记住
- 实时追踪：暂停 / 继续、全屏、下载整份文件
- 向上翻历史：滚动到顶部自动加载更早 200 行，视线位置不跳
- 内容过滤：关键字过滤并高亮命中（应用页的过滤对所有分栏同时生效）
- 日志等级着色：错误红、警告黄、成功绿、其余白
- 分栏数量由配置行数决定：一行一栏，单机单日志就是一栏铺满；同一主机可以写多行
- 目录浏览：文件名搜索、按修改时间筛选、多选批量下载
- 路径白名单：默认允许任意绝对路径，也可以关闭并限定目录
- 配置自检：`-check` 打印解析结果，启动前先核对

## 快速开始

运行目标机**不需要 Go，也不需要编译**——拿到 `logviewer` 二进制就能跑。

### 1. 准备二进制

任选一种来源：

- 直接从已有的部署目录拷贝 `logviewer`；
- 从本仓库 Releases 下载（若该版本附带了二进制资产）；
- 自己编译（只有改了代码才需要，见下方「从源码构建」）。

把二进制和脚本放进同一个目录，并赋予执行权限：

```bash
cd /data/apps && unzip logviewer_linux_amd64.zip
mv logviewer_linux_amd64 logviewer
cd logviewer
chmod +x logviewer start.sh stop.sh restart.sh
```

### 2. 配置

```bash
cp config.example.yaml config.yaml
vi config.yaml          # 至少填好 hosts 和 apps
```

### 3. 启动与访问

```bash
./start.sh              # 默认读取同目录的 config.yaml
# 浏览器打开 http://<服务器地址>:23344/
```

进程号与日志写在当前目录的 `logviewer.pid`、`logviewer.log`；停止用 `./stop.sh`，重启用 `./restart.sh`。

### 从源码构建（开发者）

只有修改代码时才需要，构建在装有 Go 的机器上完成，目标机不需要 Go：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o logviewer .
```

替换已有部署时，先把新二进制传到部署目录的 `logviewer.new`，再执行：

```bash
./stop.sh && mv -f logviewer.new logviewer && chmod +x logviewer && ./start.sh
```

构建时也可以直接注入版本号：`-ldflags "-s -w -X main.version=v2.3.1"`。

## 配置文件

`config.yaml` 是 YAML 格式，全部字段如下：

| 字段 | 说明 | 默认值 |
|:--|:--|:--|
| `port` | 监听端口 | `23344` |
| `tail_lines` | 首次打开加载的行数，也是向上翻页的起点 | 代码内 `1000`，示例文件写 `200` |
| `allow_any_path` | 「路径追踪」页是否允许打开任意绝对路径 | `false` |
| `path_roots` | `allow_any_path: false` 时额外放行的目录列表 | 空 |
| `defaults.user` | 主机未显式填写时的 SSH 用户 | `root` |
| `defaults.port` | 主机未显式填写时的 SSH 端口 | `22` |
| `defaults.dir` | 主机未显式填写时的日志根目录（代码内为 `/data/projects/drone_logs`，建议显式配置） | — |

### 主机（hosts）

主机只定义一次，后续都用昵称引用：

```yaml
defaults:
  user: root
  port: 22
  dir: /var/log/app

hosts:
  - web1: {ip: 10.0.0.11, label: Web 1, pass: 'change-me'}
  - web2: {ip: 10.0.0.12, label: Web 2, pass: 'change-me'}
```

- key 就是昵称，`apps` 和页面里都用它
- `label` 是界面显示名，不写则显示昵称
- 单台主机可覆盖 `user` / `port` / `dir`，不写就继承 `defaults`
- 昵称重复、缺少 `ip` 会在启动时直接报错

### 应用（apps）

用块写法描述「哪个应用看哪几份日志」：

```yaml
apps: |
  flightcontrol 飞控[
  web1:/data/logs/flightcontrol.log
  web2:/data/logs/flightcontrol.log
  ]

  flightcontrol-error 飞控错误[
  web1:/data/logs/flightcontrol-error.log
  ]
```

规则：

- 每块以 `应用id 显示名[` 开头、单独一行 `]` 结束；显示名可省略
- **块顺序 = 下拉框顺序，行顺序 = 分栏从左到右的顺序**
- 每行写 `主机昵称:绝对路径`，程序不做任何路径拼接，路径必须 `/` 开头
- **一行 = 一栏**：只写一行，应用日志页就只有一个铺满整宽的面板
- **同一主机可以出现多行**，例如主日志和它的 error 日志各占一栏
- 缺 `]`、行内没有冒号、路径不是绝对路径、引用了未知主机，都会在启动时报错并指出行号
- 旧版的 YAML 列表写法（`- flightcontrol: 飞控`）仍然兼容

### 路径白名单

默认 `allow_any_path: true`，任何绝对路径都能打开。若不希望这样，改成 `false` 并限定目录：

```yaml
allow_any_path: false
path_roots:
  - /var/log
  - /data/logs
```

关闭后，只有 `hosts[*].dir`、`apps` 里声明过的文件、以及 `path_roots` 下的路径可读。

### 校验与生效

```bash
./logviewer -config config.yaml -check   # 打印解析后的主机与应用清单（不含密码）
./restart.sh                             # 配置改动后重启生效（没有热重载）
```

## 页面用法

| 页面 | 地址 | 用途 |
|:--|:--|:--|
| 主机日志 | `/` | 选主机 → 逐级浏览目录 → 点文件实时追踪；支持文件名搜索、修改时间筛选、多选批量下载、全屏 |
| 应用日志 | `/app.html` | 从 `apps` 里选应用，按配置分栏；每栏独立暂停 / 全屏 / 下载，顶部过滤对所有栏生效 |
| 路径追踪 | `/tail.html` | 输入绝对路径即开即看，先显示类型 / 大小 / 修改时间；最近 8 条自动记住，URL 带 `?host=&path=` 可直接分享 |
| 全屏 | `/fullscreen.html?host=&file=` | 单文件全屏深色视图 |

公共操作：暂停 / 继续、自动滚动开关、向上翻历史；主机日志页与全屏页还支持 `Ctrl+S` 停止、`Ctrl+L` 清屏。

### 日志等级着色

| 等级 | 颜色 | 判定方式 |
|:--|:--|:--|
| ERROR / FATAL / PANIC / CRITICAL / SEVERE / ALERT / EMERG | 红，左侧红条 | 等级字段优先；无字段时兜底关键字：失败 / 错误 / 异常 / 崩溃 / 未完成 |
| WARN / WARNING | 黄，左侧黄条 | 兜底关键字：警告 / 告警 / 超时 |
| SUCCESS / OK / PASSED / DONE / COMPLETED | 绿，左侧绿条 | 兜底关键字：成功 / 已启动 / 已完成 / 就绪 |
| INFO / DEBUG / TRACE / NOTICE / 无等级 | 保持白色 | 默认 |

判定顺序：先看行首 120 字符内有没有显式等级字段（`level=error`、`[WARN]`、`ERROR 1234 ---`、`{"level":"info"}`），命中就按等级上色；整行都没有等级字段时，才按关键字兜底。这样 `level=info msg="request success"` 仍然是白色，不会被正文里的 success 误染成绿色。

## HTTP 与 WebSocket 接口

| 路径 | 说明 |
|:--|:--|
| `/`、`/index.html` | 主机日志页 |
| `/app.html` | 应用日志页 |
| `/tail.html` | 路径追踪页 |
| `/fullscreen.html` | 全屏页（需 `host`、`file` 参数） |
| `/ws` | WebSocket，消息类型：`init` / `load_prev` / `stop` / `list_dir` |
| `/api/hosts` | 主机列表 |
| `/api/apps` | 应用列表及其解析后的日志源 |
| `/api/stat` | 远端路径的存在性、大小、修改时间 |
| `/api/download` | 下载整份文件（受路径白名单约束） |
| `/api/version` | 当前版本号 |

## 服务管理

```bash
./start.sh [配置文件]     # nohup 启动，写 logviewer.pid 与 logviewer.log
./stop.sh                 # 只停本目录启动的实例
./restart.sh [配置文件]   # 先停再起；停止失败则中止，不会出现“停了没起来”
```

- `stop.sh` 会用 `/proc/<pid>/cwd` 校验 pid 归属，只在工作目录与脚本目录一致时才动手，避免误停其他目录的同名进程
- **不要**用 `pgrep -f "logviewer -config"` 停进程，会连累同机其它实例
- 同一台机器可以跑多个实例，只要端口和工作目录不同

## 环境要求

- 运行：**不需要 Go，也不需要编译**，单个静态二进制即可，Linux 及任意 Go 目标平台都可
- 构建：仅修改代码时需要，Go 1.21+，交叉编译即可（目标机不用装 Go）
- 目标主机：支持密码 SSH 登录；具备 `tail`、`find`、`stat` 命令（Linux 标准发行版都有）

## 目录结构

```
main.go            配置解析、HTTP 路由、主机与应用解析
ssh_handler.go     SSH 连接池、目录列举、tail、stat、下载
websocket.go       WebSocket 会话：init / load_prev / stop / list_dir
static/index.html  主机日志页
static/app.html    应用日志页（按主机分栏）
static/tail.html   路径追踪页
static/fullscreen.html  全屏页
config.example.yaml
start.sh, stop.sh, restart.sh
```

## 版本

当前版本 **v2.3**，版本号唯一来源是 `main.go` 里的 `var version = "v2.3"`，页面通过 `GET /api/version` 获取。不修改源码也可以注入版本号：

```bash
go build -ldflags "-s -w -X main.version=v2.3.1" -o logviewer .
./logviewer -version        # 输出 v2.3
```

版本沿革：

| 版本 | 内容 |
|:--|:--|
| v1 | 主机日志单视图 |
| v2.0 | 三种视图（主机日志 / 应用分栏 / 任意路径）+ 版本号展示 |
| v2.1 | 日志等级着色（红 / 黄 / 绿 / 白） |
| v2.2 | 同一主机可配置多份日志（一行一栏）；新增 `restart.sh` |
| v2.3 | 修复停止追踪后「全屏追踪」按钮被置灰、点击无效的问题 |

## 许可证

Apache-2.0，见 [LICENSE](LICENSE)。
