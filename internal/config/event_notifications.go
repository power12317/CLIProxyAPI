package config

import (
	"fmt"
	"net/url"
	"strings"
)

// EventNotifications configures one robot; an empty webhook disables delivery.
type EventNotifications struct {
	InstanceName string `yaml:"instance-name" json:"instance-name"`
	Platform     string `yaml:"platform" json:"platform"`
	Webhook      string `yaml:"webhook" json:"webhook"`
	Secret       string `yaml:"secret,omitempty" json:"secret,omitempty"`
}

func (c EventNotifications) Validate() error {
	if strings.TrimSpace(c.Webhook) == "" {
		return nil
	}
	if strings.TrimSpace(c.InstanceName) == "" {
		return fmt.Errorf("event notifications require an instance name")
	}
	u, err := url.Parse(c.Webhook)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("invalid event notification webhook")
	}
	valid := false
	switch c.Platform {
	case "feishu":
		valid = u.Host == "open.feishu.cn" && strings.HasPrefix(u.Path, "/open-apis/bot/v2/hook/")
	case "wecom":
		valid = u.Host == "qyapi.weixin.qq.com" && u.Path == "/cgi-bin/webhook/send" && u.Query().Get("key") != ""
	case "dingtalk":
		valid = u.Host == "oapi.dingtalk.com" && u.Path == "/robot/send" && u.Query().Get("access_token") != ""
	}
	if !valid {
		return fmt.Errorf("webhook does not match the selected notification platform")
	}
	return nil
}
