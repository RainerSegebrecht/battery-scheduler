package solcast

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client is a Solcast API client for rooftop PV forecasts.
type Client struct {
	siteID     string
	apiKey     string
	httpClient *http.Client
}

// New creates a new Solcast client.
func New(siteID, apiKey string) *Client {
	return &Client{
		siteID: siteID,
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// ForecastPeriod is a single 30-minute forecast period.
type ForecastPeriod struct {
	PeriodEnd   time.Time
	PvEstimate  float64 // kW (P50 estimate)
	PvEstimate10 float64 // kW (P10 — pessimistic)
	PvEstimate90 float64 // kW (P90 — optimistic)
}

type solcastResponse struct {
	Forecasts []struct {
		PeriodEnd    string  `json:"period_end"`
		Period       string  `json:"period"`
		PvEstimate   float64 `json:"pv_estimate"`
		PvEstimate10 float64 `json:"pv_estimate10"`
		PvEstimate90 float64 `json:"pv_estimate90"`
	} `json:"forecasts"`
}

// Forecast fetches the rooftop PV forecast from Solcast.
func (c *Client) Forecast() ([]ForecastPeriod, error) {
	url := fmt.Sprintf(
		"https://api.solcast.com.au/rooftop_sites/%s/forecasts?format=json&hours=48",
		c.siteID,
	)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling Solcast API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Solcast API returned HTTP %d", resp.StatusCode)
	}

	var result solcastResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding Solcast response: %w", err)
	}

	periods := make([]ForecastPeriod, 0, len(result.Forecasts))
	for _, f := range result.Forecasts {
		t, err := time.Parse(time.RFC3339, f.PeriodEnd)
		if err != nil {
			continue
		}
		periods = append(periods, ForecastPeriod{
			PeriodEnd:    t,
			PvEstimate:   f.PvEstimate,
			PvEstimate10: f.PvEstimate10,
			PvEstimate90: f.PvEstimate90,
		})
	}
	return periods, nil
}

// DailyKWh sums up the forecast energy for a given calendar day (local time).
// Uses the P10 (pessimistic) estimate to avoid over-optimism.
func DailyKWh(periods []ForecastPeriod, day time.Time) float64 {
	loc := day.Location()
	dayStart := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	dayEnd := dayStart.Add(24 * time.Hour)

	var total float64
	for _, p := range periods {
		t := p.PeriodEnd.In(loc)
		if t.After(dayStart) && !t.After(dayEnd) {
			// Each period is 30 minutes, so energy = power(kW) * 0.5h
			total += p.PvEstimate10 * 0.5
		}
	}
	return total
}
