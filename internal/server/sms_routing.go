package server

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"workbuddy2api/internal/haozhuma"
)

func (a *AutoEnroller) CurrentSMSClient() *haozhuma.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hzm
}
func (a *AutoEnroller) FallbackMounted() bool {
	c := a.CurrentSMSClient()
	return c != nil && c.Fallback() != nil
}
func (a *AutoEnroller) ProviderMode() string {
	c := a.CurrentSMSClient()
	if c == nil {
		return "auto"
	}
	return c.Mode()
}

// ApplySMSUpdate 在同一运行态锁下校验、写盘和发布客户端。
func (a *AutoEnroller) ApplySMSUpdate(replacement *haozhuma.Client, mode *string, persist func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if replacement != nil && (a.running || len(a.held) > 0) {
		return ErrBusyAuth
	}
	target := a.hzm
	if replacement != nil {
		target = replacement
	}
	if mode != nil {
		if target == nil {
			return errors.New("请先配置接码平台凭据")
		}
		if err := target.ValidateMode(strings.TrimSpace(*mode)); err != nil {
			return err
		}
	}
	if err := persist(); err != nil {
		return err
	}
	if mode != nil {
		_ = target.SetMode(*mode)
	}
	if replacement != nil {
		a.hzm = replacement
		if project := replacement.Project(); project != "" {
			a.sid = project
		}
	}
	return nil
}

func smsSettingsFromDoc(doc map[string]any) haozhuma.Settings {
	var c struct {
		SMS struct {
			Haozhuma haozhuma.Settings `json:"haozhuma"`
		} `json:"sms"`
	}
	raw, _ := json.Marshal(doc)
	_ = json.Unmarshal(raw, &c)
	return c.SMS.Haozhuma
}
func (h *Handler) currentSMSSettings() haozhuma.Settings {
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		return haozhuma.Settings{}
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	return smsSettingsFromDoc(doc)
}
func supplierPatchPresent(p *smsPatch) bool {
	if p == nil {
		return false
	}
	s := p.Haozhuma
	return s.User != nil || s.Pass != nil || s.Token != nil || s.Provider != nil ||
		s.EcodeBase != nil || s.EcodeKeyword != nil || s.EcodeCardType != nil ||
		s.EcodeProvince != nil || s.Fallback != nil || s.FallbackBase != nil ||
		s.FallbackCardType != nil || s.FallbackProvince != nil
}
func mergeSMSCredentials(doc map[string]any, p *smsPatch) {
	if p == nil {
		return
	}
	sms, _ := doc["sms"].(map[string]any)
	if sms == nil {
		sms = map[string]any{}
	}
	hz, _ := sms["haozhuma"].(map[string]any)
	if hz == nil {
		hz = map[string]any{}
	}
	for key, value := range map[string]*string{"user": p.Haozhuma.User, "pass": p.Haozhuma.Pass, "token": p.Haozhuma.Token} {
		if value != nil {
			v := *value
			if key != "pass" {
				v = strings.TrimSpace(v)
			}
			hz[key] = v
		}
	}
	if user, _ := hz["user"].(string); user != "" && hz["provider"] != "ecode" &&
		(p.Haozhuma.User != nil || p.Haozhuma.Pass != nil || p.Haozhuma.Token != nil) {
		hz["token"] = ""
	}
	sms["haozhuma"] = hz
	doc["sms"] = sms
}
