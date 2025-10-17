package mm

// ModemManager constants and enums

// Modem State
const (
	MMModemStateFailed       int32 = -1
	MMModemStateUnknown      int32 = 0
	MMModemStateInitializing int32 = 1
	MMModemStateLocked       int32 = 2
	MMModemStateDisabled     int32 = 3
	MMModemStateDisabling    int32 = 4
	MMModemStateEnabling     int32 = 5
	MMModemStateEnabled      int32 = 6
	MMModemStateSearching    int32 = 7
	MMModemStateRegistered   int32 = 8
	MMModemStateDisconnecting int32 = 9
	MMModemStateConnecting   int32 = 10
	MMModemStateConnected    int32 = 11
)

// Power State
const (
	MMModemPowerStateUnknown int32 = 0
	MMModemPowerStateOff     int32 = 1
	MMModemPowerStateLow     int32 = 2
	MMModemPowerStateOn      int32 = 3
)

// Registration State
const (
	MMModem3gppRegistrationStateIdle     uint32 = 0
	MMModem3gppRegistrationStateHome     uint32 = 1
	MMModem3gppRegistrationStateSearching uint32 = 2
	MMModem3gppRegistrationStateDenied   uint32 = 3
	MMModem3gppRegistrationStateUnknown  uint32 = 4
	MMModem3gppRegistrationStateRoaming  uint32 = 5
)

// Access Technology
const (
	MMModemAccessTechnologyUnknown     uint32 = 0
	MMModemAccessTechnologyPots        uint32 = 1 << 0
	MMModemAccessTechnologyGsm         uint32 = 1 << 1
	MMModemAccessTechnologyGsmCompact  uint32 = 1 << 2
	MMModemAccessTechnologyGprs        uint32 = 1 << 3
	MMModemAccessTechnologyEdge        uint32 = 1 << 4
	MMModemAccessTechnologyUmts        uint32 = 1 << 5
	MMModemAccessTechnologyHsdpa       uint32 = 1 << 6
	MMModemAccessTechnologyHsupa       uint32 = 1 << 7
	MMModemAccessTechnologyHspa        uint32 = 1 << 8
	MMModemAccessTechnologyHspaPlus    uint32 = 1 << 9
	MMModemAccessTechnology1xrtt       uint32 = 1 << 10
	MMModemAccessTechnologyEvdo0       uint32 = 1 << 11
	MMModemAccessTechnologyEvdoa       uint32 = 1 << 12
	MMModemAccessTechnologyEvdob       uint32 = 1 << 13
	MMModemAccessTechnologyLte         uint32 = 1 << 14
	MMModemAccessTechnology5gnr        uint32 = 1 << 15
)

// Location Source
const (
	MMModemLocationSource3gppLacCi  uint32 = 1 << 0
	MMModemLocationSourceGpsRaw     uint32 = 1 << 1
	MMModemLocationSourceGpsNmea    uint32 = 1 << 2
	MMModemLocationSourceCdmaBs     uint32 = 1 << 3
	MMModemLocationSourceGpsUnmanaged uint32 = 1 << 4
	MMModemLocationSourceAgpsMsa    uint32 = 1 << 5
	MMModemLocationSourceAgpsMsb    uint32 = 1 << 6
)

// SIM Lock Reason
const (
	MMLockUnknown     uint32 = 0
	MMLockNone        uint32 = 1
	MMLockSimPin      uint32 = 2
	MMLockSimPin2     uint32 = 3
	MMLockSimPuk      uint32 = 4
	MMLockSimPuk2     uint32 = 5
	MMLockPhSimPin    uint32 = 6
	MMLockPhFsimPin   uint32 = 7
	MMLockPhFsimPuk   uint32 = 8
	MMLockPhNetPin    uint32 = 9
	MMLockPhNetPuk    uint32 = 10
	MMLockPhSpPin     uint32 = 11
	MMLockPhSpPuk     uint32 = 12
	MMLockPhCorpPin   uint32 = 13
	MMLockPhCorpPuk   uint32 = 14
)

// Helper functions

func ModemStateToString(state int32) string {
	switch state {
	case MMModemStateFailed:
		return "failed"
	case MMModemStateUnknown:
		return "unknown"
	case MMModemStateInitializing:
		return "initializing"
	case MMModemStateLocked:
		return "locked"
	case MMModemStateDisabled:
		return "disabled"
	case MMModemStateDisabling:
		return "disabling"
	case MMModemStateEnabling:
		return "enabling"
	case MMModemStateEnabled:
		return "enabled"
	case MMModemStateSearching:
		return "searching"
	case MMModemStateRegistered:
		return "registered"
	case MMModemStateDisconnecting:
		return "disconnecting"
	case MMModemStateConnecting:
		return "connecting"
	case MMModemStateConnected:
		return "connected"
	default:
		return "unknown"
	}
}

func PowerStateToString(state int32) string {
	switch state {
	case MMModemPowerStateOff:
		return "off"
	case MMModemPowerStateLow:
		return "low-power"
	case MMModemPowerStateOn:
		return "on"
	default:
		return "unknown"
	}
}

func RegistrationStateToString(state uint32) string {
	switch state {
	case MMModem3gppRegistrationStateIdle:
		return "idle"
	case MMModem3gppRegistrationStateHome:
		return "home"
	case MMModem3gppRegistrationStateSearching:
		return "searching"
	case MMModem3gppRegistrationStateDenied:
		return "denied"
	case MMModem3gppRegistrationStateRoaming:
		return "roaming"
	default:
		return "unknown"
	}
}

func AccessTechnologyToString(tech uint32) string {
	if tech == MMModemAccessTechnologyUnknown {
		return "UNKNOWN"
	}

	// Check in order of preference (newer tech first)
	if tech&MMModemAccessTechnology5gnr != 0 {
		return "5G"
	}
	if tech&MMModemAccessTechnologyLte != 0 {
		return "4G"
	}
	if tech&MMModemAccessTechnologyHspaPlus != 0 {
		return "HSPA+"
	}
	if tech&MMModemAccessTechnologyHspa != 0 {
		return "HSPA"
	}
	if tech&(MMModemAccessTechnologyHsdpa|MMModemAccessTechnologyHsupa) != 0 {
		return "3G"
	}
	if tech&MMModemAccessTechnologyUmts != 0 {
		return "UMTS"
	}
	if tech&(MMModemAccessTechnologyEdge|MMModemAccessTechnologyGprs) != 0 {
		return "EDGE"
	}
	if tech&MMModemAccessTechnologyGsm != 0 {
		return "GSM"
	}

	return "UNKNOWN"
}

func LockReasonToString(lock uint32) string {
	switch lock {
	case MMLockNone:
		return "none"
	case MMLockSimPin:
		return "sim-pin"
	case MMLockSimPin2:
		return "sim-pin2"
	case MMLockSimPuk:
		return "sim-puk"
	case MMLockSimPuk2:
		return "sim-puk2"
	case MMLockPhSimPin:
		return "ph-sim-pin"
	case MMLockPhFsimPin:
		return "ph-fsim-pin"
	case MMLockPhNetPin:
		return "ph-net-pin"
	case MMLockPhSpPin:
		return "ph-sp-pin"
	case MMLockPhCorpPin:
		return "ph-corp-pin"
	default:
		return "unknown"
	}
}
