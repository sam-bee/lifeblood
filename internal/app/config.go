package app

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Address, PublicURL, DatabaseURL               string
	OuraClientID, OuraClientSecret, WebhookSecret string
	EufyEmail, EufyPassword                       string
	EncryptionKey                                 []byte
	APITokenHash                                  []byte
	TrustedProxies                                []*net.IPNet
	HistoryDays, RetentionDays                    int
}

func LoadConfig() (Config, error) { return ParseConfig(os.Getenv) }

func ParseConfig(get func(string) string) (Config, error) {
	c := Config{Address: ":8080", PublicURL: get("PUBLIC_BASE_URL"), DatabaseURL: get("DATABASE_URL"),
		OuraClientID: get("OURA_CLIENT_ID"), OuraClientSecret: get("OURA_CLIENT_SECRET"),
		EufyEmail: strings.TrimSpace(get("EUFYLIFE_EMAIL")), EufyPassword: get("EUFYLIFE_PASSWORD"),
		WebhookSecret: get("OURA_WEBHOOK_VERIFICATION_TOKEN"), HistoryDays: 90, RetentionDays: 365}
	if v := get("LISTEN_ADDRESS"); v != "" {
		c.Address = v
	}
	for name, value := range map[string]string{"DATABASE_URL": c.DatabaseURL, "PUBLIC_BASE_URL": c.PublicURL} {
		if strings.TrimSpace(value) == "" {
			return c, fmt.Errorf("%s is required", name)
		}
	}
	for name, raw := range map[string]string{"PUBLIC_BASE_URL": c.PublicURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
			return c, fmt.Errorf("%s must be a canonical HTTPS origin without a path or trailing slash", name)
		}
	}
	var err error
	c.EncryptionKey, err = base64.StdEncoding.DecodeString(get("TOKEN_ENCRYPTION_KEY"))
	if err != nil || len(c.EncryptionKey) != 32 {
		return c, fmt.Errorf("TOKEN_ENCRYPTION_KEY must be a base64-encoded 32-byte key")
	}
	if len(get("API_TOKEN")) < 32 {
		return c, fmt.Errorf("API_TOKEN must have at least 32 characters")
	}
	c.APITokenHash = digest(get("API_TOKEN"))
	if (c.EufyEmail == "") != (c.EufyPassword == "") {
		return c, fmt.Errorf("configure EUFYLIFE_EMAIL and EUFYLIFE_PASSWORD together")
	}
	if (c.OuraClientID != "" || c.OuraClientSecret != "") &&
		(c.OuraClientID == "" || c.OuraClientSecret == "" || len(c.WebhookSecret) < 32) {
		return c, fmt.Errorf("configure all Oura credentials together; webhook verification token must have at least 32 characters")
	}
	for _, pair := range []struct {
		name  string
		value *int
		max   int
	}{{"INITIAL_HISTORY_DAYS", &c.HistoryDays, 365}, {"DATA_RETENTION_DAYS", &c.RetentionDays, 3650}} {
		if v := get(pair.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > pair.max {
				return c, fmt.Errorf("%s must be between 1 and %d", pair.name, pair.max)
			}
			*pair.value = n
		}
	}
	if c.HistoryDays > c.RetentionDays {
		return c, fmt.Errorf("history cannot exceed retention")
	}
	for _, raw := range strings.Split(get("TRUSTED_PROXY_CIDRS"), ",") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err != nil {
			return c, fmt.Errorf("invalid TRUSTED_PROXY_CIDRS entry")
		}
		c.TrustedProxies = append(c.TrustedProxies, cidr)
	}
	return c, nil
}

func (c Config) OuraEnabled() bool { return c.OuraClientID != "" && c.OuraClientSecret != "" }
