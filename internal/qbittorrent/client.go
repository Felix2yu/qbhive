package qbittorrent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/models"
)

type Client struct {
	baseURL string
	user    string
	pass    string
	apiKey  string
	httpCli *http.Client
	cookie  string
}

func New(baseURL, user, pass, apiKey string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		user:    user,
		pass:    pass,
		apiKey:  strings.TrimSpace(apiKey),
		httpCli: &http.Client{Timeout: 15 * time.Second, Jar: jar},
	}
}

// SetCredentials 热替换 qB 连接参数（配置改动时调用，避免重建 client 指针）
func (c *Client) SetCredentials(baseURL, user, pass, apiKey string) {
	c.baseURL = strings.TrimRight(baseURL, "/")
	c.user = user
	c.pass = pass
	c.apiKey = strings.TrimSpace(apiKey)
	c.cookie = "" // 换凭据后必须重新登录
}

func (c *Client) usesAPIKey() bool { return c.apiKey != "" }

func (c *Client) do(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	resp, err := c._doRaw(method, path, body, contentType)
	if err != nil {
		return nil, err
	}

	// qBittorrent 在 cookie 过期时**不一定返回 401/403**，可能返回 200 + `"Fails."` / `"Unauthorized"` 字符串
	// 这里统一嗅探 body：如果看起来是认证失败，自动重登 + 重试一次
	if path != "/api/v2/auth/login" && c._looksAuthFailure(resp) {
		_ = resp.Body.Close()
		logger.Warn.Printf("qB 鉴权失败（状态=%d，响应片段），重新登录后重试 %s %s", resp.StatusCode, method, path)
		c.cookie = ""
		if c.login() != nil {
			return nil, fmt.Errorf("auto re-login failed")
		}
		return c._doRaw(method, path, body, contentType)
	}
	return resp, nil
}

// _looksAuthFailure 嗅探 qBittorrent 响应体前 200 字节，判断是否为认证失败字符串
// qBittorrent 在 cookie / API key 失效时会返回 200 OK + 类似 "Fails." / "Unauthorized" 这种纯 JSON string 值
// 这会让我们的 json.Unmarshal(..., &[]models.QBTorrent) 报 "invalid character '"' looking for beginning of value"
func (c *Client) _looksAuthFailure(resp *http.Response) bool {
	// 先嗅探 200 字节，同时把完整 body 用 tee 存到 buffer 里，让后续 ReadAll 正常读到
	peek := make([]byte, 200)
	n, _ := io.ReadFull(resp.Body, peek)
	if n == 0 {
		return false
	}
	sniff := strings.ToLower(string(peek[:n]))
	// 认证失败特征：以 " 开头（JSON string）且包含 fails / unauthorized / access / forbidden
	if peek[0] == '"' && (strings.Contains(sniff, "fails") || strings.Contains(sniff, "unauthorized") || strings.Contains(sniff, "access") || strings.Contains(sniff, "forbidden")) {
		// 把 peek 的内容 + 剩余 body 拼回去
		rest, _ := io.ReadAll(resp.Body)
		resp.Body = io.NopCloser(bytes.NewReader(append(peek[:n], rest...)))
		return true
	}
	// 把 peek 拼回去让后续正常 Read
	rest, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(append(peek[:n], rest...)))
	return false
}

// _doRaw 执行一次 HTTP 请求（含 cookie 自动登录 / 401-403 重登），返回原始 response
func (c *Client) _doRaw(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	// body 可能是一次性 Reader（如 POST 表单），如果为重登需要重放则必须可 rewind
	// 这里用 teeReader 先把 body 整体读到内存，后续两次请求都从内存读
	var bodyBytes []byte
	if body != nil {
		b, err := io.ReadAll(body)
		if err != nil {
			return nil, err
		}
		bodyBytes = b
	}
	makeBody := func() io.Reader {
		if bodyBytes == nil {
			return nil
		}
		return bytes.NewReader(bodyBytes)
	}

	var req *http.Request
	var err error
	if c.usesAPIKey() {
		req, err = http.NewRequest(method, c.baseURL+path, makeBody())
		if err != nil {
			return nil, err
		}
		// qBittorrent WebAPI 密钥只认 X-Webapi-Key 头（Authorization: Bearer 会 403）
		req.Header.Set("X-Webapi-Key", c.apiKey)
	} else {
		if c.cookie == "" {
			if err := c.login(); err != nil {
				return nil, err
			}
		}
		req, err = http.NewRequest(method, c.baseURL+path, makeBody())
		if err != nil {
			return nil, err
		}
		req.Header.Set("Cookie", c.cookie)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpCli.Do(req)
	if err != nil {
		if !c.usesAPIKey() {
			c.cookie = ""
		}
		return nil, err
	}
	if !c.usesAPIKey() && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized) {
		c.cookie = ""
		_ = resp.Body.Close()
		if err := c.login(); err != nil {
			return nil, err
		}
		req2, err := http.NewRequest(method, c.baseURL+path, makeBody())
		if err != nil {
			return nil, err
		}
		req2.Header.Set("Cookie", c.cookie)
		if contentType != "" {
			req2.Header.Set("Content-Type", contentType)
		}
		resp, err = c.httpCli.Do(req2)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func (c *Client) login() error {
	form := url.Values{}
	form.Set("username", c.user)
	form.Set("password", c.pass)
	resp, err := c.httpCli.Post(c.baseURL+"/api/v2/auth/login",
		"application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	// qB <= 5.2：200 + "Ok." + SID cookie
	// qB 5.3+：204 + 空 body + QBT_SID_<端口> cookie
	body := strings.TrimSpace(string(data))
	if body != "" && body != "Ok." {
		return fmt.Errorf("login failed: %s", body)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("login failed: HTTP %d", resp.StatusCode)
	}

	// 统一从 cookie jar 取会话（cookie 名随 qB 版本/端口变化）
	if cookies := c.httpCli.Jar.Cookies(mustParse(c.baseURL)); len(cookies) > 0 {
		var parts []string
		for _, cc := range cookies {
			parts = append(parts, cc.Name+"="+cc.Value)
		}
		c.cookie = strings.Join(parts, "; ")
		return nil
	}
	for _, cc := range resp.Cookies() {
		if strings.Contains(strings.ToUpper(cc.Name), "SID") {
			c.cookie = cc.Name + "=" + cc.Value
			return nil
		}
	}
	return nil
}

func (c *Client) GetCookie() string { return c.cookie }

func mustParse(s string) *url.URL { u, _ := url.Parse(s); return u }

func (c *Client) TestConnection() error {
	resp, err := c.do("GET", "/api/v2/app/version", nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	return nil
}

// GetTorrents 拉 torrent 列表，透传给 qBittorrent 原生参数：
//   - filter: 5.x 合法值 active/downloading/seeding/completed/stopped/running/all 等
//   - sort:   name/size/progress/upspeed/dlspeed/added_time 等
//   - reverse: 1 或 true 表示倒序
//
// 所有参数都是可选的；filter/sort 为空或 all 时不拼 query。
// 注意：qBittorrent 原生不支持 limit/offset，分页由上层（server 或前端）负责。
// GetTorrents 拉 torrent 列表。参数顺序：filter, sort, reverse；都是可选的，
// 用 variadic 兼容旧调用点。支持值见 qBittorrent WebUI API 文档。
// qBittorrent 认证失败/参数错误的特征：返回 200 OK + 纯 JSON string 值而不是数组/对象。
// 比如 ""Fails."" / ""Unauthorized"" / ""Invalid sort field""。
// 这种情况下前端拿到的是 {"success":false,"message":"invalid character '"' looking for beginning of value"}
// 完全无法分辨；这里我们在 JSON parse 失败时自动重登 + 重试一次。
func isQBErrorString(data []byte) bool {
	s := strings.TrimSpace(string(data))
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		// 看起来像 JSON string。进一步判断：去掉首尾引号后如果是纯字符串且以 / \ " 之外的字符开头，
		// 大概率是 qBittorrent 返回的错误字符串（"Fails." / "Unauthorized" / "Invalid..." 等）。
		inner := s[1 : len(s)-1]
		// 排除 JSON 转义引号开头 / 空字符串
		if len(inner) == 0 {
			return false
		}
		return true
	}
	return false
}

func (c *Client) GetTorrents(params ...string) ([]models.QBTorrent, error) {
	var filter, sort, reverse string
	if len(params) > 0 {
		filter = params[0]
	}
	if len(params) > 1 {
		sort = params[1]
	}
	if len(params) > 2 {
		reverse = params[2]
	}
	v := url.Values{}
	if f := strings.TrimSpace(filter); f != "" && f != "all" {
		v.Set("filter", f)
	}
	if s := strings.TrimSpace(sort); s != "" {
		v.Set("sort", s)
	}
	if r := strings.TrimSpace(reverse); r != "" {
		v.Set("reverse", r)
	}
	path := "/api/v2/torrents/info"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}

	parseOnce := func(data []byte) ([]models.QBTorrent, error) {
		var list []models.QBTorrent
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, err
		}
		return list, nil
	}

	runOnce := func() ([]models.QBTorrent, []byte, error) {
		resp, err := c.do("GET", path, nil, "")
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, nil, err
		}
		list, err := parseOnce(data)
		return list, data, err
	}

	// 第一次尝试
	list, data, err := runOnce()
	if err != nil {
		// 非 JSON 错误：判断是不是 qBittorrent 返回了错误字符串
		if data != nil && isQBErrorString(data) {
			logger.Warn.Printf("GetTorrents: qB 返回错误字符串 %q（filter=%q sort=%q reverse=%q），强制重新登录并重试",
				strings.TrimSpace(string(data)), filter, sort, reverse)
			c.cookie = ""
			if err2 := c.login(); err2 != nil {
				return nil, fmt.Errorf("initial parse error: %w; auto re-login failed: %v", err, err2)
			}
			// 重试一次
			list2, _, err2 := runOnce()
			if err2 != nil {
				logger.Warn.Printf("GetTorrents 重试仍失败：%v", err2)
				return nil, fmt.Errorf("parse error (filter=%q sort=%q reverse=%q): %v (qB raw body: %s)", filter, sort, reverse, err2, string(data[:min(len(data), 200)]))
			}
			return list2, nil
		}
		// 其他 parse error（不是 qB 认证/参数错误）
		logger.Warn.Printf("GetTorrents JSON 解析失败 filter=%q sort=%q reverse=%q 响应前 200 字节=%q err=%v",
			filter, sort, reverse, string(data[:min(len(data), 200)]), err)
		return nil, err
	}
	return list, nil
}

// TransferInfo 是 qBittorrent /api/v2/transfer/info 的返回。
// 字段名严格按 qB 5.x 源码（transfercontroller.cpp）：dl_info_speed / up_info_speed
// / dl_rate_limit / up_rate_limit；qB 从不返回 dl_speed / up_speed 这类 key。
type TransferInfo struct {
	DlSpeed      int64 `json:"dl_info_speed"`
	UpSpeed      int64 `json:"up_info_speed"`
	DlSpeedLimit int64 `json:"dl_rate_limit"`
	UpSpeedLimit int64 `json:"up_rate_limit"`
}

// GetTransferInfo 拉全局速度信息（很轻量，不走 info 大接口）
func (c *Client) GetTransferInfo() (*TransferInfo, error) {
	resp, err := c.do("GET", "/api/v2/transfer/info", nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var ti TransferInfo
	if err := json.Unmarshal(data, &ti); err != nil {
		return nil, err
	}
	return &ti, nil
}

// MaindataResponse 是 /api/v2/sync/maindata 的返回。
// torrents 里只包含与上次 rid 相比有变化的字段，调用方必须按 hash 合并，
// 不能直接整体反序列化成 QBTorrent（缺失字段会被当成零值）。
type MaindataResponse struct {
	Rid             int                        `json:"rid"`
	FullUpdate      bool                       `json:"full_update"`
	Torrents        map[string]json.RawMessage `json:"torrents"`
	TorrentsRemoved []string                   `json:"torrents_removed"`
	ServerState     map[string]json.RawMessage `json:"server_state"`
}

// GetMaindata 拉 qBittorrent 增量同步数据。
//
// 必须走 cookie 会话：rid 游标绑定在 WebUI 会话上，用 API key 每次都会返回全量
// （已在本机 qB 5.3 实测），增量因此失效。
func (c *Client) GetMaindata(rid int) (*MaindataResponse, error) {
	path := "/api/v2/sync/maindata?rid=" + strconv.Itoa(rid)
	data, err := c.cookieGet(path)
	if err != nil {
		// 登录本身失败时不再重试：错误凭据连续撞 qB 的失败次数会触发 IP 封禁
		if isLoginError(err) {
			return nil, err
		}
		c.cookie = ""
		if loginErr := c.login(); loginErr != nil {
			return nil, fmt.Errorf("maindata: %w (re-login: %v)", err, loginErr)
		}
		if data, err = c.cookieGet(path); err != nil {
			return nil, err
		}
	}
	// qB 会话过期时可能返回 200 + "Fails."，与 do() 的嗅探保持一致
	if isQBErrorString(data) {
		c.cookie = ""
		if loginErr := c.login(); loginErr != nil {
			return nil, fmt.Errorf("maindata auth: %w", loginErr)
		}
		if data, err = c.cookieGet(path); err != nil {
			return nil, err
		}
	}
	var resp MaindataResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("maindata parse: %w (body: %.80q)", err, string(data))
	}
	return &resp, nil
}

// cookieGet 只用 cookie 会话请求，忽略 API key。
func (c *Client) cookieGet(path string) ([]byte, error) {
	if c.cookie == "" {
		if err := c.login(); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", c.cookie)
	resp, err := c.httpCli.Do(req)
	if err != nil {
		c.cookie = ""
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.cookie = ""
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return data, nil
}

func (c *Client) GetTorrentFiles(hash string) ([]models.QBFile, error) {
	resp, err := c.do("GET", "/api/v2/torrents/files?hash="+url.QueryEscape(hash), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var list []models.QBFile
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (c *Client) SetUploadLimit(hash string, limitBytesPerSec int64) error {
	form := url.Values{}
	form.Set("hashes", hash)
	form.Set("limit", strconv.FormatInt(limitBytesPerSec, 10))
	resp, err := c.do("POST", "/api/v2/torrents/setUploadLimit",
		strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("setUploadLimit status: %d", resp.StatusCode)
	}
	return nil
}

// GetUploadLimit 读取单个 torrent 的当前上传限速（字节/秒；qB 返回 -1 表示不限速）。
// 对应 qBittorrent 5.x 的 GET /api/v2/torrents/uploadLimit?hashes=...
func (c *Client) GetUploadLimit(hash string) (int64, error) {
	resp, err := c.do("GET", "/api/v2/torrents/uploadLimit?hashes="+url.QueryEscape(hash), nil, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	// 返回形如 {"<hash>": 1048576}；未找到时 map 为空
	var m map[string]int64
	if err := json.Unmarshal(data, &m); err != nil {
		return 0, err
	}
	if v, ok := m[hash]; ok {
		return v, nil
	}
	return 0, fmt.Errorf("uploadLimit: hash %s not found in qB response", hash)
}

// StopTorrents 停止一个或多个 torrent（hashes 用 | 分隔）。
// qBittorrent 5.x 端点为 /api/v2/torrents/stop（4.x 的 /pause 已移除）。
func (c *Client) StopTorrents(hashes ...string) error {
	form := url.Values{}
	form.Set("hashes", strings.Join(hashes, "|"))
	resp, err := c.do("POST", "/api/v2/torrents/stop",
		strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stop status: %d", resp.StatusCode)
	}
	return nil
}

// StartTorrents 启动一个或多个 torrent。
// qBittorrent 5.x 端点为 /api/v2/torrents/start（4.x 的 /resume 已移除）。
func (c *Client) StartTorrents(hashes ...string) error {
	form := url.Values{}
	form.Set("hashes", strings.Join(hashes, "|"))
	resp, err := c.do("POST", "/api/v2/torrents/start",
		strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("start status: %d", resp.StatusCode)
	}
	return nil
}

// Recheck 强制校验：让 qB 重新扫描磁盘文件与 torrent 元数据是否一致。
// 归档回退把文件挪到新位置后，必须让 qB 重新对上，否则任务会一直停在 missingFiles。
func (c *Client) Recheck(hashes ...string) error {
	form := url.Values{}
	form.Set("hashes", strings.Join(hashes, "|"))
	resp, err := c.do("POST", "/api/v2/torrents/recheck",
		strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("recheck status: %d", resp.StatusCode)
	}
	return nil
}

// DeleteTorrents 从 qB 移除任务。deleteFiles=true 会连同磁盘文件一起删除（不可逆）。
// 调用方必须自己承担护栏责任：本方法不做任何路径校验。
func (c *Client) DeleteTorrents(hashes []string, deleteFiles bool) error {
	if len(hashes) == 0 {
		return fmt.Errorf("deleteTorrents: hashes 为空")
	}
	form := url.Values{}
	form.Set("hashes", strings.Join(hashes, "|"))
	form.Set("deleteFiles", strconv.FormatBool(deleteFiles))
	resp, err := c.do("POST", "/api/v2/torrents/delete",
		strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete status=%d body=%s", resp.StatusCode, string(b))
	}
	return nil
}

// SetCategory 设置任务分类（空字符串表示取消分类）。
func (c *Client) SetCategory(hashes []string, category string) error {
	if len(hashes) == 0 {
		return fmt.Errorf("setCategory: hashes 为空")
	}
	form := url.Values{}
	form.Set("hashes", strings.Join(hashes, "|"))
	form.Set("category", category)
	resp, err := c.do("POST", "/api/v2/torrents/setCategory",
		strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("setCategory status=%d body=%s", resp.StatusCode, string(b))
	}
	return nil
}

// GetTorrentInfo 按 hash 精确取任务信息（不拉全量列表）。
func (c *Client) GetTorrentInfo(hashes []string) ([]models.QBTorrent, error) {
	if len(hashes) == 0 {
		return nil, fmt.Errorf("getTorrentInfo: hashes 为空")
	}
	v := url.Values{}
	v.Set("hashes", strings.Join(hashes, "|"))
	resp, err := c.do("GET", "/api/v2/torrents/info?"+v.Encode(), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var list []models.QBTorrent
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("getTorrentInfo 解析失败：%v（前 200 字节：%.200s）", err, string(data))
	}
	return list, nil
}

// SetTags 覆盖式设置标签。qB 5.x 没有「设置标签」端点，只能先移除当前标签再追加新的；
// 差集在这里算，避免把用户的旧标签全部抹掉又加上重复项。
func (c *Client) SetTags(hashes []string, tags string) error {
	if len(hashes) == 0 {
		return fmt.Errorf("setTags: hashes 为空")
	}
	// 期望的标签集合（去重，保持顺序）
	want := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	for _, t := range strings.Split(tags, ",") {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		want = append(want, t)
	}

	list, err := c.GetTorrentInfo(hashes)
	if err != nil {
		return fmt.Errorf("读取当前标签失败：%w", err)
	}
	have := make(map[string]bool, 4)
	for _, t := range list {
		for _, tag := range strings.Split(t.Tags, ",") {
			if tag = strings.TrimSpace(tag); tag != "" {
				have[tag] = true
			}
		}
	}
	var toAdd, toDel []string
	for _, t := range want {
		if !have[t] {
			toAdd = append(toAdd, t)
		}
	}
	for t := range have {
		if !seen[t] {
			toDel = append(toDel, t)
		}
	}
	if len(toAdd) > 0 {
		if err := c.postTags("/api/v2/torrents/addTags", hashes, toAdd); err != nil {
			return err
		}
	}
	if len(toDel) > 0 {
		if err := c.postTags("/api/v2/torrents/removeTags", hashes, toDel); err != nil {
			return err
		}
	}
	return nil
}

// postTags 提交 addTags / removeTags 请求（两个端点的参数形式一致）
func (c *Client) postTags(endpoint string, hashes, tags []string) error {
	form := url.Values{}
	form.Set("hashes", strings.Join(hashes, "|"))
	form.Set("tags", strings.Join(tags, ","))
	resp, err := c.do("POST", endpoint, strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s status=%d body=%s", endpoint[strings.LastIndex(endpoint, "/")+1:], resp.StatusCode, string(b))
	}
	return nil
}

// RenameFile 通过 qB API 重命名 torrent 内的文件/子目录（qBittorrent 5.x）。
// oldPath/newPath 是 torrent 内的相对路径（例："Movie/Video.mkv" -> "Video.mkv"）。
// 调用前应先停止 torrent，避免做种中断。
func (c *Client) RenameFile(hash, oldPath, newPath string) error {
	form := url.Values{}
	form.Set("hash", hash)
	form.Set("oldPath", oldPath)
	form.Set("newPath", newPath)
	resp, err := c.do("POST", "/api/v2/torrents/renameFile",
		strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("renameFile status=%d body=%s", resp.StatusCode, string(b))
	}
	return nil
}
func (c *Client) AddTorrent(torrentData []byte, savePath, category, tags string, uploadLimitKB int, stopped bool) error {
	var buf bytes.Buffer
	boundary := "qbhive"
	writeFormField := func(name, value string, isFile bool, filename string, fileData []byte) {
		buf.WriteString("--" + boundary + "\r\n")
		if isFile {
			buf.WriteString(`Content-Disposition: form-data; name="` + name + `"; filename="` + filename + `"` + "\r\n")
			buf.WriteString("Content-Type: application/x-bittorrent\r\n\r\n")
			buf.Write(fileData)
		} else {
			buf.WriteString(`Content-Disposition: form-data; name="` + name + `"` + "\r\n\r\n")
			buf.WriteString(value)
		}
		buf.WriteString("\r\n")
	}
	writeFormField("torrents", "", true, "qbhive.torrent", torrentData)
	if savePath != "" {
		writeFormField("savepath", savePath, false, "", nil)
	}
	if category != "" {
		writeFormField("category", category, false, "", nil)
	}
	if tags != "" {
		writeFormField("tags", tags, false, "", nil)
	}
	// 不设置 skip_checking：让 qBittorrent 自己决定是否需要 hash check。
	// 如果磁盘已有同名但大小不匹配的残留文件（常见于重复添加或中途放弃的下载），
	// skip_checking=true 会导致 qB 跳过 hash check 后陷入 fast resume rejected → 报"丢失文件"。
	// 让 qB 正常做 hash check，它会自动识别不匹配文件并重新下载。
	// 注意：qBittorrent 5.x 的 /torrents/add 只读 "stopped" 参数（4.x 的 "paused"
	// 已被移除且会被静默忽略，见 qB issue #22766）。
	stoppedStr := "false"
	if stopped {
		stoppedStr = "true"
	}
	writeFormField("stopped", stoppedStr, false, "", nil)
	buf.WriteString("--" + boundary + "--\r\n")

	// 限速必须下发到「刚添加的那个任务」上，先记下添加前已有哪些 hash，
	// 添加后用差集精确定位新增项。放在 do 之前采样，避免把新任务算进基线里。
	var knownHashes map[string]bool
	if uploadLimitKB > 0 {
		knownHashes = c.torrentHashes()
	}

	resp, err := c.do("POST", "/api/v2/torrents/add", &buf,
		"multipart/form-data; boundary="+boundary)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		logger.Warn.Printf("添加种子失败 status=%d body=%s", resp.StatusCode, string(b))
	}
	if uploadLimitKB > 0 {
		go c.applyUploadLimitToAdded(knownHashes, uploadLimitKB)
	}
	return nil
}

// torrentHashes 返回当前全部任务的 hash 集合；拉取失败时返回 nil（调用方据此降级）
func (c *Client) torrentHashes() map[string]bool {
	list, err := c.GetTorrents("", "", "")
	if err != nil {
		logger.Warn.Printf("获取任务列表失败，无法为新增任务精确定位：%v", err)
		return nil
	}
	m := make(map[string]bool, len(list))
	for _, t := range list {
		m[t.Hash] = true
	}
	return m
}

// 定位新增任务的轮询参数。拆成变量是为了让测试能缩短等待，
// 生产默认 15 秒足够覆盖 qB 把新任务登记进列表的延迟。
var (
	uploadLimitLookupTimeout  = 15 * time.Second
	uploadLimitLookupInterval = 500 * time.Millisecond
)

// applyUploadLimitToAdded 把上传限速下发给本次新增的那个任务。
//
// 旧实现是 sleep 3s 后对 filter=downloading 列表里「第一个」任务限速，两个硬伤：
//  1. 多个订阅源并发添加时，限速必然打到别人头上；
//  2. 规则配置了「添加后停止」的任务不在 downloading 过滤内，限速直接丢失。
//
// 改为按 hash 差集定位：与添加前的快照比对，只认本次多出来的任务，
// 与它是否处于 downloading 无关。qB 的 /torrents/add 不返回 hash，
// 只能这样反查；差集有多个时取 added_on 最新的一条。
func (c *Client) applyUploadLimitToAdded(before map[string]bool, uploadLimitKB int) {
	if before == nil {
		logger.Warn.Printf("无法定位新增任务，跳过上传限速 %d KB/s（差集基线缺失）", uploadLimitKB)
		return
	}
	deadline := time.Now().Add(uploadLimitLookupTimeout)
	for time.Now().Before(deadline) {
		list, err := c.GetTorrents("", "added_on", "true")
		if err == nil {
			var hash string
			var maxAdded int64
			for _, t := range list {
				if before[t.Hash] {
					continue
				}
				if t.AddedOn > maxAdded {
					maxAdded = t.AddedOn
					hash = t.Hash
				}
			}
			if hash != "" {
				if err := c.SetUploadLimit(hash, int64(uploadLimitKB)*1024); err != nil {
					logger.Warn.Printf("设置新增任务上传限速失败 hash=%s：%v", hash, err)
				} else {
					logger.Info.Printf("已为新增任务 %s 设置上传限速 %d KB/s", hash, uploadLimitKB)
				}
				return
			}
		}
		time.Sleep(uploadLimitLookupInterval)
	}
	logger.Warn.Printf("新增任务已提交但 %s 内未在任务列表中出现，跳过上传限速 %d KB/s",
		uploadLimitLookupTimeout, uploadLimitKB)
}
