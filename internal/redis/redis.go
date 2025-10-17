package redis

import (
	"context"
	"fmt"
	"log"

	"github.com/redis/go-redis/v9"
)

// Client wraps the Redis client with additional functionality
type Client struct {
	client *redis.Client
	logger *log.Logger
}

// New creates a new Redis client
func New(redisURL string, logger *log.Logger) (*Client, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL: %v", err)
	}

	client := redis.NewClient(opt)
	return &Client{
		client: client,
		logger: logger,
	}, nil
}

// Ping checks if the Redis server is reachable
func (c *Client) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

// PublishInternetState publishes modem state to Redis
func (c *Client) PublishInternetState(ctx context.Context, key, field, value string) error {
	pipe := c.client.Pipeline()
	pipe.HSet(ctx, "internet", field, value)
	pipe.Publish(ctx, "internet", field)
	_, err := pipe.Exec(ctx)
	if err != nil {
		c.logger.Printf("Unable to set %s in redis: %v", field, err)
		return fmt.Errorf("cannot write to redis: %v", err)
	}
	return nil
}

// PublishModemState publishes modem state to Redis under modem hash
func (c *Client) PublishModemState(ctx context.Context, field, value string) error {
	pipe := c.client.Pipeline()
	pipe.HSet(ctx, "modem", field, value)
	pipe.Publish(ctx, "modem", field)
	_, err := pipe.Exec(ctx)
	if err != nil {
		c.logger.Printf("Unable to set modem.%s in redis: %v", field, err)
		return fmt.Errorf("cannot write to redis: %v", err)
	}
	return nil
}

// PublishRawLocationState publishes raw location state to Redis
func (c *Client) PublishRawLocationState(ctx context.Context, data map[string]interface{}) error {
	pipe := c.client.Pipeline()
	pipe.HSet(ctx, "gps:raw", data)
	_, err := pipe.Exec(ctx)
	if err != nil {
		c.logger.Printf("Unable to set raw location in redis: %v", err)
		return fmt.Errorf("cannot write raw location to redis: %v", err)
	}
	return nil
}

// PublishFilteredLocationState publishes filtered location state to Redis
func (c *Client) PublishFilteredLocationState(ctx context.Context, data map[string]interface{}) error {
	pipe := c.client.Pipeline()
	pipe.HSet(ctx, "gps:filtered", data)
	_, err := pipe.Exec(ctx)
	if err != nil {
		c.logger.Printf("Unable to set filtered location in redis: %v", err)
		return fmt.Errorf("cannot write filtered location to redis: %v", err)
	}
	return nil
}

// PublishLocationState publishes location state to Redis based on filter setting
func (c *Client) PublishLocationState(ctx context.Context, rawData, filteredData map[string]interface{}) error {
	// Store raw data to gps:raw
	if err := c.PublishRawLocationState(ctx, rawData); err != nil {
		return err
	}
	
	// Store filtered data to gps:filtered
	if err := c.PublishFilteredLocationState(ctx, filteredData); err != nil {
		return err
	}
	
	// Check filter setting to decide which data to store in main gps hash
	filterSetting, err := c.client.HGet(ctx, "modem", "gps:filter").Result()
	if err != nil && err != redis.Nil {
		c.logger.Printf("Unable to get gps:filter setting: %v", err)
		// Default to filtered if we can't read the setting
		filterSetting = "on"
	}
	
	var dataToStore map[string]interface{}
	if filterSetting == "off" {
		dataToStore = rawData
	} else {
		// Default to filtered data when setting is "on" or missing
		dataToStore = filteredData
	}
	
	// Store to main gps hash and publish timestamp field
	pipe := c.client.Pipeline()
	pipe.HSet(ctx, "gps", dataToStore)
	pipe.Publish(ctx, "gps", "timestamp") // Publish field name to trigger immediate UI refresh
	_, err = pipe.Exec(ctx)
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
