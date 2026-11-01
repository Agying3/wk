package upstream

import "workbuddy2api/internal/endpoints"

type EndpointSettings = endpoints.Settings

func (c *Client) ConfigureEndpoints(settings EndpointSettings) error {
	s, err := settings.Resolve()
	if err != nil {
		return err
	}
	c.ChatBaseCN, c.BillingBaseCN = s.ChatBaseCN, s.BillingBaseCN
	c.ChatBaseGlobal, c.BillingBaseGlob = s.ChatBaseGlobal, s.BillingBaseGlobal
	c.WebBaseCN = s.WebBaseCN
	c.EndpointProfile = s.EndpointProfile
	c.LoginOriginCN = endpoints.LegacyBillingCN
	if s.EndpointProfile == "product" || settings.WebBaseCN != "" {
		c.LoginOriginCN = s.WebBaseCN
	}
	return nil
}
