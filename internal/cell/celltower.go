package cell

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"
)

const MMModemLocationSource3gppLacCi uint32 = 1 << 0

// ParseModemManagerLocation extracts cell tower info from ModemManager's GetLocation result.
// The 3GPP LAC-CI source (key 1) returns a comma-separated string: "MCC,MNC,LAC,CID,TAC"
// where LAC, CID, and TAC are hex-encoded.
func ParseModemManagerLocation(locationData map[uint32]dbus.Variant, radioType string) (*CellTower, error) {
	lacCiVariant, ok := locationData[MMModemLocationSource3gppLacCi]
	if !ok {
		return nil, fmt.Errorf("no 3GPP location data available")
	}

	raw, ok := lacCiVariant.Value().(string)
	if !ok {
		return nil, fmt.Errorf("unexpected 3GPP location data type: %T", lacCiVariant.Value())
	}

	// Format: "MCC,MNC,LAC,CID,TAC"
	parts := strings.Split(raw, ",")
	if len(parts) < 4 {
		return nil, fmt.Errorf("unexpected 3GPP string format: %q", raw)
	}

	mcc, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, fmt.Errorf("parse mcc %q: %w", parts[0], err)
	}
	mnc, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("parse mnc %q: %w", parts[1], err)
	}

	lac, err := strconv.ParseInt(parts[2], 16, 64)
	if err != nil {
		return nil, fmt.Errorf("parse lac %q: %w", parts[2], err)
	}
	cid, err := strconv.ParseInt(parts[3], 16, 64)
	if err != nil {
		return nil, fmt.Errorf("parse cid %q: %w", parts[3], err)
	}
	if cid == 0 {
		return nil, fmt.Errorf("cell ID is zero (not registered)")
	}

	// Use TAC if present (LTE/5G), fall back to LAC (2G/3G)
	areaCode := int(lac)
	if len(parts) >= 5 {
		if tac, err := strconv.ParseInt(parts[4], 16, 64); err == nil && tac != 0 {
			areaCode = int(tac)
		}
	}

	radio := AccessTechToRadioType(radioType)
	if radio == "" {
		return nil, fmt.Errorf("unknown access technology %q", radioType)
	}

	return &CellTower{
		RadioType:         radio,
		MobileCountryCode: mcc,
		MobileNetworkCode: mnc,
		LocationAreaCode:  areaCode,
		CellId:            int(cid),
	}, nil
}

// AccessTechToRadioType maps modem-service access technology strings to BeaconDB radio types.
// Returns empty string if the access tech can't be mapped.
func AccessTechToRadioType(accessTech string) string {
	switch accessTech {
	case "5G":
		return "nr"
	case "4G":
		return "lte"
	case "UMTS", "3G", "HSPA", "HSPA+":
		return "wcdma"
	case "GSM", "EDGE":
		return "gsm"
	default:
		return ""
	}
}
