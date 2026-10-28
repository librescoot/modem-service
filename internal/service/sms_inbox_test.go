package service

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	ipc "github.com/librescoot/redis-ipc"
	redisClient "modem-service/internal/redis"
	"modem-service/internal/sms"
)

func TestSMSInboxIntegration(t *testing.T) {
	url := os.Getenv("SMS_TEST_REDIS_URL")
	if url == "" {
		t.Skip("set SMS_TEST_REDIS_URL to a dedicated scratch Redis")
	}
	logger := log.New(io.Discard, "", 0)
	client, err := redisClient.New(url, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	probe, err := ipc.New(ipc.WithURL(url))
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	store, err := sms.OpenStore(filepath.Join(t.TempDir(), "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &Service{Redis: client, Logger: logger, inbox: store}
	s.forwardDestination.Store("off")
	msg := &sms.Message{Number: "+491234567890", Text: "hello", Timestamp: time.Now(), SourcePath: "/sms/1"}
	if err := s.deliverIncomingSMS(msg); err != nil {
		t.Fatal(err)
	}
	if err := s.deliverIncomingSMS(msg); err != nil {
		t.Fatal(err)
	}
	raw, err := probe.HGet("sms:inbox", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot inboxSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 1 || snapshot.Unread != 1 {
		t.Fatalf("snapshot %+v", snapshot)
	}
	payload, _ := json.Marshal(inboxCommand{Action: "read", ID: snapshot.Messages[0].ID})
	if err := s.handleInboxCommand(string(payload)); err != nil {
		t.Fatal(err)
	}
	raw, err = probe.HGet("sms:inbox", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Unread != 0 || !snapshot.Messages[0].Read {
		t.Fatalf("read snapshot %+v", snapshot)
	}
	payload, _ = json.Marshal(inboxCommand{Action: "delete", ID: snapshot.Messages[0].ID})
	if err := s.handleInboxCommand(string(payload)); err != nil {
		t.Fatal(err)
	}
	raw, err = probe.HGet("sms:inbox", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 0 {
		t.Fatalf("delete snapshot %+v", snapshot)
	}
}
