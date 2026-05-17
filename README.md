# battery-scheduler

## git
```
git init
git branch -M main
git add .
git commit -m "Initialer Commit"
git remote add origin git@github.com:RainerSegebrecht/battery-scheduler.git
git push -u origin main
```

Externe Steuerungssoftware für Hausbatteriespeicher in Kombination mit [evcc](https://evcc.io/), Tibber-Stromtarif und Solcast-Solarprognose.

## Hintergrund

evcc steuert das Laden von E-Fahrzeugen intelligent anhand von Solarüberschuss und dynamischen Stromtarifen. Für die **Batterie** fehlt jedoch eine zeitbasierte Lade- und Haltelogik, die folgende Frage beantwortet:

> *Wann soll die Batterie aus dem Stromnetz geladen werden, damit sie um 20:00 Uhr voll ist — und wann soll sie ihren Ladestand halten, damit abends teurer Strom vermieden wird?*

`battery-scheduler` schließt diese Lücke als eigenständiger Docker-Container.

---

## Funktionsweise

### Planungslogik (2× täglich, konfigurierbar)

1. **Solcast-Prognose** abrufen: Wie viel PV-Energie (kWh, P10-pessimistisch) ist für den Zieltag zu erwarten?
2. **Aktuellen Batterie-SoC** aus evcc lesen: Wie viel Energie fehlt noch bis zum Ziel-SoC?
3. **Entscheidung**:
   - Solcast-Prognose ≥ `solar_threshold_kwh` → kein Netzladen nötig, Solar füllt die Batterie
   - Sonst: benötigte kWh berechnen → günstigste N Tibber-Stunden vor `target_time` auswählen
4. **Ladeslots** in SQLite speichern

### Steuerungsschleife (alle 45 Sekunden)

Jede Minute prüft der Controller den aktuellen Zustand und sendet einen der drei Modi an evcc:

| Bedingung | evcc `batterymode` | Wirkung |
|---|---|---|
| Aktuell in einem geplanten Ladeslot **und** SoC < Ziel | `charge` | Batterie wird aus dem Netz geladen |
| Tibber-Preis > `hold_above_price` | `hold` | Batterie wird weder ge- noch entladen — Energie für teuren Abend aufheben |
| Sonst | `normal` | evcc übernimmt die Kontrolle (Standard-Verhalten) |

> **Wichtig:** evcc setzt `batterymode` automatisch nach 60 Sekunden zurück. Deshalb sendet der Scheduler den Befehl alle 45 Sekunden erneut.

### Datenhaltung

Alle Zustände, Pläne und Prognosen werden in einer **SQLite-Datenbank** gespeichert, die über einen Docker-Volume-Mount außerhalb des Containers liegt.

---

## Voraussetzungen

- Docker + Docker Compose
- evcc läuft im selben Docker-Netzwerk (oder ist per URL erreichbar)
- Der Huawei-Wechselrichter unterstützt in evcc das aktive Battery-Control (`batterymode hold/charge`)
- Tibber-API-Token (kostenlos unter [developer.tibber.com](https://developer.tibber.com/))
- Solcast-Account mit Rooftop-Site (kostenloser Hobbyisten-Tarif reicht, 10 Abrufe/Tag)

---

## Installation

### 1. Repository klonen

```bash
git clone https://github.com/home/battery-scheduler.git
cd battery-scheduler
```

### 2. Konfiguration anlegen

```bash
cp config/config.example.yaml config/config.yaml
```

Die Datei `config/config.yaml` bearbeiten und alle Platzhalter ersetzen (siehe [Konfiguration](#konfiguration)).

### 3. Container starten

```bash
docker compose up -d
```

Logs prüfen:

```bash
docker compose logs -f battery-scheduler
```

---

## Konfiguration

Alle Einstellungen befinden sich in `config/config.yaml`. Eine vollständig kommentierte Vorlage liegt unter `config/config.example.yaml`.

```yaml
evcc:
  url: "http://evcc:7070"     # URL des evcc-Containers (Servicename im Docker-Netzwerk)
  poll_interval: "45s"        # Muss < 60s sein (evcc-Auto-Reset)

tibber:
  token: "DEIN_TIBBER_TOKEN"  # https://developer.tibber.com/

solcast:
  site_id: "DEINE_SITE_ID"    # Im Solcast-Portal unter "Your Sites"
  api_key: "DEIN_API_KEY"
  fetch_times:
    - "06:00"                 # Prognose morgens abrufen
    - "14:00"                 # Prognose nachmittags aktualisieren

battery:
  capacity_kwh: 10.0          # Nutzbare Batteriekapazität (Huawei Luna2000 10 kWh)
  max_charge_power_kw: 5.0    # Maximale Ladeleistung aus dem Netz (sun2000-5ktl = 5 kW)
  solar_threshold_kwh: 8.0    # Ab dieser PV-Prognose (kWh/Tag) kein Netzladen
  target_soc: 100             # Ziel-SoC in Prozent
  target_time: "20:00"        # Batterie muss um diese Uhrzeit voll sein
  hold_above_price: 0.25      # EUR/kWh: Batterie halten wenn Preis darüber liegt
  min_soc: 10                 # % unter dem die Batterie nie entladen wird

database:
  path: "/data/battery-scheduler.db"

log:
  level: "info"               # debug | info | warn | error
```

### Parameter-Erklärungen

**`solar_threshold_kwh`**
Der Schwellwert für die tägliche PV-Prognose. Liegt die Solcast-P10-Vorhersage (pessimistischer Wert) darüber, wird kein Netzladen geplant — die Solaranlage füllt die Batterie selbst.
Empfehlung: Im Winter niedrig ansetzen (z.B. 2–3 kWh), im Sommer höher (8–10 kWh). Ein fester Wert von 8 kWh ist ein guter Kompromiss.

**`hold_above_price`**
Liegt der aktuelle Tibber-Preis über diesem Wert, wird die Batterie im `hold`-Modus gehalten — sie wird weder ge- noch entladen. Das bewahrt die gespeicherte Energie für teure Abendstunden (z.B. Sauna-Betrieb).
Empfehlung: Entspricht ungefähr dem persönlichen Durchschnittspreis. Bei Tibber in Deutschland typischerweise 0,20–0,30 EUR/kWh.

**`target_time`**
Der Zeitpunkt, bis zu dem die Batterie auf `target_soc` geladen sein soll. Der Scheduler wählt nur Tibber-Slots aus, die **vor** diesem Zeitpunkt enden.

**`fetch_times`**
Uhrzeiten, zu denen Solcast und Tibber abgerufen und der Plan neu berechnet wird. Tibber liefert Preise für den nächsten Tag üblicherweise gegen 13:00 Uhr — der zweite Abruf um 14:00 stellt sicher, dass der Plan die aktuellsten Preise nutzt.

---

## Docker-Netzwerk

Der Container muss evcc per HTTP erreichen können. Wenn evcc und battery-scheduler im gleichen Docker-Compose-Projekt laufen, reicht der Servicename als URL. Andernfalls das externe Netzwerk in `docker-compose.yml` anpassen:

```yaml
networks:
  evcc_network:
    external: true
```

Den Netzwerknamen mit `docker network ls` ermitteln.

---

## Datenbankinhalt

Die SQLite-Datenbank (`/data/battery-scheduler.db`) enthält drei Tabellen:

| Tabelle | Inhalt |
|---|---|
| `charging_slots` | Geplante Ladezeitfenster mit Preis und Status |
| `forecasts` | Protokoll aller Solcast- und Tibber-Abrufe |
| `state_log` | Jede Steuerungsentscheidung mit Begründung, SoC und Preis |

Die Datenbank lässt sich mit jedem SQLite-Client (z.B. [DB Browser for SQLite](https://sqlitebrowser.org/)) inspizieren.

---

## Architektur

```
Tibber GraphQL API ──┐
Solcast REST API   ──┼──► battery-scheduler ──► evcc REST API ──► Huawei sun2000
SQLite (Volume)    ──┘         │
                               └──► Logs (stdout)
```

```
battery-scheduler/
├── cmd/battery-scheduler/main.go        # Einstiegspunkt, Signal-Handling, Ticker
├── integration/
│   └── integration_test.go             # Integrationstests (6 Szenarien)
├── internal/
│   ├── config/config.go                 # YAML-Laden und Validierung
│   ├── db/db.go                         # SQLite: Slots, Forecasts, State-Log
│   ├── evcc/client.go                   # evcc REST: State lesen, BatteryMode setzen
│   ├── tibber/client.go                 # Tibber GraphQL: Stundenpreise heute+morgen
│   ├── solcast/client.go                # Solcast REST: PV-Prognose (48h, P10/P50/P90)
│   ├── scheduler/scheduler.go          # Plan() und Control() Kernlogik
│   └── testutil/mocks.go               # Mock-HTTP-Server für Tests
├── .vscode/
│   ├── launch.json                      # VS Code Debugger-Konfigurationen
│   └── settings.json                    # Go-Einstellungen für VS Code
├── config/config.example.yaml
├── Dockerfile                           # Multi-stage, CGO_ENABLED=0, Alpine
└── docker-compose.yml
```

---

## Tests

### Teststrategie

Die Integrationstests in `integration/integration_test.go` starten für jeden Test drei echte HTTP-Server (`net/http/httptest`) als Ersatz für evcc, Tibber und Solcast. Die **produktive Scheduler-Logik läuft unverändert** — nur die API-URLs werden auf die Mock-Server umgebogen.

Dadurch lässt sich das gesamte System im Debugger Schritt für Schritt verfolgen, ohne dass reale API-Tokens oder eine laufende evcc-Instanz nötig sind.

### Mock-Szenarien

**Tibber-Preismuster (`PriceScenario`):**

| Szenario | Beschreibung |
|---|---|
| `cheap_night` | 00–06 Uhr sehr günstig (0,12 €), 17–21 Uhr teuer (0,38 €) |
| `cheap_midday` | 10–14 Uhr günstig (0,14 €, Wind/Solar-Überschuss), Abend teuer |
| `uniform` | Gleichmäßiger Preis (Standard: 0,28 €) den ganzen Tag |
| `expensive_all` | Alles teuer (0,40 €) — kein guter Ladezeitpunkt vorhanden |

**Solcast-Solarprofile (`SolarScenario`):**

| Szenario | P10-Tagesertrag | Bedeutung |
|---|---|---|
| `winter` | ~2,3 kWh | Netzladen erforderlich |
| `overcast` | ~7,8 kWh | Grenzfall (knapp unter Schwelle) |
| `summer` | ~21 kWh | Genug Solar, kein Netzladen |

### Testszenarien

| Test | Beschreibung | Erwartetes Ergebnis |
|---|---|---|
| `TestScenario_Winter_CheapNight` | Winter, SoC 20%, teurer aktueller Preis | Plan wählt Nachtstunden, Control → `hold` |
| `TestScenario_Summer_NoGridCharge` | Hohe Solarprognose | Kein Netzladen geplant, Control → `normal` |
| `TestScenario_BatteryFull` | SoC 100% | Kein Laden nötig, Control → `normal` oder `hold` |
| `TestScenario_CheapMidday` | Günstiger Mittag, bewölkt | Mittagsstunden werden geplant, kein `hold` |
| `TestScenario_ExpensiveAll` | Alle Preise hoch | Kein günstiger Slot, Control → `hold` |
| `TestScenario_PollingLoop` | 5× Control() hintereinander | Alle 5 Befehle identisch (kein Flip) |

### Tests ausführen

```bash
# Alle Integrationstests
CGO_ENABLED=0 GOTMPDIR=~/tmp go test ./integration/... -v

# Einzelnes Szenario
CGO_ENABLED=0 GOTMPDIR=~/tmp go test ./integration/... -v -run TestScenario_Winter_CheapNight
```

> **Hinweis:** `GOTMPDIR` muss auf ein Verzeichnis mit Ausführungsrechten zeigen. Auf manchen Linux-Systemen ist `/tmp` mit `noexec` gemountet.

---

## Debugging in VS Code

### Voraussetzung

Die offizielle [Go-Extension für VS Code](https://marketplace.visualstudio.com/items?itemName=golang.go) (`golang.go`) muss installiert sein.

### Starten

1. Repository in VS Code öffnen: `code /pfad/zu/battery-scheduler`
2. Seitliche Debug-Ansicht öffnen (`Ctrl+Shift+D`)
3. Im Dropdown oben eine der vorbereiteten Konfigurationen wählen:

> **Hinweis:** Im VS Code Go-Debugger (`mode: test`) werden `args` direkt an das kompilierte Test-Binary übergeben — nicht an `go test`. Deshalb stehen in `launch.json` `-test.v` statt `-v` und `-test.run` statt `-run`. Das ist bereits korrekt hinterlegt.

| Konfiguration | Beschreibung |
|---|---|
| **Integration Tests (all)** | Alle 6 Szenarien mit Ausgabe |
| **Integration: Winter + CheapNight** | Nur das Winter-Szenario |
| **Integration: Summer (no grid charge)** | Nur das Sommer-Szenario |
| **Integration: Battery full** | Batterie bereits voll |
| **Integration: Cheap midday** | Günstiger Mittag |
| **Integration: All prices expensive** | Alle Preise teuer |
| **Integration: Polling loop** | 5 aufeinanderfolgende Ticks |
| **Run battery-scheduler** | Startet die Anwendung (erfordert `config/config.yaml`) |

4. Breakpoints setzen (z.B. in `internal/scheduler/scheduler.go` in `Plan()` oder `decideAction()`)
5. `F5` drücken — der Debugger hält an den Breakpoints an

### Empfohlene Breakpoints zum Einstieg

| Datei | Zeile | Was passiert hier |
|---|---|---|
| `internal/scheduler/scheduler.go` | `func (s *Scheduler) Plan()` | Beginn der Planung |
| `internal/scheduler/scheduler.go` | `needsGridCharge := ...` | Entscheidung: Netzladen ja/nein? |
| `internal/scheduler/scheduler.go` | `func (s *Scheduler) selectCheapestSlots` | Slot-Auswahl aus Tibber-Preisen |
| `internal/scheduler/scheduler.go` | `func (s *Scheduler) decideAction` | Entscheidung pro Control-Tick |
| `internal/testutil/mocks.go` | `func (m *MockEvcc) handleBatteryMode` | Hier kommt der Befehl von evcc an |

---

## Entwicklung

### Lokal bauen

```bash
CGO_ENABLED=0 go build ./...
```

### Lokal ausführen

```bash
CGO_ENABLED=0 go run ./cmd/battery-scheduler -config ./config/config.yaml
```

### Docker-Image bauen

```bash
docker build -t battery-scheduler .
```
