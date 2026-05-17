package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/home/battery-scheduler/internal/config"
	"github.com/home/battery-scheduler/internal/db"
	"github.com/home/battery-scheduler/internal/evcc"
	"github.com/home/battery-scheduler/internal/scheduler"
	"github.com/home/battery-scheduler/internal/solcast"
	"github.com/home/battery-scheduler/internal/status"
	"github.com/home/battery-scheduler/internal/tibber"
)

func main() {
	configPath := flag.String("config", "/config/config.yaml", "path to YAML config file")
	dryRun := flag.Bool("dry-run", false, "observe and log decisions, but never send commands to evcc")
	showStatus := flag.Bool("status", false, "print current status to stdout and exit (read-only, no control loop)")
	flag.Parse()

	// --- Load config ---
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	// --- Resolve dry-run: CLI flag OR config file ---
	effectiveDryRun := *dryRun || cfg.Log.DryRun

	// --- Set up structured logger ---
	// In dry-run mode force INFO so all decision lines are visible.
	logLevelStr := cfg.Log.Level
	if effectiveDryRun && (logLevelStr == "warn" || logLevelStr == "error") {
		logLevelStr = "info"
	}
	var logLevel slog.Level
	switch logLevelStr {
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

	if effectiveDryRun {
		src := "flag"
		if cfg.Log.DryRun && !*dryRun {
			src = "config"
		}
		log.Info("*** DRY-RUN MODE — no commands will be sent to evcc ***", "source", src)
	}

	// --- Open database ---
	database, err := db.Open(cfg.Database.Path)
	if err != nil {
		log.Error("failed to open database", "path", cfg.Database.Path, "err", err)
		os.Exit(1)
	}
	defer database.Close()

	// --- Create API clients ---
	evccClient := evcc.New(cfg.Evcc.URL)

	// ── Status-only mode: print terminal dashboard and exit ──────────────────
	if *showStatus {
		if err := status.Print(os.Stdout, database, evccClient); err != nil {
			fmt.Fprintf(os.Stderr, "status error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// ── Normal / dry-run mode: run control loop ──────────────────────────────
	log.Info("battery-scheduler starting",
		"config", *configPath,
		"evcc_url", cfg.Evcc.URL,
		"poll_interval", cfg.Evcc.PollInterval.Duration,
		"target_time", cfg.Battery.TargetTime,
		"target_soc", cfg.Battery.TargetSOC,
		"dry_run", effectiveDryRun,
		"web_port", cfg.Web.Port,
	)
	log.Info("database opened", "path", cfg.Database.Path)

	tibberClient := tibber.New(cfg.Tibber.Token)
	solcastClient := solcast.New(cfg.Solcast.SiteID, cfg.Solcast.APIKey)

	sched := scheduler.New(cfg, database, evccClient, tibberClient, solcastClient, log)
	sched.DryRun = effectiveDryRun

	// --- Start HTTP status server ---
	mux := http.NewServeMux()
	mux.Handle("/", status.NewHandler(database, evccClient))
	addr := fmt.Sprintf(":%d", cfg.Web.Port)
	httpServer := &http.Server{Addr: addr, Handler: mux}
	go func() {
		log.Info("web status server listening", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Warn("web server error", "err", err)
		}
	}()

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
