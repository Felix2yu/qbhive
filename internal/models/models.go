package models

// 全局配置文件结构体
type AppConfig struct {
	Qbittorrent  QBConfig         `json:"qbittorrent"`
	Server       ServerConfig     `json:"server"`
	Notifier     NotifierConfig   `json:"notifier"`
	RSS          RSSConfig        `json:"rss"`
	Limiter      LimiterConfig    `json:"limiter"`
	FileManager  FileManagerConfig `json:"fileManager"`
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
	ContentPath   string  `json:"content_path"` // 5.x：单文件种子=文件完整路径，多文件种子=内容根目录完整路径
	AddedOn       int64   `json:"added_on"`       // Unix 秒，任务被添加到 qBittorrent 的时间（可视为开始时间）
	CompletedOn   int64   `json:"completion_on"`  // Unix 秒，完成时间；未完成时为 0（5.x 字段名，4.x 为 completed_on）
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
