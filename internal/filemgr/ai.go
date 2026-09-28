package filemgr

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/models"
)

// ---- AI 格式化（OpenAI 兼容 /chat/completions）----

const (
	aiTimeout     = 30 * time.Second
	aiMaxNames    = 64  // 单次请求最多携带的文件名数
	aiMaxAttempts = 3   // AI 调用失败最多重试轮数，超过后降级为仅正则清洗
	aiMaxPrompt   = 4000
)

// defaultAIPrompt 内置默认提示词。{files} 替换为待格式化文件名 JSON 数组，
// {torrent} 替换为任务名
const defaultAIPrompt = `你是文件名整理助手。输入是从下载站获取的一批文件名（JSON 数组，不含扩展名）。
请为每个文件名输出一个干净、自然的显示名：
1. 去除站点水印与推广信息（域名、URL、[www.xxx.com]、@xxx、括号广告等）
2. 将点分/下划线等机器风格命名转为空格分隔的自然名称
3. 保留有价值的信息：年份、季/集数、分辨率、语言等（可用括号或简洁后缀）
4. 不要输出文件扩展名，不要包含任何目录分隔符，不要输出解释或多余文本
5. 只输出一个严格合法的 JSON 对象：{"原文件名": "新文件名", ...}，键必须与输入完全一致
文件名列表：{files}
所属任务名：{torrent}`

// activeAIChannel 返回当前可用（启用且 ID 匹配 aiActive）的通道；无则 nil
func activeAIChannel(fm models.FileManagerConfig) *models.AIChannel {
	if !fm.AIEnabled {
		return nil
	}
	for i := range fm.AIChannels {
		ch := &fm.AIChannels[i]
		if ch.Enabled && ch.ID == fm.AIActive {
			return ch
		}
	}
	// 未指定活动通道时取第一个启用的
	for i := range fm.AIChannels {
		ch := &fm.AIChannels[i]
		if ch.Enabled {
			return ch
		}
	}
	return nil
}

type aiChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type aiChatRequest struct {
	Model       string          `json:"model"`
	Messages    []aiChatMessage `json:"messages"`
	Temperature float64         `json:"temperature"`
}

type aiChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// aiFormatNames 调用通道格式化一批 base 名（不含扩展名），返回 {原名: 新名}。
// 任何失败（网络/超时/HTTP 非 200/JSON 解析失败）返回 nil，调用方降级为仅正则清洗。
func aiFormatNames(ch models.AIChannel, names []string, torrentName string) map[string]string {
	if len(names) == 0 {
		return nil
	}
	if len(names) > aiMaxNames {
		names = names[:aiMaxNames]
	}
	prompt := strings.TrimSpace(ch.Prompt)
	if prompt == "" {
		prompt = defaultAIPrompt
	}
	filesJSON, _ := json.Marshal(names)
	prompt = strings.ReplaceAll(prompt, "{files}", string(filesJSON))
	prompt = strings.ReplaceAll(prompt, "{torrent}", torrentName)

	body, _ := json.Marshal(aiChatRequest{
		Model:       ch.Model,
		Messages:    []aiChatMessage{{Role: "user", Content: prompt}},
		Temperature: 0.2,
	})
	ctx, cancel := context.WithTimeout(context.Background(), aiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(ch.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		logger.Warn.Printf("AI 格式化：构造请求失败：%v", err)
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	if ch.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+ch.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Warn.Printf("AI 格式化：请求失败（%s / %s）：%v", ch.Name, ch.Model, err)
		return nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		logger.Warn.Printf("AI 格式化：读取响应失败：%v", err)
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		logger.Warn.Printf("AI 格式化：HTTP %d（%s / %s）：%s", resp.StatusCode, ch.Name, ch.Model, truncateStr(string(data), 200))
		return nil
	}
	var cr aiChatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		logger.Warn.Printf("AI 格式化：响应 JSON 解析失败：%v", err)
		return nil
	}
	if cr.Error != nil && cr.Error.Message != "" {
		logger.Warn.Printf("AI 格式化：接口返回错误：%s", cr.Error.Message)
		return nil
	}
	if len(cr.Choices) == 0 {
		logger.Warn.Printf("AI 格式化：响应无 choices")
		return nil
	}
	return parseAIMapping(cr.Choices[0].Message.Content, names)
}

// parseAIMapping 从模型输出中提取 JSON 映射并校验。宽容处理 markdown 代码块包裹。
func parseAIMapping(content string, names []string) map[string]string {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		logger.Warn.Printf("AI 格式化：输出中未找到 JSON 对象：%s", truncateStr(content, 200))
		return nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(content[start:end+1]), &raw); err != nil {
		logger.Warn.Printf("AI 格式化：输出 JSON 解析失败：%v", err)
		return nil
	}
	requested := make(map[string]bool, len(names))
	for _, n := range names {
		requested[n] = true
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		s, ok := v.(string)
		if !ok || !requested[k] {
			continue // 非字符串值或不认识的键直接丢弃
		}
		s = strings.TrimSpace(s)
		if s == "" || strings.ContainsAny(s, `/\`) {
			logger.Debug.Printf("AI 格式化：丢弃非法输出（含目录分隔符或为空）：%q → %q", k, s)
			continue
		}
		if cleaned := sanitizeCleaned(k, s); cleaned != k {
			out[k] = cleaned
		}
	}
	return out
}

// collectAINames 收集待 AI 格式化的 base 名（正则清洗后、去扩展名、去重、截断）
func collectAINames(files []models.QBFile, custom []*regexp.Regexp) []string {
	seen := make(map[string]bool)
	var names []string
	for _, f := range files {
		segs := strings.Split(strings.Trim(f.Name, "/"), "/")
		if len(segs) == 0 {
			continue
		}
		base := segs[len(segs)-1]
		if e := extOf(base); e != "" && e != base {
			base = base[:len(base)-len(e)]
		}
		base = cleanBase(base, custom)
		if base == "" || seen[base] {
			continue
		}
		seen[base] = true
		names = append(names, base)
		if len(names) >= aiMaxNames {
			break
		}
	}
	return names
}

// extOf 取文件名扩展名（不含点）；无扩展名返回空串
func extOf(name string) string {
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	return name[i:]
}

// truncateStr 截断字符串用于日志
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// randomHex 生成 n 字节的随机 hex 串（审计 ID 用）
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", n*2, time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
