// Package status provides a read-only terminal dashboard showing past decisions
// and future charging slots from the database.
package status

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/home/battery-scheduler/internal/db"
	"github.com/home/battery-scheduler/internal/evcc"
)

const (
	colReset  = "\033[0m"
	colBold   = "\033[1m"
	colGreen  = "\033[32m"
	colYellow = "\033[33m"
	colRed    = "\033[31m"
	colCyan   = "\033[36m"
	colGray   = "\033[90m"
	colBlue   = "\033[34m"
)

// Print writes a full status report to w.
// It shows:
//   - current evcc state
//   - upcoming charging slots
//   - recent control decisions from the DB log
func Print(w io.Writer, database *db.DB, evccClient *evcc.Client) error {
	now := time.Now()
	fmt.Fprintf(w, "\n%s%s battery-scheduler status  —  %s%s\n",
		colBold, colCyan, now.Format("Mon 02.01.2006 15:04:05"), colReset)
	fmt.Fprintln(w, strings.Repeat("─", 72))

	// ── Current evcc state ───────────────────────────────────────────────────
	fmt.Fprintf(w, "\n%s▶ Live-Zustand (evcc)%s\n", colBold, colReset)
	state, err := evccClient.State()
	if err != nil {
		fmt.Fprintf(w, "  %sEvcc nicht erreichbar: %v%s\n", colRed, err, colReset)
	} else {
		modeColor := modeCol(state.BatteryMode)
		fmt.Fprintf(w, "  Batterie SoC      : %s%.0f %%%s\n", colBold, state.BatterySoC, colReset)
		fmt.Fprintf(w, "  Batterie Modus    : %s%s%s\n", modeColor, state.BatteryMode, colReset)
		fmt.Fprintf(w, "  Batterieladung    : %+.0f W\n", state.BatteryPower)
		fmt.Fprintf(w, "  PV-Leistung       : %.0f W\n", state.PvPower)
		fmt.Fprintf(w, "  Netzbezug         : %+.0f W  (+ = Bezug, - = Einspeisung)\n", state.GridPower)
		fmt.Fprintf(w, "  Tibber-Preis jetzt: %s%.4f EUR/kWh%s\n",
			priceCol(state.TariffGrid), state.TariffGrid, colReset)
	}

	// ── Upcoming charging slots ──────────────────────────────────────────────
	fmt.Fprintf(w, "\n%s▶ Geplante Ladeslots (nächste 48 h)%s\n", colBold, colReset)
	upcoming, err := database.UpcomingSlots()
	if err != nil {
		fmt.Fprintf(w, "  %sFehler beim Lesen der DB: %v%s\n", colRed, err, colReset)
	} else if len(upcoming) == 0 {
		fmt.Fprintf(w, "  %skeine geplanten Slots%s\n", colGray, colReset)
	} else {
		fmt.Fprintf(w, "  %-22s  %-22s  %s\n", "Start", "Ende", "Preis (EUR/kWh)")
		fmt.Fprintln(w, "  "+strings.Repeat("·", 58))
		for _, s := range upcoming {
			marker := "  "
			if now.After(s.StartTime) && now.Before(s.EndTime) {
				marker = colGreen + "▶ " + colReset
			}
			fmt.Fprintf(w, "  %s%-22s  %-22s  %s%.4f%s\n",
				marker,
				s.StartTime.Local().Format("Mon 02.01. 15:04"),
				s.EndTime.Local().Format("Mon 02.01. 15:04"),
				priceCol(s.PriceEUR), s.PriceEUR, colReset,
			)
		}
	}

	// ── All slots (past + future) ────────────────────────────────────────────
	fmt.Fprintf(w, "\n%s▶ Alle Ladeslots (letzte 20)%s\n", colBold, colReset)
	allSlots, err := database.AllSlots(20)
	if err != nil {
		fmt.Fprintf(w, "  %sFehler: %v%s\n", colRed, err, colReset)
	} else if len(allSlots) == 0 {
		fmt.Fprintf(w, "  %snoch keine Einträge%s\n", colGray, colReset)
	} else {
		fmt.Fprintf(w, "  %-22s  %-22s  %-10s  %s\n", "Start", "Ende", "Preis", "Status")
		fmt.Fprintln(w, "  "+strings.Repeat("·", 66))
		for _, s := range allSlots {
			status := colGray + "vergangen" + colReset
			if s.StartTime.After(now) {
				status = colGreen + "geplant  " + colReset
			} else if now.After(s.StartTime) && now.Before(s.EndTime) {
				status = colGreen + colBold + "aktiv    " + colReset
			}
			fmt.Fprintf(w, "  %-22s  %-22s  %s%.4f%s      %s\n",
				s.StartTime.Local().Format("Mon 02.01. 15:04"),
				s.EndTime.Local().Format("Mon 02.01. 15:04"),
				priceCol(s.PriceEUR), s.PriceEUR, colReset,
				status,
			)
		}
	}

	// ── Recent control decisions ─────────────────────────────────────────────
	fmt.Fprintf(w, "\n%s▶ Letzte Entscheidungen (State-Log)%s\n", colBold, colReset)
	log, err := database.RecentStateLog(20)
	if err != nil {
		fmt.Fprintf(w, "  %sFehler: %v%s\n", colRed, err, colReset)
	} else if len(log) == 0 {
		fmt.Fprintf(w, "  %snoch keine Einträge%s\n", colGray, colReset)
	} else {
		fmt.Fprintf(w, "  %-19s  %-5s  %-5s  %-8s  %s\n",
			"Zeit", "SoC%", "Preis", "Aktion", "Begründung")
		fmt.Fprintln(w, "  "+strings.Repeat("·", 90))
		for _, e := range log {
			dryMark := ""
			reason := e.Reason
			if strings.HasPrefix(reason, "[dry-run] ") {
				dryMark = colYellow + "[D] " + colReset
				reason = strings.TrimPrefix(reason, "[dry-run] ")
			}
			// Truncate long reasons for display
			if len(reason) > 52 {
				reason = reason[:49] + "..."
			}
			fmt.Fprintf(w, "  %-19s  %s%4.0f%%%s  %s%.4f%s  %s%-8s%s  %s%s\n",
				e.Timestamp.Local().Format("02.01. 15:04:05"),
				colBold, e.BatterySOC, colReset,
				priceCol(e.GridPrice), e.GridPrice, colReset,
				modeCol(e.Action), e.Action, colReset,
				dryMark, reason,
			)
		}
	}

	fmt.Fprintln(w, "\n"+strings.Repeat("─", 72))
	return nil
}

func modeCol(mode string) string {
	switch mode {
	case "charge":
		return colGreen + colBold
	case "hold":
		return colYellow + colBold
	case "normal":
		return colBlue
	default:
		return colGray
	}
}

func priceCol(price float64) string {
	switch {
	case price < 0.15:
		return colGreen
	case price < 0.25:
		return colBlue
	case price < 0.32:
		return colYellow
	default:
		return colRed
	}
}
