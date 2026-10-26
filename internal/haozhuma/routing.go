package haozhuma

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

type modeKey struct{}

// WithMode 只覆盖本次任务，不改变全局模式。
func WithMode(ctx context.Context, mode string) context.Context {
	if mode == "" {
		return ctx
	}
	return context.WithValue(ctx, modeKey{}, mode)
}

type phoneRoute struct {
	client *Client
	sid    string
}

var ErrDuplicatePhone = errors.New("接码平台重复返回已占用或本批次已尝试的号码")

func validMode(mode string) bool {
	switch mode {
	case "", "auto", "haozhuma", "ecode", "both":
		return true
	}
	return false
}

func (c *Client) Mode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode == "" {
		return "auto"
	}
	return c.mode
}

func (c *Client) Fallback() *Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fallback
}

func (c *Client) suppliers() (hz, ec *Client) {
	if c.Provider == "ecode" {
		ec = c
	} else {
		hz = c
	}
	if f := c.Fallback(); f != nil {
		ec = f
	}
	return
}

func (c *Client) ValidateMode(mode string) error {
	if !validMode(mode) {
		return errors.New("取号模式只能是 auto/haozhuma/ecode/both")
	}
	hz, ec := c.suppliers()
	if mode == "ecode" && ec == nil {
		return errors.New("尚未配置 E码云，无法选择仅 E码云")
	}
	if mode == "haozhuma" && hz == nil {
		return errors.New("尚未配置豪猪，无法选择仅豪猪")
	}
	if mode == "both" && (hz == nil || ec == nil) {
		return errors.New("双平台轮换需要同时配置豪猪和 E码云")
	}
	return nil
}

func (c *Client) SetMode(mode string) error {
	mode = strings.TrimSpace(mode)
	if err := c.ValidateMode(mode); err != nil {
		return err
	}
	c.mu.Lock()
	c.mode = mode
	c.mu.Unlock()
	return nil
}

// SetFallback 只在客户端发布给工作线程之前调用。
func (c *Client) SetFallback(f *Client) error {
	if f != nil && (c.Provider == "ecode" || f.Provider != "ecode" || f.Keyword == "") {
		return errors.New("兜底必须是配置了独立短信关键词的 E码云客户端")
	}
	c.mu.Lock()
	c.fallback = f
	c.mu.Unlock()
	return nil
}

func (c *Client) effectiveMode(ctx context.Context) string {
	if m, ok := ctx.Value(modeKey{}).(string); ok && m != "" {
		return m
	}
	return c.Mode()
}

func (c *Client) ordered(ctx context.Context, alternate bool) ([]*Client, error) {
	m := c.effectiveMode(ctx)
	if err := c.ValidateMode(m); err != nil {
		return nil, err
	}
	hz, ec := c.suppliers()
	switch m {
	case "haozhuma":
		return []*Client{hz}, nil
	case "ecode":
		return []*Client{ec}, nil
	}
	if hz == nil {
		return []*Client{ec}, nil
	}
	if ec == nil {
		return []*Client{hz}, nil
	}
	if m == "both" && alternate {
		c.mu.Lock()
		alt := c.preferFallback
		c.preferFallback = !alt
		c.mu.Unlock()
		if alt {
			return []*Client{ec, hz}, nil
		}
	}
	return []*Client{hz, ec}, nil
}

func (c *Client) Summary(ctx context.Context) (Summary, error) {
	clients, err := c.ordered(ctx, false)
	if err != nil {
		return Summary{}, err
	}
	result := Summary{Occupied: -1}
	var failures []error
	available := false
	for _, supplier := range clients {
		s, err := supplier.summaryDirect(ctx)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		available = true
		result.Balance += s.Balance
		if s.Occupied >= 0 {
			result.Occupied = s.Occupied
		}
	}
	if !available {
		return result, errors.Join(failures...)
	}
	// 部分平台故障不抹掉仍可用平台的余额。
	if len(failures) > 0 {
		result.Warning = "部分接码平台不可用，余额仅包含可用平台"
	}
	return result, nil
}

func (c *Client) GetPhone(ctx context.Context, sid string) (string, error) {
	clients, err := c.ordered(ctx, true)
	if err != nil {
		return "", err
	}
	var failures []error
	for _, supplier := range clients {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		project := sid
		if supplier.Provider == "ecode" && supplier.Keyword != "" {
			project = supplier.Keyword
		}
		phone, err := supplier.getPhoneDirect(ctx, project)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		c.mu.Lock()
		if c.routes == nil {
			c.routes = make(map[string]phoneRoute)
		}
		if _, exists := c.routes[phone]; exists {
			c.mu.Unlock()
			// 该号属于原持有者：不能释放、拉黑或覆盖来源，否则会破坏在途收码。
			return "", ErrDuplicatePhone
		}
		c.routes[phone] = phoneRoute{client: supplier, sid: project}
		c.mu.Unlock()
		return phone, nil
	}
	return "", errors.Join(failures...)
}

func (c *Client) route(sid, phone string) phoneRoute {
	c.mu.Lock()
	r, ok := c.routes[phone]
	c.mu.Unlock()
	if ok {
		return r
	}
	if c.Provider == "ecode" && c.Keyword != "" {
		sid = c.Keyword
	}
	return phoneRoute{client: c, sid: sid}
}

func (c *Client) GetMessage(ctx context.Context, sid, phone string) (string, error) {
	r := c.route(sid, phone)
	return r.client.getMessageDirect(ctx, r.sid, phone)
}

func (c *Client) Release(ctx context.Context, sid, phone string) error {
	r := c.route(sid, phone)
	return r.client.releaseDirect(ctx, r.sid, phone)
}

func (c *Client) Blacklist(ctx context.Context, sid, phone string) error {
	r := c.route(sid, phone)
	return r.client.blacklistDirect(ctx, r.sid, phone)
}

// ForgetPhone 必须在拉黑/释放全部完成后调用，不能提前丢失来源。
func (c *Client) ForgetPhone(phone string) {
	c.mu.Lock()
	delete(c.routes, phone)
	c.mu.Unlock()
}

func (c *Client) IsEcodePhone(phone string) bool {
	return c.route("", phone).client.Provider == "ecode"
}

func (c *Client) ReleaseAll(ctx context.Context) error {
	hz, _ := c.suppliers()
	if hz == nil {
		return nil
	}
	return hz.releaseAllDirect(ctx)
}

func validEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func (c *Client) tokenValue() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Token
}

func (c *Client) String() string { return fmt.Sprintf("SMS client (%s)", c.Provider) }
