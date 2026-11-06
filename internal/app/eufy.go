package app

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/sam-bee/lifeblood/internal/eufylife"
)

type EufyClient interface {
	Snapshot(context.Context) (eufylife.Snapshot, error)
	Status() map[string]any
}

func (s *Server) eufySnapshot(c *echo.Context) (eufylife.Snapshot, error) {
	v, err := s.Eufy.Snapshot(c.Request().Context())
	if err == nil {
		return v, nil
	}
	if retry, ok := s.Eufy.Status()["retry_at"].(time.Time); ok {
		c.Response().Header().Set("Retry-After", strconv.Itoa(max(1, int(time.Until(retry).Seconds())+1)))
	}
	if errors.Is(err, eufylife.ErrNotConfigured) {
		return v, fail(503, "eufy_not_configured")
	}
	if errors.Is(err, eufylife.ErrAuthentication) {
		return v, fail(503, "eufy_authentication_failed")
	}
	return v, fail(502, "eufy_upstream_unavailable")
}

func (s *Server) eufyMembers(c *echo.Context) error {
	v, err := s.eufySnapshot(c)
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"source": "eufy", "data": v.Members, "cache": v.Cache})
}

func (s *Server) eufyDevices(c *echo.Context) error {
	v, err := s.eufySnapshot(c)
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"source": "eufy", "data": v.Devices, "cache": v.Cache})
}

func eufyHasMember(v eufylife.Snapshot, id string) bool {
	for _, m := range v.Members {
		if m.ID == id {
			return true
		}
	}
	for _, m := range v.Measurements {
		if m.CustomerID == id {
			return true
		}
	}
	return false
}

func (s *Server) eufyLatestWeight(c *echo.Context) error {
	id := c.QueryParam("customer_id")
	if len(id) > 256 {
		return fail(400, "invalid_customer_id")
	}
	v, err := s.eufySnapshot(c)
	if err != nil {
		return err
	}
	if id == "" {
		for _, m := range v.Members {
			if m.Default {
				id = m.ID
				break
			}
		}
		if id == "" {
			ids := map[string]bool{}
			for _, m := range v.Members {
				ids[m.ID] = true
			}
			for _, m := range v.Measurements {
				ids[m.CustomerID] = true
			}
			delete(ids, "")
			if len(ids) > 1 {
				return fail(400, "customer_id_required")
			}
			for candidate := range ids {
				id = candidate
			}
		}
	}
	if id != "" && !eufyHasMember(v, id) {
		return fail(404, "unknown_customer")
	}
	for _, m := range v.Measurements {
		if m.CustomerID == id && m.WeightKG != nil {
			return c.JSON(200, map[string]any{"source": "eufy", "data": m, "cache": v.Cache})
		}
	}
	return fail(404, "measurement_not_found")
}

func (s *Server) eufyMeasurements(c *echo.Context) error {
	// Validate filters before touching the cloud. All filtering is local.
	id := c.QueryParam("customer_id")
	if len(id) > 256 {
		return fail(400, "invalid_customer_id")
	}
	limit, offset := 100, 0
	for _, p := range []struct {
		name     string
		dest     *int
		min, max int
	}{
		{"limit", &limit, 1, 500}, {"offset", &offset, 0, 1000000},
	} {
		if raw := c.QueryParam(p.name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < p.min || n > p.max {
				return fail(400, "invalid_"+p.name)
			}
			*p.dest = n
		}
	}
	var start, end time.Time
	for _, p := range []struct {
		name string
		dest *time.Time
	}{{"start", &start}, {"end", &end}} {
		if raw := c.QueryParam(p.name); raw != "" {
			date, err := time.Parse(time.DateOnly, raw)
			if err != nil {
				return fail(400, "invalid_"+p.name+"_date")
			}
			*p.dest = date
		}
	}
	if !start.IsZero() && !end.IsZero() && !end.After(start) {
		return fail(400, "invalid_date_range")
	}
	v, err := s.eufySnapshot(c)
	if err != nil {
		return err
	}
	if id != "" && !eufyHasMember(v, id) {
		return fail(404, "unknown_customer")
	}
	matches := make([]eufylife.Measurement, 0)
	for _, m := range v.Measurements {
		if id != "" && m.CustomerID != id || !start.IsZero() && m.Timestamp.Before(start) || !end.IsZero() && !m.Timestamp.Before(end) {
			continue
		}
		matches = append(matches, m)
	}
	from := min(offset, len(matches))
	to := min(from+limit, len(matches))
	var next *int
	if to < len(matches) {
		next = &to
	}
	return c.JSON(200, map[string]any{"source": "eufy", "data": matches[from:to], "total": len(matches), "next_offset": next,
		"skipped_records": v.SkippedRecords, "cache": v.Cache})
}
