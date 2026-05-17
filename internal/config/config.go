package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration wraps time.Duration for YAML unmarshalling (e.g. "45s", "1m")
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	dur, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	d.Duration = dur
	return nil
}

// Config is the top-level configuration structure.
type Config struct {
	Evcc     EvccConfig     `yaml:"evcc"`
	Tibber   TibberConfig   `yaml:"tibber"`
	Solcast  SolcastConfig  `yaml:"solcast"`
	Battery  BatteryConfig  `yaml:"battery"`
	Database DatabaseConfig `yaml:"database"`
	Log      LogConfig      `yaml:"log"`
}

type EvccConfig struct {
	URL          string   `yaml:"url"`           // e.g. http://evcc:7070
	PollInterval Duration `yaml:"poll_interval"` // how often to run the control loop, must be < 60s
}

type TibberConfig struct {
	Token string `yaml:"token"`
}

type SolcastConfig struct {
	SiteID     string   `yaml:"site_id"`
	APIKey     string   `yaml:"api_key"`
	FetchTimes []string `yaml:"fetch_times"` // e.g. ["06:00", "14:00"] local time
}

type BatteryConfig struct {
	CapacityKWh          float64 `yaml:"capacity_kwh"`              // total usable battery capacity
	MaxChargePowerKW     float64 `yaml:"max_charge_power_kw"`       // max grid charge power
	SolarThresholdKWh    float64 `yaml:"solar_threshold_kwh"`       // if solcast forecast >= this, skip grid charging
	TargetSOC            int     `yaml:"target_soc"`                // desired SOC % at target time
	TargetTime           string  `yaml:"target_time"`               // "HH:MM" local time
	HoldAbovePrice       float64 `yaml:"hold_above_price"`          // EUR/kWh: hold battery when price is above this
	MinSOC               int     `yaml:"min_soc"`                   // never discharge below this %
	MinPlanningWindowHrs int     `yaml:"min_planning_window_hours"` // if less than this many hours remain until target_time today, plan for tomorrow instead (default 8)
}

type DatabaseConfig struct {
	Path string `yaml:"path"`
}

type LogConfig struct {
	Level string `yaml:"level"` // debug, info, warn, error
}

// Load reads and parses the YAML config file at the given path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening config file: %w", err)
	}
	defer f.Close()

	cfg := &Config{}
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

func (c *Config) validate() error {
	if c.Evcc.URL == "" {
		return fmt.Errorf("evcc.url is required")
	}
	if c.Evcc.PollInterval.Duration == 0 {
		c.Evcc.PollInterval.Duration = 45 * time.Second
	}
	if c.Evcc.PollInterval.Duration >= 60*time.Second {
		return fmt.Errorf("evcc.poll_interval must be < 60s (evcc auto-resets batterymode after 60s)")
	}
	if c.Tibber.Token == "" {
		return fmt.Errorf("tibber.token is required")
	}
	if c.Solcast.SiteID == "" || c.Solcast.APIKey == "" {
		return fmt.Errorf("solcast.site_id and solcast.api_key are required")
	}
	if c.Battery.CapacityKWh <= 0 {
		return fmt.Errorf("battery.capacity_kwh must be > 0")
	}
	if c.Battery.MaxChargePowerKW <= 0 {
		return fmt.Errorf("battery.max_charge_power_kw must be > 0")
	}
	if c.Battery.TargetSOC <= 0 || c.Battery.TargetSOC > 100 {
		return fmt.Errorf("battery.target_soc must be between 1 and 100")
	}
	if c.Battery.TargetTime == "" {
		return fmt.Errorf("battery.target_time is required (e.g. \"20:00\")")
	}
	if _, err := time.Parse("15:04", c.Battery.TargetTime); err != nil {
		return fmt.Errorf("battery.target_time must be in HH:MM format: %w", err)
	}
	if c.Battery.MinPlanningWindowHrs <= 0 {
		c.Battery.MinPlanningWindowHrs = 8 // plan for tomorrow if fewer than 8h remain today
	}
	if c.Database.Path == "" {
		c.Database.Path = "/data/battery-scheduler.db"
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	return nil
}
