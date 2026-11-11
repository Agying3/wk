package groups

// PlaygroundSnapshot copies the last local metadata revision. It intentionally
// does not call reloadShared: opening the console must not update authority or
// initialize files. Inference rechecks membership through the normal APIs.
func (s *Store) PlaygroundSnapshot() ([]GroupInfo, map[string][]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	groups := make([]GroupInfo, 0, len(s.groups))
	for _, name := range s.groups {
		region := s.regions[name]
		if region == "" {
			region = RegionAll
		}
		groups = append(groups, GroupInfo{Name: name, Region: region, Default: name == DefaultGroup})
	}
	membership := make(map[string][]string, len(s.accountOf))
	for uid, names := range s.accountOf {
		membership[uid] = append([]string(nil), names...)
	}
	return groups, membership
}
