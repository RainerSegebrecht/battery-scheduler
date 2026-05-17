package status

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/home/battery-scheduler/internal/db"
	"github.com/home/battery-scheduler/internal/evcc"
)

// pageData holds all data rendered into the HTML template.
type pageData struct {
	Now           string
	State         *evcc.SiteState
	StateErr      string
	UpcomingSlots []db.ChargingSlot
	AllSlots      []db.ChargingSlot
	StateLog      []db.StateEntry
	DBErr         string
}

// NewHandler returns an http.Handler that serves the status dashboard.
// It queries evcc and the database on every request (no caching — the page
// itself auto-refreshes every 30 s so stale data is fine).
func NewHandler(database *db.DB, evccClient *evcc.Client) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "" {
			http.NotFound(w, r)
			return
		}

		data := pageData{
			Now: time.Now().Format("Mon 02.01.2006 15:04:05"),
		}

		// Live evcc state
		state, err := evccClient.State()
		if err != nil {
			data.StateErr = err.Error()
		} else {
			data.State = state
		}

		// DB data
		upcoming, err := database.UpcomingSlots()
		if err != nil {
			data.DBErr = err.Error()
		} else {
			data.UpcomingSlots = upcoming
		}
		allSlots, err := database.AllSlots(20)
		if err != nil && data.DBErr == "" {
			data.DBErr = err.Error()
		} else {
			data.AllSlots = allSlots
		}
		logEntries, err := database.RecentStateLog(30)
		if err != nil && data.DBErr == "" {
			data.DBErr = err.Error()
		} else {
			data.StateLog = logEntries
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, data); err != nil {
			http.Error(w, fmt.Sprintf("template error: %v", err), http.StatusInternalServerError)
		}
	})
}

// ── Template helpers ──────────────────────────────────────────────────────────

var tmplFuncs = template.FuncMap{
	"modeClass": func(mode string) string {
		switch mode {
		case "charge":
			return "mode-charge"
		case "hold":
			return "mode-hold"
		case "normal":
			return "mode-normal"
		default:
			return ""
		}
	},
	"priceClass": func(price float64) string {
		switch {
		case price < 0.15:
			return "price-cheap"
		case price < 0.25:
			return "price-ok"
		case price < 0.32:
			return "price-mid"
		default:
			return "price-exp"
		}
	},
	"fmtTime": func(t time.Time) string {
		return t.Local().Format("Mon 02.01. 15:04")
	},
	"fmtTS": func(t time.Time) string {
		return t.Local().Format("02.01. 15:04:05")
	},
	"isActive": func(s db.ChargingSlot) bool {
		now := time.Now()
		return now.After(s.StartTime) && now.Before(s.EndTime)
	},
	"isFuture": func(s db.ChargingSlot) bool {
		return s.StartTime.After(time.Now())
	},
	"stripDryRun": func(reason string) string {
		return strings.TrimPrefix(reason, "[dry-run] ")
	},
	"isDryRun": func(reason string) bool {
		return strings.HasPrefix(reason, "[dry-run]")
	},
}

var tmpl = template.Must(template.New("status").Funcs(tmplFuncs).Parse(htmlTemplate))

const htmlTemplate = `<!DOCTYPE html>
<html lang="de">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="30">
  <title>battery-scheduler</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: ui-monospace, "Cascadia Code", "Fira Code", monospace;
      font-size: 14px;
      background: #0d1117;
      color: #c9d1d9;
      padding: 1.5rem;
    }
    h1 { color: #58a6ff; font-size: 1.2rem; margin-bottom: .25rem; }
    .ts { color: #8b949e; font-size: .85rem; margin-bottom: 1.5rem; }
    .ts small { color: #484f58; }
    h2 {
      color: #8b949e;
      font-size: .8rem;
      text-transform: uppercase;
      letter-spacing: .08em;
      margin: 1.5rem 0 .5rem;
      border-bottom: 1px solid #21262d;
      padding-bottom: .25rem;
    }
    .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: .75rem; margin-bottom: 1rem; }
    .card {
      background: #161b22;
      border: 1px solid #21262d;
      border-radius: 6px;
      padding: .75rem 1rem;
    }
    .card .label { color: #8b949e; font-size: .75rem; margin-bottom: .2rem; }
    .card .value { font-size: 1.3rem; font-weight: 600; }
    .mode-charge { color: #3fb950; }
    .mode-hold   { color: #d29922; }
    .mode-normal { color: #58a6ff; }
    .price-cheap { color: #3fb950; }
    .price-ok    { color: #58a6ff; }
    .price-mid   { color: #d29922; }
    .price-exp   { color: #f85149; }
    .err { color: #f85149; background: #1c1010; border: 1px solid #4b1818; border-radius: 4px; padding: .5rem .75rem; margin-bottom: 1rem; }

    /* ── Scrollable table container ─────────────────────────────────────── */
    .tbl-wrap {
      border: 1px solid #21262d;
      border-radius: 6px;
      overflow: hidden;          /* clip rounded corners */
    }
    .tbl-scroll {
      max-height: 193px;         /* header (32px) + 5 × data rows (32px each) */
      overflow-y: auto;
      overflow-x: auto;
    }
    /* Webkit scrollbar — subtle dark style */
    .tbl-scroll::-webkit-scrollbar { width: 6px; height: 6px; }
    .tbl-scroll::-webkit-scrollbar-track { background: #161b22; }
    .tbl-scroll::-webkit-scrollbar-thumb { background: #30363d; border-radius: 3px; }
    .tbl-scroll::-webkit-scrollbar-thumb:hover { background: #484f58; }
    /* Firefox */
    .tbl-scroll { scrollbar-width: thin; scrollbar-color: #30363d #161b22; }

    table { width: 100%; border-collapse: collapse; font-size: .82rem; }
    thead tr { position: sticky; top: 0; z-index: 1; background: #161b22; }
    th {
      color: #8b949e; font-weight: 400; text-align: left;
      padding: .4rem .6rem;
      border-bottom: 1px solid #21262d;
      white-space: nowrap;
    }
    td { padding: .3rem .6rem; border-bottom: 1px solid #161b22; white-space: nowrap; }
    tr:last-child td { border-bottom: none; }
    tr.active-slot td { background: #0d2818; }
    tr.past-slot td   { color: #484f58; }
    .badge {
      display: inline-block; padding: .1rem .45rem; border-radius: 3px;
      font-size: .75rem; font-weight: 600; line-height: 1.4;
    }
    .badge-charge { background: #0d2818; color: #3fb950; }
    .badge-hold   { background: #1f1a0a; color: #d29922; }
    .badge-normal { background: #0c1a2e; color: #58a6ff; }
    .badge-dr     { background: #1c1010; color: #8b949e; font-weight: 400; }
    .nowrap { white-space: nowrap; }
    .right  { text-align: right; }
    footer { margin-top: 2rem; color: #484f58; font-size: .75rem; }
  </style>
</head>
<body>

<h1>⚡ battery-scheduler</h1>
<p class="ts">{{.Now}} <small>— Seite aktualisiert sich automatisch alle 30 s</small></p>

{{if .StateErr}}<div class="err">evcc nicht erreichbar: {{.StateErr}}</div>{{end}}
{{if .DBErr}}<div class="err">Datenbankfehler: {{.DBErr}}</div>{{end}}

{{if .State}}
<h2>Live-Zustand</h2>
<div class="grid">
  <div class="card">
    <div class="label">Batterie SoC</div>
    <div class="value">{{printf "%.0f" .State.BatterySoC}} %</div>
  </div>
  <div class="card">
    <div class="label">Batterie Modus</div>
    <div class="value {{modeClass .State.BatteryMode}}">{{.State.BatteryMode}}</div>
  </div>
  <div class="card">
    <div class="label">Batterieladung</div>
    <div class="value">{{printf "%+.0f" .State.BatteryPower}} W</div>
  </div>
  <div class="card">
    <div class="label">PV-Leistung</div>
    <div class="value">{{printf "%.0f" .State.PvPower}} W</div>
  </div>
  <div class="card">
    <div class="label">Netzbezug</div>
    <div class="value">{{printf "%+.0f" .State.GridPower}} W</div>
  </div>
  <div class="card">
    <div class="label">Tibber-Preis jetzt</div>
    <div class="value {{priceClass .State.TariffGrid}}">{{printf "%.4f" .State.TariffGrid}} €/kWh</div>
  </div>
</div>
{{end}}

<h2>Geplante Ladeslots (nächste 48 h)</h2>
{{if .UpcomingSlots}}
<div class="tbl-wrap"><div class="tbl-scroll">
<table>
  <thead><tr><th>Start</th><th>Ende</th><th class="right">Preis</th><th>Status</th></tr></thead>
  <tbody>
  {{range .UpcomingSlots}}
  <tr class="{{if isActive .}}active-slot{{else}}future-slot{{end}}">
    <td>{{fmtTime .StartTime}}</td>
    <td>{{fmtTime .EndTime}}</td>
    <td class="right {{priceClass .PriceEUR}}">{{printf "%.4f" .PriceEUR}} €/kWh</td>
    <td>{{if isActive .}}<span class="badge badge-charge">▶ aktiv</span>{{else}}<span class="badge badge-normal">geplant</span>{{end}}</td>
  </tr>
  {{end}}
  </tbody>
</table>
</div></div>
{{else}}<p style="color:#484f58;font-size:.85rem">Keine geplanten Slots.</p>{{end}}

<h2>Alle Ladeslots (letzte 20)</h2>
{{if .AllSlots}}
<div class="tbl-wrap"><div class="tbl-scroll">
<table>
  <thead><tr><th>Start</th><th>Ende</th><th class="right">Preis</th><th>Status</th></tr></thead>
  <tbody>
  {{range .AllSlots}}
  <tr class="{{if isActive .}}active-slot{{else if isFuture .}}future-slot{{else}}past-slot{{end}}">
    <td>{{fmtTime .StartTime}}</td>
    <td>{{fmtTime .EndTime}}</td>
    <td class="right {{priceClass .PriceEUR}}">{{printf "%.4f" .PriceEUR}} €/kWh</td>
    <td>
      {{if isActive .}}<span class="badge badge-charge">▶ aktiv</span>
      {{else if isFuture .}}<span class="badge badge-normal">geplant</span>
      {{else}}<span style="color:#484f58">vergangen</span>{{end}}
    </td>
  </tr>
  {{end}}
  </tbody>
</table>
</div></div>
{{else}}<p style="color:#484f58;font-size:.85rem">Noch keine Einträge.</p>{{end}}

<h2>Letzte Entscheidungen</h2>
{{if .StateLog}}
<div class="tbl-wrap"><div class="tbl-scroll">
<table>
  <thead><tr><th>Zeit</th><th class="right">SoC</th><th class="right">Preis</th><th>Aktion</th><th>Begründung</th></tr></thead>
  <tbody>
  {{range .StateLog}}
  <tr>
    <td class="nowrap">{{fmtTS .Timestamp}}</td>
    <td class="right">{{printf "%.0f" .BatterySOC}} %</td>
    <td class="right {{priceClass .GridPrice}}">{{printf "%.4f" .GridPrice}}</td>
    <td>
      {{if isDryRun .Reason}}
        <span class="badge badge-dr">dry-run</span>
      {{end}}
      <span class="badge {{if eq .Action "charge"}}badge-charge{{else if eq .Action "hold"}}badge-hold{{else}}badge-normal{{end}}">{{.Action}}</span>
    </td>
    <td style="white-space:normal;min-width:200px;color:#8b949e">{{stripDryRun .Reason}}</td>
  </tr>
  {{end}}
  </tbody>
</table>
</div></div>
{{else}}<p style="color:#484f58;font-size:.85rem">Noch keine Einträge.</p>{{end}}

<footer>battery-scheduler &mdash; Web-Status</footer>
</body>
</html>`
