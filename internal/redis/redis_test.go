package redis

import (
	"fmt"
	"log"
	"os"
	"testing"
	"time"
)

func getTestRedisURL() string {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		url = "redis://localhost:6379"
	}
	return url
}

func setupTestClient(t *testing.T) (*Client, func()) {
	t.Helper()

	logger := log.New(os.Stdout, "test: ", log.LstdFlags)
	client, err := New(getTestRedisURL(), logger)
	if err != nil {
		t.Fatalf("Failed to create client: %v", err)
	}

	if err := client.Ping(); err != nil {
		t.Skipf("Redis not available: %v", err)
	}

	cleanup := func() {
		client.client.Hash("internet").Clear()
		client.client.Hash("modem").Clear()
		client.client.Hash("gps").Clear()
		client.client.Hash("sms").Clear()
		client.client.Del(SMSReceivedStream, SMSSentStream)
		client.Close()
	}

	return client, cleanup
}

func TestNew(t *testing.T) {
	logger := log.New(os.Stdout, "test: ", log.LstdFlags)

	tests := []struct {
		name     string
		redisURL string
		wantErr  bool
		wantHost string
		wantPort int
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
		field   string
		value   string
		wantErr bool
	}{
		{
			name:    "publish status",
			field:   "status",
			value:   "connected",
			wantErr: false,
		},
		{
			name:    "publish modem-state",
			field:   "modem-state",
			value:   "registered",
			wantErr: false,
		},
		{
			name:    "publish ip-address",
			field:   "ip-address",
			value:   "10.0.0.1",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := client.PublishInternetState(tt.field, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("PublishInternetState() error = %v, wantErr %v", err, tt.wantErr)
			}

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

	err := client.PublishInternetState("status", "connected")
	if err != nil {
		t.Fatalf("First publish failed: %v", err)
	}

	err = client.PublishInternetState("status", "connected")
	if err != nil {
		t.Fatalf("Second publish failed: %v", err)
	}

	err = client.PublishInternetState("status", "disconnected")
	if err != nil {
		t.Fatalf("Third publish failed: %v", err)
	}

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
				"snr":       "0.0",
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
		})
	}
}

func TestPublishSMSState(t *testing.T) {
	client, cleanup := setupTestClient(t)
	defer cleanup()

	tests := []struct {
		name    string
		field   string
		value   string
		wantErr bool
	}{
		{
			name:    "publish state",
			field:   "state",
			value:   "sending",
			wantErr: false,
		},
		{
			name:    "publish last-sent-to",
			field:   "last-sent-to",
			value:   "+4915112345678",
			wantErr: false,
		},
		{
			name:    "publish unread-count",
			field:   "unread-count",
			value:   "3",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := client.PublishSMSState(tt.field, tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("PublishSMSState() error = %v, wantErr %v", err, tt.wantErr)
			}

			if !tt.wantErr {
				val, err := client.client.Hash("sms").Get(tt.field)
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

func TestPublishIncomingSMS(t *testing.T) {
	client, cleanup := setupTestClient(t)
	defer cleanup()

	msg := IncomingSMS{
		From:      "+4930",
		Text:      "hello there",
		Timestamp: "2026-06-15T12:00:00Z",
	}
	if err := client.PublishIncomingSMS(msg, 1); err != nil {
		t.Fatalf("PublishIncomingSMS() error = %v", err)
	}

	// One entry on the stream carrying the message.
	entries, err := client.client.Do("XRANGE", SMSReceivedStream, "-", "+")
	if err != nil {
		t.Fatalf("XRANGE failed: %v", err)
	}
	list, ok := entries.([]interface{})
	if !ok || len(list) != 1 {
		t.Fatalf("expected 1 stream entry, got %#v", entries)
	}

	// Convenience fields land on the hash.
	want := map[string]string{
		"last-received-from": "+4930",
		"last-received-text": "hello there",
		"last-received-at":   "2026-06-15T12:00:00Z",
		"unread-count":       "1",
	}
	for field, w := range want {
		got, err := client.client.Hash("sms").Get(field)
		if err != nil {
			t.Errorf("Failed to get field %s: %v", field, err)
			continue
		}
		if got != w {
			t.Errorf("Field %s = %q, want %q", field, got, w)
		}
	}
}

func TestPublishSMSSendResult(t *testing.T) {
	client, cleanup := setupTestClient(t)
	defer cleanup()

	ok := SMSSendResult{
		RequestID: "req-1",
		To:        "+4930",
		Text:      "outbound",
		Outcome:   "sent",
		Timestamp: "2026-06-15T12:00:00Z",
	}
	if err := client.PublishSMSSendResult(ok); err != nil {
		t.Fatalf("PublishSMSSendResult(sent) error = %v", err)
	}

	fail := SMSSendResult{
		To:        "+4931",
		Text:      "broken",
		Outcome:   "error",
		Error:     "send sms: network timeout",
		Timestamp: "2026-06-15T12:01:00Z",
	}
	if err := client.PublishSMSSendResult(fail); err != nil {
		t.Fatalf("PublishSMSSendResult(error) error = %v", err)
	}

	entries, err := client.client.Do("XRANGE", SMSSentStream, "-", "+")
	if err != nil {
		t.Fatalf("XRANGE failed: %v", err)
	}
	list, ok2 := entries.([]interface{})
	if !ok2 || len(list) != 2 {
		t.Fatalf("expected 2 stream entries, got %#v", entries)
	}

	// The hash reflects the LAST outcome: an error, so state=error but the
	// last-sent-* fields still describe the earlier successful send.
	want := map[string]string{
		"state":        "error",
		"last-sent-to": "+4930",
		"last-sent-at": "2026-06-15T12:00:00Z",
	}
	for field, w := range want {
		got, err := client.client.Hash("sms").Get(field)
		if err != nil {
			t.Errorf("Failed to get field %s: %v", field, err)
			continue
		}
		if got != w {
			t.Errorf("Field %s = %q, want %q", field, got, w)
		}
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

func TestConcurrentPublishing(t *testing.T) {
	client, cleanup := setupTestClient(t)
	defer cleanup()

	done := make(chan bool)

	go func() {
		for i := 0; i < 10; i++ {
			client.PublishInternetState("test-field", fmt.Sprintf("value-%d", i))
			time.Sleep(10 * time.Millisecond)
		}
		done <- true
	}()

	go func() {
		for i := 0; i < 10; i++ {
			client.PublishModemState("test-field", fmt.Sprintf("modem-%d", i))
			time.Sleep(10 * time.Millisecond)
		}
		done <- true
	}()

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

	<-done
	<-done
	<-done

	val, err := client.client.Hash("internet").Get("test-field")
	if err != nil {
		t.Logf("Internet test-field not found (expected after concurrent updates): %v", err)
	} else {
		t.Logf("Final internet test-field value: %s", val)
	}
}
