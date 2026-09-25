package models

// 全局配置文件结构体
type AppConfig struct {
	Qbittorrent  QBConfig      `json:"qbittorrent"`
	Server       ServerConfig  `json:"server"`
	Notifier     NotifierConfig `json:"notifier"`
	RSS          RSSConfig     `json:"rss"`
	Limiter      LimiterConfig `json:"limiter"`
	FileManager  FileManagerConfig `json:"fileManager"`
}

type QBConfig struct {
	URL      string `json:"url"`      // 如 http://192.168.1.10:8080
	Username string `json:"username"`
	Password string `json:"password"`
}

type ServerConfig struct {
	Listen string `json:"listen"` // 如 :8088
}

type NotifierConfig struct {
	Enabled   bool              `json:"enabled"`
	AppriseURLs []string        `json:"appriseUrls"` // apprise 的多个通知 URL
	FilterTitle string          `json:"filterTitle"`
}

type RSSConfig struct {
	Enabled  bool        `json:"enabled"`
	Interval int         `json:"interval"` // 分钟
	Proxies  []string    `json:"proxies"`  // 可选代理
	Feeds    []RSSFeed   `json:"feeds"`
}

type RSSFeed struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	URL      string      `json:"url"`
	Enabled  bool        `json:"enabled"`
	Rules    []RSSRule   `json:"rules"`
}

type RSSRule struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Mode        string `json:"mode"`        // keyword / regex
	Include     string `json:"include"`     // 匹配表达式（标题中必须包含）
	Exclude     string `json:"exclude"`     // 排除表达式
	SavePath    string `json:"savePath"`    // qBittorrent 保存路径
	Category    string `json:"category"`
	Tags        string `json:"tags"`
	UploadLimit int    `json:"uploadLimit"` // KB/s，0 表示不限制
}

type LimiterConfig struct {
	Enabled  bool   `json:"enabled"`
	Interval int    `json:"interval"` // 秒，刷新限速的间隔
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
	Enabled bool `json:"enabled"`
	// 完成事件扫描间隔（秒）
	ScanInterval int `json:"scanInterval"`
}

// qBittorrent torrent 信息（来自 /api/v2/torrents/info）
type QBTorrent struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	State       string  `json:"state"`
	Progress    float64 `json:"progress"`
	Size        int64   `json:"size"`
	Downloaded  int64   `json:"downloaded"`
	Uploaded    int64   `json:"uploaded"`
	UploadSpeed int64   `json:"upspeed"`
	DownloadSpeed int64 `json:"dlspeed"`
	Category    string  `json:"category"`
	Tags        string  `json:"tags"`
	SavePath    string  `json:"save_path"`
}

// torrent 文件列表项
type QBFile struct {
	Name      string  `json:"name"`
	Size      int64   `json:"size"`
	Progress  float64 `json:"progress"`
	Downloaded int64  `json:"downloaded"`
}

// API 通用响应
type APIResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}
