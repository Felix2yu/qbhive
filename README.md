# QBHive

qBittorrent 的 Web 管理面板与自动化工具箱：一个单二进制、零外部依赖的 Go 服务，自带网页前端，
把「看任务、订 RSS、发通知、控限速、整理文件」这几件事集中到一个界面里。

- 后端：Go + [Gin](https://github.com/gin-gonic/gin)，`CGO_ENABLED=0` 纯静态二进制
- 前端：原生 HTML/CSS/JS，构建时随二进制一同打包，无需 Node 工具链
- 对接：qBittorrent WebUI API v2，**仅支持 qBittorrent 5.x**（stop/start 端点、`stopped` 状态与 filter；
  不兼容 4.x 的 pause/resume 端点），支持 Cookie 登录与 v5.2.0+ 的 API Key

## 功能

### 概览
- 紧凑状态带：下载中 / 做种中 / 停滞 / 停止未完成 / 已完成 / 异常 计数
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
- 可在设置页勾选通知正文包含的字段（任务名 / 大小 / 分类 / 时间 / 路径 / 标签 / Hash）
- 支持一键发送测试通知

### 限速规则
- 按正则匹配任务名称，批量下发上传限速
- 只对「限速值发生变化」的任务调用 qB API，避免无效请求

### 文件管理（自动归档 & 文件名清理）
- **单文件自动归档**：下载完成后若内容目录（5.x `content_path`，回退 `savePath/任务名`）下只有一个文件，自动上移到保存路径并清理空目录
- 对全部完成做种态生效（stoppedUP / uploading / stalledUP / queuedUP / forcedUP），不依赖「完成即暂停」设置；校验中（checkingUP）暂缓
- 优先走 qB 的 `renameFile` API（保持做种一致性），失败时回退本地 `os.Rename`；qB 的磁盘移动是异步的，提交后不立即删目录，留待下一轮确认目录已空再清理，避免误删尚未移动的文件
- **文件名自动清理**：自动识别并移除下载站附加的域名水印（`www.xxx.com - ` 前缀、`[xxx.com]`/`【xxx.com】` 括号、`@xxx.com` 后缀等）与 emoji / 颜文字装饰（🔥★♥、`(◕‿◕)`、`(╯°□°）╯` 等）；带域名合法性启发式（不误删 `S01E01`、`v1.2.3`、`Movie.2023.GER`），含实文字的括号组（`(2023)`、`【4K】`、假名/韩文）不受影响，覆盖种子内全部文件名与顶层目录名；内置规则 + 自定义正则
- **AI 格式化**（可选）：正则清洗后调用 OpenAI 兼容接口二次美化文件名，支持本地（Ollama 等）与云端多通道切换、自定义提示词；每任务只调用一次，失败自动降级为仅正则清洗
- **安全护栏**：保留扩展名、结果非空、不含非法字符；目标已存在时自动加 `_hash8` 兜底名，仍冲突则跳过——**任何情况下绝不覆盖、不删除已有文件**
- **审计与回退**：每次自动重命名记录在 `data/filemgr_audit.jsonl`，网页「文件」页可查看（时间 / 任务 / 变更 / 方式 / 状态）并一键回退
- **批量重命名（模板）**：「文件」页选任务 → 输入模板 → 预览 → 执行。变量：`{date}` 完成日期、
  `{type}` 类型分类（video/audio/image/archive/doc/other）、`{index}` 任务内序号、
  `{orig}` 原文件名、`{title}` 任务名；仅重命名文件名（扩展名保留），目录名不变；
  执行时按当前文件列表重新计算，同样写入审计日志、可回退

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
    "filterTitle": "",
    "fields": ["name", "size", "category", "addedOn", "completedOn", "savePath", "tags", "hash"]
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
  "fileManager": {
    "enabled": false,
    "scanInterval": 15,
    "cleanEnabled": false,
    "cleanRules": ["^【[^】]*】\\s*"],
    "aiEnabled": false,
    "aiActive": "ai1",
    "aiChannels": [
      {
        "id": "ai1",
        "name": "本地 Ollama",
        "baseURL": "http://127.0.0.1:11434/v1",
        "apiKey": "",
        "model": "qwen2.5:7b",
        "prompt": "",
        "enabled": true
      }
    ]
  }
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
| `notifier.fields` | 完成通知正文包含的字段 key 列表，缺省/空 = 全部发送 |
| `rss.interval` | RSS 拉取间隔（分钟） |
| `rss.proxies` | 可选代理列表 |
| `rss.feeds[].rules[].mode` | `keyword` 或 `regex` |
| `rss.feeds[].rules[].uploadLimit` | 该规则下载任务的上传限速（KB/s），`0` 表示不限制 |
| `limiter.rules[].match` | 匹配任务名的正则 |
| `limiter.rules[].uploadLimit` | 命中后的上传限速（KB/s），`0` 表示不限制 |
| `fileManager.scanInterval` | 单文件归档扫描间隔（秒） |
| `fileManager.cleanEnabled` | 文件名自动清理开关（默认关闭） |
| `fileManager.cleanRules` | 自定义清理正则列表（在内置规则之后执行，匹配内容被移除），最多 32 条、单条 ≤256 字符 |
| `fileManager.aiEnabled` | AI 格式化文件名开关（默认关闭） |
| `fileManager.aiActive` | 当前使用的 AI 通道 ID；未指定时用第一个启用的通道 |
| `fileManager.aiChannels[]` | OpenAI 兼容通道（`baseURL`/`apiKey`/`model`/`prompt`），本地与云端皆可；`prompt` 为空用内置默认，支持 `{files}`/`{torrent}` 变量，≤4000 字符 |

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
3. **文件** —— 文件名审计日志（时间 / 任务 / 变更 / 方式 / 状态），可一键回退任意自动重命名
4. **RSS** —— 订阅源与规则管理，查看每个源的抓取状态与最近条目
5. **设置** —— 主题、qBittorrent 连接（可测试连接）、监听地址、限速规则、通知、文件管理，
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
  filemgr/           单文件自动归档、文件名清理（正则 + AI）、审计与回退
  web/               HTTP API + 鉴权
  models/            配置与数据结构
web/static/          前端（原生 HTML/CSS/JS）
data/                运行状态（finished.json / rss_seen.json / filemgr_clean.json / filemgr_audit.jsonl），不入库
```

## CI / 发布

- **Build**（`.github/workflows/build.yml`）：push / PR 到 `main` 时执行 `go vet`、`go build`、`go test`，
  并在 `main` 上构建 linux/amd64 + linux/arm64 镜像推到 GHCR，合并为 `:latest` 多架构 manifest
- **Release**（`.github/workflows/release.yml`）：发布 GitHub Release 时交叉编译
  4 个平台二进制 + `SHA256SUMS.txt`，自动挂到该 Release 上

## License

本仓库暂未附带 LICENSE 文件，使用前请与作者确认授权方式。
