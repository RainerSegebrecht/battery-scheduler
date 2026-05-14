package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite database connection.
type DB struct {
	conn *sql.DB
}

// ChargingSlot represents a planned grid-charge window.
type ChargingSlot struct {
	ID        int64
	StartTime time.Time
	EndTime   time.Time
	PriceEUR  float64
	Active    bool
	CreatedAt time.Time
}

// ForecastEntry stores the latest Solcast and Tibber fetch results.
type ForecastEntry struct {
	FetchedAt      time.Time
	SolcastKWh     float64
	TibberFetched  bool
}

// StateEntry stores the last known state for audit/debug purposes.
type StateEntry struct {
	Timestamp   time.Time
	BatterySOC  float64
	BatteryMode string
	GridPrice   float64
	Action      string // "charge", "hold", "normal"
	Reason      string
}

// Open opens (or creates) the SQLite database at the given path.
func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	conn.SetMaxOpenConns(1) // SQLite is single-writer

	db := &DB{conn: conn}
	if err := db.migrate(); err != nil {
		return nil, fmt.Errorf("running migrations: %w", err)
	}
	return db, nil
}

// Close closes the database connection.
func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) migrate() error {
	_, err := db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS charging_slots (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			start_time  DATETIME NOT NULL,
			end_time    DATETIME NOT NULL,
			price_eur   REAL NOT NULL,
			active      INTEGER NOT NULL DEFAULT 1,
			created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);

		CREATE TABLE IF NOT EXISTS forecasts (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			fetched_at     DATETIME NOT NULL,
			solcast_kwh    REAL NOT NULL,
			tibber_fetched INTEGER NOT NULL DEFAULT 0
		);

		CREATE TABLE IF NOT EXISTS state_log (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			battery_soc  REAL NOT NULL,
			battery_mode TEXT NOT NULL,
			grid_price   REAL NOT NULL,
			action       TEXT NOT NULL,
			reason       TEXT NOT NULL
		);
	`)
	return err
}

// UpsertChargingSlots replaces all future charging slots with new ones.
// This is called after each planning run.
func (db *DB) UpsertChargingSlots(slots []ChargingSlot) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Remove future slots (past slots remain for history)
	now := time.Now().UTC()
	if _, err := tx.Exec(`DELETE FROM charging_slots WHERE start_time > ?`, now); err != nil {
		return err
	}

	for _, s := range slots {
		_, err := tx.Exec(
			`INSERT INTO charging_slots (start_time, end_time, price_eur, active) VALUES (?, ?, ?, ?)`,
			s.StartTime.UTC(), s.EndTime.UTC(), s.PriceEUR, boolToInt(s.Active),
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ActiveSlotAt returns the active charging slot that covers the given time, if any.
func (db *DB) ActiveSlotAt(t time.Time) (*ChargingSlot, error) {
	row := db.conn.QueryRow(
		`SELECT id, start_time, end_time, price_eur, active, created_at
		 FROM charging_slots
		 WHERE active = 1 AND start_time <= ? AND end_time > ?
		 LIMIT 1`,
		t.UTC(), t.UTC(),
	)
	s := &ChargingSlot{}
	var startStr, endStr, createdStr string
	err := row.Scan(&s.ID, &startStr, &endStr, &s.PriceEUR, &s.Active, &createdStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.StartTime, _ = time.Parse(time.RFC3339, startStr)
	s.EndTime, _ = time.Parse(time.RFC3339, endStr)
	s.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	return s, nil
}

// InsertForecast records a new forecast fetch.
func (db *DB) InsertForecast(f ForecastEntry) error {
	_, err := db.conn.Exec(
		`INSERT INTO forecasts (fetched_at, solcast_kwh, tibber_fetched) VALUES (?, ?, ?)`,
		f.FetchedAt.UTC(), f.SolcastKWh, boolToInt(f.TibberFetched),
	)
	return err
}

// LatestForecast returns the most recently stored forecast.
func (db *DB) LatestForecast() (*ForecastEntry, error) {
	row := db.conn.QueryRow(
		`SELECT fetched_at, solcast_kwh, tibber_fetched FROM forecasts ORDER BY fetched_at DESC LIMIT 1`,
	)
	f := &ForecastEntry{}
	var fetchedStr string
	var tibberInt int
	err := row.Scan(&fetchedStr, &f.SolcastKWh, &tibberInt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.FetchedAt, _ = time.Parse(time.RFC3339, fetchedStr)
	f.TibberFetched = tibberInt == 1
	return f, nil
}

// LogState records a control-loop action for audit purposes.
func (db *DB) LogState(s StateEntry) error {
	_, err := db.conn.Exec(
		`INSERT INTO state_log (timestamp, battery_soc, battery_mode, grid_price, action, reason)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		s.Timestamp.UTC(), s.BatterySOC, s.BatteryMode, s.GridPrice, s.Action, s.Reason,
	)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
