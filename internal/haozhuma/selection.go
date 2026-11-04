package haozhuma

import (
	"context"
	"fmt"
	"strings"
)

// NormalizeISP 校验官方现行运营商参数；旧 UI 的 2/3 不再作为新配置接受。
func NormalizeISP(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	parts := strings.Split(value, ",")
	seen := make(map[string]bool, len(parts))
	for i, part := range parts {
		part = strings.TrimSpace(part)
		switch part {
		case "1", "5", "9", "14", "16":
		default:
			return "", fmt.Errorf("运营商只能是 1（移动）、5（联通）、9（电信）、14（广电）、16（虚拟运营商）")
		}
		if seen[part] {
			return "", fmt.Errorf("运营商参数重复：%s", part)
		}
		seen[part] = true
		parts[i] = part
	}
	return strings.Join(parts, ","), nil
}

// legacyISP 保留旧 UI 的用户选择含义：旧值 2=联通、3=电信。
// 只在读取已保存的手动设置时使用，不用于接受新监控配置。
func legacyISP(value string) string {
	var result []string
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		switch part {
		case "2":
			part = "5"
		case "3":
			part = "9"
		}
		if part != "" && !seen[part] {
			seen[part] = true
			result = append(result, part)
		}
	}
	return strings.Join(result, ",")
}

// PhoneSelection 是本批监控的取号约束，不修改共享客户端的手动配置。
// ISP=nil 沿用旧手动优先级；非 nil 为独立筛选，空字符串表示不限。
type PhoneSelection struct {
	CardType string
	UID      string
	ISP      *string
	Province string
}

// NormalizeCardType 对应 getPhone.ascription：1 虚拟、2 实卡，空字符串不限。
func NormalizeCardType(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "1" || value == "2" {
		return value, nil
	}
	return "", fmt.Errorf("号码类型只能为 1（虚拟）、2（实卡），留空表示不限")
}

// NormalizeProvince 使用用户提供的官方省份代码表；空字符串不限。
func NormalizeProvince(value string) (string, error) {
	value = strings.TrimSpace(value)
	switch value {
	case "", "11", "12", "13", "14", "15", "21", "22", "23", "31", "32", "33", "34", "35", "36", "37", "41", "42", "43", "44", "45", "46", "50", "51", "52", "53", "54", "61", "62", "63", "64", "65":
		return value, nil
	default:
		return "", fmt.Errorf("省份需使用豪猪官方省份代码，留空表示不限")
	}
}

type phoneSelectionKey struct{}

func WithPhoneSelection(ctx context.Context, selection PhoneSelection) context.Context {
	if selection.ISP != nil {
		isp := *selection.ISP
		selection.ISP = &isp
	}
	return context.WithValue(ctx, phoneSelectionKey{}, selection)
}
