package redis

import (
	"fmt"
	"log"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

// Fault codes for modem issues
const (
	FaultCodeModemRecoveryFailed = 1
)

// Client wraps the Redis IPC client
type Client struct {
	client        *ipc.Client
	logger        *log.Logger
	faultStream   *ipc.StreamPublisher
	faultSet      *ipc.FaultSet
	modemHandler  *ipc.QueueHandler[string]
	vehicleWatch  *ipc.HashWatcher
	settingsWatch *ipc.HashWatcher
}

// ModemCommandHandler is called when modem enable/disable commands are received
type ModemCommandHandler func(command string) error

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

	// Create fault stream publisher for events:faults
	faultStream := client.NewStreamPublisher("events:faults")

	// Create fault set for internet:fault
	faultSet := client.NewFaultSet("internet:fault", "internet", "fault")

	return &Client{
		client:      client,
		logger:      logger,
		faultStream: faultStream,
		faultSet:    faultSet,
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

// StartModemCommandHandler starts listening for modem enable/disable commands on scooter:modem list
func (c *Client) StartModemCommandHandler(handler ModemCommandHandler) error {
	c.modemHandler = ipc.HandleRequests(c.client, "scooter:modem", func(cmd string) error {
		c.logger.Printf("Received modem command: %s", cmd)
		return handler(cmd)
	})
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
		c.logger.Printf("Setting %s changed: %s", field, value)
		return handler(value)
	})
}

// StartSettingsWatching begins watching after all fields are registered
func (c *Client) StartSettingsWatching() {
	if c.settingsWatch != nil {
		c.settingsWatch.StartWithSync()
	}
}

// LogFault logs a fault event to the events:faults stream
func (c *Client) LogFault(group string, code int, description string) error {
	_, err := c.faultStream.Add(map[string]any{
		"group":       group,
		"code":        code,
		"description": description,
	})
	if err != nil {
		c.logger.Printf("Failed to log fault: %v", err)
	}
	return err
}

// AddFault adds a fault code to the internet:fault set
func (c *Client) AddFault(code int) error {
	return c.faultSet.Add(code)
}

// RemoveFault removes a fault code from the internet:fault set
func (c *Client) RemoveFault(code int) error {
	return c.faultSet.Remove(code)
}

// ClearFaults clears all faults from the internet:fault set
func (c *Client) ClearFaults() error {
	return c.faultSet.Clear()
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
	if c.vehicleWatch != nil {
		c.vehicleWatch.Stop()
	}
	if c.settingsWatch != nil {
		c.settingsWatch.Stop()
	}
	return c.client.CloseWithTimeout(10 * time.Second)
}
