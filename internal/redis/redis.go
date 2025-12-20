package redis

import (
	"fmt"
	"log"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

// Client wraps the Redis IPC client
type Client struct {
	client *ipc.Client
	logger *log.Logger
}

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
	}, nil
}

// Ping checks if the Redis server is reachable
func (c *Client) Ping() error {
	return c.client.Ping()
}

// PublishInternetState publishes internet state to Redis using SetIfChanged
func (c *Client) PublishInternetState(key, field, value string) error {
	_, err := c.client.Hash("internet").SetIfChanged(field, value)
	if err != nil {
		c.logger.Printf("Unable to set %s in redis: %v", field, err)
		return fmt.Errorf("cannot write to redis: %v", err)
	}
	return nil
}

// PublishModemState publishes modem state to Redis under modem hash using SetIfChanged
func (c *Client) PublishModemState(field, value string) error {
	_, err := c.client.Hash("modem").SetIfChanged(field, value)
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

// Close closes the Redis client
func (c *Client) Close() error {
	return c.client.Close()
}
