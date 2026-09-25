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

// New 创建客户端。apiKey 非空时优先走 Bearer 认证（qBittorrent v5.2.0+），
// 否则走 cookie 会话方式，user/pass 必填。
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

// usesAPIKey 判断是否走 Bearer 模式
func (c *Client) usesAPIKey() bool {
	return c.apiKey != ""
}

func (c *Client) do(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	var req *http.Request
	var err error

	if c.usesAPIKey() {
		// Bearer 模式：无状态，直接带 Authorization 头，跳过 login/cookie
		req, err = http.NewRequest(method, c.baseURL+path, body)
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
		req, err = http.NewRequest(method, c.baseURL+path, body)
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

	// cookie 模式下遇到 401/403，重新 login 重试一次
	if !c.usesAPIKey() && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized) {
		c.cookie = ""
		_ = resp.Body.Close()
		if err := c.login(); err != nil {
			return nil, err
		}
		req2, err := http.NewRequest(method, c.baseURL+path, body)
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

func mustParse(s string) *url.URL {
	u, _ := url.Parse(s)
	return u
}

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

// GetTorrents 获取 torrent 列表
func (c *Client) GetTorrents() ([]models.QBTorrent, error) {
	resp, err := c.do("GET", "/api/v2/torrents/info", nil, "")
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
		return nil, err
	}
	return list, nil
}

// GetTorrentFiles 获取 torrent 的文件列表
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

// SetUploadLimit 对单个 torrent 设置上传速度限制（bytes/s）
// limit 为 -1 表示无限制
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

// AddTorrent 上传 torrent 文件
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
	writeFormField("skip_checking", "true", false, "", nil)
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
		logger.Warn.Printf("AddTorrent status=%d body=%s", resp.StatusCode, string(b))
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
	list, err := c.GetTorrents()
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
