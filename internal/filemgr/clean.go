package filemgr

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/models"
)

// ---- 域名合法性启发式 ----

// domainCandidateRe 从匹配文本中提取域名候选串
var domainCandidateRe = regexp.MustCompile(`(?i)(?:https?://)?(?:www\.)?[a-z0-9-]+(?:\.[a-z0-9-]+)+`)

// extBlacklist 疑似文件扩展名/语言标记黑名单：这些「TLD」视为扩展名或语言后缀
// 而非域名，避免 "Movie.zh.srt"、"Movie.2023.1080p.BluRay" 类正常名被误判
var extBlacklist = map[string]bool{
	"mkv": true, "mp4": true, "avi": true, "mov": true, "wmv": true, "flv": true, "ts": true,
	"m2ts": true, "rmvb": true, "iso": true, "zip": true, "rar": true, "7z": true, "tar": true,
	"gz": true, "pdf": true, "epub": true, "mobi": true, "azw3": true, "flac": true, "mp3": true,
	"wav": true, "ape": true, "exe": true, "jpg": true, "jpeg": true, "png": true, "gif": true,
	"webp": true, "nfo": true, "srt": true, "ass": true, "ssa": true, "sub": true, "sup": true,
	"torrent": true, "txt": true, "md": true, "html": true, "htm": true, "url": true,
	// 常见语言/地区标记（多为双字母或缩写，易与短域名混淆）
	"zh": true, "en": true, "jp": true, "kr": true, "tw": true, "hk": true, "fr": true,
	"de": true, "es": true, "it": true, "ru": true, "chs": true, "cht": true, "eng": true,
	"ger": true, "fre": true, "kor": true, "jpn": true, "h264": true, "h265": true,
	"x264": true, "x265": true, "hevc": true, "avc": true, "hdr": true, "dv": true,
	"webdl": true, "webrip": true, "bluray": true, "remux": true, "1080p": true, "720p": true,
	"2160p": true, "4320p": true, "aac": true, "ddp": true, "dd": true, "ac3": true,
	"dts": true, "atmos": true, "ma": true, "hd": true, "uhd": true, "sd": true,
}

// tldWhitelist 常见 TLD 白名单：用于「独立域名 token」等无锚定位置的模式。
// 这些模式如果只靠启发式（纯字母 TLD）容易误删 "Movie.2023.GER" 类场景命名，
// 因此要求 TLD 命中白名单或带 www. 前缀才认作水印域名。
// 刻意排除国家/地区 TLD（us/uk/jp/kr/de/fr 等），避免误删语言/地区标记。
var tldWhitelist = map[string]bool{
	"com": true, "net": true, "org": true, "cc": true, "me": true, "io": true, "tv": true,
	"xyz": true, "top": true, "club": true, "info": true, "biz": true, "pro": true,
	"site": true, "online": true, "live": true, "fun": true, "red": true, "vip": true,
	"video": true, "cloud": true, "tech": true, "app": true, "icu": true, "lol": true,
}

// looksLikeDomain 判断候选串是否像真实域名：含点 + 纯字母 TLD 结构，
// TLD 不在扩展名黑名单，整体长度 ≥ 5。
// S01E01（无点）、v1.2.3（TLD 非纯字母）、Movie.zh（黑名单）均不匹配
func looksLikeDomain(s string) bool {
	s = strings.ToLower(s)
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	if len(s) < 5 || strings.Count(s, ".") < 1 {
		return false
	}
	parts := strings.Split(s, ".")
	tld := parts[len(parts)-1]
	if len(tld) < 2 || len(tld) > 24 {
		return false
	}
	for _, r := range tld {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	if extBlacklist[tld] {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// validateDomainCandidate 校验匹配文本中的域名候选是否可信。
// strict=true 用于无锚定位置的模式（后缀/独立 token）：要求带 www. 前缀，
// 或 TLD 命中白名单，进一步压缩误删面。
func validateDomainCandidate(m string, strict bool) bool {
	d := strings.ToLower(domainCandidateRe.FindString(m))
	if d == "" {
		return false
	}
	if strings.HasPrefix(d, "www.") {
		return true
	}
	if !looksLikeDomain(d) {
		return false
	}
	if !strict {
		return true
	}
	tld := d[strings.LastIndex(d, ".")+1:]
	return tldWhitelist[tld]
}

// ---- 内置默认清洗模式 ----
// 作用于去掉扩展名后的 base；前两条位置锚定（前缀）用宽松校验，
// 后三条（后缀/独立 token）用严格校验，全部为「替换为空串」语义。

var defaultCleanPatterns = []*regexp.Regexp{
	// 1) 前缀 "www.example.com - " / "example.com - " / "https://a.com — "
	//    （分隔符含全角 ：·~—）
	regexp.MustCompile(`(?i)^\s*(?:https?://)?(?:www\.)?[a-z0-9-]+(?:\.[a-z0-9-]+)+\s*[-–—~·:：]\s*`),
	// 2) 前缀括号包裹 "[example.com] " / "【example.com】" / "(www.example.com) "
	regexp.MustCompile(`(?i)^\s*[\[（(【「]\s*(?:https?://)?(?:www\.)?[a-z0-9-]+(?:\.[a-z0-9-]+)+\s*[\]）)】」]\s*`),
	// 3) 后缀 " - example.com" / ".example.com" / "@example.com" / "＠example.com"
	regexp.MustCompile(`(?i)[\s._\-–—~@＠]+(?:https?://)?(?:www\.)?[a-z0-9-]+(?:\.[a-z0-9-]+)+\s*$`),
	// 4) 后缀括号包裹 " [example.com]" / "【example.com】"
	regexp.MustCompile(`(?i)\s*[\[（(【「]\s*(?:https?://)?(?:www\.)?[a-z0-9-]+(?:\.[a-z0-9-]+)+\s*[\]）)】」]\s*$`),
	// 5) 独立域名 token（处理 "www.a.com.b.com Movie" 等连写残留）：
	//    分隔符边界上的完整域名，须严格校验（www. 前缀或白名单 TLD）
	regexp.MustCompile(`(?:^|[\s._\-–—~@＠\[\]（）()【】「」])(?:https?://)?(?:www\.)?[a-z0-9-]+(?:\.[a-z0-9-]+){1,3}(?:$|[\s\-–—~@＠])`),
}

// replaceWithBoundary 带前置边界校验的替换：拒绝在域名/单词 token 内部开始的
// 匹配（前置字符为字母数字或点时作废），避免 "www.example.com" 类完整 token
// 被削成残段。
func replaceWithBoundary(re *regexp.Regexp, s string, strict bool) string {
	locs := re.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		if loc[0] > 0 {
			prev := s[loc[0]-1]
			isBoundary := prev == ' ' || prev == '\t' || strings.ContainsRune("_–—~@＠[]（）()【】「」", rune(prev))
			if !isBoundary {
				continue // token 内部匹配，作废
			}
		}
		m := s[loc[0]:loc[1]]
		if !validateDomainCandidate(m, strict) {
			continue
		}
		b.WriteString(s[last:loc[0]])
		last = loc[1]
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// replaceSuffixToken 模式 3（后缀）专用替换。边界判定不看前置字符（前置是
// 单词字符属正常场景，如 "Movie - site.com"），而是看匹配开头连续分隔符
// 是否仅由 "."/"-" 组成——那是域名 token 的内部字符，命中则作废，
// 避免 "www.example.com" 被削成 "www"。
func replaceSuffixToken(re *regexp.Regexp, s string, strict bool) string {
	locs := re.FindAllStringIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		m := s[loc[0]:loc[1]]
		i := 0
		for i < len(m) && strings.ContainsRune(".-_ –—~@＠", rune(m[i])) {
			i++
		}
		// 开头分隔符仅由点/连字符组成 → 域名 token 内部匹配，作废
		tokenInternal := i > 0
		for _, r := range m[:i] {
			if r != '.' && r != '-' {
				tokenInternal = false
				break
			}
		}
		if tokenInternal {
			continue
		}
		if !validateDomainCandidate(m, strict) {
			continue
		}
		b.WriteString(s[last:loc[0]])
		last = loc[1]
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// ---- emoji / 颜文字清理 ----

// isDecorRune 判断是否为装饰性符号（emoji / 颜文字构件 / 水印符号）。
// 这些码位不会出现在正常中英文文件名里，删除零风险；刻意排除 CJK、
// 假名、谚文、全角字母数字等真实文字区，绝不误删。
func isDecorRune(r rune) bool {
	switch {
	case r == 0x200B, r == 0x200C, r == 0x200D, r == 0x2060: // 零宽字符与 ZWJ
		return true
	case r >= 0x20D0 && r <= 0x20FF: // 键帽等符号组合修饰（0️⃣ 的尾部）
		return true
	case r >= 0xFE00 && r <= 0xFE4F: // 变体选择符 / 竖排呈现形式（颜文字 ︵）
		return true
	case r >= 0x2500 && r <= 0x25FF: // 框线 / 块元素 / 几何形状（颜文字 ┻━╯◐）
		return true
	case r >= 0x2600 && r <= 0x27BF: // 杂项符号与装饰（★♥☀✅）
		return true
	case r >= 0x2B00 && r <= 0x2BFF: // ⭐⬆ 等
		return true
	case r >= 0x1F000 && r <= 0x1FAFF: // emoji 主区（含肤色修饰符、国旗）
		return true
	case r == 0x00A9, r == 0x00AE, r == 0x2122: // © ® ™
		return true
	}
	return false
}

// stripDecor 移除字符串中的全部装饰性符号
func stripDecor(s string) string {
	if !strings.ContainsFunc(s, isDecorRune) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if !isDecorRune(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// kaomojiGroupRe 匹配「内容不含任何实文字」的括号组，即颜文字
// (╯°□°)、(◕‿◕)、（｡◕‿‿◕｡） 等。内容排除 CJK/假名/谚文/全角字母数字，
// 因此 (2023)、【4K】、(Extended)、(한국어)、(ツ) 这类含真实文字的括号组不受影响；
// 内容类排除括号字符本身，组不会跨越另一对括号。
var kaomojiGroupRe = regexp.MustCompile(
	`[\[（(【「][^\[\]（）()【】「」\w\x{3040}-\x{30ff}\x{3130}-\x{318f}\x{3400}-\x{4dbf}\x{4e00}-\x{9fff}\x{f900}-\x{faff}\x{ac00}-\x{d7af}\x{ff10}-\x{ff19}\x{ff21}-\x{ff3a}\x{ff41}-\x{ff5a}]{1,32}[\]）)】」]`)

// cleanBase 对单个 base 名执行清洗：内置模式（前两条宽松、后三条严格）→
// 自定义正则 → 颜文字括号组 → emoji/装饰符号 → 循环至稳定（最多 5 轮），
// 处理 "[a.com] [b.com] Movie" 叠加场景
func cleanBase(base string, custom []*regexp.Regexp) string {
	cur := base
	for round := 0; round < 5; round++ {
		next := cur
		for i, re := range defaultCleanPatterns {
			strict := i >= 2 // 模式 3/4/5 无位置锚定，须严格 TLD 校验
			switch i {
			case 2:
				// 模式 3 的分隔符类含 "." 与 "-"（域名 token 内部字符），
				// 边界判定须看开头分隔符构成而非前置字符
				next = replaceSuffixToken(re, next, strict)
			case 4:
				// 模式 5 前置边界看前一个字符
				next = replaceWithBoundary(re, next, strict)
			default:
				next = re.ReplaceAllStringFunc(next, func(m string) string {
					if !validateDomainCandidate(m, strict) {
						return m
					}
					return ""
				})
			}
		}
		for _, re := range custom {
			next = re.ReplaceAllString(next, "")
		}
		next = kaomojiGroupRe.ReplaceAllString(next, "")
		next = stripDecor(next)
		// 删除后的残留空格合并为一个（"Movie 🔥 Sub" → "Movie Sub"）
		next = strings.Join(strings.Fields(next), " ")
		next = strings.Trim(strings.TrimSpace(next), "-–—~·")
		if next == cur {
			break
		}
		cur = next
	}
	if strings.TrimSpace(cur) == "" {
		// 原名就是纯 emoji/纯域名：整名清空无意义，保留原名（sanitizeCleaned 二次兜底）
		return base
	}
	return cur
}

// sanitizeCleaned 清洗结果安全校验，任一护栏不满足即返回原名（跳过重命名）：
//   - 清洗后为空（原名就是纯域名）→ 保留原名
//   - 非法字符 \/:*?"<>| 与控制字符 → 替换为 "_"，替换后为空也回退原名
//   - 长度上限 200 字节（文件名组件上限 255）→ 回退原名，保安全
//   - 与原名 EqualFold 相同 → 回退原名（规避大小写-only rename 的 FS 兼容问题）
func sanitizeCleaned(origBase, cleaned string) string {
	cleaned = strings.Trim(strings.TrimSpace(cleaned), ".")
	if cleaned == "" {
		return origBase
	}
	replaced := false
	var b strings.Builder
	for _, r := range cleaned {
		switch {
		case strings.ContainsRune(`\/:*?"<>|`, r):
			b.WriteByte('_')
			replaced = true
		case r < 0x20 || r == 0x7f:
			// 控制字符直接丢弃
			replaced = true
		default:
			b.WriteRune(r)
		}
	}
	cleaned = strings.Trim(strings.TrimSpace(b.String()), ".")
	if cleaned == "" {
		return origBase
	}
	if replaced {
		logger.Debug.Printf("文件清洗：结果含非法字符已替换下划线：%q → %q", origBase, cleaned)
	}
	if len(cleaned) > 200 {
		logger.Debug.Printf("文件清洗：结果超长（%d 字节），保留原名：%q", len(cleaned), origBase)
		return origBase
	}
	if strings.EqualFold(cleaned, origBase) {
		return origBase
	}
	return cleaned
}

// compileCleanRules 编译并校验用户自定义规则：单条 ≤256 字符、总数 ≤32，
// 编译失败的规则记 Warn 并跳过（不中断整轮）。RE2 线性时间，无回溯风险。
func compileCleanRules(rules []string) []*regexp.Regexp {
	if len(rules) > 32 {
		rules = rules[:32]
	}
	var out []*regexp.Regexp
	for _, r := range rules {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if len(r) > 256 {
			logger.Warn.Printf("文件清洗：自定义规则超长（%d 字符）已跳过：%s…", len(r), r[:32])
			continue
		}
		re, err := regexp.Compile(r)
		if err != nil {
			logger.Warn.Printf("文件清洗：自定义规则编译失败已跳过（%s）：%v", r, err)
			continue
		}
		out = append(out, re)
	}
	return out
}

// ---- 重命名计划 ----

// cleanRename 一条待提交的重命名（torrent 内相对路径）
type cleanRename struct {
	Old      string `json:"old"`
	New      string `json:"new"`
	Attempts int    `json:"attempts,omitempty"`
	Via      string `json:"via,omitempty"`    // regex / ai
	AuditID  string `json:"auditId,omitempty"` // 关联的审计记录 ID
}

// buildCleanPlan 逐文件清洗种子的完整路径（含全部目录段），生成重命名计划。
//
// 必须按「文件」而非「目录段」提交：qB 的 /torrents/renameFile 只接受文件路径，
// 对目录段直接 409（没有这个文件）；目录改名只能通过把其中每个文件
// rename 到新完整路径来实现，空目录由 qB 侧随之消失。
//
// 算法：
//  1. 每个 file.Name 按 "/" 拆段逐段清洗：文件末段（base）走 cleanBase →（可选）
//     AI 格式化 → sanitize；目录段走 cleanBase → sanitize；扩展名永不参与清洗
//  2. 新完整路径 ≠ 旧完整路径 → 生成一条 cleanRename
//  3. 计划内目标去重：两条清洗到同一目标路径时，后者文件段加 "_hash8" 后缀兜底
//  4. 磁盘冲突预检：目标在 savePath 下已存在（绝不覆盖已有文件）→ 加 _hash8 后缀，
//     仍冲突则跳过该条并 Warn
//
// aiNames：AI 返回的 base 名映射（post-regex base → AI 新名）；nil 表示 AI 未启用/失败
func buildCleanPlan(files []models.QBFile, custom []*regexp.Regexp, aiNames map[string]string, savePath, hash string) []cleanRename {
	suffix := hash
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	usedTargets := make(map[string]bool)
	var plan []cleanRename

	for _, f := range files {
		if f.Name == "" {
			continue
		}
		segs := strings.Split(strings.Trim(f.Name, "/"), "/")
		newSegs := make([]string, 0, len(segs))
		changed := false
		viaAI := false
		for i, seg := range segs {
			isFileSeg := i == len(segs)-1
			// 文件段拆出扩展名：扩展名永不参与清洗，清洗后原样拼回
			ext := ""
			workSeg := seg
			if isFileSeg {
				if e := filepath.Ext(seg); e != "" && e != seg {
					ext = e
					workSeg = seg[:len(seg)-len(e)]
				}
			}
			cleaned := cleanBase(workSeg, custom)
			// AI 映射以正则清洗后的 base 名为键（AI 对清洗结果做二次格式化）
			if isFileSeg && aiNames != nil {
				if v, ok := aiNames[cleaned]; ok && strings.TrimSpace(v) != "" {
					cleaned = strings.TrimSpace(v)
					viaAI = true
				}
			}
			cleaned = sanitizeCleaned(workSeg, cleaned) + ext
			if cleaned != seg {
				changed = true
			}
			newSegs = append(newSegs, cleaned)
		}
		oldFull := strings.Join(segs, "/")
		newFull := strings.Join(newSegs, "/")
		if !changed || newFull == oldFull {
			continue
		}
		// 越界护栏：清洗后的目标必须仍严格落在 savePath 内（拒绝 . / .. / 逃逸段）
		if relDst, err := filepath.Rel(savePath, filepath.Join(savePath, filepath.FromSlash(newFull))); err != nil || relDst == "." || strings.HasPrefix(relDst, "..") {
			logger.Warn.Printf("文件清洗：目标 %s 越出保存根，跳过 %s", newFull, oldFull)
			continue
		}
		// 实体存在性护栏：qB 记录的文件在磁盘上并不存在（与存储脱钩的僵尸任务）时，
		// renameFile 只会改内部元数据——文件永远洗不掉，还会阻碍日后重新校验找回，必须跳过。
		if _, err := os.Lstat(filepath.Join(savePath, filepath.FromSlash(oldFull))); err != nil {
			continue
		}
		// 计划内目标去重：同目标第二条起在文件段加 _hash8
		if usedTargets[newFull] {
			ext := path.Ext(newFull)
			newFull = newFull[:len(newFull)-len(ext)] + "_" + suffix + ext
		}
		// 磁盘冲突预检：绝不覆盖已存在的文件
		if _, err := os.Lstat(filepath.Join(savePath, filepath.FromSlash(newFull))); err == nil {
			ext := path.Ext(newFull)
			alt := newFull[:len(newFull)-len(ext)] + "_" + suffix + ext
			if _, err2 := os.Lstat(filepath.Join(savePath, filepath.FromSlash(alt))); err2 == nil {
				logger.Warn.Printf("文件清洗：目标 %s 与 %s 均已存在，跳过 %s（绝不覆盖）", newFull, alt, oldFull)
				continue
			}
			logger.Info.Printf("文件清洗：目标 %s 已存在，改用兜底名 %s", newFull, alt)
			newFull = alt
		}
		if newFull == oldFull {
			continue
		}
		usedTargets[newFull] = true
		via := "regex"
		if viaAI {
			via = "ai"
		}
		plan = append(plan, cleanRename{Old: oldFull, New: newFull, Via: via})
	}
	return plan
}
