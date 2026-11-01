package groups

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ChangeAccounts reads, merges, validates and commits memberships under one
// lock. Only account-groups.json is written, once; a write failure leaves the
// in-memory memberships unchanged. Invalid accounts do not block valid ones.
func (s *Store) ChangeAccounts(regions map[string]string, targets []string, mode string) (map[string]error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mode != "replace" && mode != "append" && mode != "remove" {
		return nil, fmt.Errorf("分组操作必须为 replace、append 或 remove")
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("请选择目标分组")
	}
	valid := make(map[string]bool, len(s.groups))
	for _, group := range s.groups {
		valid[group] = true
	}
	for _, target := range targets {
		if !valid[strings.TrimSpace(target)] {
			return nil, fmt.Errorf("目标分组不存在")
		}
	}
	targets = cleanGroups(targets, valid)
	next := make(map[string][]string, len(s.accountOf)+len(regions))
	for uid, current := range s.accountOf {
		next[uid] = current
	}
	results := make(map[string]error, len(regions))
	changed := false
	for uid, region := range regions {
		current := s.accountOf[uid]
		if len(current) == 0 {
			current = []string{DefaultGroup}
		}
		var candidate []string
		switch mode {
		case "replace":
			candidate = targets
		case "append":
			candidate = append(append([]string(nil), current...), targets...)
		case "remove":
			for _, group := range current {
				if !slices.Contains(targets, group) {
					candidate = append(candidate, group)
				}
			}
		}
		if len(candidate) == 0 {
			candidate = []string{DefaultGroup}
		}
		candidate = cleanGroups(candidate, valid)
		var issue error
		for _, group := range candidate {
			groupRegion := s.regions[group]
			if groupRegion != "" && !RegionCompatible(groupRegion, region) {
				issue = fmt.Errorf("账号区域与分组 %s 不兼容", group)
				break
			}
		}
		results[uid] = issue
		if issue == nil && !slices.Equal(s.accountOf[uid], candidate) {
			next[uid] = candidate
			changed = true
		}
	}
	if !changed {
		return results, nil
	}
	raw, err := json.MarshalIndent(next, "", "  ")
	if err == nil {
		err = writeFileAtomic(s.accFp, append(raw, '\n'))
	}
	if err != nil {
		return results, fmt.Errorf("分组保存失败，当前批次未修改")
	}
	s.accountOf = next
	s.dirtyAcc = false
	return results, nil
}
