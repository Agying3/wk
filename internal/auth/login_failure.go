package auth

import "fmt"

// LoginFailure is the non-secret error contract between the login helper and
// the management server. Provider response bodies and authorization URLs must
// never be included in this payload.
type LoginFailure struct {
	Stage  string `json:"stage"`
	Code   int    `json:"code,omitempty"`
	Status int    `json:"status,omitempty"`
}

func (e *LoginFailure) Pending() bool {
	return e.Stage == "token" && e.Code == 11217 && (e.Status == 0 || e.Status == 200)
}

func (e *LoginFailure) Valid() bool {
	return e.Stage == "authorization" || e.Stage == "token" || e.Stage == "account" || e.Stage == "state"
}

func (e *LoginFailure) Error() string {
	if e.Pending() {
		return "登录尚未完成，请先在浏览器完成授权"
	}
	step := map[string]string{"authorization": "发起授权", "token": "获取登录令牌", "account": "读取登录账号", "state": "读取授权会话"}[e.Stage]
	if step == "" {
		step = "登录"
	}
	reason := map[int]string{12005: "席位已满", 11212: "授权已过期", 11216: "试用已过期"}[e.Code]
	msg := step + "失败"
	if reason != "" {
		msg += "：" + reason
	}
	if e.Status != 0 {
		msg += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	if e.Code != 0 {
		msg += fmt.Sprintf(" (code=%d)", e.Code)
	}
	return msg
}
