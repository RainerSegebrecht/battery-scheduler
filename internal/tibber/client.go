package tibber

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const defaultTibberURL = "https://api.tibber.com/v1-beta/gql"

// Client is a Tibber GraphQL API client.
type Client struct {
	token      string
	baseURL    string // overridable for testing
	httpClient *http.Client
}

// New creates a new Tibber client pointing to the real Tibber API.
func New(token string) *Client {
	return NewWithURL(token, defaultTibberURL)
}

// NewWithURL creates a Tibber client with a custom GraphQL endpoint (used in tests).
func NewWithURL(token, baseURL string) *Client {
	return &Client{
		token:   token,
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// PriceSlot is a single hourly price entry from Tibber.
type PriceSlot struct {
	StartsAt time.Time
	Total    float64 // EUR/kWh including taxes
	Energy   float64 // base energy price
	Tax      float64 // tax component
	Level    string  // VERY_CHEAP | CHEAP | NORMAL | EXPENSIVE | VERY_EXPENSIVE
}

// graphqlRequest is the JSON body for a GraphQL request.
type graphqlRequest struct {
	Query string `json:"query"`
}

type tibberResponse struct {
	Data struct {
		Viewer struct {
			Homes []struct {
				CurrentSubscription struct {
					PriceInfo struct {
						Today    []priceEntry `json:"today"`
						Tomorrow []priceEntry `json:"tomorrow"`
					} `json:"priceInfo"`
				} `json:"currentSubscription"`
			} `json:"homes"`
		} `json:"viewer"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type priceEntry struct {
	Total    float64 `json:"total"`
	Energy   float64 `json:"energy"`
	Tax      float64 `json:"tax"`
	StartsAt string  `json:"startsAt"`
	Level    string  `json:"level"`
}

// Prices fetches today's and tomorrow's hourly prices from Tibber.
func (c *Client) Prices() ([]PriceSlot, error) {
	query := `{
		viewer {
			homes {
				currentSubscription {
					priceInfo {
						today {
							total
							energy
							tax
							startsAt
							level
						}
						tomorrow {
							total
							energy
							tax
							startsAt
							level
						}
					}
				}
			}
		}
	}`

	body, err := json.Marshal(graphqlRequest{Query: query})
	if err != nil {
		return nil, fmt.Errorf("marshalling query: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling Tibber API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Tibber API returned HTTP %d", resp.StatusCode)
	}

	var result tibberResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding Tibber response: %w", err)
	}
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("Tibber API error: %s", result.Errors[0].Message)
	}
	if len(result.Data.Viewer.Homes) == 0 {
		return nil, fmt.Errorf("no homes found in Tibber account")
	}

	priceInfo := result.Data.Viewer.Homes[0].CurrentSubscription.PriceInfo
	allEntries := append(priceInfo.Today, priceInfo.Tomorrow...)

	slots := make([]PriceSlot, 0, len(allEntries))
	for _, e := range allEntries {
		t, err := time.Parse(time.RFC3339, e.StartsAt)
		if err != nil {
			continue
		}
		slots = append(slots, PriceSlot{
			StartsAt: t,
			Total:    e.Total,
			Energy:   e.Energy,
			Tax:      e.Tax,
			Level:    e.Level,
		})
	}
	return slots, nil
}
