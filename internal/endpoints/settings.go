package endpoints

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	LegacyChatCN    = "https://copilot.tencent.com"
	LegacyBillingCN = "https://www.codebuddy.cn"
	ProductCN       = "https://www.workbuddy.cn"
	Global          = "https://www.workbuddy.ai"
)

// Settings centralizes business API origins. Legacy is the default until an
// operator explicitly selects product or overrides an origin. No failover or
// proxy bypass is implied by choosing an origin.
type Settings struct {
	EndpointProfile   string `json:"endpoint_profile,omitempty"`
	ChatBaseCN        string `json:"chat_base_cn,omitempty"`
	BillingBaseCN     string `json:"billing_base_cn,omitempty"`
	ChatBaseGlobal    string `json:"chat_base_global,omitempty"`
	BillingBaseGlobal string `json:"billing_base_global,omitempty"`
	WebBaseCN         string `json:"web_base_cn,omitempty"`
}

func (s Settings) Resolve() (Settings, error) {
	if s.EndpointProfile != "" && s.EndpointProfile != "legacy" && s.EndpointProfile != "product" {
		return s, fmt.Errorf("upstream.endpoint_profile must be legacy or product")
	}
	chat, billing := LegacyChatCN, LegacyBillingCN
	if s.EndpointProfile == "product" {
		chat, billing = ProductCN, ProductCN
	}
	for _, field := range []struct {
		name     string
		target   *string
		fallback string
	}{
		{"chat_base_cn", &s.ChatBaseCN, chat}, {"billing_base_cn", &s.BillingBaseCN, billing},
		{"chat_base_global", &s.ChatBaseGlobal, Global}, {"billing_base_global", &s.BillingBaseGlobal, Global},
		{"web_base_cn", &s.WebBaseCN, ProductCN},
	} {
		if *field.target == "" {
			*field.target = field.fallback
		}
		u, err := url.Parse(*field.target)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return s, fmt.Errorf("upstream.%s must be an HTTP(S) base URL without credentials, query or fragment", field.name)
		}
		*field.target = strings.TrimRight(u.String(), "/")
	}
	return s, nil
}
