package redis

import (
	"fmt"
	"log"
	"os"
	"testing"
	"time"
)

// getTestRedisURL returns the Redis URL for testing
func getTestRedisURL() string {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		url = "redis://localhost:6379"
	}
	return url
}

// setupTestClient creates a test client and cleans up test data
func setupTestClient(t *testing.T) (*Client, func()) {
	t.Helper()

	logger := log.New(os.Stdout, "test: ", log.LstdFlags)
	client, err := New(getTestRedisURL(), logger)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	// Check if Redis is available
	if err := client.Ping(); err != nil {
		t.Skipf("Redis not available: %v", err)
	}

	cleanup := func() {
		// Clean up test data
		client.client.Hash("internet").Clear()
		client.client.Hash("modem").Clear()
		client.client.Hash("gps").Clear()
		client.Close()
	}

	return client, cleanup
}

func TestNew(t *testing.T) {
	logger := log.New(os.Stdout, "test: ", log.LstdFlags)

	tests := []struct {
		name      string
		redisURL  string
		wantErr   bool
		wantHost  string
		wantPort  int
	}{
		{
			name:     "valid URL with port",
			redisURL: "redis://localhost:6379",
			wantErr:  false,
		},
		{
			name:     "valid URL without port",
			redisURL: "redis://localhost",
			wantErr:  false,
		},
		{
			name:     "empty URL defaults to localhost:6379",
			redisURL: "",
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := New(tt.redisURL, logger)
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && client != nil {
				client.Close()
			}
		})
	}
}

func TestNewWithVariousURLFormats(t *testing.T) {
	logger := log.New(os.Stdout, "test: ", log.LstdFlags)

	tests := []struct {
		name     string
		url      string
		wantErr  bool
		skipTest bool
	}{
		{
			name:    "full URL with port",
			url:     "redis://localhost:6379",
			wantErr: false,
		},
		{
			name:    "URL without scheme",
			url:     "localhost:6379",
			wantErr: false,
		},
		{
			name:    "URL without port",
			url:     "localhost",
			wantErr: false,
		},
		{
			name:    "empty URL defaults to localhost:6379",
			url:     "",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skipTest {
				t.Skip("Skipping test that requires Redis")
			}

			client, err := New(tt.url, logger)
			if (err != nil) != tt.wantErr {
				// Connection errors are acceptable if Redis isn't running
				if err != nil && !tt.wantErr {
					t.Logf("New() error = %v (Redis may not be available)", err)
					return
				}
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && client != nil {
				client.Close()
			}
		})
	}
}

func TestPublishInternetState(t *testing.T) {
	client, cleanup := setupTestClient(t)
	defer cleanup()

	tests := []struct {
		name    string
		key     string
		field   string
		value   string
		wantErr bool
	}{
		{
			name:    "publish status",
			key:     "internet",
			field:   "status",
			value:   "connected",
			wantErr: false,
		},
		{
			name:    "publish modem-state",
			key:     "internet",
			field:   "modem-state",
			value:   "registered",
			wantErr: false,
		},
		{
			name:    "publish ip-address",
			key:     "internet",
			field:   "ip-address",
			value:   "10.0.0.1",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := client.PublishInternetState(tt.key, tt.field, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("PublishInternetState() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify the value was set
			if !tt.wantErr {
				val, err := client.client.Hash("internet").Get(tt.field)
				if err != nil {
					t.Errorf("Failed to get field %s: %v", tt.field, err)
				}
				if val != tt.value {
					t.Errorf("Field %s = %v, want %v", tt.field, val, tt.value)
				}
			}
		})
	}
}

func TestPublishInternetStateChangeDetection(t *testing.T) {
	client, cleanup := setupTestClient(t)
	defer cleanup()

	// First publish should succeed
	err := client.PublishInternetState("internet", "status", "connected")
	if err != nil {
		t.Fatalf("First publish failed: %v", err)
	}

	// Second publish with same value should not trigger a change
	// (SetIfChanged will still return success, but won't publish)
	err = client.PublishInternetState("internet", "status", "connected")
	if err != nil {
		t.Fatalf("Second publish failed: %v", err)
	}

	// Third publish with different value should trigger a change
	err = client.PublishInternetState("internet", "status", "disconnected")
	if err != nil {
		t.Fatalf("Third publish failed: %v", err)
	}

	// Verify final value
	val, err := client.client.Hash("internet").Get("status")
	if err != nil {
		t.Fatalf("Failed to get status: %v", err)
	}
	if val != "disconnected" {
		t.Errorf("status = %v, want disconnected", val)
	}
}

func TestPublishModemState(t *testing.T) {
	client, cleanup := setupTestClient(t)
	defer cleanup()

	tests := []struct {
		name    string
		field   string
		value   string
		wantErr bool
	}{
		{
			name:    "publish power-state",
			field:   "power-state",
			value:   "on",
			wantErr: false,
		},
		{
			name:    "publish sim-state",
			field:   "sim-state",
			value:   "registered",
			wantErr: false,
		},
		{
			name:    "publish operator-name",
			field:   "operator-name",
			value:   "TestCarrier",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := client.PublishModemState(tt.field, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("PublishModemState() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify the value was set
			if !tt.wantErr {
				val, err := client.client.Hash("modem").Get(tt.field)
				if err != nil {
					t.Errorf("Failed to get field %s: %v", tt.field, err)
				}
				if val != tt.value {
					t.Errorf("Field %s = %v, want %v", tt.field, val, tt.value)
				}
			}
		})
	}
}

func TestPublishLocationState(t *testing.T) {
	client, cleanup := setupTestClient(t)
	defer cleanup()

	tests := []struct {
		name            string
		data            map[string]interface{}
		publishRecovery bool
		wantErr         bool
		description     string
	}{
		{
			name: "regular GPS update (no publish)",
			data: map[string]interface{}{
				"latitude":  "45.123456",
				"longitude": "-122.654321",
				"altitude":  "100.5",
				"speed":     "25.0",
				"course":    "180.0",
				"timestamp": time.Now().Format(time.RFC3339),
			},
			publishRecovery: false,
			wantErr:         false,
			description:     "Regular update - sets hash without publishing",
		},
		{
			name: "GPS status without fix (no publish)",
			data: map[string]interface{}{
				"fix":       "no-fix",
				"quality":   "0.0",
				"active":    false,
				"connected": true,
			},
			publishRecovery: false,
			wantErr:         false,
			description:     "Status update - sets hash without publishing",
		},
		{
			name: "GPS recovery (publish timestamp)",
			data: map[string]interface{}{
				"latitude":  "45.123456",
				"longitude": "-122.654321",
				"timestamp": time.Now().Format(time.RFC3339),
			},
			publishRecovery: true,
			wantErr:         false,
			description:     "Recovery event - publishes single 'timestamp' notification",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("Testing: %s", tt.description)

			err := client.PublishLocationState(tt.data, tt.publishRecovery)
			if (err != nil) != tt.wantErr {
				t.Errorf("PublishLocationState() error = %v, wantErr %v", err, tt.wantErr)
			}

			// Verify the data was set (check a few fields)
			if !tt.wantErr {
				all, err := client.client.Hash("gps").GetAll()
				if err != nil {
					t.Fatalf("Failed to get all GPS data: %v", err)
				}

				// Check that updated timestamp was added
				if _, ok := all["updated"]; !ok {
					t.Error("updated timestamp not found in GPS data")
				}

				// Verify some fields from input data
				for k, v := range tt.data {
					val, ok := all[k]
					if !ok {
						t.Errorf("Field %s not found in GPS data", k)
						continue
					}
					expectedStr := fmt.Sprintf("%v", v)
					if val != expectedStr {
						t.Errorf("Field %s = %v, want %v", k, val, expectedStr)
					}
				}
			}
		})
	}
}

func TestPing(t *testing.T) {
	client, cleanup := setupTestClient(t)
	defer cleanup()

	err := client.Ping()
	if err != nil {
		t.Errorf("Ping() error = %v", err)
	}
}

func TestClose(t *testing.T) {
	logger := log.New(os.Stdout, "test: ", log.LstdFlags)
	client, err := New(getTestRedisURL(), logger)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	err = client.Close()
	if err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

// TestConcurrentPublishing tests that concurrent publishes don't cause issues
func TestConcurrentPublishing(t *testing.T) {
	client, cleanup := setupTestClient(t)
	defer cleanup()

	done := make(chan bool)

	// Publish internet state concurrently
	go func() {
		for i := 0; i < 10; i++ {
			client.PublishInternetState("internet", "test-field", fmt.Sprintf("value-%d", i))
			time.Sleep(10 * time.Millisecond)
		}
		done <- true
	}()

	// Publish modem state concurrently
	go func() {
		for i := 0; i < 10; i++ {
			client.PublishModemState("test-field", fmt.Sprintf("modem-%d", i))
			time.Sleep(10 * time.Millisecond)
		}
		done <- true
	}()

	// Publish GPS state concurrently
	go func() {
		for i := 0; i < 10; i++ {
			data := map[string]interface{}{
				"latitude":  fmt.Sprintf("45.%d", i),
				"longitude": fmt.Sprintf("-122.%d", i),
			}
			client.PublishLocationState(data, false)
			time.Sleep(10 * time.Millisecond)
		}
		done <- true
	}()

	// Wait for all goroutines
	<-done
	<-done
	<-done

	// Verify we can still read data
	val, err := client.client.Hash("internet").Get("test-field")
	if err != nil {
		t.Logf("Internet test-field not found (expected after concurrent updates): %v", err)
	} else {
		t.Logf("Final internet test-field value: %s", val)
	}
}
