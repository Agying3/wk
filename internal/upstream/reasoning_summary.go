package upstream

// Add the summary setting only for an explicitly requested reasoning effort.
// Explicit values (including null/false) remain owned by the caller. This has
// no cache-retention or pricing configuration side effects.
func addReasoningSummary(body map[string]any) {
	if body == nil {
		return
	}
	if _, explicit := body["reasoning_summary"]; explicit {
		return
	}
	_, flat := body["reasoning_effort"].(string)
	nested, _ := body["reasoning"].(map[string]any)
	_, inner := nested["effort"].(string)
	if flat || inner {
		body["reasoning_summary"] = "auto"
	}
}
