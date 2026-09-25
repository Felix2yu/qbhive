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
	baseURL  string
	user     string
	pass     string
	httpCli  *http.Client
	cookie   string
}

func New(baseURL, user, pass string) *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		user:    user,
		pass:    pass,
		httpCli: &http.Client{Timeout: 15 * time.Second, Jar: jar},
	}
}

func (c *Client) do(method, path string, body io.Reader, contentType string) (*http.Response, error) {
	if c.cookie == "" {
		if err := c.login(); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", c.cookie)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpCli.Do(req)
	if err != nil {
		c.cookie = ""
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		c.cookie = ""
		// 重试一次
		_ = resp.Body.Close()
		if err := c.login(); err != nil {
			return nil, err
		}
		req.Header.Set("Cookie", c.cookie)
		resp, err = c.httpCli.Do(req)
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
	// 从 Set-Cookie 解析 SID
	for _, cc := range resp.Cookies() {
		if cc.Name == "SID" {
			c.cookie = "SID=" + cc.Value
			return nil
		}
	}
	// 有些版本 cookie jar 会自动保存
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
	buf.WriteString("--bthive\r\n")
	buf.WriteString(`Content-Disposition: form-data; name="torrents"; filename="bthive.torrent"` + "\r\n")
	buf.WriteString("Content-Type: application/x-bittorrent\r\n\r\n")
	buf.Write(torrentData)
	buf.WriteString("\r\n")

	if savePath != "" {
		buf.WriteString(`--bthive` + "\r\n")
		buf.WriteString(`Content-Disposition: form-data; name="savepath"` + "\r\n\r\n")
		buf.WriteString(savePath + "\r\n")
	}
	if category != "" {
		buf.WriteString(`--bthive` + "\r\n")
		buf.WriteString(`Content-Disposition: form-data; name="category"` + "\r\n\r\n")
		buf.WriteString(category + "\r\n")
	}
	if tags != "" {
		buf.WriteString(`--bthive` + "\r\n")
		buf.WriteString(`Content-Disposition: form-data; name="tags"` + "\r\n\r\n")
		buf.WriteString(tags + "\r\n")
	}
	// skip_checking = true, paused = false
	buf.WriteString(`--bthive` + "\r\n")
	buf.WriteString(`Content-Disposition: form-data; name="skip_checking"` + "\r\n\r\n")
	buf.WriteString("true\r\n")

	buf.WriteString(`--bthive` + "\r\n")
	buf.WriteString(`Content-Disposition: form-data; name="paused"` + "\r\n\r\n")
	buf.WriteString("false\r\n")

	buf.WriteString("--bthive--\r\n")

	resp, err := c.do("POST", "/api/v2/torrents/add", &buf, "multipart/form-data; boundary=bthive")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		logger.Warn.Printf("AddTorrent status=%d body=%s", resp.StatusCode, string(b))
	}

	// 如果设置了上传限速，延迟应用（等 torrent 被加入后才能通过 hash 设置）
	if uploadLimitKB > 0 {
		// 延迟几秒后尝试根据最近添加的 torrent 查找并应用
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
	// 简单做法：对最近几个正在下载的 torrent 应用限速
	// 实际可通过 tags 或 category 识别，这里取前一个 DOWNLOADING
	for _, t := range list {
		if t.State == "downloading" || t.State == "stalledDL" {
			_ = c.SetUploadLimit(t.Hash, int64(uploadLimitKB)*1024)
			return
		}
	}
}
