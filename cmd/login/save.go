// save.go 把一次登录的结果落盘成凭证文件。
//
// login.sh 里的内联 Python 重写文件，只保留 account/auth 两个键，
// 会把既有账号的 _device_identity、_device_profile 一起丢掉（重新登录后可能退回旧
// Windows 档案），新账号也不会像服务端那样建立 Mac 档案。现在统一走这里：
//
//   - 已有账号：沿用磁盘上的设备标识与档案；
//   - 新账号：建立 Mac 档案（与服务端新增账号的默认一致）；
//   - 写入按 0600 原子替换，accessToken 为空或文件不属于同一 uid 时拒绝覆盖。
//
// 说明：写回的内容按标准结构重建（account/auth + 设备元数据 + 授权元数据），文件里
// 手工加的非标准键不会保留——服务端 persistAccount 也是这个口径。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/device"
)

// loginResult 是 poll 子命令的输出；save 只读这些字段。
type loginResult struct {
	AccessToken        string `json:"access_token"`
	RefreshToken       string `json:"refresh_token"`
	ExpiresIn          int64  `json:"expires_in"`
	Domain             string `json:"domain"`
	UID                string `json:"uid"`
	EnterpriseID       string `json:"enterprise_id"`
	Nickname           string `json:"nickname"`
	AccountType        string `json:"account_type"`
	OneIDAccountID     string `json:"oneid_account_id"`
	IDP                string `json:"idp"`
	DepartmentFullName string `json:"department_full_name"`
	TokenType          string `json:"token_type"`
}

// authFilePath 取凭证文件路径：显式给了 WB2A_LOGIN_AUTH_FILE 就用它（login.sh 这样调用），
// 否则按服务端目录约定落在 ./auths/workbuddy-<uid>.json。
func authFilePath(uid string) string {
	if value := strings.TrimSpace(os.Getenv("WB2A_LOGIN_AUTH_FILE")); value != "" {
		return value
	}
	return filepath.Join("auths", "workbuddy-"+uid+".json")
}

func saveLogin(in io.Reader) error {
	var result loginResult
	if err := json.NewDecoder(io.LimitReader(in, 1<<20)).Decode(&result); err != nil {
		return fmt.Errorf("parse login result failed")
	}
	result.AccessToken = strings.TrimSpace(result.AccessToken)
	result.UID = strings.TrimSpace(result.UID)
	if result.AccessToken == "" || result.UID == "" {
		return fmt.Errorf("login result is incomplete")
	}
	// UID 来自上游响应，不允许它改变凭证目录或 Windows 文件流。
	if result.UID == "." || result.UID == ".." || strings.ContainsAny(result.UID, `<>:"/\|?*`) || strings.ContainsFunc(result.UID, func(r rune) bool { return r < 32 || r == 127 }) {
		return fmt.Errorf("login result has invalid uid")
	}
	var expiresAt int64
	if result.ExpiresIn > 0 {
		now := time.Now().Unix()
		if result.ExpiresIn > math.MaxInt64-now {
			return fmt.Errorf("login result has invalid expiry")
		}
		expiresAt = now + result.ExpiresIn
	}
	path := authFilePath(result.UID)
	account := &auth.Auth{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    expiresAt,
		Domain:       result.Domain,
		UID:          result.UID,
		EnterpriseID: result.EnterpriseID,
		Nickname:     result.Nickname,
		FilePath:     path,
	}
	account.Authority = auth.Authority{
		AccountType:        result.AccountType,
		OneIDAccountID:     result.OneIDAccountID,
		IDP:                result.IDP,
		DepartmentFullName: result.DepartmentFullName,
		TokenType:          result.TokenType,
	}
	switch raw, err := os.ReadFile(path); {
	case err == nil:
		existing, parseErr := auth.Parse(raw)
		if parseErr != nil || existing.UID != result.UID {
			return fmt.Errorf("refuse to overwrite credential: unreadable or different uid")
		}
		account.DeviceID, account.DeviceProfile = existing.DeviceID, existing.DeviceProfile
	case os.IsNotExist(err):
		id, profile, err := device.NewMac()
		if err != nil {
			return fmt.Errorf("create device profile failed")
		}
		account.DeviceID, account.DeviceProfile = id, profile
	default:
		return fmt.Errorf("read existing credential failed")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create auth directory failed")
		}
	}
	if err := account.SaveAtomic(); err != nil {
		return fmt.Errorf("save credential failed")
	}
	return nil
}
