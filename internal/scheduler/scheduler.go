package scheduler

import (
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/home/battery-scheduler/internal/config"
	"github.com/home/battery-scheduler/internal/db"
	"github.com/home/battery-scheduler/internal/evcc"
	"github.com/home/battery-scheduler/internal/solcast"
	"github.com/home/battery-scheduler/internal/tibber"
)

// Scheduler orchestrates the battery charging logic.
type Scheduler struct {
	cfg     *config.Config
	db      *db.DB
	evcc    *evcc.Client
	tibber  *tibber.Client
	solcast *solcast.Client
	log     *slog.Logger

	lastPlanDate time.Time // track which day we last planned for
}

// New creates a new Scheduler instance.
func New(
	cfg *config.Config,
	database *db.DB,
	evccClient *evcc.Client,
	tibberClient *tibber.Client,
	solcastClient *solcast.Client,
	log *slog.Logger,
) *Scheduler {
	return &Scheduler{
		cfg:     cfg,
		db:      database,
		evcc:    evccClient,
		tibber:  tibberClient,
		solcast: solcastClient,
		log:     log,
	}
}

// Plan fetches forecasts and builds the charging schedule for the upcoming target window.
// It should be called periodically (e.g. at the configured fetch times).
func (s *Scheduler) Plan() error {
	now := time.Now()

	// --- 1. Determine the target time to plan for ---
	// If fewer than MinPlanningWindowHrs remain until today's target time, plan
	// for tomorrow instead. This ensures enough cheap slots are available.
	targetTime := s.nextTargetTime(now)
	s.log.Info("planning for target", "target", targetTime.Local().Format("2006-01-02 15:04"), "window_remaining", targetTime.Sub(now).Round(time.Minute))

	// --- 2. Fetch Solcast forecast ---
	s.log.Info("fetching Solcast forecast")
	forecastPeriods, err := s.solcast.Forecast()
	if err != nil {
		return fmt.Errorf("solcast forecast: %w", err)
	}

	solarKWh := solcast.DailyKWh(forecastPeriods, targetTime)
	s.log.Info("Solcast forecast", "date", targetTime.Format("2006-01-02"), "kwh_p10", solarKWh)

	// --- 2. Fetch current battery SoC from evcc ---
	state, err := s.evcc.State()
	if err != nil {
		return fmt.Errorf("evcc state: %w", err)
	}
	currentSOC := state.BatterySoC
	s.log.Info("battery state", "soc", currentSOC, "mode", state.BatteryMode)

	// --- 3. Decide if grid charging is needed ---
	needsGridCharge := s.needsGridCharging(solarKWh, currentSOC)
	s.log.Info("planning decision",
		"solar_kwh", solarKWh,
		"solar_threshold_kwh", s.cfg.Battery.SolarThresholdKWh,
		"needs_grid_charge", needsGridCharge,
	)

	// Store forecast in DB
	if err := s.db.InsertForecast(db.ForecastEntry{
		FetchedAt:     now,
		SolcastKWh:    solarKWh,
		TibberFetched: needsGridCharge,
	}); err != nil {
		s.log.Warn("failed to store forecast", "err", err)
	}

	if !needsGridCharge {
		s.log.Info("sufficient solar forecast, clearing grid-charge schedule")
		return s.db.UpsertChargingSlots(nil) // clear future slots
	}

	// --- 4. Fetch Tibber prices ---
	s.log.Info("fetching Tibber prices")
	priceSlots, err := s.tibber.Prices()
	if err != nil {
		return fmt.Errorf("tibber prices: %w", err)
	}

	// --- 5. Calculate how many hours of charging we need ---
	neededKWh := s.neededChargeKWh(currentSOC)
	neededHours := math.Ceil(neededKWh / s.cfg.Battery.MaxChargePowerKW)
	s.log.Info("charge requirement",
		"current_soc", currentSOC,
		"target_soc", s.cfg.Battery.TargetSOC,
		"needed_kwh", neededKWh,
		"needed_hours", neededHours,
	)

	if neededHours <= 0 {
		s.log.Info("battery already at or above target SoC, clearing schedule")
		return s.db.UpsertChargingSlots(nil)
	}

	// --- 6. Select cheapest N hours before the target time ---
	slots := s.selectCheapestSlots(priceSlots, targetTime, int(neededHours))
	if len(slots) == 0 {
		s.log.Warn("no suitable charging slots found before target time")
		return nil
	}

	s.log.Info("planned charging slots", "count", len(slots))
	for _, sl := range slots {
		s.log.Info("  slot",
			"start", sl.StartTime.Local().Format("15:04"),
			"end", sl.EndTime.Local().Format("15:04"),
			"price", fmt.Sprintf("%.4f EUR/kWh", sl.PriceEUR),
		)
	}

	return s.db.UpsertChargingSlots(slots)
}

// Control is the fast control loop — called every poll_interval.
// It reads the current state and sends the appropriate battery mode command to evcc.
func (s *Scheduler) Control() error {
	now := time.Now()

	// Re-plan if we haven't planned for the upcoming target day yet
	targetTime := s.nextTargetTime(now)
	planDay := targetTime.Truncate(24 * time.Hour)
	if !s.lastPlanDate.Equal(planDay) {
		if err := s.Plan(); err != nil {
			s.log.Warn("planning failed, using existing schedule", "err", err)
		} else {
			s.lastPlanDate = planDay
		}
	}

	// Fetch current state
	state, err := s.evcc.State()
	if err != nil {
		return fmt.Errorf("evcc state: %w", err)
	}

	// Determine desired action
	action, reason := s.decideAction(now, state)

	s.log.Debug("control loop",
		"soc", state.BatterySoC,
		"grid_price", state.TariffGrid,
		"action", action,
		"reason", reason,
	)

	// Send command to evcc
	if err := s.evcc.SetBatteryMode(evcc.BatteryMode(action)); err != nil {
		return fmt.Errorf("setting battery mode: %w", err)
	}

	// Log to DB
	_ = s.db.LogState(db.StateEntry{
		Timestamp:   now,
		BatterySOC:  state.BatterySoC,
		BatteryMode: string(action),
		GridPrice:   state.TariffGrid,
		Action:      string(action),
		Reason:      reason,
	})

	return nil
}

// decideAction determines the appropriate battery mode based on current state.
func (s *Scheduler) decideAction(now time.Time, state *evcc.SiteState) (evcc.BatteryMode, string) {
	// Check if we're in a planned charging slot
	slot, err := s.db.ActiveSlotAt(now)
	if err != nil {
		s.log.Warn("failed to check active slot", "err", err)
	}

	if slot != nil {
		// Only charge if battery is not yet at target SoC
		if state.BatterySoC < float64(s.cfg.Battery.TargetSOC) {
			return evcc.BatteryModeCharge, fmt.Sprintf(
				"in cheap slot (%.4f EUR/kWh), SoC %.0f%% < target %d%%",
				slot.PriceEUR, state.BatterySoC, s.cfg.Battery.TargetSOC,
			)
		}
		return evcc.BatteryModeNormal, fmt.Sprintf(
			"in cheap slot but already at target SoC %.0f%%", state.BatterySoC,
		)
	}

	// Check if current price is high → hold battery
	if state.TariffGrid > s.cfg.Battery.HoldAbovePrice && state.BatterySoC > float64(s.cfg.Battery.MinSOC) {
		return evcc.BatteryModeHold, fmt.Sprintf(
			"price %.4f EUR/kWh > hold threshold %.4f EUR/kWh, preserving battery for peak",
			state.TariffGrid, s.cfg.Battery.HoldAbovePrice,
		)
	}

	return evcc.BatteryModeNormal, "no active slot, price below hold threshold — normal operation"
}

// needsGridCharging returns true if the solar forecast is insufficient to fill the battery
// to the target SoC by the target time.
func (s *Scheduler) needsGridCharging(solarKWh, currentSOC float64) bool {
	if solarKWh >= s.cfg.Battery.SolarThresholdKWh {
		return false
	}
	neededKWh := s.neededChargeKWh(currentSOC)
	return neededKWh > 0
}

// neededChargeKWh returns how many kWh are needed to reach the target SoC.
func (s *Scheduler) neededChargeKWh(currentSOC float64) float64 {
	targetSOC := float64(s.cfg.Battery.TargetSOC)
	if currentSOC >= targetSOC {
		return 0
	}
	return (targetSOC - currentSOC) / 100.0 * s.cfg.Battery.CapacityKWh
}

// selectCheapestSlots returns the cheapest n full-hour slots from Tibber prices
// that fall before the given target time.
func (s *Scheduler) selectCheapestSlots(prices []tibber.PriceSlot, before time.Time, n int) []db.ChargingSlot {
	// Filter to future slots before target time
	now := time.Now()
	var candidates []tibber.PriceSlot
	for _, p := range prices {
		end := p.StartsAt.Add(time.Hour)
		if p.StartsAt.After(now) && end.Before(before) || end.Equal(before) {
			candidates = append(candidates, p)
		}
	}

	// Sort by price ascending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Total < candidates[j].Total
	})

	if n > len(candidates) {
		n = len(candidates)
	}
	cheapest := candidates[:n]

	// Convert to db.ChargingSlot
	slots := make([]db.ChargingSlot, len(cheapest))
	for i, p := range cheapest {
		slots[i] = db.ChargingSlot{
			StartTime: p.StartsAt,
			EndTime:   p.StartsAt.Add(time.Hour),
			PriceEUR:  p.Total,
			Active:    true,
		}
	}
	return slots
}

// nextTargetTime returns the target time to plan for.
// If fewer than MinPlanningWindowHrs hours remain until today's target, it returns
// tomorrow's target time — ensuring enough Tibber slots (including night hours) are available.
func (s *Scheduler) nextTargetTime(now time.Time) time.Time {
	loc := now.Location()
	t, _ := time.ParseInLocation("15:04", s.cfg.Battery.TargetTime, loc)
	todayTarget := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)

	minWindow := time.Duration(s.cfg.Battery.MinPlanningWindowHrs) * time.Hour
	if now.After(todayTarget) || todayTarget.Sub(now) < minWindow {
		return todayTarget.Add(24 * time.Hour)
	}
	return todayTarget
}
