package upstream

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"time"

	"workbuddy2api/internal/auth"
)

type capabilityScope struct {
	region, uid, enterprise, accountType, endpoint string
	anonymousToken                                 [32]byte
}

type capabilityEntry struct {
	updated time.Time
	efforts map[string][]string
}

func (c *Client) capabilityKey(a *auth.Auth) capabilityScope {
	s := a.Snapshot()
	frozen := authFromCredentials(s)
	key := capabilityScope{region: frozen.Region(), uid: s.UID, enterprise: s.EnterpriseID, accountType: s.AccountType, endpoint: c.chatBase(frozen)}
	// Library callers may have a token before learning their UID. They must
	// not all share the empty-UID bucket; keep only an in-memory digest.
	if s.UID == "" {
		key.anonymousToken = sha256.Sum256([]byte(s.AccessToken))
	}
	return key
}

func (c *Client) storeCapabilities(a *auth.Auth, efforts map[string][]string) {
	key := c.capabilityKey(a)
	copy := make(map[string][]string, len(efforts))
	for id, values := range efforts {
		copy[id] = append([]string(nil), values...)
	}
	c.effortsMu.Lock()
	defer c.effortsMu.Unlock()
	if c.efforts == nil {
		c.efforts = make(map[capabilityScope]capabilityEntry)
	}
	// Bound entries even when an operator imports/removes many accounts.
	if _, exists := c.efforts[key]; !exists && len(c.efforts) >= 4096 {
		var oldest capabilityScope
		var at time.Time
		for candidate, entry := range c.efforts {
			if at.IsZero() || entry.updated.Before(at) {
				oldest, at = candidate, entry.updated
			}
		}
		delete(c.efforts, oldest)
	}
	// A successful catalogue without declarations clears only this account.
	c.efforts[key] = capabilityEntry{updated: time.Now(), efforts: copy}
}

func (c *Client) accountEffortBody(a *auth.Auth, body []byte) ([]byte, error) {
	if a == nil || len(body) == 0 {
		return body, nil
	}
	if !bytes.Contains(body, []byte(`"reasoning_effort"`)) && !bytes.Contains(body, []byte(`"reasoningEffort"`)) {
		return body, nil
	}
	key := c.capabilityKey(a)
	c.effortsMu.RLock()
	entry, found := c.efforts[key]
	c.effortsMu.RUnlock()
	if !found || len(entry.efforts) == 0 || time.Since(entry.updated) > 2*time.Hour {
		return body, nil
	}
	// RawMessage avoids rebuilding the nested messages/tools object merely to
	// adjust one scalar after account selection. The prepared input is immutable.
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, ErrUnprocessableBody
	}
	var model string
	_ = json.Unmarshal(fields["model"], &model)
	name := "reasoning_effort"
	if _, exists := fields[name]; !exists {
		name = "reasoningEffort"
	}
	var before string
	if json.Unmarshal(fields[name], &before) != nil || len(entry.efforts[model]) == 0 {
		return body, nil
	}
	values := map[string]any{"model": model, name: before}
	normalizeReasoningEffort(values, entry.efforts)
	if values[name] == before {
		return body, nil
	}
	fields[name], _ = json.Marshal(values[name])
	out, err := json.Marshal(fields)
	if err != nil {
		return nil, ErrUnprocessableBody
	}
	return out, nil
}
