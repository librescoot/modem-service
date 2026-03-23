package cell

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	beaconDBURL    = "https://api.beacondb.net/v1/geolocate"
	requestTimeout = 5 * time.Second
)

var userAgent string

func SetVersion(version string) {
	userAgent = "librescoot-modem-service/" + version + " (https://librescoot.org)"
}

type CellTower struct {
	RadioType         string `json:"radioType"`
	MobileCountryCode int    `json:"mobileCountryCode"`
	MobileNetworkCode int    `json:"mobileNetworkCode"`
	LocationAreaCode  int    `json:"locationAreaCode"`
	CellId            int    `json:"cellId"`
}

type geolocateRequest struct {
	CellTowers []CellTower `json:"cellTowers"`
}

type geolocateResponse struct {
	Location struct {
		Lat float64 `json:"lat"`
		Lng float64 `json:"lng"`
	} `json:"location"`
	Accuracy float64 `json:"accuracy"`
}

type CellLocation struct {
	Latitude  float64
	Longitude float64
	Accuracy  float64
}

func Geolocate(ctx context.Context, towers []CellTower) (*CellLocation, error) {
	if len(towers) == 0 {
		return nil, fmt.Errorf("no cell towers provided")
	}

	body, err := json.Marshal(geolocateRequest{CellTowers: towers})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", beaconDBURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("beacondb returned %d", resp.StatusCode)
	}

	var result geolocateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &CellLocation{
		Latitude:  result.Location.Lat,
		Longitude: result.Location.Lng,
		Accuracy:  result.Accuracy,
	}, nil
}
