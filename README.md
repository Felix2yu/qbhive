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
- **转码后处理**（下载完成即跑一次外部命令）：按 qB 报告的任务文件列表，对每个命中白名单的视频文件各跑一条
  shell 命令，典型是调用 macOS 快捷指令转码；可按任务的**分类 / 标签**过滤（包含 + 排除），全服务同时只跑一条命令、
  其余文件排队，命令在后台异步执行、不阻塞扫描，失败退避重试 2 次后放弃，已派发的文件不会重复跑（重启同样）；
  设置页可填测试路径「运行一次」验证命令本身
- **批量重命名（模板）**：「文件」页选任务 → 输入模板 → 预览 → 执行。变量：`{date}` 完成日期、
  `{type}` 类型分类（video/audio/image/archive/doc/other）、`{index}` 任务内序号、
  `{orig}` 原文件名、`{title}` 任务名；仅重命名文件名（扩展名保留），目录名不变；
  执行时按当前文件列表重新计算，同样写入审计日志、可回退

### 转码后处理（示例：Permute）

下载完成后，对该任务文件列表里每个命中白名单的视频文件各跑一条 shell 命令。设置页
「文件管理 → 转码后处理」里填：

```
命令：/usr/bin/shortcuts run "Permute HEVC 50%缩放" -i "$FILE"
```

⚠️ 快捷指令名必须与 `shortcuts list` 输出**逐字符一致**（空格、半全角都算），
差一个空格就会报「找不到快捷指令」。拿不准就先在终端跑 `shortcuts list` 对照。

- 目标从哪来：以 qB 的 `/torrents/files`（任务自己的文件列表）为准，不翻目录猜路径。
  文件散在共享分类根（几百个任务挤在同一个目录）时，也只有 qB 知道哪些文件属于本任务；
  文件绝对路径 = `save_path` + qB 报出的相对路径（拼接后会校验没越出 `save_path`）
- 一个种子里多个视频**逐个派发**：每轮扫描最多交出一个文件，且全服务同时只跑一条命令，
  其余文件在后面的轮次里排队（转码吃满 CPU 与磁盘带宽，并发跑只会互相拖慢）
- 归档在途时让路：qB 的 `renameFile` 磁盘移动是异步的，原目录还没清空就先不转，
  等文件到了最终路径才派发，不会出现「先转码后移动」拿错路径的情况
- 文件还挂在 libtorrent 的未完成临时名（尾缀 `.!qB`）、或磁盘上暂时取不到时，
  只算「这一轮没落齐」，下轮再看；连续 20 轮都拿不到才放弃这个任务
- 派发过的文件记在 `filemgr_clean.json` 的 `postDispatched` 里，重启不会把同一个文件再转一遍
- 命令走 `sh -c`，可以自己写引号、管道；`$FILE` 是文件的绝对路径（务必带双引号，
  路径含空格/中文才安全），也可以用 `{file}` 占位符（自动加单引号，同样安全）
- 后台异步执行，不阻塞扫描；`timeout` 是**总预算**（含重试），`0` 表示不限
- 「扩展名白名单」建议填 `mov,m4v,mkv,mp4`，只有视频才转码
- **分类 / 标签过滤**（`categoryInclude`/`categoryExclude`/`tagInclude`/`tagExclude`、
  不区分大小写，多条之间用逗号分隔——`, ， 、 ; ； |` 与换行都算分隔符）：对**整个任务**生效，
  被挡掉的任务连 `/torrents/files` 都不会请求。
  包含名单留空即不过滤；非空时**未分类 / 无标签的任务不命中任何包含名单**（会被跳过）。
  标签命中**任意一个**即算，不要求全部。同一个任务同时命中包含与排除时**排除优先**。
- 过滤结果是持久化的（`filemgr_clean.json` 里记 `postDone` + 当时的名单指纹），但**改了名单就会重扫历史
  任务**：先前被挡掉、漏转的会补转，已经转过的不会重转（按 `postDispatched` 的文件级去重），
  无需重启、无需手改状态文件。因此「先按分类过滤、后来想放宽」是可逆的；反过来收紧名单也只影响之后
  还会被转的文件，已经转出来的产物不会撤回
- 首次接入先填测试路径点「运行一次」，只验证命令本身，不做任何清理。「测试运行」还可以填
  **模拟分类 / 标签**：值会原样交给命令的 `$CATEGORY` / `$TAGS`，用来验证脚本里按分类、标签
  分派的分支；结果里同时回报「按当前名单这一组会不会被转码」（只是回报，命令照样跑）

**按标签分派不同快捷指令**：任务在 qB 里的分类与标签已经直接交给脚本了
（`$CATEGORY`、`$TAGS`——逗号分隔的标签原值，无标签时为空串），按种子标签二选一就用它们，
不必去读文件元数据：

```sh
#!/bin/sh
case ",$TAGS," in
  *,videoai*) exec /usr/bin/shortcuts run "Permute HEVC 50%缩放" -i "$1" ;;
esac
exec /usr/bin/shortcuts run "Permute HEVC" -i "$1"
```

需要按**视频元数据**（写进容器里的标签）决定用哪个快捷指令时，别把判断塞进单行命令里，
写成脚本更可维护（qbhive 的命令栏填 `~/bin/xxx.sh "$FILE"` 即可）。例如按 `videoai` 标签二选一：

```sh
#!/bin/sh
# 有 videoai 标签 → 缩放到 50%；没有 → 保持原分辨率
tag=$(/opt/homebrew/bin/ffprobe -v error -show_format -select_streams v:0 -show_streams \
        -of default=nw=1 "$1" 2>/dev/null | grep -i -m1 -E '^(TAG:)?videoai=' | sed 's/^[^=]*=//')
if [ -n "$tag" ]; then exec /usr/bin/shortcuts run "Permute HEVC 50%缩放" -i "$1"; fi
exec /usr/bin/shortcuts run "Permute HEVC" -i "$1"
```

两个容易踩的点：

- **键名形态因容器而异**，匹配要兼容：mkv 的标签会被规范化成大写（ffprobe 输出
  `TAG:VIDEOAI=1`），mov/mp4 的 udta 里则保持原样小写（`TAG:videoai=Enhanced using ...`，
  值里常含空格与分号）。正则要大小写不敏感、`TAG:` 前缀可有可无。
  另外 **ffmpeg 自己写不进 udta 自定义标签**（`-metadata videoai=1` 对 mp4/mov 无效），
  但 AI 增强类工具写进去的能被 ffprobe 正常读到——所以「mp4/mov 读不到标签」不能一概而论，
  要以文件实际元数据为准（`ffprobe -show_format 文件名 | grep -i videoai`）。
- macOS 的 `/bin/sh`（bash 3.2）在 `set -u` 下会把 `"$NAME"「` 这种**紧跟全角标点**的
  变量引用当成 `NAME「` 报 `unbound variable`，变量后接中文标点一律写 `${NAME}`。

**按大小清理（可选，默认关闭）**：命令成功后比较「产物 vs 原片」——产物更小就删原片
保留产物，产物更大就保留原片并删产物。产物要同时满足「与原片同目录、**与原片同名**
（只换扩展名）、mtime 不早于命令开始时刻」才被认下来，找不到产物就不动任何文件；
产物小于原片 5%（疑似半成品）时两边都不删。
加「同名」这条是因为文件可能散在共享分类根：只按「同目录 + 新文件」认产物，
会把别的任务刚下完的片子当成产物，进而删掉原片或删掉别人的文件。
删除会写进「文件」页审计日志（状态 `已删除`，不可回退）。⚠️ 删掉原片会让 qB 认为种子缺文件。

**删除前校验（默认开启，硬闸门）**：单看体积判断「转码成功」是不够的——转码被中断、
封装损坏时命令照样返回 0，只留一个同样更小的坏文件，按大小判定就会拿坏文件顶掉完好的
原片。开启后删原片**必须**同时满足：

- 本机能找到 `ffprobe`（`probePath` 指定，否则按 `PATH → /opt/homebrew/bin →
  /usr/local/bin → /usr/bin` 自动探测；Homebrew 装的 ffmpeg 不在启动程序的 shell 的
  PATH 里时，把绝对路径填进 `probePath`）
- 产物能被 ffprobe 解出视频流（校验日志会打出编码与分辨率）
- 产物体积在「原片 5% ~ 原片大小」之间

校验器缺失或产物校验不通过时，**一个文件都不删**（原片完整保留，日志里写明原因）。
未通过校验的产物也保留，不做二次删除——「删不掉的视频」比「多一个占空间的文件」安全。
关闭该选项（设置页「删除前校验产物」）会退回到纯按大小判定。

### 运行日志
- 四把 logger（INFO / WARN / ERROR / DEBUG）在写 stdout/stderr 的同时旁路抄一份进内存环形缓冲，
  网页「日志」页即可查看；stdout 输出格式一字未改，`server.log` / `docker logs` 照旧能 grep
- 缓冲只保留最近 **2000 条**，级别前缀与时间头剥掉后按「级别 + 时间 + 正文」存下来；
  **只在内存里**，不落盘，服务重启即清空——要翻历史仍然得看 `server.log`
- 页面上支持：级别筛选、关键字搜索（都在浏览器侧做，切换筛选不会漏日志）、2 秒增量轮询
  （按 `cursor` 拉新行，既不重复也不丢）、「滚到底部」开关、按当前筛选导出 txt
- 接口 `GET /api/logs?since=&limit=`，与其它接口共用 `QBHIVE_TOKEN` 鉴权（未登录 401）。
  日志正文里可能出现订阅地址、通知渠道串，**不要把监听地址暴露到局域网**

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
| `fileManager.postProcess.enabled` | 下载完成后处理开关（默认关闭，可与自动归档并存） |
| `fileManager.postProcess.command` | 单行 shell 命令，走 `sh -c`；支持 `{file}`/`{dir}`/`{name}`/`{category}`/`{tags}`/`{savepath}` 占位符，以及同名大写环境变量 `$FILE`/`$DIR`/`$NAME`/`$CATEGORY`/`$TAGS`/`$SAVEPATH`（`$TAGS` 是 qB 的逗号分隔标签原值） |
| `fileManager.postProcess.extensions` | 扩展名白名单（多条用逗号分隔、不写点，分隔符兼容 `, ， 、 ; ； |` 与换行），留空表示不过滤 |
| `fileManager.postProcess.categoryInclude` | 分类白名单（不区分大小写，分隔符兼容 `, ， 、 ; ； |` 与换行），留空表示不按分类过滤；未分类的任务不命中任何白名单 |
| `fileManager.postProcess.categoryExclude` | 分类黑名单（分隔符兼容 `, ， 、 ; ； |` 与换行），命中的任务整任务跳过转码，优先级高于包含 |
| `fileManager.postProcess.tagInclude` | 标签白名单（分隔符兼容 `, ， 、 ; ； |` 与换行），任务标签命中任意一个即转；留空表示不按标签过滤 |
| `fileManager.postProcess.tagExclude` | 标签黑名单（分隔符兼容 `, ， 、 ; ； |` 与换行），任务标签命中任意一个即整任务跳过，优先级高于包含 |
| `fileManager.postProcess.timeout` | 命令总超时秒数，`0` 表示不限 |
| `fileManager.postProcess.sizePrune` | 命令成功后按大小二选一清理（产物更小则删原片，否则保留原片删产物） |
| `fileManager.postProcess.verify` | 删原片前用 ffprobe 校验产物是否真能解出视频流（`true`/`false`，缺省按 `true`）。校验不通过或找不到 ffprobe 则不删任何文件 |
| `fileManager.postProcess.probePath` | ffprobe 绝对路径，留空自动探测（默认按 `PATH → /opt/homebrew/bin → /usr/local/bin → /usr/bin` 找） |

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
5. **日志** —— 服务进程内存里的运行日志（最近 2000 条），可按级别筛选、搜索、导出
6. **设置** —— 主题、qBittorrent 连接（可测试连接）、监听地址、限速规则、通知、文件管理，
   改完点「保存全部设置」即热生效

## 项目结构

```
cmd/server/          入口：装配各模块、信号处理
internal/
  config/            配置读写与热重载
  logger/            分级日志：stdout/stderr + 内存环形缓冲（供网页「日志」页读取）
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
