// Package testutil provides mock HTTP servers for evcc, Tibber and Solcast.
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

// MockEvcc is an in-process HTTP server that mimics the evcc REST API.
type MockEvcc struct {
	Server *httptest.Server

	mu          sync.Mutex
	state       evccState
	modeHistory []string // ordered list of batterymode commands received
}

type evccState struct {
	BatterySoC   float64
	BatteryPower float64
	BatteryMode  string
	GridPower    float64
	HomePower    float64
	PvPower      float64
	TariffGrid   float64
}

// NewMockEvcc creates and starts a mock evcc server with the given initial state.
func NewMockEvcc(
	batterySoC float64,
	tariffGrid float64,
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
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", m.handleState)
	mux.HandleFunc("/api/batterymode/", m.handleBatteryMode)

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
		"result": map[string]any{
			"batterySoC":   s.BatterySoC,
			"batteryPower": s.BatteryPower,
			"batteryMode":  s.BatteryMode,
			"gridPower":    s.GridPower,
			"homePower":    s.HomePower,
			"pvPower":      s.PvPower,
			"tariffGrid":   s.TariffGrid,
		},
	})
}

func (m *MockEvcc) handleBatteryMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// path: /api/batterymode/{mode}
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

// ──────────────────────────────────────────────────────────────────────────────
// MockTibber
// ──────────────────────────────────────────────────────────────────────────────

// PriceScenario describes the price pattern served by MockTibber.
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

// MockTibber is an in-process HTTP server that mimics the Tibber GraphQL API.
type MockTibber struct {
	Server   *httptest.Server
	Scenario PriceScenario
	// BasePrice is used for ScenarioUniform (EUR/kWh)
	BasePrice float64
}

// NewMockTibber creates and starts a mock Tibber server.
func NewMockTibber(scenario PriceScenario) *MockTibber {
	m := &MockTibber{Scenario: scenario, BasePrice: 0.28}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1-beta/gql", m.handleGQL)
	m.Server = httptest.NewServer(mux)
	return m
}

// URL returns the full GraphQL endpoint URL for use with tibber.NewWithURL.
func (m *MockTibber) URL() string { return m.Server.URL + "/v1-beta/gql" }

// Close shuts down the mock server.
func (m *MockTibber) Close() { m.Server.Close() }

func (m *MockTibber) handleGQL(w http.ResponseWriter, r *http.Request) {
	prices := m.buildPrices()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"data": map[string]any{
			"viewer": map[string]any{
				"homes": []any{
					map[string]any{
						"currentSubscription": map[string]any{
							"priceInfo": map[string]any{
								"today":    prices[:24],
								"tomorrow": prices[24:],
							},
						},
					},
				},
			},
		},
	})
}

func (m *MockTibber) buildPrices() []map[string]any {
	now := time.Now().Truncate(time.Hour)
	// Start of today 00:00
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	prices := make([]map[string]any, 48)
	for i := 0; i < 48; i++ {
		t := today.Add(time.Duration(i) * time.Hour)
		h := t.Hour()
		price := m.priceForHour(h)
		level := priceLevel(price)
		prices[i] = map[string]any{
			"total":    price,
			"energy":   price * 0.7,
			"tax":      price * 0.3,
			"startsAt": t.Format(time.RFC3339),
			"level":    level,
		}
	}
	return prices
}

func (m *MockTibber) priceForHour(h int) float64 {
	switch m.Scenario {
	case ScenarioCheapNight:
		if h >= 0 && h < 6 {
			return 0.12 // very cheap at night
		}
		if h >= 17 && h < 21 {
			return 0.38 // expensive evening (sauna time)
		}
		return 0.26
	case ScenarioCheapMidday:
		if h >= 10 && h < 14 {
			return 0.14 // cheap midday (solar/wind surplus)
		}
		if h >= 17 && h < 21 {
			return 0.36
		}
		return 0.25
	case ScenarioExpensiveAll:
		return 0.40
	default: // ScenarioUniform
		return m.BasePrice
	}
}

func priceLevel(p float64) string {
	switch {
	case p < 0.15:
		return "VERY_CHEAP"
	case p < 0.22:
		return "CHEAP"
	case p < 0.30:
		return "NORMAL"
	case p < 0.36:
		return "EXPENSIVE"
	default:
		return "VERY_EXPENSIVE"
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
	// Build 48h of 30-min periods from now
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

// pvForHour returns (P50, P10, P90) kW for a given hour depending on scenario.
func (m *MockSolcast) pvForHour(h int) (float64, float64, float64) {
	// No solar at night
	if h < 6 || h >= 20 {
		return 0, 0, 0
	}
	peak := m.peakKW()
	// Simple bell-curve: peak at noon (h=12), zero at 6 and 20
	dist := 1.0 - float64(abs(h-13))/7.0
	if dist < 0 {
		dist = 0
	}
	p50 := peak * dist
	p10 := p50 * 0.7  // pessimistic 70%
	p90 := p50 * 1.15 // optimistic 115%
	return p50, p10, p90
}

func (m *MockSolcast) peakKW() float64 {
	switch m.Scenario {
	case ScenarioWinter:
		return 0.6 // very low winter irradiation → ~1.5 kWh P10 per day
	case ScenarioOvercast:
		return 2.0 // ~5 kWh P10
	default: // ScenarioSummer
		return 5.5 // ~14 kWh P10 (5.8 kWp * typical summer factor)
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
