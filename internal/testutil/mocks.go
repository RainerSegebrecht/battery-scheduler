// Package testutil provides mock HTTP servers for evcc and Solcast.
// Each mock server is a real net/http/httptest.Server so the production client
// code runs unchanged — only the base-URL is pointed at localhost.
package testutil

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// ──────────────────────────────────────────────────────────────────────────────
// MockEvcc
// ──────────────────────────────────────────────────────────────────────────────

// PriceScenario describes the hourly price pattern served by the mock tariff endpoint.
type PriceScenario string

const (
	// ScenarioCheapNight: 00–06 very cheap, rest expensive
	ScenarioCheapNight PriceScenario = "cheap_night"
	// ScenarioCheapMidday: 10–14 cheap (wind/sun), rest normal
	ScenarioCheapMidday PriceScenario = "cheap_midday"
	// ScenarioUniform: flat price all day
	ScenarioUniform PriceScenario = "uniform"
	// ScenarioExpensiveAll: high prices all day (no good slot)
	ScenarioExpensiveAll PriceScenario = "expensive_all"
)

// MockEvcc is an in-process HTTP server that mimics the evcc REST API,
// including /api/state, /api/batterymode/{mode} and /api/tariff/grid.
type MockEvcc struct {
	Server *httptest.Server

	mu            sync.Mutex
	state         evccState
	priceScenario PriceScenario
	modeHistory   []string // ordered list of batterymode commands received
}

type evccState struct {
	BatterySoC              float64
	BatteryPower            float64
	BatteryMode             string
	BatteryDischargeControl bool
	GridPower               float64
	HomePower               float64
	PvPower                 float64
	TariffGrid              float64
	VehicleCharging         bool
}

// NewMockEvcc creates and starts a mock evcc server with the given initial state.
func NewMockEvcc(
	batterySoC float64,
	tariffGrid float64,
	priceScenario PriceScenario,
) *MockEvcc {
	m := &MockEvcc{
		state: evccState{
			BatterySoC:  batterySoC,
			BatteryMode: "normal",
			GridPower:   200,
			HomePower:   800,
			PvPower:     0,
			TariffGrid:  tariffGrid,
		},
		priceScenario: priceScenario,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", m.handleState)
	mux.HandleFunc("/api/batterymode/", m.handleBatteryMode)
	mux.HandleFunc("/api/tariff/grid", m.handleTariff)

	m.Server = httptest.NewServer(mux)
	return m
}

// URL returns the base URL of the mock server (e.g. "http://127.0.0.1:PORT").
func (m *MockEvcc) URL() string { return m.Server.URL }

// Close shuts down the mock server.
func (m *MockEvcc) Close() { m.Server.Close() }

// SetState allows tests to update the mock state mid-run.
func (m *MockEvcc) SetState(soc, tariff float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.BatterySoC = soc
	m.state.TariffGrid = tariff
}

// SetDischargeControl allows tests to simulate evcc's batteryDischargeControl
// and vehicle-charging state.
func (m *MockEvcc) SetDischargeControl(dischargeControl bool, vehicleCharging bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.BatteryDischargeControl = dischargeControl
	m.state.VehicleCharging = vehicleCharging
}

// ModeHistory returns all batterymode commands that were received in order.
func (m *MockEvcc) ModeHistory() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]string, len(m.modeHistory))
	copy(cp, m.modeHistory)
	return cp
}

// LastMode returns the most recent batterymode command received.
func (m *MockEvcc) LastMode() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.modeHistory) == 0 {
		return ""
	}
	return m.modeHistory[len(m.modeHistory)-1]
}

func (m *MockEvcc) handleState(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	s := m.state
	m.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"battery": map[string]any{
			"soc":   s.BatterySoC,
			"power": s.BatteryPower,
		},
		"batteryMode":             s.BatteryMode,
		"batteryDischargeControl": s.BatteryDischargeControl,
		"grid": map[string]any{
			"power": s.GridPower,
		},
		"homePower":  s.HomePower,
		"pvPower":    s.PvPower,
		"tariffGrid": s.TariffGrid,
		"loadpoints": func() []any {
			if s.VehicleCharging {
				return []any{map[string]any{"charging": true}}
			}
			return []any{}
		}(),
	})
}

func (m *MockEvcc) handleBatteryMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/batterymode/"), "/")
	mode := parts[0]

	m.mu.Lock()
	m.state.BatteryMode = mode
	m.modeHistory = append(m.modeHistory, mode)
	m.mu.Unlock()

	slog.Debug("mock evcc: batterymode set", "mode", mode)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"result": mode})
}

// handleTariff serves /api/tariff/grid with 15-minute slots for 48 hours,
// using the configured PriceScenario.
func (m *MockEvcc) handleTariff(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	scenario := m.priceScenario
	m.mu.Unlock()

	today := time.Date(
		time.Now().Year(), time.Now().Month(), time.Now().Day(),
		0, 0, 0, 0, time.Now().Location(),
	)

	// 48h × 4 quarter-hours = 192 slots
	type rate struct {
		Start string  `json:"start"`
		End   string  `json:"end"`
		Value float64 `json:"value"`
	}
	rates := make([]rate, 0, 192)
	for i := 0; i < 192; i++ {
		slotStart := today.Add(time.Duration(i) * 15 * time.Minute)
		slotEnd := slotStart.Add(15 * time.Minute)
		h := slotStart.Hour()
		rates = append(rates, rate{
			Start: slotStart.Format(time.RFC3339),
			End:   slotEnd.Format(time.RFC3339),
			Value: priceForHour(scenario, h),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"rates": rates})
}

func priceForHour(scenario PriceScenario, h int) float64 {
	switch scenario {
	case ScenarioCheapNight:
		if h >= 0 && h < 6 {
			return 0.12
		}
		if h >= 17 && h < 21 {
			return 0.38
		}
		return 0.26
	case ScenarioCheapMidday:
		if h >= 10 && h < 14 {
			return 0.14
		}
		if h >= 17 && h < 21 {
			return 0.36
		}
		return 0.25
	case ScenarioExpensiveAll:
		return 0.40
	default: // ScenarioUniform
		return 0.28
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// MockSolcast
// ──────────────────────────────────────────────────────────────────────────────

// SolarScenario describes the PV yield served by MockSolcast.
type SolarScenario string

const (
	// ScenarioWinter: low yield (~1.5 kWh/day P10) — grid charging needed
	ScenarioWinter SolarScenario = "winter"
	// ScenarioOvercast: medium yield (~5 kWh/day P10) — borderline
	ScenarioOvercast SolarScenario = "overcast"
	// ScenarioSummer: high yield (~14 kWh/day P10) — no grid charging needed
	ScenarioSummer SolarScenario = "summer"
)

// MockSolcast is an in-process HTTP server that mimics the Solcast rooftop API.
type MockSolcast struct {
	Server   *httptest.Server
	SiteID   string
	Scenario SolarScenario
}

// NewMockSolcast creates and starts a mock Solcast server.
func NewMockSolcast(siteID string, scenario SolarScenario) *MockSolcast {
	m := &MockSolcast{SiteID: siteID, Scenario: scenario}
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/rooftop_sites/%s/forecasts", siteID), m.handleForecast)
	m.Server = httptest.NewServer(mux)
	return m
}

// URL returns the base URL for use with solcast.NewWithURL.
func (m *MockSolcast) URL() string { return m.Server.URL }

// Close shuts down the mock server.
func (m *MockSolcast) Close() { m.Server.Close() }

func (m *MockSolcast) handleForecast(w http.ResponseWriter, r *http.Request) {
	forecasts := m.buildForecasts()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"forecasts": forecasts})
}

func (m *MockSolcast) buildForecasts() []map[string]any {
	start := time.Now().Truncate(30 * time.Minute)
	periods := make([]map[string]any, 96)
	for i := 0; i < 96; i++ {
		t := start.Add(time.Duration(i+1) * 30 * time.Minute)
		h := t.Hour()
		p50, p10, p90 := m.pvForHour(h)
		periods[i] = map[string]any{
			"period_end":    t.Format(time.RFC3339),
			"period":        "PT30M",
			"pv_estimate":   p50,
			"pv_estimate10": p10,
			"pv_estimate90": p90,
		}
	}
	return periods
}

func (m *MockSolcast) pvForHour(h int) (float64, float64, float64) {
	if h < 6 || h >= 20 {
		return 0, 0, 0
	}
	peak := m.peakKW()
	dist := 1.0 - float64(abs(h-13))/7.0
	if dist < 0 {
		dist = 0
	}
	p50 := peak * dist
	p10 := p50 * 0.7
	p90 := p50 * 1.15
	return p50, p10, p90
}

func (m *MockSolcast) peakKW() float64 {
	switch m.Scenario {
	case ScenarioWinter:
		return 0.6
	case ScenarioOvercast:
		return 2.0
	default: // ScenarioSummer
		return 5.5
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
