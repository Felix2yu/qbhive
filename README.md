# QBHive

qBittorrent 的 Web 管理面板与自动化工具箱：一个单二进制、零外部依赖的 Go 服务，自带网页前端，
把「看任务、订 RSS、发通知、控限速、整理文件」这几件事集中到一个界面里。

- 后端：Go + [Gin](https://github.com/gin-gonic/gin)，`CGO_ENABLED=0` 纯静态二进制
- 前端：原生 HTML/CSS/JS，构建时随二进制一同打包，无需 Node 工具链
- 对接：qBittorrent WebUI API v2（支持 Cookie 登录与 v5.2.0+ 的 API Key）

## 功能

### 概览
- 紧凑状态带：下载中 / 做种中 / 停滞 / 暂停未完成 / 已完成 / 异常 计数
- 上下行速度条，直观显示当前速度占全局限速的比例
- 最近活跃任务列表，后台静默刷新不打断操作

### 任务
- 状态筛选（活跃 / 下载中 / 做种中 / 已完成 / 全量等）、Top N、关键字搜索（名称 / 分类 / 标签）
- 列排序 + 前端分页，数千条任务也能流畅浏览
- 单任务上传限速：表格内直接改，点按钮即下发

### RSS 自动下载
- 多订阅源管理，可单独启停；支持可选代理
- 每个源下多条规则：关键词 / 正则两种模式，包含 + 排除表达式
- 命中后自动提交 qBittorrent，可指定保存路径、分类、标签、上传限速
- 条目去重持久化（`data/rss_seen.json`），首次接入源只记录不下载（快照模式）
- 运行状态面板：最近拉取时间 / 成功与否 / 条目数 / 命中数 / 提交数 / 最近 100 条处理记录
- 支持强制立即拉取、重置单个或全部源的去重状态

### 完成通知
- 基于 [Apprise-Go](https://github.com/unraid/apprise-go)，一行一个 URL 即可接入上百种渠道
  （Telegram / Discord / Slack / 企业微信 / 邮件 / Gotify / Bark ...）
- 每 10 秒扫描完成事件，已通知哈希持久化到 `data/finished.json`，重启不重发
- 支持一键发送测试通知

### 限速规则
- 按正则匹配任务名称，批量下发上传限速
- 只对「限速值发生变化」的任务调用 qB API，避免无效请求

### 文件管理（单文件自动归档）
- 下载完成后若 `savePath/任务名/` 下只有一个文件，自动上移到保存路径并清理空目录
- 优先走 qB 的 `renameFile` API（保持做种一致性），失败时回退本地 `os.Rename`

### 其它
- 亮色 / 暗色 / 跟随系统 三主题
- 可选 Token 登录鉴权（设置 `QBHIVE_TOKEN` 环境变量即启用）
- 所有配置均可在网页「设置」页修改并热重载，无需重启

## 快速开始

### Docker Compose（推荐）

```bash
git clone https://github.com/Felix2yu/qbhive.git
cd qbhive
docker compose up -d
```

镜像来自 GHCR 多架构 manifest（linux/amd64 + linux/arm64），`data/` 目录会挂载为配置与状态持久化卷。

### Docker 单命令

```bash
docker run -d --name qbhive \
  -p 8088:8088 \
  -v "$PWD/data:/app/data" \
  -e TZ=Asia/Shanghai \
  ghcr.io/felix2yu/qbhive:latest
```

> 注意：qbittorrent 需要能被容器访问到；如果 qB 在另一台机器/容器里，
> 建议像 `docker-compose.yml` 那样挂同一网络，或把 `qbittorrent.url` 写成容器可达的地址。

### 直接下载二进制

从 [Releases](https://github.com/Felix2yu/qbhive/releases) 页面下载对应平台的二进制（附 `SHA256SUMS.txt`）：

```bash
chmod +x qbhive-linux-amd64
./qbhive-linux-amd64 -config config.json -web web/static
```

已提供：`linux/amd64`、`linux/arm64`、`darwin/arm64`、`darwin/amd64`。

### 从源码构建

需要 Go 1.27+（版本以 `go.mod` 为准）：

```bash
go build ./...          # 构建
go vet ./...            # 静态检查
go test ./...           # 单元测试
go build -o qbhive ./cmd/server
```

## 配置

首次启动会在配置路径生成/读取 `config.json`，也可以直接在网页「设置」页修改。
示例：

```json
{
  "qbittorrent": {
    "url": "http://192.168.1.10:8080",
    "username": "admin",
    "password": "admin",
    "apiKey": ""
  },
  "server": { "listen": ":8088" },
  "notifier": {
    "enabled": false,
    "appriseUrls": ["telegram://BOT_TOKEN/CHAT_ID"],
    "filterTitle": ""
  },
  "rss": {
    "enabled": true,
    "interval": 15,
    "proxies": [],
    "feeds": [
      {
        "id": "feed1",
        "name": "示例订阅源",
        "url": "https://example.com/feed",
        "enabled": true,
        "rules": [
          {
            "id": "rule1",
            "name": "规则 1",
            "enabled": true,
            "mode": "keyword",
            "include": "keyword",
            "exclude": "",
            "savePath": "",
            "category": "",
            "tags": "",
            "uploadLimit": 0
          }
        ]
      }
    ]
  },
  "limiter": {
    "enabled": false,
    "interval": 10,
    "rules": [
      { "id": "l1", "name": "限速示例", "enabled": true, "match": "^SomePrefix", "uploadLimit": 512 }
    ]
  },
  "fileManager": { "enabled": false, "scanInterval": 15 }
}
```

### 字段说明

| 字段 | 说明 |
| --- | --- |
| `qbittorrent.url` | qBittorrent WebUI 地址 |
| `qbittorrent.username` / `password` | Cookie 登录凭据 |
| `qbittorrent.apiKey` | qBittorrent v5.2.0+ 的 API Key，**填写后优先使用并跳过 Cookie 登录** |
| `server.listen` | Web 监听地址，默认 `:8088` |
| `notifier.appriseUrls` | 每行/每项一个 Apprise URL |
| `rss.interval` | RSS 拉取间隔（分钟） |
| `rss.proxies` | 可选代理列表 |
| `rss.feeds[].rules[].mode` | `keyword` 或 `regex` |
| `rss.feeds[].rules[].uploadLimit` | 该规则下载任务的上传限速（KB/s），`0` 表示不限制 |
| `limiter.rules[].match` | 匹配任务名的正则 |
| `limiter.rules[].uploadLimit` | 命中后的上传限速（KB/s），`0` 表示不限制 |
| `fileManager.scanInterval` | 单文件归档扫描间隔（秒） |

### 环境变量

| 变量 | 说明 |
| --- | --- |
| `QBHIVE_CONFIG` | 配置文件路径（默认 `config.json`；容器内为 `/app/data/config.json`） |
| `QBHIVE_WEB` | 静态资源目录（默认 `web/static`；容器内为 `/app/web/static`） |
| `QBHIVE_TOKEN` | 设置后 Web UI 需要登录，API 走 Bearer Token / Cookie |
| `TZ` | 时区，例如 `Asia/Shanghai` |

## 使用

启动后浏览器访问 `http://<主机>:8088`：

1. **概览** —— 速度、状态计数、最近活跃任务
2. **任务** —— 全量任务浏览、搜索、排序、单任务限速
3. **RSS** —— 订阅源与规则管理，查看每个源的抓取状态与最近条目
4. **设置** —— 主题、qBittorrent 连接（可测试连接）、监听地址、限速规则、通知、文件管理，
   改完点「保存全部设置」即热生效

## 项目结构

```
cmd/server/          入口：装配各模块、信号处理
internal/
  config/            配置读写与热重载
  qbittorrent/       qB WebUI API v2 客户端
  scheduler/         后台调度：完成通知扫描、状态持久化
  notifier/          Apprise-Go 通知
  rss/               RSS 拉取、规则匹配、去重与提交
  limiter/           按规则下发上传限速
  filemgr/           单文件自动归档
  web/               HTTP API + 鉴权
  models/            配置与数据结构
web/static/          前端（原生 HTML/CSS/JS）
data/                运行状态（finished.json / rss_seen.json），不入库
```

## CI / 发布

- **Build**（`.github/workflows/build.yml`）：push / PR 到 `main` 时执行 `go vet`、`go build`、`go test`，
  并在 `main` 上构建 linux/amd64 + linux/arm64 镜像推到 GHCR，合并为 `:latest` 多架构 manifest
- **Release**（`.github/workflows/release.yml`）：发布 GitHub Release 时交叉编译
  4 个平台二进制 + `SHA256SUMS.txt`，自动挂到该 Release 上

## License

本仓库暂未附带 LICENSE 文件，使用前请与作者确认授权方式。
