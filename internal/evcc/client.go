package evcc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client is an HTTP client for the evcc REST API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New creates a new evcc API client.
func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// SiteState holds the subset of /api/state we care about.
type SiteState struct {
	BatterySoC              float64 // battery state of charge in %
	BatteryPower            float64 // battery power in W (positive = charging, negative = discharging)
	BatteryMode             string  // normal | hold | charge  (effective mode)
	GridPower               float64 // grid power in W (positive = import, negative = export)
	HomePower               float64 // home consumption in W
	PvPower                 float64 // PV production in W
	TariffGrid              float64 // current grid tariff in EUR/kWh
	BatteryDischargeControl bool    // true = evcc protects battery from discharging during vehicle charging
	VehicleCharging         bool    // true = at least one loadpoint is actively charging a vehicle
}

// UnmarshalJSON maps the nested evcc /api/state JSON structure to SiteState.
func (s *SiteState) UnmarshalJSON(data []byte) error {
	var raw struct {
		Battery struct {
			SoC   float64 `json:"soc"`
			Power float64 `json:"power"`
		} `json:"battery"`
		BatteryMode             string `json:"batteryMode"`
		BatteryDischargeControl bool   `json:"batteryDischargeControl"`
		Grid                    struct {
			Power float64 `json:"power"`
		} `json:"grid"`
		HomePower  float64 `json:"homePower"`
		PvPower    float64 `json:"pvPower"`
		TariffGrid float64 `json:"tariffGrid"`
		Loadpoints []struct {
			Charging bool `json:"charging"`
		} `json:"loadpoints"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.BatterySoC = raw.Battery.SoC
	s.BatteryPower = raw.Battery.Power
	s.BatteryMode = raw.BatteryMode
	s.BatteryDischargeControl = raw.BatteryDischargeControl
	s.GridPower = raw.Grid.Power
	s.HomePower = raw.HomePower
	s.PvPower = raw.PvPower
	s.TariffGrid = raw.TariffGrid
	for _, lp := range raw.Loadpoints {
		if lp.Charging {
			s.VehicleCharging = true
			break
		}
	}
	return nil
}

// stateResponse — evcc returns state directly at the top level (no envelope).
type stateResponse = SiteState

// State fetches the current site state from evcc.
func (c *Client) State() (*SiteState, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/api/state")
	if err != nil {
		return nil, fmt.Errorf("fetching state: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("state returned HTTP %d", resp.StatusCode)
	}

	var envelope stateResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decoding state: %w", err)
	}
	return &envelope, nil
}

// BatteryMode represents the battery operating mode.
type BatteryMode string

const (
	BatteryModeNormal BatteryMode = "normal"
	BatteryModeHold   BatteryMode = "hold"
	BatteryModeCharge BatteryMode = "charge"
)

// SetBatteryMode sets the battery operating mode via the evcc API.
// Note: evcc auto-resets this after 60 seconds, so it must be called repeatedly.
func (c *Client) SetBatteryMode(mode BatteryMode) error {
	url := fmt.Sprintf("%s/api/batterymode/%s", c.baseURL, mode)
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("setting battery mode %q: %w", mode, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("set battery mode returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// TariffSlot represents a single price slot from the Tibber tariff.
type TariffSlot struct {
	Start time.Time
	End   time.Time
	Price float64 // EUR/kWh
}

// tariffResponse wraps /api/tariff/{type}
type tariffResponse struct {
	Result struct {
		Rates []struct {
			Start string  `json:"start"`
			End   string  `json:"end"`
			Price float64 `json:"price"`
		} `json:"rates"`
	} `json:"result"`
}

// Tariff fetches the price forecast from evcc (which proxies Tibber).
// tariffType is typically "grid".
func (c *Client) Tariff(tariffType string) ([]TariffSlot, error) {
	resp, err := c.httpClient.Get(fmt.Sprintf("%s/api/tariff/%s", c.baseURL, tariffType))
	if err != nil {
		return nil, fmt.Errorf("fetching tariff: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tariff returned HTTP %d", resp.StatusCode)
	}

	var envelope tariffResponse
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decoding tariff: %w", err)
	}

	slots := make([]TariffSlot, 0, len(envelope.Result.Rates))
	for _, r := range envelope.Result.Rates {
		start, err := time.Parse(time.RFC3339, r.Start)
		if err != nil {
			continue
		}
		end, err := time.Parse(time.RFC3339, r.End)
		if err != nil {
			continue
		}
		slots = append(slots, TariffSlot{Start: start, End: end, Price: r.Price})
	}
	return slots, nil
}
