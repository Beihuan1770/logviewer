# logviewer v1 · 主机日志精简版

从 [logviewer](https://github.com/Beihuan1770/logviewer) v2.2 切分出来的精简版：**只保留主机日志**，打开页面直接浏览目录、点文件、实时追踪。没有应用日志分栏，也没有任意路径追踪。

> 2026-10-27 同步了主线的性能与会话修复：向上翻页按字节窗口增量读取、WebSocket 100ms 批量推送、批量渲染 + DOM 节点上限、每条实时追踪独立 SSH 连接（避免快速切换日志时耗尽 sshd 会话）。

> 面向内网使用：**没有登录鉴权**，主机密码以明文写在 `config.yaml` 里。不要暴露到公网。

## 和主线的区别

| 项 | 主线 v2.x | 本精简版 v1.3 |
|:--|:--|:--|
| 页面 | 主机日志 / 应用日志 / 路径追踪（顶部三个 Tab） | **只有主机日志**（无 Tab） |
| 配置项 | `hosts` + `apps` + `allow_any_path` + `path_roots` | `port` + `tail_lines` + `idle_pause_minutes` + `defaults` + `hosts` |
| 默认端口 | 23344 | **13344** |
| 日志等级着色、翻页、过滤、下载、全屏 | 有 | 有（同一套前端逻辑） |

## 功能

- 按主机浏览日志目录，逐级进入子目录
- 文件名搜索、按修改时间范围筛选
- 点击文件实时追踪，可暂停 / 继续、全屏、清屏
- 向上翻历史：滚动到顶部自动加载更早 200 行，视线位置不跳
- 内容搜索并高亮命中
- 日志等级着色：错误红、警告黄、成功绿、其余白
- 终端彩色转义（ANSI）：日志里的 `\x1b[31m` 颜色码渲染成彩色，支持 16 / 256 / 真彩，其余控制序列丢弃
- 虚拟滚动：数据全量留内存（上限 30000 行），页面只渲染可视窗口约 2500 行，长时间挂机与任意上下翻都不卡
- 等级过滤：错误 / 警告 / 信息 三个开关
- 后台自动暂停：标签页在后台超过 `idle_pause_minutes`（默认 30）分钟自动停止追踪、释放 SSH，切回前台自动恢复
- 单文件下载、多选批量下载
- 路径白名单：只允许读取 `hosts[*].dir` 下的路径

## 快速开始

运行目标机**不需要 Go，也不需要编译**——拿到 `logviewer` 二进制就能跑。

```bash
# 1. 准备目录（二进制 + 脚本放一起）
mkdir -p /data/apps/logviewer-v1 && cd /data/apps/logviewer-v1
chmod +x logviewer start.sh stop.sh restart.sh

# 2. 配置
cp config.example.yaml config.yaml
vi config.yaml              # 填好 hosts 与 defaults.dir

# 3. 启动并访问
./start.sh                  # 默认读取同目录 config.yaml
# 浏览器打开 http://<服务器地址>:13344/
```

进程号与日志写在当前目录的 `logviewer.pid`、`logviewer.log`；停止用 `./stop.sh`，重启用 `./restart.sh`。

### 从源码构建（开发者）

只有修改代码时才需要，目标机不需要 Go：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o logviewer .
```

替换部署：把新二进制传到部署目录的 `logviewer.new`，然后

```bash
./stop.sh && mv -f logviewer.new logviewer && chmod +x logviewer && ./start.sh
```

## 配置文件

| 字段 | 说明 | 默认值 |
|:--|:--|:--|
| `port` | 监听端口 | 示例写 `13344`，代码内兜底 `23344` |
| `tail_lines` | 首次打开加载的行数，也是向上翻页的起点 | 代码内 `1000`，示例写 `200` |
| `idle_pause_minutes` | 标签页在后台超过该分钟数后自动停止追踪，切回前台自动恢复；`0` 关闭 | `30` |
| `defaults.user` | 主机未显式填写时的 SSH 用户 | `root` |
| `defaults.port` | 主机未显式填写时的 SSH 端口 | `22` |
| `defaults.dir` | 主机日志页的默认根目录 | 代码内 `/data/projects/drone_logs`，建议显式配置 |

```yaml
port: 13344
tail_lines: 200

defaults:
  user: root
  port: 22
  dir: /var/log/app

hosts:
  - web1: {ip: 10.0.0.11, label: Web 1, pass: 'change-me'}
  - web2: {ip: 10.0.0.12, label: Web 2, pass: 'change-me'}
```

- 主机只定义一次，key 就是昵称；`label` 是界面显示名
- 单台主机可覆盖 `user` / `port` / `dir`
- 昵称重复或缺少 `ip` 会在启动时报错

校验解析结果（不输出密码）：

```bash
./logviewer -config config.yaml -check
```

配置改动后执行 `./restart.sh` 生效（没有热重载）。

## 页面

| 地址 | 用途 |
|:--|:--|
| `/` | 主机日志：选主机 → 浏览目录 → 点文件实时追踪 |
| `/fullscreen.html?host=&file=` | 单文件全屏深色视图 |

快捷键：`Ctrl+S` 停止追踪，`Ctrl+L` 清屏。

## 服务管理

```bash
./start.sh [配置文件]     # nohup 启动，写 logviewer.pid 与 logviewer.log
./stop.sh                 # 只停本目录启动的实例（/proc/<pid>/cwd 校验归属）
./restart.sh [配置文件]   # 先停再起；停止失败则中止
```

**不要**用 `pgrep -f "logviewer -config"` 停进程，会连累同机其它实例。

## 环境要求

- 运行：不需要 Go，单个静态二进制即可
- 构建：Go 1.21+（仅改代码时需要）
- 目标主机：支持密码 SSH 登录；具备 `tail`、`find`、`stat` 命令

## 版本

当前版本 **v1.3**，来源是 `main.go` 的 `var version = "v1.3"`，页面从 `GET /api/version` 获取。

版本沿革：

| 版本 | 内容 |
|:--|:--|
| v1.0 | 从主线切分的首版：只有主机日志，端口 13344 |
| v1.1 | 同步主线 v2.4 的修复：按字节窗口翻页、WebSocket 批量推送、批量渲染、每条 tail 独立 SSH 连接（快速切换日志不再耗尽会话） |
| v1.2 | 同步主线 v2.5 / v2.6：等级过滤（错误 / 警告 / 信息）、标签页后台超时自动暂停 |
| v1.3 | 同步主线 v2.7：终端 ANSI 彩色渲染、窗口化虚拟滚动（DOM 只渲染约 2500 行） |

## 许可证

Apache-2.0，见 [LICENSE](LICENSE)。
