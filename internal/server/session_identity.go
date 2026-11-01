package server

import (
	"context"
	"net/http"

	"workbuddy2api/internal/session"
)

type requestSession struct {
	Identity     session.Identity
	LegacyPrompt string
	Namespace    string
}
type requestSessionKey struct{}

// This is the sole request-admission entry point for affinity identity. All
// consumers reuse the context value, never rebuild an ID from rewritten text.
func (h *Handler) resolveSession(doc map[string]any, r *http.Request) (requestSession, error) {
	namespace := routeNamespace(r)
	if existing, ok := r.Context().Value(requestSessionKey{}).(requestSession); ok && existing.Namespace == namespace {
		return existing, nil
	}
	var normalized map[string]any
	if raw, _ := session.ExplicitIdentity(doc, r.Header); raw == "" {
		var err error
		normalized, err = session.PrefixDocument(doc)
		if err != nil {
			return requestSession{}, err
		}
		if h.cfg.Upstream != nil {
			if _, err = h.cfg.Upstream.PrepareChatBodyWithRewrite(normalized, nil); err != nil {
				return requestSession{}, err
			}
		}
	}
	resolved := requestSession{Identity: h.sessionResolver.Resolve(namespace, doc, normalized, r.Header), Namespace: namespace}
	if h.cfg.PromptGuard != nil {
		resolved.LegacyPrompt = h.cfg.PromptGuard.LegacySessionKey(namespace, doc, r.Header)
	}
	return resolved, nil
}

func withRequestSession(r *http.Request, identity requestSession) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), requestSessionKey{}, identity))
}
