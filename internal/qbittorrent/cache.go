package qbittorrent

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/models"
)

// ErrCacheNotReady 表示首次全量同步尚未完成，调用方应回退到直连 qB。
var ErrCacheNotReady = errors.New("torrent cache not ready")

const (
	cacheSyncInterval     = 2 * time.Second
	cacheRetryInterval    = 5 * time.Second
	cacheFallbackInterval = 10 * time.Second
	cacheHTTPTimeout      = 60 * time.Second
)

// TorrentCache 维护一份 qBittorrent 全量任务快照。
//
// 数据来自 /api/v2/sync/maindata?rid=N：首次全量（本机 7158 条约 27MB），
// 之后每次只返回变化的任务（约 8-9KB）。这让 Web 列表、概览统计和后台扫描
// 都不再反复触发 qB 序列化全部任务。
//
// 注意：qB 的 rid 游标绑定在 WebUI cookie 会话上，用 API key 每次都会返回全量，
// 所以增量模式必须用账号密码登录；只有 API key 时退化为定时全量刷新。
type TorrentCache struct {
	mu       sync.RWMutex
	torrents map[string]models.QBTorrent
	raw      map[string]json.RawMessage // 保留原始 JSON，增量时按字段合并
	stateRaw map[string]json.RawMessage // server_state 的增量同样需要合并
	stats    TransferInfo
	ready    bool

	cmu    sync.Mutex
	url    string
	user   string
	pass   string
	apiKey string
	client *Client
	rid    int
	gen    uint64 // 凭据代次：同步途中换配置则丢弃本次结果
	// deltaDisabled：cookie 登录失败时切到 API key 全量轮询，
	// 避免每 5 秒重试错误密码触发 qB 的登录封禁（默认 5 次封 IP 1 小时）
	deltaDisabled bool
	failCount     int

	stopCh   chan struct{}
	stopOnce sync.Once
	started  bool

	lastErr     string
	lastErrAt   time.Time
	firstSynced bool
}

func NewTorrentCache(url, user, pass, apiKey string) *TorrentCache {
	c := &TorrentCache{
		torrents: make(map[string]models.QBTorrent),
		raw:      make(map[string]json.RawMessage),
		stateRaw: make(map[string]json.RawMessage),
		stopCh:   make(chan struct{}),
	}
	c.setCredentialsLocked(url, user, pass, apiKey)
	return c
}

// SetCredentials 热更新 qB 连接参数。凭据变化后 rid 归零；
// URL 变化意味着换了实例，旧数据不可再信，需等下一次全量。
func (c *TorrentCache) SetCredentials(url, user, pass, apiKey string) {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	c.setCredentialsLocked(url, user, pass, apiKey)
}

func (c *TorrentCache) setCredentialsLocked(url, user, pass, apiKey string) {
	if c.client != nil && c.url == url && c.user == user && c.pass == pass && c.apiKey == apiKey {
		return
	}
	urlChanged := c.client != nil && c.url != url
	newClient := New(url, user, pass, apiKey)
	newClient.httpCli.Timeout = cacheHTTPTimeout
	c.client = newClient
	c.url, c.user, c.pass, c.apiKey = url, user, pass, apiKey
	c.rid = 0
	c.gen++
	c.firstSynced = false
	c.deltaDisabled = false
	c.failCount = 0
	if urlChanged {
		c.mu.Lock()
		c.ready = false
		c.torrents = make(map[string]models.QBTorrent)
		c.raw = make(map[string]json.RawMessage)
		c.stateRaw = make(map[string]json.RawMessage)
		c.stats = TransferInfo{}
		c.mu.Unlock()
	}
}

func (c *TorrentCache) Start() {
	c.cmu.Lock()
	if c.started {
		c.cmu.Unlock()
		return
	}
	c.started = true
	c.cmu.Unlock()
	go c.loop()
}

func (c *TorrentCache) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

func (c *TorrentCache) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

// Refresh 立即同步一次。常驻循环内部也走这里；诊断和测试可手动触发。
func (c *TorrentCache) Refresh() error { return c.syncOnce() }

func (c *TorrentCache) loop() {
	for {
		err := c.syncOnce()
		wait := c.pollInterval()
		if err != nil {
			wait = c.backoff()
			c.logSyncError(err)
		} else {
			c.resetBackoff()
		}
		select {
		case <-c.stopCh:
			return
		case <-time.After(wait):
		}
	}
}

// pollInterval 增量模式 2s；只能整表刷新时放宽到 10s，避免频繁拉 20MB。
func (c *TorrentCache) pollInterval() time.Duration {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	if c.pass != "" && !c.deltaDisabled {
		return cacheSyncInterval
	}
	return cacheFallbackInterval
}

// backoff 失败后退避（5s → 10s → … 封顶 5m），避免错误凭据反复登录把 qB 触发封禁。
func (c *TorrentCache) backoff() time.Duration {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	c.failCount++
	if c.failCount > 10 {
		c.failCount = 10
	}
	d := cacheRetryInterval * time.Duration(1<<(c.failCount-1))
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func (c *TorrentCache) resetBackoff() {
	c.cmu.Lock()
	c.failCount = 0
	c.cmu.Unlock()
}

func (c *TorrentCache) syncOnce() error {
	c.cmu.Lock()
	cli, rid, gen := c.client, c.rid, c.gen
	canDelta := c.pass != "" && !c.deltaDisabled
	apiKey := c.apiKey
	c.cmu.Unlock()

	started := time.Now()
	if !canDelta {
		return c.syncFullList(cli, gen, started)
	}

	resp, err := cli.GetMaindata(rid)
	if err != nil {
		if apiKey != "" && isLoginError(err) {
			c.cmu.Lock()
			if c.gen == gen && !c.deltaDisabled {
				c.deltaDisabled = true
				logger.Warn.Printf("任务快照：cookie 登录失败，改用 API key 全量轮询（请检查 qB 用户名/密码）：%v", err)
			}
			c.cmu.Unlock()
		}
		return err
	}
	if resp.Rid != 0 {
		c.cmu.Lock()
		if c.gen == gen {
			c.rid = resp.Rid
		}
		c.cmu.Unlock()
	}
	if c.stale(gen) {
		return nil
	}

	if resp.FullUpdate {
		c.applyFull(resp, started)
	} else {
		c.applyDelta(resp)
	}
	c.clearSyncError()
	return nil
}

// syncFullList 是无密码（仅 API key）时的降级路径：定期整表替换。
func (c *TorrentCache) syncFullList(cli *Client, gen uint64, started time.Time) error {
	list, err := cli.GetTorrents("all", "", "")
	if err != nil {
		return err
	}
	if c.stale(gen) {
		return nil
	}
	next := make(map[string]models.QBTorrent, len(list))
	for _, t := range list {
		next[t.Hash] = t
	}
	c.mu.Lock()
	c.torrents = next
	c.raw = make(map[string]json.RawMessage)
	c.ready = true
	c.mu.Unlock()
	c.logFirstSync(len(next), started)
	c.clearSyncError()
	return nil
}

func (c *TorrentCache) applyFull(resp *MaindataResponse, started time.Time) {
	next := make(map[string]models.QBTorrent, len(resp.Torrents))
	raw := make(map[string]json.RawMessage, len(resp.Torrents))
	for hash, body := range resp.Torrents {
		var t models.QBTorrent
		if err := json.Unmarshal(body, &t); err != nil {
			logger.Warn.Printf("任务快照：解析 %s 失败：%v", hash[:min(len(hash), 8)], err)
			continue
		}
		if t.Hash == "" {
			t.Hash = hash
		}
		next[hash] = t
		raw[hash] = body
	}
	state := mergeState(nil, resp.ServerState)
	var ti TransferInfo
	_ = json.Unmarshal(state, &ti)

	c.mu.Lock()
	c.torrents = next
	c.raw = raw
	c.stateRaw = resp.ServerState
	c.stats = ti
	c.ready = true
	c.mu.Unlock()
	c.logFirstSync(len(next), started)
}

func (c *TorrentCache) applyDelta(resp *MaindataResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for hash, patch := range resp.Torrents {
		merged, err := mergeJSONObject(c.raw[hash], patch)
		if err != nil {
			logger.Warn.Printf("任务快照：合并 %s 增量失败：%v", hash[:min(len(hash), 8)], err)
			continue
		}
		var t models.QBTorrent
		if err := json.Unmarshal(merged, &t); err != nil {
			logger.Warn.Printf("任务快照：解析 %s 增量失败：%v", hash[:min(len(hash), 8)], err)
			continue
		}
		if t.Hash == "" {
			t.Hash = hash
		}
		c.torrents[hash] = t
		c.raw[hash] = merged
	}
	for _, hash := range resp.TorrentsRemoved {
		delete(c.torrents, hash)
		delete(c.raw, hash)
	}
	if len(resp.ServerState) > 0 {
		merged := make(map[string]json.RawMessage, len(c.stateRaw)+len(resp.ServerState))
		for k, v := range c.stateRaw {
			merged[k] = v
		}
		for k, v := range resp.ServerState {
			merged[k] = v
		}
		var ti TransferInfo
		if raw, err := json.Marshal(merged); err == nil && json.Unmarshal(raw, &ti) == nil {
			c.stats = ti
			c.stateRaw = merged
		}
	}
}

func (c *TorrentCache) stale(gen uint64) bool {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	return c.gen != gen
}

// isLoginError 判断错误是否来自 cookie 登录失败（与网络/超时区分）。
func isLoginError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "login failed") || strings.Contains(s, "maindata auth") ||
		strings.Contains(s, "HTTP 401") || strings.Contains(s, "HTTP 403")
}

func (c *TorrentCache) clearSyncError() {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	c.lastErr = ""
}

func (c *TorrentCache) logFirstSync(n int, started time.Time) {
	c.cmu.Lock()
	first := !c.firstSynced
	c.firstSynced = true
	c.lastErr = ""
	c.cmu.Unlock()
	if first {
		logger.Info.Printf("任务快照：首次全量同步完成，共 %d 条，耗时 %s", n, time.Since(started).Round(time.Millisecond))
		return
	}
	// 增量模式下再次收到 full_update 说明 qB 会话被重置（重启/过期），
	// 只能重新全量；记录一次便于发现 qB 端异常。
	logger.Info.Printf("任务快照：qB 重发全量数据，已刷新 %d 条", n)
}

func (c *TorrentCache) logSyncError(err error) {
	c.cmu.Lock()
	defer c.cmu.Unlock()
	// 同一错误不刷屏：连续失败只在首次记录，恢复后再报
	if c.lastErr == err.Error() && time.Since(c.lastErrAt) < time.Minute {
		return
	}
	c.lastErr = err.Error()
	c.lastErrAt = time.Now()
	logger.Warn.Printf("任务快照同步失败：%v", err)
}

// Stats 返回概览需要的计数和全局速度，全部来自内存快照。
func (c *TorrentCache) Stats() (TorrentStats, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.ready {
		return TorrentStats{}, false
	}
	out := TorrentStats{
		DlSpeed:      c.stats.DlSpeed,
		UpSpeed:      c.stats.UpSpeed,
		DlSpeedLimit: c.stats.DlSpeedLimit,
		UpSpeedLimit: c.stats.UpSpeedLimit,
	}
	for _, t := range c.torrents {
		if t.DownloadSpeed > 0 || t.UploadSpeed > 0 {
			out.ActiveCount++
		}
		switch t.State {
		case "stoppedUP":
			out.StoppedUP++
		case "stoppedDL":
			out.StoppedDL++
		case "downloading", "forcedDL", "metaDL", "forcedMetaDL":
			out.DownloadingCount++
		case "uploading", "forcedUP":
			out.SeedingCount++
		case "stalledUP", "stalledDL":
			out.StalledCount++
		case "error", "missingFiles":
			out.ErroredCount++
		}
	}
	return out, true
}

type TorrentStats struct {
	DlSpeed          int64
	UpSpeed          int64
	DlSpeedLimit     int64
	UpSpeedLimit     int64
	ActiveCount      int
	StoppedUP        int
	StoppedDL        int
	DownloadingCount int
	SeedingCount     int
	StalledCount     int
	ErroredCount     int
}

// GetTorrents 实现与 Client.GetTorrents 相同的参数语义（filter, sort, reverse），
// 但全部在内存中完成，不再请求 qB。
func (c *TorrentCache) GetTorrents(params ...string) ([]models.QBTorrent, error) {
	var filter, sortField, reverse string
	if len(params) > 0 {
		filter = params[0]
	}
	if len(params) > 1 {
		sortField = params[1]
	}
	if len(params) > 2 {
		reverse = params[2]
	}

	c.mu.RLock()
	if !c.ready {
		c.mu.RUnlock()
		return nil, ErrCacheNotReady
	}
	out := make([]models.QBTorrent, 0, len(c.torrents))
	for _, t := range c.torrents {
		if matchTorrentFilter(t, filter) {
			out = append(out, t)
		}
	}
	c.mu.RUnlock()

	sortTorrents(out, sortField, isReverse(reverse))
	return out, nil
}

// qBittorrent 5.x TorrentFilter 的判定（src/base/bittorrent/torrentimpl.cpp）
func matchTorrentFilter(t models.QBTorrent, filter string) bool {
	switch filter {
	case "", "all":
		return true
	case "downloading":
		return inStates(t.State, "downloading", "forcedDL", "metaDL", "forcedMetaDL",
			"stalledDL", "checkingDL", "stoppedDL", "queuedDL")
	case "seeding":
		return inStates(t.State, "uploading", "forcedUP", "stalledUP", "checkingUP", "queuedUP")
	case "completed":
		return inStates(t.State, "uploading", "forcedUP", "stalledUP", "checkingUP", "stoppedUP", "queuedUP")
	case "stopped":
		return inStates(t.State, "stoppedUP", "stoppedDL", "missingFiles", "error")
	case "running":
		return !inStates(t.State, "stoppedUP", "stoppedDL", "missingFiles", "error")
	case "active":
		return t.DownloadSpeed > 0 || t.UploadSpeed > 0
	case "inactive":
		return t.DownloadSpeed <= 0 && t.UploadSpeed <= 0
	case "stalled":
		return inStates(t.State, "stalledUP", "stalledDL")
	case "stalled_uploading":
		return t.State == "stalledUP"
	case "stalled_downloading":
		return t.State == "stalledDL"
	case "checking":
		return inStates(t.State, "checkingUP", "checkingDL", "checkingResumeData")
	case "moving":
		return t.State == "moving"
	case "errored":
		return inStates(t.State, "error", "missingFiles")
	default:
		// qB 对未知 filter 静默回退为 all
		return true
	}
}

func inStates(state string, allowed ...string) bool {
	for _, s := range allowed {
		if state == s {
			return true
		}
	}
	return false
}

func isReverse(v string) bool {
	switch v {
	case "1", "yes", "on", "true", "TRUE", "True":
		return true
	}
	return false
}

// sortTorrents 复刻 /torrents/info 的排序语义：先排序再由调用方截断。
func sortTorrents(list []models.QBTorrent, field string, reverse bool) {
	if field == "" || len(list) < 2 {
		return
	}
	less := func(a, b models.QBTorrent) bool {
		switch field {
		case "added_on":
			return a.AddedOn < b.AddedOn
		case "completion_on":
			return a.CompletedOn < b.CompletedOn
		case "last_activity":
			return a.LastActivity < b.LastActivity
		case "size":
			return a.Size < b.Size
		case "downloaded":
			return a.Downloaded < b.Downloaded
		case "uploaded":
			return a.Uploaded < b.Uploaded
		case "dlspeed":
			return a.DownloadSpeed < b.DownloadSpeed
		case "upspeed":
			return a.UploadSpeed < b.UploadSpeed
		case "progress":
			return a.Progress < b.Progress
		case "ratio":
			return a.Ratio < b.Ratio
		case "num_seeds":
			return a.Seeds < b.Seeds
		case "num_leechs":
			return a.Peers < b.Peers
		case "name":
			return a.Name < b.Name
		case "state":
			return a.State < b.State
		case "category":
			return a.Category < b.Category
		case "tags":
			return a.Tags < b.Tags
		case "save_path":
			return a.SavePath < b.SavePath
		case "content_path":
			return a.ContentPath < b.ContentPath
		default:
			return false
		}
	}
	if reverse {
		sort.SliceStable(list, func(i, j int) bool { return less(list[j], list[i]) })
	} else {
		sort.SliceStable(list, func(i, j int) bool { return less(list[i], list[j]) })
	}
}

// mergeJSONObject 把 qB 的增量字段合并进已有对象，返回合并后的完整 JSON。
func mergeJSONObject(base, patch json.RawMessage) (json.RawMessage, error) {
	obj := unmarshalObject(base)
	var upd map[string]json.RawMessage
	if err := json.Unmarshal(patch, &upd); err != nil {
		return nil, err
	}
	for k, v := range upd {
		obj[k] = v
	}
	return json.Marshal(obj)
}

func mergeState(base, patch map[string]json.RawMessage) json.RawMessage {
	merged := make(map[string]json.RawMessage, len(base)+len(patch))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range patch {
		merged[k] = v
	}
	b, _ := json.Marshal(merged)
	return b
}

func unmarshalObject(body json.RawMessage) map[string]json.RawMessage {
	obj := make(map[string]json.RawMessage)
	if len(body) > 0 {
		if err := json.Unmarshal(body, &obj); err != nil {
			return make(map[string]json.RawMessage)
		}
	}
	return obj
}
