package codexstatus

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

func (m *Monitor) notify(c Credential) {
	m.mu.Lock()
	cfg, now := m.cfg, m.now()
	id := credentialKey{c.ID, c.Owner}
	if m.ctx.Err() != nil || c.ID == "" || strings.TrimSpace(cfg.Webhook) == "" || now.Before(m.notified[id].Add(time.Hour)) {
		m.mu.Unlock()
		return
	}
	m.notified[id] = now
	m.mu.Unlock()
	file := c.File
	if file == "" {
		file = c.ID
	}
	account := ""
	if c.Email != "" {
		account = fmt.Sprintf("的账号「%s」", c.Email)
	}
	message := fmt.Sprintf("CPA 实例「%s」%s凭据「%s」可能出现降智。", cfg.InstanceName, account, file)
	go func() {
		if err := m.send(m.ctx, cfg, message); err != nil && m.ctx.Err() == nil {
			log.Warn("event notification delivery failed")
		}
	}()
}

func notificationRequest(ctx context.Context, cfg config.EventNotifications, message string, now time.Time) (*http.Request, error) {
	endpoint := cfg.Webhook
	var payload map[string]any
	switch cfg.Platform {
	case "feishu":
		payload = map[string]any{"msg_type": "text", "content": map[string]string{"text": message}}
		if cfg.Secret != "" {
			stamp := fmt.Sprint(now.Unix())
			mac := hmac.New(sha256.New, []byte(stamp+"\n"+cfg.Secret))
			payload["timestamp"], payload["sign"] = stamp, base64.StdEncoding.EncodeToString(mac.Sum(nil))
		}
	case "wecom", "dingtalk":
		payload = map[string]any{"msgtype": "text", "text": map[string]string{"content": message}}
		if cfg.Platform == "dingtalk" && cfg.Secret != "" {
			stamp := fmt.Sprint(now.UnixMilli())
			mac := hmac.New(sha256.New, []byte(cfg.Secret))
			_, _ = mac.Write([]byte(stamp + "\n" + cfg.Secret))
			u, err := url.Parse(endpoint)
			if err != nil {
				return nil, fmt.Errorf("invalid webhook")
			}
			q := u.Query()
			q.Set("timestamp", stamp)
			q.Set("sign", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
			u.RawQuery = q.Encode()
			endpoint = u.String()
		}
	default:
		return nil, fmt.Errorf("unsupported notification platform")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, err
}

func sendNotification(ctx context.Context, cfg config.EventNotifications, message string) error {
	req, err := notificationRequest(ctx, cfg, message, time.Now())
	if err != nil {
		return err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook transport failed")
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Warn("could not close webhook response")
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Code       *int `json:"code"`
		ErrCode    *int `json:"errcode"`
		StatusCode *int `json:"StatusCode"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&result); err != nil {
		return fmt.Errorf("invalid webhook response")
	}
	code := result.ErrCode
	if cfg.Platform == "feishu" {
		code = result.Code
		if code == nil {
			code = result.StatusCode
		}
	}
	if code == nil || *code != 0 {
		return fmt.Errorf("webhook rejected notification")
	}
	return nil
}
