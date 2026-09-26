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
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
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
	if string(data) != "Ok." {
		return fmt.Errorf("login failed: %s", string(data))
	}
	for _, cc := range resp.Cookies() {
		if cc.Name == "SID" {
			c.cookie = "SID=" + cc.Value
			return nil
		}
	}
	if cookies := c.httpCli.Jar.Cookies(mustParse(c.baseURL)); len(cookies) > 0 {
		var parts []string
		for _, cc := range cookies {
			parts = append(parts, cc.Name+"="+cc.Value)
		}
		c.cookie = strings.Join(parts, "; ")
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
//   - filter: active/downloading/seeding/completed/paused/all 等
//   - sort:   name/size/progress/upspeed/dlspeed/added_time 等
//   - reverse: 1 或 true 表示倒序
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
	if len(params) > 0 { filter = params[0] }
	if len(params) > 1 { sort = params[1] }
	if len(params) > 2 { reverse = params[2] }
	v := url.Values{}
	if f := strings.TrimSpace(filter); f != "" && f != "all" { v.Set("filter", f) }
	if s := strings.TrimSpace(sort); s != "" { v.Set("sort", s) }
	if r := strings.TrimSpace(reverse); r != "" { v.Set("reverse", r) }
	path := "/api/v2/torrents/info"
	if len(v) > 0 { path += "?" + v.Encode() }

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

// TransferInfo 是 qBittorrent /api/v2/transfer/info 的返回
type TransferInfo struct {
	DlSpeed      int64 `json:"dl_speed"`
	UpSpeed      int64 `json:"up_speed"`
	DlSpeedLimit int64 `json:"dl_speed_limit"`
	UpSpeedLimit int64 `json:"up_speed_limit"`
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

// PauseTorrents 暂停一个或多个 torrent（hashes 逗号分隔）
func (c *Client) PauseTorrents(hashes ...string) error {
	form := url.Values{}
	form.Set("hashes", strings.Join(hashes, "|"))
	resp, err := c.do("POST", "/api/v2/torrents/pause",
		strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pause status: %d", resp.StatusCode)
	}
	return nil
}

// ResumeTorrents 恢复一个或多个 torrent
func (c *Client) ResumeTorrents(hashes ...string) error {
	form := url.Values{}
	form.Set("hashes", strings.Join(hashes, "|"))
	resp, err := c.do("POST", "/api/v2/torrents/resume",
		strings.NewReader(form.Encode()),
		"application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("resume status: %d", resp.StatusCode)
	}
	return nil
}

// RenameFile 通过 qB API 重命名 torrent 内的文件/子目录（qB 4.6+）。
// oldPath/newPath 是 torrent 内的相对路径（例："Movie/Video.mkv" -> "Video.mkv"）。
// 调用前应先暂停 torrent，避免做种中断。
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
func (c *Client) AddTorrent(torrentData []byte, savePath, category, tags string, uploadLimitKB int) error {
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
	writeFormField("paused", "false", false, "", nil)
	buf.WriteString("--" + boundary + "--\r\n")

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
		go func() {
			time.Sleep(3 * time.Second)
			c.applyLatestUploadLimit(uploadLimitKB)
		}()
	}
	return nil
}

func (c *Client) applyLatestUploadLimit(uploadLimitKB int) {
	list, err := c.GetTorrents("downloading", "", "")
	if err != nil || len(list) == 0 {
		return
	}
	for _, t := range list {
		if t.State == "downloading" || t.State == "stalledDL" {
			_ = c.SetUploadLimit(t.Hash, int64(uploadLimitKB)*1024)
			return
		}
	}
}
