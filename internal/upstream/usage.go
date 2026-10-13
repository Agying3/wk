package upstream

// MergeUsage merges partial cumulative usage snapshots. Missing fields do not
// erase prior observations; an explicitly supplied zero does. Alias groups
// are replaced together so a stale flat field cannot override a new nested
// cache counter through a later max() conversion.
func MergeUsage(dst, src map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	present := func(keys ...string) bool {
		for _, k := range keys {
			if v, ok := src[k]; ok && v != nil {
				return true
			}
		}
		return false
	}
	nested := func(object string, keys ...string) bool {
		m, _ := src[object].(map[string]any)
		for _, k := range keys {
			if v, ok := m[k]; ok && v != nil {
				return true
			}
		}
		return false
	}
	input := present("input_tokens", "prompt_tokens")
	output := present("output_tokens", "completion_tokens")
	if input {
		delete(dst, "input_tokens")
		delete(dst, "prompt_tokens")
	}
	if output {
		delete(dst, "output_tokens")
		delete(dst, "completion_tokens")
	}
	if (input || output) && !present("total_tokens") {
		delete(dst, "total_tokens")
	}
	read := present("prompt_cache_hit_tokens", "cache_read_input_tokens") || nested("prompt_tokens_details", "cached_tokens") || nested("input_tokens_details", "cached_tokens")
	write := present("prompt_cache_miss_tokens", "cache_creation_input_tokens") || nested("input_tokens_details", "cache_creation_input_tokens", "cache_write_tokens")
	if read {
		delete(dst, "prompt_cache_hit_tokens")
		delete(dst, "cache_read_input_tokens")
		for _, k := range []string{"prompt_tokens_details", "input_tokens_details"} {
			if m, ok := dst[k].(map[string]any); ok {
				delete(m, "cached_tokens")
			}
		}
	}
	if write {
		delete(dst, "prompt_cache_miss_tokens")
		delete(dst, "cache_creation_input_tokens")
		if m, ok := dst["input_tokens_details"].(map[string]any); ok {
			delete(m, "cache_creation_input_tokens")
			delete(m, "cache_write_tokens")
		}
	}
	var merge func(map[string]any, map[string]any)
	merge = func(a, b map[string]any) {
		for k, v := range b {
			if v == nil {
				continue
			}
			if child, ok := v.(map[string]any); ok {
				old, _ := a[k].(map[string]any)
				if old == nil {
					old = map[string]any{}
				}
				merge(old, child)
				a[k] = old
			} else {
				a[k] = v
			}
		}
	}
	merge(dst, src)
	return dst
}
