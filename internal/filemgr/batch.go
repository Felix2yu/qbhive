package filemgr

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/models"
)

// ---- 批量重命名（模板）----
//
// 纯手动触发（「文件」页）：选任务 → 输入模板 → 预览 → 执行。
// 只重命名文件 base 段（不动目录段），复用 clean pass 的 applyRenames 管线
// （Stop → 批量 renameFile → Start）与 sanitizeCleaned 护栏；
// 冲突策略与 buildCleanPlan 完全一致：计划内目标去重 + 磁盘预检，
// 目标已存在加 _hash8 兜底，仍冲突跳过，任何情况下不覆盖已有文件。

// batchTemplateVars 模板支持的全部变量（供校验与前端提示）
var batchTemplateVars = []string{"{date}", "{type}", "{index}", "{orig}", "{title}"}

// batchPlaceholderRe 提取模板中的占位符
var batchPlaceholderRe = regexp.MustCompile(`\{([a-zA-Z]+)\}`)

// batchTypeMap 扩展名 → 类型分类（{type} 变量取值）
var batchTypeMap = map[string]string{
	"mkv": "video", "mp4": "video", "avi": "video", "mov": "video", "wmv": "video",
	"flv": "video", "ts": "video", "m2ts": "video", "rmvb": "video", "webm": "video",
	"mpg": "video", "mpeg": "video", "vob": "video",
	"mp3": "audio", "flac": "audio", "wav": "audio", "ape": "audio", "m4a": "audio",
	"ogg": "audio", "opus": "audio", "wma": "audio", "aac": "audio",
	"jpg": "image", "jpeg": "image", "png": "image", "gif": "image", "webp": "image",
	"bmp": "image", "svg": "image", "tif": "image", "tiff": "image",
	"zip": "archive", "rar": "archive", "7z": "archive", "tar": "archive",
	"gz": "archive", "bz2": "archive", "xz": "archive", "zst": "archive",
	"pdf": "doc", "epub": "doc", "mobi": "doc", "azw3": "doc", "doc": "doc",
	"docx": "doc", "xls": "doc", "xlsx": "doc", "ppt": "doc", "pptx": "doc",
	"txt": "doc", "md": "doc", "nfo": "doc",
}

// batchTypeOf 按扩展名返回类型分类，未命中归 other
func batchTypeOf(ext string) string {
	t := batchTypeMap[strings.ToLower(strings.TrimPrefix(ext, "."))]
	if t == "" {
		return "other"
	}
	return t
}

// validateBatchTemplate 校验模板合法性：
//   - 非空、长度 ≤256 字符
//   - 全部占位符必须属于支持集合（未知变量直接报错，避免静默展开成字面量）
//   - 至少包含 {index} 或 {orig} 之一——缺少变化变量的模板会让所有文件
//     撞到同一个目标名（虽有 _hash8 兜底但产出全是残名，等于误操作）
func validateBatchTemplate(tpl string) error {
	tpl = strings.TrimSpace(tpl)
	if tpl == "" {
		return fmt.Errorf("模板不能为空")
	}
	if len(tpl) > 256 {
		return fmt.Errorf("模板过长（%d 字符，上限 256）", len(tpl))
	}
	hasVarying := false
	for _, m := range batchPlaceholderRe.FindAllStringSubmatch(tpl, -1) {
		name := m[1]
		switch name {
		case "date", "type", "index", "orig", "title":
			if name == "index" || name == "orig" {
				hasVarying = true
			}
		default:
			return fmt.Errorf("未知变量 {%s}，支持：%s", name, strings.Join(batchTemplateVars, " "))
		}
	}
	if !hasVarying {
		return fmt.Errorf("模板须包含 {index} 或 {orig} 之一，否则所有文件会重名")
	}
	return nil
}

// expandBatchTemplate 展开单个文件名模板
func expandBatchTemplate(tpl, orig, title, dateStr, typeStr string, index int) string {
	return strings.NewReplacer(
		"{date}", dateStr,
		"{type}", typeStr,
		"{index}", strconv.Itoa(index),
		"{orig}", orig,
		"{title}", title,
	).Replace(tpl)
}

// batchDateOf 推导 {date} 取值：完成时间 → 添加时间 → 当前时间（兜底）
func batchDateOf(t models.QBTorrent) string {
	ts := t.CompletedOn
	if ts <= 0 {
		ts = t.AddedOn
	}
	if ts <= 0 {
		ts = time.Now().Unix()
	}
	return time.Unix(ts, 0).Format("2006-01-02")
}

// buildBatchPlan 按模板为任务内全部文件生成重命名计划。
//   - 仅处理文件 base 段（扩展名保留原样），目录段不动——批量重命名的语义是
//     归一化文件名，动目录会连带子树、放大误操作面
//   - {index} 按 GetTorrentFiles 返回顺序 1 起编（全部文件，含无变化的），
//     保证序号与任务文件列表稳定对应
//   - 目标名经 sanitizeCleaned 护栏（非空 / 非法字符替换 / 长度 / 与原名相同跳过）
//   - 计划内目标去重 + 磁盘冲突预检，与 buildCleanPlan 同策略（绝不覆盖）
func buildBatchPlan(files []models.QBFile, t models.QBTorrent, template string) []cleanRename {
	suffix := t.Hash
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	dateStr := batchDateOf(t)
	title := strings.TrimSpace(t.Name)

	type entry struct {
		old, new string
	}
	var entries []entry
	seenOld := make(map[string]bool)

	for i, f := range files {
		if f.Name == "" {
			continue
		}
		dir := path.Dir(f.Name)
		base := path.Base(f.Name)
		if base == "" || base == "." || base == "/" {
			continue
		}
		ext := filepath.Ext(base)
		origBase := base
		if ext != "" && ext != base {
			origBase = base[:len(base)-len(ext)]
		}
		cleaned := sanitizeCleaned(origBase, expandBatchTemplate(
			template, origBase, title, dateStr, batchTypeOf(ext), i+1))
		newBase := cleaned + ext
		if newBase == base {
			continue
		}
		oldFull := f.Name
		newFull := newBase
		if dir != "." && dir != "" {
			newFull = dir + "/" + newBase
		}
		if seenOld[oldFull] {
			continue
		}
		seenOld[oldFull] = true
		entries = append(entries, entry{old: oldFull, new: newFull})
	}

	// 与 buildCleanPlan 相同的冲突处理：计划内目标去重 → 磁盘预检兜底
	sort.SliceStable(entries, func(a, b int) bool { return entries[a].old < entries[b].old })
	usedTargets := make(map[string]bool)
	var plan []cleanRename
	for _, e := range entries {
		newName := e.new
		if usedTargets[newName] {
			ext := filepath.Ext(newName)
			base := newName[:len(newName)-len(ext)]
			newName = base + "_" + suffix + ext
		}
		dst := filepath.Join(t.SavePath, filepath.FromSlash(newName))
		if _, err := os.Stat(dst); err == nil {
			ext := filepath.Ext(newName)
			base := newName[:len(newName)-len(ext)]
			alt := base + "_" + suffix + ext
			altDst := filepath.Join(t.SavePath, filepath.FromSlash(alt))
			if _, err2 := os.Stat(altDst); err2 == nil {
				logger.Warn.Printf("批量重命名：目标 %s 与 %s 均已存在，跳过 %s（绝不覆盖）", newName, alt, e.old)
				continue
			}
			logger.Info.Printf("批量重命名：目标 %s 已存在，改用兜底名 %s", newName, alt)
			newName = alt
		}
		if newName == e.old {
			continue
		}
		usedTargets[newName] = true
		plan = append(plan, cleanRename{Old: e.old, New: newName, Via: "manual"})
	}
	return plan
}

// loadTorrentAndFiles 取任务元信息与文件列表（找不到任务返回明确错误）
func (m *Manager) loadTorrentAndFiles(hash string) (*models.QBTorrent, []models.QBFile, error) {
	list, err := m.client.GetTorrents("all", "", "")
	if err != nil {
		return nil, nil, fmt.Errorf("获取任务列表失败：%w", err)
	}
	var tor *models.QBTorrent
	for i := range list {
		if list[i].Hash == hash {
			tor = &list[i]
			break
		}
	}
	if tor == nil {
		return nil, nil, fmt.Errorf("任务不存在或已从 qBittorrent 删除")
	}
	files, err := m.client.GetTorrentFiles(hash)
	if err != nil {
		return nil, nil, fmt.Errorf("获取文件列表失败：%w", err)
	}
	return tor, files, nil
}

// PreviewBatch 生成批量重命名预览计划（只读，不提交、不落盘）
func (m *Manager) PreviewBatch(hash, template string) ([]cleanRename, error) {
	if err := validateBatchTemplate(template); err != nil {
		return nil, err
	}
	tor, files, err := m.loadTorrentAndFiles(hash)
	if err != nil {
		return nil, err
	}
	return buildBatchPlan(files, *tor, template), nil
}

// ApplyBatchRename 提交批量重命名并写入审计日志（via=manual，可回退）。
// 提交成功的条目挂到 cleanState 的 pending 上：若清理已开启，下轮 handleClean
// 会核销确认（置 AISettled 防止 AI 又把手改过的文件名再格式化一遍）；
// 清理未开启时条目保持「已提交」状态，回退不受影响。
func (m *Manager) ApplyBatchRename(hash, template string) (int, error) {
	if err := validateBatchTemplate(template); err != nil {
		return 0, err
	}
	tor, files, err := m.loadTorrentAndFiles(hash)
	if err != nil {
		return 0, err
	}
	plan := buildBatchPlan(files, *tor, template)
	if len(plan) == 0 {
		return 0, nil
	}
	committed := m.applyRenames(hash, plan)
	if len(committed) == 0 {
		return 0, fmt.Errorf("renameFile 全部提交失败，详见服务日志")
	}
	m.mu.Lock()
	rec := m.cleanState[hash]
	if rec == nil {
		rec = &cleanRecord{}
		m.cleanState[hash] = rec
	}
	for i := range committed {
		e := m.audit.Append(auditEntry{
			Hash:    hash,
			Torrent: tor.Name,
			Old:     committed[i].Old,
			New:     committed[i].New,
			Via:     "manual",
			Status:  AuditCommitted,
		})
		committed[i].AuditID = e.ID
	}
	rec.Pending = append(rec.Pending, committed...)
	rec.AISettled = true
	rec.Done = false
	m.mu.Unlock()
	m.saveCleanState()
	logger.Info.Printf("批量重命名 [%s] 已提交 %d/%d 条（模板：%s）", tor.Name, len(committed), len(plan), template)
	return len(committed), nil
}
