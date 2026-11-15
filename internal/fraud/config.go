package fraud

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Duration is a time.Duration that reads and writes JSON as "1h30m".
type Duration struct{ time.Duration }

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("fraud: duration must be a string like \"1h\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("fraud: %w", err)
	}
	d.Duration = v
	return nil
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// Config holds every threshold. A zero Config enables no rule and allows
// everything. Amount thresholds are per currency, in minor units. A rule
// with no threshold for a payment's currency does not apply to it.
type Config struct {
	// HistoryWindow bounds how far back the card's history is read (for
	// relative amounts). Velocity windows must be shorter.
	HistoryWindow Duration `json:"history_window"`

	Velocity struct {
		Window    Duration         `json:"window"`
		MaxCount  int              `json:"max_count"`  // fires when count (including this one) is above
		MaxAmount map[string]int64 `json:"max_amount"` // per currency, minor units
		Action    Decision         `json:"action"`
	} `json:"velocity"`

	AmountAbsolute struct {
		Max    map[string]int64 `json:"max"` // per currency, minor units
		Action Decision         `json:"action"`
	} `json:"amount_absolute"`

	AmountRelative struct {
		MaxMultiple float64  `json:"max_multiple"` // of the card's median earlier amount
		MinHistory  int      `json:"min_history"`  // earlier transactions needed before the rule applies
		Action      Decision `json:"action"`
	} `json:"amount_relative"`

	CountryMismatch struct {
		Enabled bool     `json:"enabled"`
		Action  Decision `json:"action"`
	} `json:"geo_country_mismatch"`

	Distance struct {
		MaxKm  float64  `json:"max_km"`
		Action Decision `json:"action"`
	} `json:"geo_distance"`
}

func (c Config) action(rule string) Decision {
	var a Decision
	switch rule {
	case RuleVelocityCount, RuleVelocityAmount:
		a = c.Velocity.Action
	case RuleAmountAbsolute:
		a = c.AmountAbsolute.Action
	case RuleAmountRelative:
		a = c.AmountRelative.Action
	case RuleCountryMismatch:
		a = c.CountryMismatch.Action
	case RuleDistance:
		a = c.Distance.Action
	}
	if a == "" {
		return Review
	}
	return a
}

// Validate checks the config is coherent.
func (c Config) Validate() error {
	for _, rule := range AllRules {
		if a := c.action(rule); a != Review && a != Block {
			return fmt.Errorf("fraud: %s action %q must be review or block", rule, a)
		}
	}
	if c.Velocity.Window.Duration > c.HistoryWindow.Duration {
		return fmt.Errorf("fraud: velocity window %s is longer than history window %s", c.Velocity.Window.Duration, c.HistoryWindow.Duration)
	}
	return nil
}

//go:embed default.json
var defaultJSON []byte

// Default returns the embedded default configuration (internal/fraud/default.json).
func Default() Config {
	c, err := Parse(defaultJSON)
	if err != nil {
		panic("fraud: embedded default.json is invalid: " + err.Error())
	}
	return c
}

// Parse reads a JSON config strictly (unknown fields are errors) and validates it.
func Parse(b []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("fraud: parsing config: %w", err)
	}
	return c, c.Validate()
}

// Load reads a config file.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied config path
	if err != nil {
		return Config{}, err
	}
	return Parse(b)
}
