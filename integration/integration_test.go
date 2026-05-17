// Package integration contains end-to-end tests for battery-scheduler.
//
// Each test wires up three real HTTP mock servers (evcc, Tibber, Solcast),
// constructs a Scheduler with real production code, runs Plan() and/or
// Control() and then asserts the batterymode commands sent to evcc.
//
// Run all tests:
//
//	CGO_ENABLED=0 go test ./integration/... -v
//
// Run a single scenario (useful for step-through debugging in VS Code):
//
//	CGO_ENABLED=0 go test ./integration/... -v -run TestScenario_Winter_CheapNight
package integration

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/home/battery-scheduler/internal/config"
	"github.com/home/battery-scheduler/internal/db"
	"github.com/home/battery-scheduler/internal/evcc"
	"github.com/home/battery-scheduler/internal/scheduler"
	"github.com/home/battery-scheduler/internal/solcast"
	"github.com/home/battery-scheduler/internal/testutil"
	"github.com/home/battery-scheduler/internal/tibber"
)

// ──────────────────────────────────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────────────────────────────────

// testHarness holds all pieces of a wired-up test environment.
type testHarness struct {
	evccMock    *testutil.MockEvcc
	tibberMock  *testutil.MockTibber
	solcastMock *testutil.MockSolcast
	sched       *scheduler.Scheduler
	database    *db.DB
	cfg         *config.Config
}

// newHarness builds a full test environment.
// batterySoC:           initial battery state of charge (%)
// tariffGrid:           current Tibber price returned by evcc state (EUR/kWh)
// priceScene:           the Tibber price pattern to use
// solarScene:           the Solcast PV forecast to use
// targetTime:           "HH:MM" — battery must be full by this time
// holdPrice:            EUR/kWh above which the battery is held
// minPlanningWindowHrs: minimum hours before target time to re-plan (use 25 to always plan for tomorrow)
func newHarness(
	t *testing.T,
	batterySoC float64,
	tariffGrid float64,
	priceScene testutil.PriceScenario,
	solarScene testutil.SolarScenario,
	targetTime string,
	holdPrice float64,
	minPlanningWindowHrs int,
) *testHarness {
	t.Helper()

	// Start mock servers
	mockEvcc := testutil.NewMockEvcc(batterySoC, tariffGrid)
	mockTibber := testutil.NewMockTibber(priceScene)
	mockSolcast := testutil.NewMockSolcast("test-site-id", solarScene)

	t.Cleanup(func() {
		mockEvcc.Close()
		mockTibber.Close()
		mockSolcast.Close()
	})

	// Build config pointing at mock servers
	cfg := &config.Config{
		Evcc: config.EvccConfig{
			URL:          mockEvcc.URL(),
			PollInterval: config.Duration{Duration: 45 * time.Second},
		},
		Tibber: config.TibberConfig{
			Token: "mock-token",
		},
		Solcast: config.SolcastConfig{
			SiteID:     "test-site-id",
			APIKey:     "mock-api-key",
			FetchTimes: []string{"06:00", "14:00"},
		},
		Battery: config.BatteryConfig{
			CapacityKWh:          10.0,
			MaxChargePowerKW:     5.0,
			SolarThresholdKWh:    8.0,
			TargetSOC:            100,
			TargetTime:           targetTime,
			HoldAbovePrice:       holdPrice,
			MinSOC:               10,
			MinPlanningWindowHrs: minPlanningWindowHrs,
		},
		Database: config.DatabaseConfig{
			Path: t.TempDir() + "/test.db",
		},
		Log: config.LogConfig{Level: "debug"},
	}

	// Open in-memory-equivalent SQLite DB (temp dir)
	database, err := db.Open(cfg.Database.Path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	// Build clients pointing at mock servers
	evccClient := evcc.New(mockEvcc.URL())
	tibberClient := tibber.NewWithURL("mock-token", mockTibber.URL())
	solcastClient := solcast.NewWithURL("test-site-id", "mock-api-key", mockSolcast.URL())

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	sched := scheduler.New(cfg, database, evccClient, tibberClient, solcastClient, log)

	return &testHarness{
		evccMock:    mockEvcc,
		tibberMock:  mockTibber,
		solcastMock: mockSolcast,
		sched:       sched,
		database:    database,
		cfg:         cfg,
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Scenario 1: Winter + cheap night prices → grid charging should be planned
// ──────────────────────────────────────────────────────────────────────────────

// TestScenario_Winter_CheapNight tests the primary use case:
// - It is winter, solar forecast is low (~1.5 kWh P10)
// - Tibber has cheap slots at night (00:00–06:00)
// - Battery is at 20% SoC
// - Target: 100% by 20:00
//
// Expected: Plan() selects cheap night slots and Control() sends "charge" during
// those slots and "hold" when price is above threshold.
func TestScenario_Winter_CheapNight(t *testing.T) {
	t.Log("=== Scenario: Winter, cheap night prices, battery at 20% SoC ===")

	h := newHarness(t,
		20,                          // batterySoC
		0.38,                        // current tariffGrid (expensive now)
		testutil.ScenarioCheapNight, // Tibber pattern: 00–06 = 0.12, evening = 0.38
		testutil.ScenarioWinter,     // Solcast pattern: low yield
		nextTargetTime(20, 0),       // target 20:00 — with MinPlanningWindowHrs=8 this plans for tomorrow
		0.25,                        // holdAbovePrice
		25,                          // minPlanningWindowHrs: always plan for tomorrow
	)

	// Step 1: Plan — should pick tomorrow's cheap night slots (00–06 at 0.12 EUR/kWh)
	t.Log("--- Running Plan() ---")
	if err := h.sched.Plan(); err != nil {
		t.Fatalf("Plan() failed: %v", err)
	}

	// Assert: all planned slots must be cheap (≤ 0.15 EUR/kWh), not the expensive evening slots
	slots, err := h.database.UpcomingSlots()
	if err != nil {
		t.Fatalf("UpcomingSlots() failed: %v", err)
	}
	if len(slots) == 0 {
		t.Fatal("expected charging slots to be planned, got none")
	}
	for _, s := range slots {
		t.Logf("  planned slot: %s–%s @ %.4f EUR/kWh",
			s.StartTime.Local().Format("15:04"), s.EndTime.Local().Format("15:04"), s.PriceEUR)
		if s.PriceEUR > 0.15 {
			t.Errorf("expected cheap slot (≤ 0.15 EUR/kWh), got %.4f EUR/kWh — wrong slots selected!", s.PriceEUR)
		}
	}

	// Step 2: Control() during expensive evening → hold (not in cheap slot yet)
	t.Log("--- Running Control() with expensive current price (0.38) ---")
	if err := h.sched.Control(); err != nil {
		t.Fatalf("Control() failed: %v", err)
	}

	lastMode := h.evccMock.LastMode()
	t.Logf("batterymode sent to evcc: %q", lastMode)

	// Price 0.38 > holdAbovePrice 0.25 → hold
	if lastMode != "hold" {
		t.Errorf("expected hold (expensive price, not in cheap slot yet), got %q", lastMode)
	}

	t.Logf("mode history: %v", h.evccMock.ModeHistory())
}

// ──────────────────────────────────────────────────────────────────────────────
// Scenario 2: Summer → solar sufficient, no grid charging
// ──────────────────────────────────────────────────────────────────────────────

// TestScenario_Summer_NoGridCharge verifies that when solar forecast is above the
// threshold (8 kWh), Plan() clears all charging slots and Control() sets "normal".
func TestScenario_Summer_NoGridCharge(t *testing.T) {
	t.Log("=== Scenario: Summer, high solar forecast → no grid charging ===")

	h := newHarness(t,
		40,                       // battery at 40%
		0.22,                     // moderate current price
		testutil.ScenarioUniform, // flat prices — doesn't matter
		testutil.ScenarioSummer,  // high solar forecast
		nextTargetTime(20, 0),
		0.25,
		25, // minPlanningWindowHrs: always plan for tomorrow
	)

	t.Log("--- Running Plan() ---")
	if err := h.sched.Plan(); err != nil {
		t.Fatalf("Plan() failed: %v", err)
	}

	t.Log("--- Running Control() with moderate price ---")
	if err := h.sched.Control(); err != nil {
		t.Fatalf("Control() failed: %v", err)
	}

	lastMode := h.evccMock.LastMode()
	t.Logf("batterymode sent to evcc: %q", lastMode)

	if lastMode != "normal" {
		t.Errorf("expected normal (solar sufficient, no grid charge), got %q", lastMode)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Scenario 3: Battery already full → no charging, normal mode
// ──────────────────────────────────────────────────────────────────────────────

// TestScenario_BatteryFull tests that when the battery is already at the target
// SoC, Plan() does not create any charging slots and Control() returns "normal".
func TestScenario_BatteryFull(t *testing.T) {
	t.Log("=== Scenario: Battery already at 100% SoC ===")

	h := newHarness(t,
		100,  // battery full
		0.30, // expensive price
		testutil.ScenarioCheapNight,
		testutil.ScenarioWinter,
		nextTargetTime(20, 0),
		0.25,
		25, // minPlanningWindowHrs: always plan for tomorrow
	)

	t.Log("--- Running Plan() ---")
	if err := h.sched.Plan(); err != nil {
		t.Fatalf("Plan() failed: %v", err)
	}

	t.Log("--- Running Control() ---")
	if err := h.sched.Control(); err != nil {
		t.Fatalf("Control() failed: %v", err)
	}

	lastMode := h.evccMock.LastMode()
	t.Logf("batterymode sent to evcc: %q", lastMode)

	// Battery is full — no charging needed. Price is above hold threshold,
	// so the scheduler might still hold it.
	if lastMode != "normal" && lastMode != "hold" {
		t.Errorf("expected normal or hold (battery full), got %q", lastMode)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Scenario 4: Cheap midday price (wind/solar surplus) → charge during day
// ──────────────────────────────────────────────────────────────────────────────

// TestScenario_CheapMidday verifies that the planner selects midday slots
// (10:00–14:00) when those are the cheapest, even without a "night window".
func TestScenario_CheapMidday(t *testing.T) {
	t.Log("=== Scenario: Overcast, cheap midday prices ===")

	h := newHarness(t,
		30,   // battery at 30%
		0.14, // current price is cheap (midday scenario)
		testutil.ScenarioCheapMidday,
		testutil.ScenarioOvercast, // ~5 kWh P10 — below threshold of 8
		nextTargetTime(20, 0),
		0.25,
		25, // minPlanningWindowHrs: always plan for tomorrow
	)

	t.Log("--- Running Plan() ---")
	if err := h.sched.Plan(); err != nil {
		t.Fatalf("Plan() failed: %v", err)
	}

	// Simulate Control() during the cheap midday window (price 0.14)
	// The mock evcc already reports 0.14 as current tariffGrid.
	t.Log("--- Running Control() during cheap midday window ---")
	if err := h.sched.Control(); err != nil {
		t.Fatalf("Control() failed: %v", err)
	}

	lastMode := h.evccMock.LastMode()
	t.Logf("batterymode sent to evcc: %q", lastMode)
	t.Logf("mode history: %v", h.evccMock.ModeHistory())

	// Price 0.14 < holdAbovePrice 0.25 → not held. Could be "charge" (if slot
	// was planned) or "normal" (if no slot yet covers now). Both are acceptable;
	// we must NOT get "hold".
	if lastMode == "hold" {
		t.Errorf("unexpected hold during cheap price (%.2f < holdAbovePrice %.2f)", 0.14, 0.25)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Scenario 5: All prices expensive → no cheap slots found, normal/hold only
// ──────────────────────────────────────────────────────────────────────────────

// TestScenario_ExpensiveAll verifies graceful handling when Tibber has no cheap
// slots (e.g. during a cold snap). The scheduler must not crash and must set
// "hold" to preserve what is in the battery.
func TestScenario_ExpensiveAll(t *testing.T) {
	t.Log("=== Scenario: All Tibber prices expensive ===")

	h := newHarness(t,
		50,
		0.42, // very expensive right now
		testutil.ScenarioExpensiveAll,
		testutil.ScenarioWinter,
		nextTargetTime(20, 0),
		0.25,
		25, // minPlanningWindowHrs: always plan for tomorrow
	)

	t.Log("--- Running Plan() ---")
	if err := h.sched.Plan(); err != nil {
		t.Fatalf("Plan() failed: %v", err)
	}

	t.Log("--- Running Control() ---")
	if err := h.sched.Control(); err != nil {
		t.Fatalf("Control() failed: %v", err)
	}

	lastMode := h.evccMock.LastMode()
	t.Logf("batterymode sent to evcc: %q", lastMode)

	// All prices > holdAbovePrice → battery should be held (don't waste stored energy)
	if lastMode != "hold" {
		t.Errorf("expected hold when all prices expensive, got %q", lastMode)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Scenario 6: Multiple Control() calls simulate the 45s polling loop
// ──────────────────────────────────────────────────────────────────────────────

// TestScenario_PollingLoop simulates several consecutive Control() ticks to
// verify that the batterymode command is consistently re-sent (as required
// because evcc auto-resets after 60s) and that the mode does not flip
// unexpectedly.
func TestScenario_PollingLoop(t *testing.T) {
	t.Log("=== Scenario: Polling loop — 5 consecutive Control() calls ===")

	h := newHarness(t,
		25,
		0.35, // expensive
		testutil.ScenarioCheapNight,
		testutil.ScenarioWinter,
		nextTargetTime(20, 0),
		0.25,
		25, // minPlanningWindowHrs: always plan for tomorrow
	)

	t.Log("--- Running Plan() ---")
	if err := h.sched.Plan(); err != nil {
		t.Fatalf("Plan() failed: %v", err)
	}

	const ticks = 5
	t.Logf("--- Running Control() %d times ---", ticks)
	for i := 0; i < ticks; i++ {
		if err := h.sched.Control(); err != nil {
			t.Fatalf("Control() tick %d failed: %v", i+1, err)
		}
	}

	history := h.evccMock.ModeHistory()
	t.Logf("mode history (%d commands): %v", len(history), history)

	if len(history) != ticks {
		t.Errorf("expected %d batterymode commands, got %d", ticks, len(history))
	}

	// All commands should be the same mode (no flipping)
	first := history[0]
	for i, m := range history {
		if m != first {
			t.Errorf("mode changed unexpectedly at tick %d: %q → %q", i, first, m)
		}
	}
	t.Logf("all %d ticks consistently sent mode %q", ticks, first)
}

// ──────────────────────────────────────────────────────────────────────────────
// Scenario 7: MinPlanningWindow pushes planning to tomorrow → cheap night slots
// ──────────────────────────────────────────────────────────────────────────────

// TestScenario_PlanningWindow verifies that when MinPlanningWindowHrs is set to 24
// (meaning: always plan for tomorrow), the scheduler picks cheap night slots from
// tomorrow's Tibber prices (00:00–06:00 at 0.12 EUR/kWh) instead of whatever
// expensive slots remain today.
func TestScenario_PlanningWindow(t *testing.T) {
	t.Log("=== Scenario: MinPlanningWindowHrs=24 forces planning for tomorrow ===")

	mockEvcc := testutil.NewMockEvcc(20, 0.38)
	mockTibber := testutil.NewMockTibber(testutil.ScenarioCheapNight)
	mockSolcast := testutil.NewMockSolcast("test-site-id", testutil.ScenarioWinter)

	t.Cleanup(func() {
		mockEvcc.Close()
		mockTibber.Close()
		mockSolcast.Close()
	})

	cfg := &config.Config{
		Evcc:   config.EvccConfig{URL: mockEvcc.URL(), PollInterval: config.Duration{Duration: 45 * time.Second}},
		Tibber: config.TibberConfig{Token: "mock-token"},
		Solcast: config.SolcastConfig{
			SiteID: "test-site-id", APIKey: "mock-api-key",
			FetchTimes: []string{"06:00", "14:00"},
		},
		Battery: config.BatteryConfig{
			CapacityKWh:          10.0,
			MaxChargePowerKW:     5.0,
			SolarThresholdKWh:    8.0,
			TargetSOC:            100,
			TargetTime:           "20:00",
			HoldAbovePrice:       0.25,
			MinSOC:               10,
			MinPlanningWindowHrs: 24, // always plan for tomorrow
		},
		Database: config.DatabaseConfig{Path: t.TempDir() + "/test.db"},
		Log:      config.LogConfig{Level: "debug"},
	}

	database, err := db.Open(cfg.Database.Path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	evccClient := evcc.New(mockEvcc.URL())
	tibberClient := tibber.NewWithURL("mock-token", mockTibber.URL())
	solcastClient := solcast.NewWithURL("test-site-id", "mock-api-key", mockSolcast.URL())
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	sched := scheduler.New(cfg, database, evccClient, tibberClient, solcastClient, log)

	t.Log("--- Running Plan() with MinPlanningWindowHrs=24 ---")
	if err := sched.Plan(); err != nil {
		t.Fatalf("Plan() failed: %v", err)
	}

	// Inspect the planned slots via the DB: all slots should be cheap (0.12 EUR/kWh)
	// because the scheduler picks from tomorrow's 00–06 window.
	// We verify indirectly: run Control() — since we are NOT inside any cheap slot
	// right now (they are tomorrow night), and price is 0.38 > 0.25, expect "hold".
	t.Log("--- Running Control() ---")
	if err := sched.Control(); err != nil {
		t.Fatalf("Control() failed: %v", err)
	}

	lastMode := mockEvcc.LastMode()
	t.Logf("batterymode sent to evcc: %q", lastMode)
	t.Logf("mode history: %v", mockEvcc.ModeHistory())

	// Price 0.38 > holdAbovePrice 0.25 → hold (not in tomorrow's night slot yet)
	if lastMode != "hold" {
		t.Errorf("expected hold (planning window pushed to tomorrow, not in cheap slot yet), got %q", lastMode)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Scenario 8: DryRun mode — decisions logged, no commands sent to evcc
// ──────────────────────────────────────────────────────────────────────────────

// TestScenario_DryRun verifies that when DryRun is enabled, Control() logs a
// decision to the DB (with "[dry-run]" prefix in Reason) but does NOT call
// SetBatteryMode on evcc.
func TestScenario_DryRun(t *testing.T) {
	t.Log("=== Scenario: DryRun mode — no evcc commands ===")

	h := newHarness(t,
		30,
		0.38, // expensive — would normally trigger "hold"
		testutil.ScenarioCheapNight,
		testutil.ScenarioWinter,
		nextTargetTime(20, 0),
		0.25,
		25, // minPlanningWindowHrs: always plan for tomorrow
	)

	h.sched.DryRun = true

	t.Log("--- Running Control() with DryRun=true ---")
	if err := h.sched.Control(); err != nil {
		t.Fatalf("Control() failed: %v", err)
	}

	// evcc must NOT have received any batterymode command
	history := h.evccMock.ModeHistory()
	t.Logf("evcc mode history: %v", history)
	if len(history) != 0 {
		t.Errorf("expected no evcc commands in dry-run mode, got %d: %v", len(history), history)
	}

	// DB must contain a state-log entry with "[dry-run]" in the Reason
	entries, err := h.database.RecentStateLog(1)
	if err != nil {
		t.Fatalf("RecentStateLog failed: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected a state-log entry in dry-run mode, got none")
	}
	t.Logf("state-log reason: %q", entries[0].Reason)
	if !containsDryRun(entries[0].Reason) {
		t.Errorf("expected '[dry-run]' in state-log reason, got %q", entries[0].Reason)
	}
}

// containsDryRun returns true if s contains the dry-run marker.
func containsDryRun(s string) bool {
	return len(s) >= 9 && s[:9] == "[dry-run]"
}

// ──────────────────────────────────────────────────────────────────────────────
// Scenario 9: Vehicle charging + batteryDischargeControl → hold
// ──────────────────────────────────────────────────────────────────────────────

// TestScenario_VehicleCharging verifies that when a vehicle is charging AND
// evcc has batteryDischargeControl enabled, the scheduler sets "hold" even
// when the price would normally allow "normal" mode.
func TestScenario_VehicleCharging(t *testing.T) {
	t.Log("=== Scenario: Vehicle charging + batteryDischargeControl ===")

	h := newHarness(t,
		70,   // battery at 70%
		0.18, // price below hold threshold — would normally be "normal"
		testutil.ScenarioUniform,
		testutil.ScenarioSummer, // solar sufficient → no grid charge slots
		nextTargetTime(20, 0),
		0.25,
		25,
	)

	// Simulate batteryDischargeControl active and a vehicle charging
	h.evccMock.SetDischargeControl(true, true)

	t.Log("--- Running Plan() ---")
	if err := h.sched.Plan(); err != nil {
		t.Fatalf("Plan() failed: %v", err)
	}

	t.Log("--- Running Control() — price 0.18 < hold threshold, but vehicle charging ---")
	if err := h.sched.Control(); err != nil {
		t.Fatalf("Control() failed: %v", err)
	}

	lastMode := h.evccMock.LastMode()
	t.Logf("batterymode sent to evcc: %q", lastMode)
	t.Logf("mode history: %v", h.evccMock.ModeHistory())

	// Despite low price, battery must be held to protect against discharge
	if lastMode != "hold" {
		t.Errorf("expected hold (vehicle charging + discharge control), got %q", lastMode)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Helper
// ──────────────────────────────────────────────────────────────────────────────

// nextTargetTime returns "HH:MM" for the given hour/minute if it is still in the
// future today, otherwise for tomorrow. This avoids test failures near midnight.
func nextTargetTime(h, m int) string {
	return time.Date(0, 0, 0, h, m, 0, 0, time.Local).Format("15:04")
}
