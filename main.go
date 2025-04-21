package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/joho/godotenv"
)

type EnvoyResponse struct {
	Production []ProductionEntry `json:"production"`
}

type ProductionEntry struct {
	Type       string  `json:"type"`
	WNow       float64 `json:"wNow"`
	WhLifetime float64 `json:"whLifetime"`
	WhToday    float64 `json:"whToday,omitempty"`
	RMSVoltage float64 `json:"rmsVoltage,omitempty"`
}

type Config struct {
	APIKey   string
	SystemID string
}

type Reading struct {
	Date    time.Time // will be formatted YYYYMMDD
	Power   int       // watts
	Energy  int       // watt-hours
	Voltage int       // volts (optional)
}

type State struct {
	Date     string  `json:"date"`     // format: YYYY-MM-DD
	Baseline float64 `json:"baseline"` // whLifetime at midnight
}

const statePath = "/data/state.json"

type Options struct {
	ApiKey    string `short:"a" long:"api-key" description:"The PVOutput API key" env:"API_KEY" required:"true"`
	EnvFile   string `short:"e" long:"env-file" description:"Path to a file containing environment variables"`
	IpAddress string `short:"i" long:"ip-address" description:"The IP address (or hostname) of the Envoy Gateway" env:"IP_ADDRESS" required:"true"`
	Token     string `short:"t" long:"token" description:"The API token for the Envoy Gateway" env:"TOKEN" required:"true"`
	SystemID  string `short:"s" long:"system-id" description:"The PVOutput System ID" env:"SYSTEM_ID" required:"true"`
}

var opts Options

func main() {
	_, err := flags.Parse(&opts)

	if err != nil {
		os.Exit(1)
	}

	if opts.EnvFile != "" {
		err := godotenv.Load(opts.EnvFile)
		if err != nil {
			log.Fatalf("Error loading '%s' environment file", opts.EnvFile)
		}
	}

	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	// https://enphase.com/download/iq-gateway-access-using-local-apis-or-local-ui-token-based-authentication-tech-brief
	req, err := http.NewRequest("GET", fmt.Sprintf("https://%s/production.json", opts.IpAddress), nil)
	if err != nil {
		log.Fatalf("Failed to create request: %v", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", opts.Token))

	resp, err := httpClient.Do(req)

	if err != nil {
		log.Fatalf("Failed to send request: %v", err)
	}

	defer resp.Body.Close()

	var readings EnvoyResponse

	if err := json.NewDecoder(resp.Body).Decode(&readings); err != nil {
		log.Fatalf("Failed to decode JSON: %v", err)
	}

	var wattHoursToday int
	var wattsNow float64
	var voltage float64

	for _, p := range readings.Production {
		if p.Type == "inverters" {
			wattHoursToday = calculateTodaysWattHours(p.WhLifetime)
		} else if p.Type == "eim" {
			wattsNow = p.WNow
			voltage = p.RMSVoltage
		}
	}

	cfg := Config{
		APIKey:   opts.ApiKey,
		SystemID: opts.SystemID,
	}

	reading := Reading{
		Date:    time.Now(),
		Power:   int(wattsNow),
		Energy:  wattHoursToday, // @todo may need * 1000
		Voltage: int(voltage),
	}

	err = upload(cfg, reading)

	if err != nil {
		log.Fatalf("Upload to PVOutput failed: %v", err)
	}

	os.Exit(0)
}

func upload(cfg Config, r Reading) error {
	form := url.Values{}
	form.Set("d", r.Date.Format("20060102"))
	form.Set("t", r.Date.Format("15:04"))
	form.Set("v1", fmt.Sprintf("%d", r.Energy))
	form.Set("v2", fmt.Sprintf("%d", r.Power))
	if r.Voltage > 0 {
		form.Set("v6", fmt.Sprintf("%d", r.Voltage))
	}

	req, err := http.NewRequest("POST", "https://pvoutput.org/service/r2/addstatus.jsp", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("X-Pvoutput-Apikey", cfg.APIKey)
	req.Header.Set("X-Pvoutput-SystemId", cfg.SystemID)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)

	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload failed: %s", resp.Status)
	}

	return nil
}

func calculateTodaysWattHours(whLifetime float64) int {
	todayWh, err := loadOrInit(whLifetime)

	if err != nil {
		log.Printf("Warning: could not load state file, defaulting to zero: %v", err)
		todayWh = 0
	}

	return int(todayWh)
}

func loadOrInit(currentWh float64) (float64, error) {
	today := time.Now().Format("2006-01-02")

	f, err := os.Open(statePath)

	if err != nil {
		return initState(today, currentWh)
	}

	defer f.Close()

	var s State
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		return 0, fmt.Errorf("failed to parse state file: %w", err)
	}

	if s.Date != today {
		// new day, reset baseline
		return initState(today, currentWh)
	}

	return currentWh - s.Baseline, nil
}

func initState(date string, baseline float64) (float64, error) {
	state := State{Date: date, Baseline: baseline}
	f, err := os.Create(statePath)

	if err != nil {
		return 0, fmt.Errorf("failed to write state file: %w", err)
	}

	defer f.Close()

	if err := json.NewEncoder(f).Encode(state); err != nil {
		return 0, fmt.Errorf("failed to encode state: %w", err)
	}

	// new day, zero energy so far
	return 0, nil
}
