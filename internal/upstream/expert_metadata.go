package upstream

import "encoding/json"

func (e MarketExpert) requiresProtection() bool {
	if e.IsCloud == 1 {
		return true
	}
	var detail map[string]json.RawMessage
	if json.Unmarshal(e.PayDetail, &detail) == nil {
		for _, name := range []string{"pay_mode", "payMode"} {
			if raw, present := detail[name]; present && string(raw) != "null" {
				var mode float64
				if json.Unmarshal(raw, &mode) == nil {
					return mode > 0
				}
			}
		}
	}
	return e.NeedPay == 1
}
