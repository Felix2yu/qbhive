package categorizer

import (
	"fmt"
	"strings"

	"github.com/Felix2yu/qbhive/internal/models"
)

// 规则与条件的数量上限，与限速规则、清理正则保持同一量级，防止配置无限膨胀
const (
	maxRules = 32
	maxConds = 8
)

// compiledRule 编译后的单条规则：条件已预编译，可安全地对全量任务反复求值
type compiledRule struct {
	rule     models.CategoryRule
	matchers []condMatcher
}

// Match 判断任务名是否命中该规则。
//
// 条件数组是 AND 语义（全部满足才命中）。**空条件数组视为不命中而非恒真**——
// 新建规则时用户往往还没填条件，若按空 AND = true 处理，这条空规则会立刻把
// 库里所有任务吞进它的分类，属于最危险的一类误伤。
func (r compiledRule) Match(name string) bool {
	if len(r.matchers) == 0 {
		return false
	}
	for _, m := range r.matchers {
		if !m.Match(name) {
			return false
		}
	}
	return true
}

// compileRules 编译整个规则集。遇到过非法条件直接返回错误，
// 由调用方决定是拒绝保存还是降级为「本轮不分类」。
func compileRules(rules []models.CategoryRule) ([]compiledRule, error) {
	out := make([]compiledRule, 0, len(rules))
	for i, r := range rules {
		if !r.Enabled {
			continue
		}
		cr := compiledRule{rule: r}
		for _, c := range r.When {
			m, err := compileCond(c)
			if err != nil {
				return nil, fmt.Errorf("规则「%s」: %w", ruleDisplayName(r, i), err)
			}
			cr.matchers = append(cr.matchers, m)
		}
		out = append(out, cr)
	}
	return out, nil
}

func ruleDisplayName(r models.CategoryRule, i int) string {
	if strings.TrimSpace(r.Name) != "" {
		return r.Name
	}
	return fmt.Sprintf("第 %d 条", i+1)
}

// ValidateRules 校验规则集，返回给用户看的中文错误；空串表示通过。
//
// 这里的校验与 web.validateConfig 是同一套（后者直接调用本函数），
// 保证「保存时拦截」与「运行时行为」用的是同一份判断，不会出现
// 保存通过但运行时静默失效的情况。
func ValidateRules(rules []models.CategoryRule) string {
	if len(rules) > maxRules {
		return fmt.Sprintf("自动分类规则数量不能超过 %d 条", maxRules)
	}
	for i, r := range rules {
		label := ruleDisplayName(r, i)
		if strings.TrimSpace(r.Category) == "" {
			return fmt.Sprintf("规则「%s」未填写分类名", label)
		}
		if msg := ValidateCategoryName(r.Category); msg != "" {
			return fmt.Sprintf("规则「%s」的分类名不合法：%s", label, msg)
		}
		if len(r.When) > maxConds {
			return fmt.Sprintf("规则「%s」的条件数量不能超过 %d 个", label, maxConds)
		}
		for _, c := range r.When {
			switch c.Type {
			case models.CondCode, models.CondLatinCount, models.CondCJKCount, models.CondRegex:
			default:
				return fmt.Sprintf("规则「%s」含有未知的条件类型: %s", label, c.Type)
			}
			if _, err := compileCond(c); err != nil {
				return fmt.Sprintf("规则「%s」: %v", label, err)
			}
		}
	}
	return ""
}

// ValidateCategoryName 校验 qB 分类名。qB 对分类名有一组非法字符约束
// （反斜杠、路径穿越片段、控制字符等），写进去会被 qB 静默拒绝或产生
// 无法删除的畸形分类，所以必须在保存前拦掉。
func ValidateCategoryName(name string) string {
	if len(name) > 64 {
		return "长度不能超过 64 字符"
	}
	if strings.Contains(name, "\\") {
		return "不能包含反斜杠"
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return "不能以斜杠开头或结尾"
	}
	if strings.Contains(name, "//") {
		return "不能包含连续的斜杠"
	}
	if strings.Contains(name, "..") {
		return "不能包含 .."
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "不能包含控制字符"
		}
	}
	return ""
}

// rulesSignature 规则集指纹：规则内容、分类目标、兜底与闸门开关任一变化都会改变它。
// 用于判断「这条任务此前的分类结论是否还成立」——改了规则就要重算，
// 否则用户调整规则后历史任务永远停在旧分类上。
func rulesSignature(ac models.AutoCategoryConfig) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("onlyIfEmpty=%v;fallback=%s;", ac.OnlyIfEmpty != nil && *ac.OnlyIfEmpty, ac.Fallback))
	for _, r := range ac.Rules {
		if !r.Enabled {
			continue
		}
		b.WriteString(fmt.Sprintf("{%s=>%s:", r.ID, r.Category))
		for _, c := range r.When {
			b.WriteString(fmt.Sprintf("[%s|%s|%d|%d|%d|%v]", c.Type, c.Pattern, c.Min, c.MinLetters, c.MinDigits, c.Anchored != nil && *c.Anchored))
		}
		b.WriteString("}")
	}
	return b.String()
}

// DefaultRules 用户自用的三条内置规则。顺序即优先级：编号式 → 英文长名 → 中文长名。
func DefaultRules() []models.CategoryRule {
	anchored := true
	return []models.CategoryRule{
		{
			ID:       "code",
			Name:     "编号式命名",
			Enabled:  true,
			Category: "编号",
			When: []models.CategoryCond{
				{Type: models.CondCode, MinLetters: 2, MinDigits: 2, Anchored: &anchored},
			},
		},
		{
			ID:       "latin",
			Name:     "英文长名",
			Enabled:  true,
			Category: "外文",
			When: []models.CategoryCond{
				{Type: models.CondLatinCount, Min: defaultLatinMin},
			},
		},
		{
			ID:       "cjk",
			Name:     "中文长名",
			Enabled:  true,
			Category: "中文",
			When: []models.CategoryCond{
				{Type: models.CondCJKCount, Min: defaultCJKMin},
			},
		},
	}
}
