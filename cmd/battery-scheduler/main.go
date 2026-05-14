package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/home/battery-scheduler/internal/config"
	"github.com/home/battery-scheduler/internal/db"
	"github.com/home/battery-scheduler/internal/evcc"
	"github.com/home/battery-scheduler/internal/scheduler"
	"github.com/home/battery-scheduler/internal/solcast"
	"github.com/home/battery-scheduler/internal/tibber"
)

func main() {
	configPath := flag.String("config", "/config/config.yaml", "path to YAML config file")
	flag.Parse()

	// --- Load config ---
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	// --- Set up structured logger ---
	var logLevel slog.Level
	switch cfg.Log.Level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(log)

	log.Info("battery-scheduler starting",
		"config", *configPath,
		"evcc_url", cfg.Evcc.URL,
		"poll_interval", cfg.Evcc.PollInterval.Duration,
		"target_time", cfg.Battery.TargetTime,
		"target_soc", cfg.Battery.TargetSOC,
	)

	// --- Open database ---
	database, err := db.Open(cfg.Database.Path)
	if err != nil {
		log.Error("failed to open database", "path", cfg.Database.Path, "err", err)
		os.Exit(1)
	}
	defer database.Close()
	log.Info("database opened", "path", cfg.Database.Path)

	// --- Create API clients ---
	evccClient := evcc.New(cfg.Evcc.URL)
	tibberClient := tibber.New(cfg.Tibber.Token)
	solcastClient := solcast.New(cfg.Solcast.SiteID, cfg.Solcast.APIKey)

	// --- Create scheduler ---
	sched := scheduler.New(cfg, database, evccClient, tibberClient, solcastClient, log)

	// --- Run initial plan immediately ---
	log.Info("running initial planning")
	if err := sched.Plan(); err != nil {
		log.Warn("initial planning failed", "err", err)
	}

	// --- Set up periodic re-planning at configured fetch times ---
	go runFetchLoop(sched, cfg, log)

	// --- Main control loop ---
	ticker := time.NewTicker(cfg.Evcc.PollInterval.Duration)
	defer ticker.Stop()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	log.Info("control loop started", "interval", cfg.Evcc.PollInterval.Duration)

	for {
		select {
		case <-ticker.C:
			if err := sched.Control(); err != nil {
				log.Warn("control loop error", "err", err)
			}
		case sig := <-quit:
			log.Info("shutting down", "signal", sig)
			return
		}
	}
}

// runFetchLoop triggers Plan() at each configured fetch time (e.g. "06:00", "14:00").
func runFetchLoop(sched *scheduler.Scheduler, cfg *config.Config, log *slog.Logger) {
	if len(cfg.Solcast.FetchTimes) == 0 {
		log.Warn("no solcast.fetch_times configured, skipping periodic planning")
		return
	}

	for {
		next := nextFetchTime(cfg.Solcast.FetchTimes)
		log.Info("next forecast fetch scheduled", "at", next.Local().Format("2006-01-02 15:04"))
		time.Sleep(time.Until(next))

		log.Info("running scheduled planning")
		if err := sched.Plan(); err != nil {
			log.Warn("scheduled planning failed", "err", err)
		}
	}
}

// nextFetchTime returns the next wall-clock time matching any of the given HH:MM strings.
func nextFetchTime(fetchTimes []string) time.Time {
	now := time.Now()
	loc := now.Location()

	var candidates []time.Time
	for _, ft := range fetchTimes {
		t, err := time.ParseInLocation("15:04", ft, loc)
		if err != nil {
			continue
		}
		candidate := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		if candidate.Before(now) || candidate.Equal(now) {
			candidate = candidate.Add(24 * time.Hour)
		}
		candidates = append(candidates, candidate)
	}

	if len(candidates) == 0 {
		return now.Add(12 * time.Hour) // fallback
	}

	earliest := candidates[0]
	for _, c := range candidates[1:] {
		if c.Before(earliest) {
			earliest = c
		}
	}
	return earliest
}
