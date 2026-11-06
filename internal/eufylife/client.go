// Package eufylife reads the undocumented Eufy Life smart-scale cloud API.
// Protocol references: m4ary/eufylife-api-hacs, MatthewHallCom/eufylife-mcp,
// and ohsugeek/eufylife-api (see docs/eufy.md). No upstream login payloads,
// credentials, or tokens are returned to callers.
package eufylife

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

const CacheTTL = 5 * time.Minute

var (
	ErrNotConfigured  = errors.New("eufy_not_configured")
	ErrAuthentication = errors.New("eufy_authentication_failed")
	ErrUpstream       = errors.New("eufy_upstream_unavailable")
)

type Member struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Default bool   `json:"default"`
}

type Device struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Alias string `json:"alias_name,omitempty"`
}

type Measurement struct {
	ID                     string     `json:"id"`
	CustomerID             string     `json:"customer_id"`
	DeviceID               string     `json:"device_id"`
	ProductCode            string     `json:"product_code,omitempty"`
	Timestamp              time.Time  `json:"timestamp"`
	UpdatedAt              *time.Time `json:"updated_at,omitempty"`
	WeightKG               *float64   `json:"weight_kg"`
	BMI                    *float64   `json:"bmi"`
	BodyFatPercent         *float64   `json:"body_fat_percent"`
	BodyFatMassKG          *float64   `json:"body_fat_mass_kg"`
	MuscleMassKG           *float64   `json:"muscle_mass_kg"`
	SkeletalMuscleMassKG   *float64   `json:"skeletal_muscle_mass_kg"`
	BoneMassKG             *float64   `json:"bone_mass_kg"`
	WaterPercent           *float64   `json:"water_percent"`
	SubcutaneousFatPercent *float64   `json:"subcutaneous_fat_percent"`
	VisceralFatLevel       *float64   `json:"visceral_fat_level"`
	BMR                    *float64   `json:"bmr_kcal_per_day"`
	BodyAge                *float64   `json:"body_age_years"`
	ProteinPercent         *float64   `json:"protein_percent"`
	HeartRate              *float64   `json:"heart_rate_bpm"`
}

type CacheInfo struct {
	FetchedAt  time.Time `json:"fetched_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	TTLSeconds int       `json:"ttl_seconds"`
}

// Snapshot is immutable after publication. Filters must create their own slices.
type Snapshot struct {
	Members        []Member
	Devices        []Device
	Measurements   []Measurement
	SkippedRecords int
	Cache          CacheInfo
}

type session struct {
	token, userID string
	expiresAt     time.Time
	members       []Member
	devices       []Device
}

type Client struct {
	email, password string
	http            *http.Client
	baseURL         string
	now             func() time.Time
	mu              sync.Mutex
	flight          chan struct{}
	session         session
	snapshot        Snapshot
	lastErr         error
	retryAt         time.Time
}

func New(email, password string) *Client {
	return &Client{email: email, password: password, baseURL: "https://api.eufylife.com", now: time.Now,
		http: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *Client) Enabled() bool { return c.email != "" && c.password != "" }

// Snapshot coalesces simultaneous callers, including requests for different
// members or date ranges. Canceled callers do not cancel the shared refresh.
// Errors also have a five-minute cooldown (longer if Retry-After requires it).
func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	if !c.Enabled() {
		return Snapshot{}, ErrNotConfigured
	}
	for {
		c.mu.Lock()
		if c.now().Before(c.snapshot.Cache.ExpiresAt) {
			v := c.snapshot
			c.mu.Unlock()
			return v, nil
		}
		if c.now().Before(c.retryAt) {
			err := c.lastErr
			c.mu.Unlock()
			return Snapshot{}, err
		}
		if c.flight == nil {
			c.flight = make(chan struct{})
			go c.refresh(context.WithoutCancel(ctx))
		}
		done := c.flight
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		case <-done:
		}
	}
}

func (c *Client) Status() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := "not_connected"
	if !c.Enabled() {
		status = "not_configured"
	} else if c.lastErr != nil {
		status = c.lastErr.Error()
	} else if !c.snapshot.Cache.FetchedAt.IsZero() {
		status = "connected"
	}
	out := map[string]any{"source": "eufy", "configured": c.Enabled(), "status": status, "cache_ttl_seconds": int(CacheTTL.Seconds())}
	if !c.snapshot.Cache.FetchedAt.IsZero() {
		out["cache"] = c.snapshot.Cache
		out["cache_fresh"] = c.now().Before(c.snapshot.Cache.ExpiresAt)
	}
	if c.now().Before(c.retryAt) {
		out["retry_at"] = c.retryAt
	}
	return out
}

func (c *Client) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Only this refresh goroutine uses the session; flight excludes other refreshes.
	s := c.session
	retry := CacheTTL
	var err error
	if s.token == "" || !c.now().Add(5*time.Minute).Before(s.expiresAt) {
		s, err = c.login(ctx, &retry)
	}
	var snapshot Snapshot
	if err == nil {
		snapshot, err = c.read(ctx, s, &retry)
		// A rejected token gets one re-login; other errors never cause login loops.
		if errors.Is(err, ErrAuthentication) {
			s, err = c.login(ctx, &retry)
			if err == nil {
				snapshot, err = c.read(ctx, s, &retry)
			}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = s
	c.lastErr = err
	if err != nil {
		c.retryAt = c.now().Add(retry)
		// Expired data is never presented as a successful fresh response.
		c.snapshot = Snapshot{}
	} else {
		c.retryAt = time.Time{}
		snapshot.Cache = CacheInfo{FetchedAt: c.now().UTC(), ExpiresAt: c.now().UTC().Add(CacheTTL), TTLSeconds: int(CacheTTL.Seconds())}
		c.snapshot = snapshot
	}
	close(c.flight)
	c.flight = nil
}

func (c *Client) login(ctx context.Context, retry *time.Duration) (session, error) {
	// These fixed identifiers identify the public mobile app, not the account.
	body, _ := json.Marshal(map[string]string{"client_id": "eufy-app", "client_secret": "8FHf22gaTKu7MZXqz5zytw", "email": c.email, "password": c.password})
	var out struct {
		Code      int      `json:"res_code"`
		Token     string   `json:"access_token"`
		UserID    string   `json:"user_id"`
		ExpiresIn int64    `json:"expires_in"`
		Members   []Member `json:"customers"`
		Devices   []Device `json:"devices"`
	}
	raw, err := c.request(ctx, http.MethodPost, "/v1/user/v2/email/login", body, session{}, retry)
	if err != nil {
		return session{}, err
	}
	if json.Unmarshal(raw, &out) != nil {
		return session{}, ErrUpstream
	}
	if out.Code != 1 {
		return session{}, ErrAuthentication
	}
	if out.Token == "" || out.UserID == "" {
		return session{}, ErrUpstream
	}
	if out.ExpiresIn == 0 {
		out.ExpiresIn = 30 * 24 * 60 * 60
	}
	if out.ExpiresIn < 1 || out.ExpiresIn > 365*24*60*60 {
		return session{}, ErrUpstream
	}
	if out.Members == nil {
		out.Members = []Member{}
	}
	if out.Devices == nil {
		out.Devices = []Device{}
	}
	return session{token: out.Token, userID: out.UserID, expiresAt: c.now().Add(time.Duration(out.ExpiresIn) * time.Second), members: out.Members, devices: out.Devices}, nil
}

func (c *Client) request(ctx context.Context, method, path string, body []byte, s session, retry *time.Duration) ([]byte, error) {
	r, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, ErrUpstream
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Accept-Language", "en-US,en;q=0.9")
	r.Header.Set("User-Agent", "EufyLife-iOS-3.3.7")
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Category", "Health")
		r.Header.Set("Language", "en")
		r.Header.Set("Timezone", "UTC")
		r.Header.Set("Country", "US")
	} else {
		r.Header.Set("User-Agent", "Eufylife-iOS-3.3.7-281")
		r.Header.Set("Uid", s.userID)
		r.Header.Set("Token", s.token)
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, ErrUpstream
	}
	defer resp.Body.Close()
	if raw := resp.Header.Get("Retry-After"); raw != "" {
		var delay time.Duration
		if seconds, err := strconv.ParseInt(raw, 10, 32); err == nil {
			delay = time.Duration(seconds) * time.Second
		} else if until, err := http.ParseTime(raw); err == nil {
			delay = until.Sub(c.now())
		}
		if delay > *retry {
			*retry = min(delay, 24*time.Hour)
		}
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, ErrAuthentication
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ErrUpstream
	}
	const maxBody = 16 * 1024 * 1024
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(raw) > maxBody || !json.Valid(raw) {
		return nil, ErrUpstream
	}
	return bytes.TrimSpace(raw), nil
}

type record struct {
	ID          string              `json:"id"`
	CustomerID  string              `json:"customer_id"`
	DeviceID    string              `json:"device_id"`
	ProductCode string              `json:"product_code"`
	Created     int64               `json:"create_time"`
	Updated     int64               `json:"update_time"`
	Scale       map[string]*float64 `json:"scale_data"`
}

func (c *Client) read(ctx context.Context, s session, retry *time.Duration) (Snapshot, error) {
	raw, err := c.request(ctx, http.MethodGet, "/v1/device/data", nil, s, retry)
	if err != nil {
		return Snapshot{}, err
	}
	var records []record
	if len(raw) > 0 && raw[0] == '[' {
		err = json.Unmarshal(raw, &records)
	} else {
		var out struct {
			Code *int            `json:"res_code"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &out) != nil || out.Code == nil {
			return Snapshot{}, ErrUpstream
		}
		if *out.Code != 1 {
			return Snapshot{}, ErrUpstream
		}
		if len(out.Data) == 0 || bytes.Equal(out.Data, []byte("null")) || out.Data[0] != '[' {
			return Snapshot{}, ErrUpstream
		}
		err = json.Unmarshal(out.Data, &records)
	}
	if err != nil {
		return Snapshot{}, ErrUpstream
	}
	v := Snapshot{Members: s.members, Devices: s.devices, Measurements: []Measurement{}}
	for _, r := range records {
		stamp := r.Created
		if stamp == 0 {
			stamp = r.Updated
		}
		if r.CustomerID == "" || len(r.Scale) == 0 || stamp <= 0 || stamp > 253402300799 {
			v.SkippedRecords++
			continue
		}
		m := Measurement{ID: r.ID, CustomerID: r.CustomerID, DeviceID: r.DeviceID, ProductCode: r.ProductCode, Timestamp: time.Unix(stamp, 0).UTC()}
		if r.Updated > 0 && r.Updated <= 253402300799 {
			updated := time.Unix(r.Updated, 0).UTC()
			m.UpdatedAt = &updated
		}
		m.WeightKG = metric(r.Scale, "weight", 0.1)
		m.BMI = metric(r.Scale, "bmi", 1)
		m.BodyFatPercent = metric(r.Scale, "body_fat", 1)
		m.BodyFatMassKG = metric(r.Scale, "body_fat_mass", 1)
		m.MuscleMassKG = metric(r.Scale, "muscle_mass", 1)
		m.SkeletalMuscleMassKG = metric(r.Scale, "skeletal_muscle_mass", 1)
		m.BoneMassKG = metric(r.Scale, "bone_mass", 1)
		m.WaterPercent = metric(r.Scale, "water", 1)
		m.SubcutaneousFatPercent = metric(r.Scale, "subcutaneous_fat_rate", 1)
		m.VisceralFatLevel = metric(r.Scale, "visceral_fat", 1)
		m.BMR = metric(r.Scale, "bmr", 1)
		m.BodyAge = metric(r.Scale, "body_age", 1)
		m.ProteinPercent = metric(r.Scale, "protein_ratio", 1)
		m.HeartRate = metric(r.Scale, "heart_rate", 1)
		v.Measurements = append(v.Measurements, m)
	}
	sort.SliceStable(v.Measurements, func(i, j int) bool { return v.Measurements[i].Timestamp.After(v.Measurements[j].Timestamp) })
	return v, nil
}

func metric(scale map[string]*float64, key string, factor float64) *float64 {
	v := scale[key]
	if v == nil || *v <= 0 {
		return nil
	}
	n := math.Round(*v*factor*100) / 100
	return &n
}
