package redis

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

// GPSSnapshotChannel is the pub/sub channel that carries full GPS TPV snapshots
// as JSON. Consumers can subscribe here to receive pushed updates instead of
// polling the `gps` hash. The hash is still maintained for backward-compatible
// readers; this channel is purely additive.
const GPSSnapshotChannel = "gps:tpv"

// Fault codes for modem issues
const (
	FaultCodeModemRecoveryFailed = 1
)

// Client wraps the Redis IPC client
type Client struct {
	client        *ipc.Client
	logger        *log.Logger
	faults        *ipc.FaultReporter
	modemHandler  *ipc.QueueHandler[string]
	smsHandler    *ipc.QueueHandler[string]
	vehicleWatch  *ipc.HashWatcher
	settingsWatch *ipc.HashWatcher
}

// ModemCommandHandler is called when modem enable/disable commands are received
type ModemCommandHandler func(command string) error

// SMSCommandHandler is called when an outbound SMS request is received on the
// scooter:sms queue. The payload is the raw JSON command string.
type SMSCommandHandler func(payload string) error

// VehicleStateHandler is called when vehicle state changes
type VehicleStateHandler func(state string) error

// New creates a new Redis client using redis-ipc
func New(redisURL string, logger *log.Logger) (*Client, error) {
	client, err := ipc.New(
		ipc.WithURL(redisURL),
		ipc.WithCodec(ipc.StringCodec{}), // Use plain strings, not JSON (matches existing IPC)
		ipc.WithOnDisconnect(func(err error) {
			logger.Printf("Redis disconnected: %v", err)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create redis-ipc client: %v", err)
	}

	return &Client{
		client: client,
		logger: logger,
		faults: client.NewFaultReporter("internet"),
	}, nil
}

// Ping checks if the Redis server is reachable
func (c *Client) Ping() error {
	return c.client.Ping()
}

// PublishInternetState publishes an internet-state field to Redis. Callers
// (service.Service) perform their own change detection against the in-memory
// LastState, so this only writes and notifies — no GET round-trip. Uses the
// Sync() option so read-after-write consumers (and anything that treats an
// error return as "definitely landed") see the field on next HGET.
func (c *Client) PublishInternetState(field, value string) error {
	err := c.client.Hash("internet").Set(field, value, ipc.Sync())
	if err != nil {
		c.logger.Printf("Unable to set %s in redis: %v", field, err)
		return fmt.Errorf("cannot write to redis: %v", err)
	}
	return nil
}

// PublishModemState publishes a modem-state field to Redis. Callers perform
// change detection against LastState; this is a plain Set + notify.
// Synchronous for the same reason as PublishInternetState.
func (c *Client) PublishModemState(field, value string) error {
	err := c.client.Hash("modem").Set(field, value, ipc.Sync())
	if err != nil {
		c.logger.Printf("Unable to set modem.%s in redis: %v", field, err)
		return fmt.Errorf("cannot write to redis: %v", err)
	}
	return nil
}

// PublishLocationState publishes location state to Redis.
// publishRecovery should be true only when GPS becomes available after significant outage
// or on first fix after initialization. When true, publishes a "timestamp" notification.
// When false, updates the hash without publishing (silent update).
func (c *Client) PublishLocationState(data map[string]interface{}, publishRecovery bool) error {
	// Add updated timestamp to track when data was last refreshed
	data["updated"] = time.Now().Format(time.RFC3339)

	// Handle GPS publishing based on recovery status
	// When publishRecovery is true: publish "timestamp" notification (GPS recovered)
	// When publishRecovery is false: silent update (no pub/sub notification)
	var err error
	if publishRecovery {
		err = c.client.Hash("gps").SetManyPublishOne(data, "timestamp")
	} else {
		err = c.client.Hash("gps").SetMany(data, ipc.NoPublish())
	}

	if err != nil {
		c.logger.Printf("Unable to set location in redis: %v", err)
		return fmt.Errorf("cannot write location to redis: %v", err)
	}
	return nil
}

// PublishGPSSnapshot publishes a full TPV snapshot to the gps:tpv pub/sub
// channel as JSON. Subscribers get the complete current GPS state in one
// message — no HGETALL roundtrip needed. Async (fire-and-forget); send loss
// on a wedged Redis is acceptable because the next tick will publish again
// and any polling consumer can still read the hash.
func (c *Client) PublishGPSSnapshot(data map[string]interface{}) error {
	payload, err := json.Marshal(data)
	if err != nil {
		c.logger.Printf("Unable to encode GPS snapshot: %v", err)
		return fmt.Errorf("encode gps snapshot: %v", err)
	}
	if _, err := c.client.Publish(GPSSnapshotChannel, payload); err != nil {
		c.logger.Printf("Unable to publish GPS snapshot: %v", err)
		return fmt.Errorf("publish gps snapshot: %v", err)
	}
	return nil
}

// PublishCellLocationState publishes cell tower geolocation to Redis.
func (c *Client) PublishCellLocationState(data map[string]interface{}) error {
	data["updated"] = time.Now().Format(time.RFC3339)
	err := c.client.Hash("cell-location").SetMany(data, ipc.NoPublish())
	if err != nil {
		c.logger.Printf("Unable to set cell-location in redis: %v", err)
		return fmt.Errorf("cannot write cell-location to redis: %v", err)
	}
	return nil
}

// PublishSMSState sets a single field on the "sms" hash and notifies the "sms"
// channel with the field name — the standard librescoot hash+channel
// convention. Synchronous, like PublishModemState, so the field is readable by
// the time we return.
func (c *Client) PublishSMSState(field, value string) error {
	err := c.client.Hash("sms").Set(field, value, ipc.Sync())
	if err != nil {
		c.logger.Printf("Unable to set sms.%s in redis: %v", field, err)
		return fmt.Errorf("cannot write to redis: %v", err)
	}
	return nil
}

// PublishSMSFields sets several "sms" hash fields atomically and publishes a
// single notification (notifyField) on the "sms" channel. Used for multi-field
// updates — an incoming message, or a completed send — so subscribers get one
// wake-up and then HGET the fields they need, the same batch pattern
// PublishLocationState uses for GPS.
func (c *Client) PublishSMSFields(fields map[string]string, notifyField string) error {
	data := make(map[string]interface{}, len(fields))
	for k, v := range fields {
		data[k] = v
	}
	err := c.client.Hash("sms").SetManyPublishOne(data, notifyField, ipc.Sync())
	if err != nil {
		c.logger.Printf("Unable to set sms fields in redis: %v", err)
		return fmt.Errorf("cannot write to redis: %v", err)
	}
	return nil
}

// StartModemCommandHandler starts listening for modem enable/disable commands on scooter:modem list
func (c *Client) StartModemCommandHandler(handler ModemCommandHandler) error {
	c.modemHandler = ipc.HandleRequests(c.client, "scooter:modem", func(cmd string) error {
		c.logger.Printf("Received modem command: %s", cmd)
		return handler(cmd)
	})
	return nil
}

// StartSMSCommandHandler starts listening for outbound SMS requests on the
// scooter:sms list. Unlike the other queues (plain-string commands), the
// payload is a JSON object {"to":...,"text":...}; the handler unmarshals it.
func (c *Client) StartSMSCommandHandler(handler SMSCommandHandler) error {
	c.smsHandler = ipc.HandleRequests(c.client, "scooter:sms", func(payload string) error {
		// The payload carries a recipient and message body — don't log it.
		c.logger.Printf("Received SMS send command")
		return handler(payload)
	})
	return nil
}

// Power inhibitor wiring. modem-service registers a block inhibitor in the
// power:inhibits hash while the modem is powered, so pm-service holds off
// suspend until the modem has been told to shut down and confirmed off. The
// "who" string MUST match pm-service's modem check (hasOnlyModemBlockingInhibitors).
const (
	powerInhibitHash    = "power:inhibits"
	powerInhibitChannel = "power:inhibits"
	modemInhibitID      = "modem-active"
	modemInhibitWho     = "librescoot-modem"
)

// inhibitData mirrors the JSON shape pm-service reads from the power:inhibits hash.
type inhibitData struct {
	ID       string `json:"id"`
	Who      string `json:"who"`
	What     string `json:"what"`
	Why      string `json:"why"`
	Type     string `json:"type"`
	Duration int64  `json:"duration"`
	Created  int64  `json:"created"`
}

// AddModemInhibitor registers a block inhibitor so pm-service does not suspend
// while the modem is powered. Idempotent: HSET overwrites and pm-service
// reconciles from HGETALL, so calling it on every enable is safe.
func (c *Client) AddModemInhibitor() error {
	payload, err := json.Marshal(inhibitData{
		ID:      modemInhibitID,
		Who:     modemInhibitWho,
		What:    "power-state-change",
		Why:     "modem is powered",
		Type:    "block",
		Created: time.Now().Unix(),
	})
	if err != nil {
		return fmt.Errorf("marshal modem inhibitor: %w", err)
	}
	if err := c.client.HSet(powerInhibitHash, modemInhibitID, string(payload)); err != nil {
		return fmt.Errorf("set modem inhibitor: %w", err)
	}
	if _, err := c.client.Publish(powerInhibitChannel, "add:"+modemInhibitID); err != nil {
		return fmt.Errorf("publish modem inhibitor add: %w", err)
	}
	return nil
}

// RemoveModemInhibitor clears the block inhibitor. HDEL is required: pm-service
// reconciles from the hash, so publishing "remove:" without deleting the field
// would leave the inhibitor asserted. redis-ipc has no HDel wrapper, so use Do.
func (c *Client) RemoveModemInhibitor() error {
	if _, err := c.client.Do("HDEL", powerInhibitHash, modemInhibitID); err != nil {
		return fmt.Errorf("del modem inhibitor: %w", err)
	}
	if _, err := c.client.Publish(powerInhibitChannel, "remove:"+modemInhibitID); err != nil {
		return fmt.Errorf("publish modem inhibitor remove: %w", err)
	}
	return nil
}

// StartVehicleStateWatcher starts watching vehicle state changes
func (c *Client) StartVehicleStateWatcher(handler VehicleStateHandler) error {
	c.vehicleWatch = c.client.NewHashWatcher("vehicle")
	c.vehicleWatch.OnField("state", func(value string) error {
		c.logger.Printf("vehicle state=%s", value)
		return handler(value)
	})
	c.vehicleWatch.StartWithSync()
	return nil
}

// SettingHandler is called when a setting changes
type SettingHandler func(value string) error

// StartSettingsWatcher starts watching settings hash for specific fields
func (c *Client) StartSettingsWatcher(field string, handler SettingHandler) {
	if c.settingsWatch == nil {
		c.settingsWatch = c.client.NewHashWatcher("settings")
	}
	c.settingsWatch.OnField(field, func(value string) error {
		// Don't log the value here: some settings (e.g. cellular.sim-pin) are
		// sensitive. Per-field handlers can log a redacted summary themselves.
		c.logger.Printf("Setting %s changed", field)
		return handler(value)
	})
}

// StartSettingsWatching begins watching after all fields are registered
func (c *Client) StartSettingsWatching() {
	if c.settingsWatch != nil {
		c.settingsWatch.StartWithSync()
	}
}

// RaiseFault marks a fault as present. Idempotent: emits no stream entry
// if the code is already raised.
func (c *Client) RaiseFault(code int, description string) error {
	if err := c.faults.Raise(code, description); err != nil {
		c.logger.Printf("Failed to raise fault %d: %v", code, err)
		return err
	}
	return nil
}

// ClearFault marks a fault as absent. Idempotent: emits no stream entry
// if the code wasn't raised.
func (c *Client) ClearFault(code int) error {
	if err := c.faults.Clear(code); err != nil {
		c.logger.Printf("Failed to clear fault %d: %v", code, err)
		return err
	}
	return nil
}

// Close closes the Redis client and stops all handlers with a tight timeout.
// redis-ipc's default Close is 30s × 2 phases = 60s worst case, which is more
// than we need for a local Redis instance — 10s is plenty and keeps shutdown
// snappy enough to stay well under systemd's default 90s TimeoutStopSec even
// when other shutdown work is queued behind us.
func (c *Client) Close() error {
	if c.modemHandler != nil {
		c.modemHandler.Stop()
	}
	if c.smsHandler != nil {
		c.smsHandler.Stop()
	}
	if c.vehicleWatch != nil {
		c.vehicleWatch.Stop()
	}
	if c.settingsWatch != nil {
		c.settingsWatch.Stop()
	}
	return c.client.CloseWithTimeout(10 * time.Second)
}
