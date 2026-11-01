package promptguard

import "errors"

// RulePreview simulates one selected candidate, including for a disabled rule.
// It neither changes configuration nor allocates a business-session binding.
type RulePreview struct {
	Matches     int    `json:"matches"`
	Output      string `json:"output"`
	RuleEnabled bool   `json:"rule_enabled"`
	VariantID   string `json:"variant_id,omitempty"`
}

func PreviewRule(rule Rule, variantID, text string) (RulePreview, error) {
	const limit = 1 << 20
	if rule.Match == "" || len(text) > limit {
		return RulePreview{}, errors.New("匹配原文不能为空，测试文本最多 1 MiB")
	}
	if len(rule.Variants) > 0 {
		found := false
		for _, v := range rule.Variants {
			if v.ID == variantID {
				rule.Replacement = v.Replacement
				found = true
				break
			}
		}
		if !found {
			return RulePreview{}, errors.New("请选择有效的替换候选")
		}
	} else if variantID != "" {
		return RulePreview{}, errors.New("替换候选不存在")
	}
	count := literalMatchCount(text, rule.Match, rule.RequireEndBoundary)
	delta := len(rule.Replacement) - len(rule.Match)
	if delta > 0 && count > (limit-len(text))/delta {
		return RulePreview{}, errors.New("替换后的测试文本超过 1 MiB，请缩短测试文本")
	}
	return RulePreview{Matches: count, Output: replaceLiteral(text, rule), RuleEnabled: rule.Enabled, VariantID: variantID}, nil
}
