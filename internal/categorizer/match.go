package categorizer

import (
	"fmt"
	"regexp"
	"unicode"

	"github.com/Felix2yu/qbhive/internal/models"
)

// condMatcher 编译后的条件求值器。编译一次、对全量任务反复求值，
// 因此把正则编译与参数归一化都前置到这里，避免每轮扫描重复解析。
type condMatcher interface {
	Match(name string) bool
}

// compileCond 把一条配置条件编译成求值器。
// 未知的条件类型一律报错，绝不能静默当成「不命中」——那会让用户写错的规则
// 看起来像是「没匹配上」，而不是「写错了」。
func compileCond(c models.CategoryCond) (condMatcher, error) {
	switch c.Type {
	case models.CondCode:
		return compileCodeCond(c)
	case models.CondLatinCount:
		min := c.Min
		if min <= 0 {
			min = defaultLatinMin
		}
		return countMatcher{count: countLatin, min: min}, nil
	case models.CondCJKCount:
		min := c.Min
		if min <= 0 {
			min = defaultCJKMin
		}
		return countMatcher{count: countCJK, min: min}, nil
	case models.CondRegex:
		if c.Pattern == "" {
			return nil, fmt.Errorf("regex 条件的 pattern 不能为空")
		}
		re, err := regexp.Compile(c.Pattern)
		if err != nil {
			return nil, fmt.Errorf("regex 条件正则无效: %w", err)
		}
		return regexMatcher{re: re}, nil
	default:
		return nil, fmt.Errorf("未知的条件类型: %q", c.Type)
	}
}

// 三条内置规则的默认阈值，用户在 UI 里可以改
const (
	defaultLatinMin    = 15
	defaultCJKMin      = 10
	defaultMinLetters  = 2
	defaultMinDigits   = 2
	defaultCodeAnchored = true
)

// compileCodeCond 编号式命名：字母段 + 连字符 + 数字段。
//
// 默认要求锚定在名称开头，且字母段 ≥2、数字段 ≥2。这两个约束是防误伤的关键：
// 不加锚定时 `[A-Za-z]+-[0-9]+` 会把 `Movie.2020.H-264.1080p` 里的编码标签
// H-264 判成编号；不限制位数时 `X-9` 这种短片段也会命中。
func compileCodeCond(c models.CategoryCond) (condMatcher, error) {
	letters := c.MinLetters
	if letters <= 0 {
		letters = defaultMinLetters
	}
	digits := c.MinDigits
	if digits <= 0 {
		digits = defaultMinDigits
	}
	anchored := defaultCodeAnchored
	if c.Anchored != nil {
		anchored = *c.Anchored
	}
	body := fmt.Sprintf(`[A-Za-z]{%d,}[-_][0-9]{%d,}`, letters, digits)
	if anchored {
		body = "^" + body
	}
	re, err := regexp.Compile(body)
	if err != nil {
		return nil, fmt.Errorf("code 条件编译失败: %w", err)
	}
	return regexMatcher{re: re}, nil
}

type regexMatcher struct{ re *regexp.Regexp }

func (m regexMatcher) Match(name string) bool { return m.re.MatchString(name) }

// countMatcher 阈值型条件：统计函数返回值 ≥ min 即命中
type countMatcher struct {
	count func(string) int
	min   int
}

func (m countMatcher) Match(name string) bool { return m.count(name) >= m.min }

// countLatin 统计字符串里的 ASCII 英文字母个数。
// 手写循环而不是用正则：实测同一语义下正则慢约 60 倍（1456ns vs 24ns）。
func countLatin(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			n++
		}
	}
	return n
}

// countCJK 统计字符串里的汉字个数（CJK 统一表意文字）。
func countCJK(s string) int {
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			n++
		}
	}
	return n
}
