package server

import (
	"fmt"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

const taskExpertMaxPages = 5

// 先收齐任务需要的真实条目，再发送召唤和对话，避免只找到一部分却消耗配额。
// 按 ID 去重；空页、末页、重复页及页数上限都能终止查询。
func taskMarketExperts(c *upstream.Client, a *auth.Auth, expertType string, need int, match func(upstream.MarketExpert) bool) ([]upstream.MarketExpert, error) {
	var result []upstream.MarketExpert
	seen := make(map[string]bool)
	for page := 1; page <= taskExpertMaxPages; page++ {
		rows, err := c.MarketExpertListPage(a, expertType, page)
		if err != nil {
			return nil, err
		}
		added := 0
		for _, e := range rows {
			if strings.TrimSpace(e.ExpertID) == "" || seen[e.ExpertID] {
				continue
			}
			seen[e.ExpertID] = true
			added++
			if e.ExpertType != "" && e.ExpertType != expertType {
				continue
			}
			if match == nil || match(e) {
				result = append(result, e)
				if len(result) >= need {
					return result, nil
				}
			}
		}
		if added == 0 || len(rows) < upstream.MarketExpertPageSize {
			break
		}
	}
	return nil, fmt.Errorf("专家市场符合条件的条目不足：需要 %d，找到 %d（最多查询 %d 页）", need, len(result), taskExpertMaxPages)
}

func taskLighthouseExpert(c *upstream.Client, a *auth.Auth) (upstream.MarketExpert, error) {
	const lighthouseID = "ex_2cvvUZQhDyeJ"
	match := func(e upstream.MarketExpert) bool { return e.ExpertID == lighthouseID }
	rows, err := c.MarketExpertSearch(a, "agent", "腾讯轻量云")
	if err != nil {
		return upstream.MarketExpert{}, err
	}
	for _, e := range rows {
		if match(e) && (e.ExpertType == "" || e.ExpertType == "agent") {
			return e, nil
		}
	}
	// 搜索未命中时有界翻页；不能用固定的旧版本和付费属性代替真实条目。
	rows, err = taskMarketExperts(c, a, "agent", 1, match)
	if err != nil {
		return upstream.MarketExpert{}, err
	}
	return rows[0], nil
}
