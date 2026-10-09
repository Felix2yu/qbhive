package models

// 全局配置文件结构体
type AppConfig struct {
	Qbittorrent QBConfig          `json:"qbittorrent"`
	Server      ServerConfig      `json:"server"`
	Notifier    NotifierConfig    `json:"notifier"`
	RSS         RSSConfig         `json:"rss"`
	Limiter     LimiterConfig     `json:"limiter"`
	FileManager FileManagerConfig `json:"fileManager"`
	Proxy       ProxyConfig       `json:"proxy"`
}

// ProxyConfig 控制 qbhive 所有出网请求（RSS 抓取、通知、远程 qB/AI）走哪个代理。
// qB/Ollama 等 localhost 目标恒定直连，不受影响。
//
// 零值（旧 config.json 缺省）Mode="" 等价 "system"：自动跟随 macOS 系统代理。
type ProxyConfig struct {
	// Mode: "" / "system"（跟随 macOS 系统代理，失败回退 HTTP_PROXY 环境变量）、
	// "manual"（固定用 URL）、"off"（强制直连，忽略系统代理与环境变量）
	Mode string `json:"mode"`
	// URL 手动代理地址，Mode=manual 时生效；支持 http:// host:port 与 socks5:// host:port
	URL string `json:"url"`
}

type QBConfig struct {
	URL      string `json:"url"`      // 如 http://192.168.1.10:8080
	Username string `json:"username"` // 用户名（cookie 模式）
	Password string `json:"password"` // 密码（cookie 模式）
	APIKey   string `json:"apiKey"`   // API key (qBittorrent v5.2.0+)；填写则优先使用，跳过 cookie 登录
}

type ServerConfig struct {
	Listen string `json:"listen"` // 如 :8088
}

type NotifierConfig struct {
	Enabled     bool     `json:"enabled"`
	AppriseURLs []string `json:"appriseUrls"`
	FilterTitle string   `json:"filterTitle"`
	// Fields 完成通知正文包含的字段 key（取值见 NotifyFields）；
	// 空表示全部发送（兼容未配置该选项的旧 config.json）
	Fields []string `json:"fields"`
}

// NotifyFields 完成通知的可选字段清单：
// Key 用于配置持久化，Label 用于 UI 展示；切片顺序即通知正文中的行顺序。
var NotifyFields = []struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}{
	{"name", "任务名"},
	{"size", "文件大小"},
	{"category", "分类"},
	{"addedOn", "添加时间"},
	{"completedOn", "完成时间"},
	{"savePath", "保存路径"},
	{"tags", "标签"},
	{"hash", "Hash"},
}

type RSSConfig struct {
	Enabled  bool      `json:"enabled"`
	Interval int       `json:"interval"` // 分钟
	Proxies  []string  `json:"proxies"`  // 可选代理
	Feeds    []RSSFeed `json:"feeds"`
}

type RSSFeed struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	URL     string    `json:"url"`
	Enabled bool      `json:"enabled"`
	Rules   []RSSRule `json:"rules"`
}

type RSSRule struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Mode        string `json:"mode"`    // keyword / regex
	Include     string `json:"include"` // 匹配表达式（标题中必须包含）
	Exclude     string `json:"exclude"` // 排除表达式
	SavePath    string `json:"savePath"`
	Category    string `json:"category"`
	Tags        string `json:"tags"`
	UploadLimit int    `json:"uploadLimit"` // KB/s，0 表示不限制
	Stopped     bool   `json:"stopped"`     // 添加到 qBittorrent 后是否保持停止（不自动开始下载）
}

type LimiterConfig struct {
	Enabled  bool        `json:"enabled"`
	Interval int         `json:"interval"` // 秒，刷新限速的间隔
	Rules    []LimitRule `json:"rules"`
}

type LimitRule struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Match       string `json:"match"`       // 正则匹配 torrent 名称
	UploadLimit int    `json:"uploadLimit"` // KB/s，0 表示无限制
}

type FileManagerConfig struct {
	Enabled      bool `json:"enabled"`
	ScanInterval int  `json:"scanInterval"` // 秒
	// CleanEnabled 文件名自动清理开关：移除下载站附加的域名前缀/后缀等冗余模式。
	// 旧 config.json 缺省时零值 false，默认关闭
	CleanEnabled bool     `json:"cleanEnabled"`
	CleanRules   []string `json:"cleanRules,omitempty"` // 自定义清理正则，每条一条，匹配内容被移除
	// AIEnabled AI 格式化文件名开关：正则清洗后调用 OpenAI 兼容接口二次美化文件名
	AIEnabled   bool              `json:"aiEnabled"`
	AIChannels  []AIChannel       `json:"aiChannels,omitempty"` // AI 通道列表（本地 / 云端皆可）
	AIActive    string            `json:"aiActive"`             // 当前使用的通道 ID
	PostProcess PostProcessConfig `json:"postProcess"`          // 下载完成后的外部命令后处理
}

// PostProcessConfig 下载完成后的外部命令后处理（例如调用 macOS 快捷指令启动
// Permute 转码）。目标文件取自 qB 的任务文件列表（/torrents/files），命中白名单的
// 每个视频各派发一次、全局一次只跑一条命令；归档还在异步移动时先让路，
// 保证命令拿到的是最终路径。
//
// Command 是单行 shell 命令，走 /bin/sh -c 执行，因此可以自己写引号、管道、
// 环境变量展开。同时提供两套取值方式：
//
//   - 占位符：{file} {dir} {name} {category} {tags} {savepath}，会被替换成**加了单引号的
//     字面量**（含空格与中文的路径也安全），适合直接拼命令
//   - 环境变量：$FILE $DIR $NAME $CATEGORY $TAGS $SAVEPATH，值原样传入不做任何处理，
//     适合用户自己用双引号包裹（如 "$FILE"）的场景。$TAGS 是 qB 的逗号分隔标签原值
//     （如 "转码,1080p"），无标签时为空串
type PostProcessConfig struct {
	Enabled bool `json:"enabled"`
	// Command 单行 shell 命令模板，如
	// /usr/bin/shortcuts run "Permute HEVC 50% 缩放" -i "$FILE"
	Command string `json:"command"`
	// Extensions 扩展名白名单（逗号分隔，不写点），只有命中才执行；留空表示不按扩展名过滤
	Extensions string `json:"extensions,omitempty"`
	// CategoryInclude 分类白名单（逗号分隔，不区分大小写）。留空表示不按分类过滤；
	// 非空时只有分类命中其中任意一项的任务才转码。未分类（category 为空）的任务
	// 不命中任何白名单项，因此会被跳过。
	CategoryInclude string `json:"categoryInclude,omitempty"`
	// CategoryExclude 分类黑名单（逗号分隔）：任务分类命中其中任意一项就整任务跳过转码。
	// 优先级高于白名单——同时命中两边时不转。
	CategoryExclude string `json:"categoryExclude,omitempty"`
	// TagInclude 标签白名单（逗号分隔）。留空表示不按标签过滤；非空时任务的 qB 标签里
	// 至少有一个命中才转码（命中任意一个即可，不要求全部）。无标签的任务不命中，会被跳过。
	TagInclude string `json:"tagInclude,omitempty"`
	// TagExclude 标签黑名单（逗号分隔）：任务标签里命中任意一项就整任务跳过转码，
	// 优先级高于白名单。
	TagExclude string `json:"tagExclude,omitempty"`
	// Timeout 单次命令超时秒数，0 表示不限
	Timeout int `json:"timeout"`
	// SizePrune 命令成功后按大小二选一清理：产物比源文件小则删源文件保留产物，
	// 产物比源文件大则保留源文件并删掉产物。删除动作会写审计日志（不可回退）
	SizePrune bool `json:"sizePrune"`
	// Verify 删除源文件之前先用 ffprobe 校验产物是不是真能解出视频流。
	//
	// 这是防误删的硬闸门：转码「退出码 0」并不代表产物可用（转码中途被快捷键
	// 打断、封装损坏都会留下一个小体积文件），此时若只按大小判定就删原片，
	// 用户丢的是完整的源片。开启后以下三种情况一律**不动任何文件**：
	//
	//   - 本机找不到 ffprobe（校验器不可用）
	//   - 产物解不出视频流（转码失败留下的坏文件）
	//   - 产物体积小于源文件 postMinOutputRatio（疑似半成品）
	//
	// 指针语义：老配置文件里没有这个字段时反序列化为 nil，由 config 层补成 true，
	// 避免升级后校验静默关闭、退回到「只看大小」的旧风险行为。
	Verify *bool `json:"verify,omitempty"`
	// ProbePath 指定 ffprobe 的绝对路径（留空则按 PATH → /opt/homebrew/bin →
	// /usr/local/bin → /usr/bin 的顺序自动探测）。Homebrew 装的 ffmpeg 若不在
	// 当前登录 shell 的 PATH 里，就可以在这里填死路径。
	ProbePath string `json:"probePath,omitempty"`
}

// AIChannel OpenAI 兼容的 AI 通道配置（一个通道 = 一个 baseURL + model 组合，
// 本地模型如 Ollama 与云端服务只是不同的 baseURL）
type AIChannel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`    // 展示名，如「本地 Ollama」
	BaseURL string `json:"baseURL"` // 如 http://localhost:11434/v1
	APIKey  string `json:"apiKey"`  // 可空（本地模型常无需）
	Model   string `json:"model"`   // 如 qwen2.5:7b
	Prompt  string `json:"prompt"`  // 自定义提示词，为空用内置默认；支持 {files}/{torrent} 变量
	Enabled bool   `json:"enabled"`
}

// qBittorrent torrent 信息（来自 /api/v2/torrents/info）
// 字段名严格匹配 qBittorrent WebUI API v2 返回的 JSON key
type QBTorrent struct {
	Hash          string  `json:"hash"`
	Name          string  `json:"name"`
	State         string  `json:"state"`
	Progress      float64 `json:"progress"`
	Size          int64   `json:"size"`
	Downloaded    int64   `json:"downloaded"`
	Uploaded      int64   `json:"uploaded"`
	UploadSpeed   int64   `json:"upspeed"`
	DownloadSpeed int64   `json:"dlspeed"`
	Category      string  `json:"category"`
	Tags          string  `json:"tags"`
	SavePath      string  `json:"save_path"`
	ContentPath   string  `json:"content_path"`  // 5.x：单文件种子=文件完整路径，多文件种子=内容根目录完整路径
	AddedOn       int64   `json:"added_on"`      // Unix 秒，任务被添加到 qBittorrent 的时间（可视为开始时间）
	CompletedOn   int64   `json:"completion_on"` // Unix 秒，完成时间；未完成时为 0（5.x 字段名，4.x 为 completed_on）
	LastActivity  int64   `json:"last_activity"`
	Ratio         float64 `json:"ratio"`
	Seeds         int     `json:"num_seeds"`
	Peers         int     `json:"num_leechs"`
}

// torrent 文件列表项（来自 /api/v2/torrents/files）
// 5.x 返回字段：index / name / size / progress / priority / availability /
// piece_range；4.x 独有的 downloaded 字段已移除
type QBFile struct {
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
}

// API 通用响应
type APIResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}
