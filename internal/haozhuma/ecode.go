package haozhuma

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const DefaultEcodeBase = "https://api.eomsg.com/zc/data.php"

type temporaryEcodeError struct{ err error }

func (e *temporaryEcodeError) Error() string { return e.err.Error() }
func (e *temporaryEcodeError) Unwrap() error { return e.err }

// TemporaryEcodeFailure 只标记传输故障和可重试 HTTP 状态；凭据/业务错误不重试。
func TemporaryEcodeFailure(err error) bool {
	var temporary *temporaryEcodeError
	return errors.As(err, &temporary)
}

func NewEcode(base, token string) *Client {
	if base == "" {
		base = DefaultEcodeBase
	}
	return &Client{Base: base, Token: token, Provider: "ecode", Timeout: 15 * time.Second, HTTP: newHTTPClient()}
}

func (c *Client) ecodeGet(ctx context.Context, action string, q url.Values) (string, error) {
	if !validEndpoint(c.Base) {
		return "", errors.New("E码云 API 地址格式不正确")
	}
	u, _ := url.Parse(c.Base)
	token := c.tokenValue()
	q.Set("code", action)
	q.Set("token", token)
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", errors.New("无法构造 E码云请求")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", &temporaryEcodeError{fmt.Errorf("E码云网络错误: %w", redactNetErr(err))}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", &temporaryEcodeError{fmt.Errorf("读取 E码云响应失败: %w", err)}
	}
	text := strings.TrimSpace(string(body))
	if token != "" {
		text = strings.ReplaceAll(text, token, "[redacted]")
	}
	if resp.StatusCode != http.StatusOK {
		err := &APIError{Provider: "ecode", API: action, Code: strconv.Itoa(resp.StatusCode), Msg: fmt.Sprintf("E码云 HTTP %d", resp.StatusCode)}
		if resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500 && resp.StatusCode < 600 {
			return "", &temporaryEcodeError{err}
		}
		return "", err
	}
	if strings.HasPrefix(text, "ERROR:") {
		return "", &APIError{Provider: "ecode", API: action, Code: "-1", Msg: truncate(strings.TrimPrefix(text, "ERROR:"), 160)}
	}
	return text, nil
}

func (c *Client) ecodeSummary(ctx context.Context) (Summary, error) {
	text, err := c.ecodeGet(ctx, "leftAmount", url.Values{})
	if err != nil {
		return Summary{Occupied: -1}, err
	}
	balance, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(balance) || math.IsInf(balance, 0) {
		return Summary{Occupied: -1}, errors.New("E码云余额响应格式不正确")
	}
	return Summary{Balance: balance, Occupied: -1}, nil
}

var ecodePhonePattern = regexp.MustCompile(`^[0-9]{7,15}$`)

func (c *Client) ecodePhone(ctx context.Context, keyword string) (string, error) {
	if keyword == "" {
		return "", errors.New("请配置 E码云短信关键词")
	}
	card := c.CardType
	if card == "" {
		card = "全部"
	}
	phone, err := c.ecodeGet(ctx, "getPhone", url.Values{
		"keyWord": {keyword}, "cardType": {card}, "province": {c.Province},
	})
	if err != nil {
		return "", err
	}
	if !ecodePhonePattern.MatchString(phone) {
		return "", errors.New("E码云未返回有效手机号")
	}
	return phone, nil
}

func (c *Client) ecodeMessage(ctx context.Context, keyword, phone string) (string, error) {
	text, err := c.ecodeGet(ctx, "getMsg", url.Values{"phone": {phone}, "keyWord": {keyword}})
	if err != nil {
		return "", err
	}
	if strings.Contains(text, "[尚未收到]") {
		return "", nil
	}
	return text, nil
}
