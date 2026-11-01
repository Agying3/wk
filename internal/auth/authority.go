package auth

// Authority is optional provider metadata. Old credential files may omit all
// fields. It is preserved on refresh, export and reload; it is not a credential.
type Authority struct {
	AccountType        string
	OneIDAccountID     string
	IDP                string
	DepartmentFullName string
	TokenType          string
}

func (c Credentials) AddAuthorityMetadata(token, account map[string]any) {
	if c.TokenType != "" {
		token["tokenType"] = c.TokenType
	}
	for key, value := range map[string]string{"type": c.AccountType, "oneidAccountId": c.OneIDAccountID, "idp": c.IDP, "departmentFullName": c.DepartmentFullName} {
		if value != "" {
			account[key] = value
		}
	}
}

// ApplyRefreshIfCurrent prevents an in-flight refresh overwriting a newer
// credential installed by another operation. Duplicate waiters are harmless.
func (a *Auth) ApplyRefreshIfCurrent(before, next Credentials) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.UID != before.UID || a.EnterpriseID != before.EnterpriseID || a.Domain != before.Domain || a.AccessToken != before.AccessToken || a.RefreshToken != before.RefreshToken || a.ExpiresAt != before.ExpiresAt || a.Authority != before.Authority {
		return false
	}
	a.AccessToken, a.RefreshToken, a.Domain, a.ExpiresAt = next.AccessToken, next.RefreshToken, next.Domain, next.ExpiresAt
	a.Authority = next.Authority
	return true
}
