package sms

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersistenceAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "sms.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	msg, err := store.Add(Message{Number: "+49123", Text: "hello", Timestamp: now}, now)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.MarkRead(msg.ID); err != nil || !ok {
		t.Fatalf("mark read: %v %v", ok, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entries, err := store.List(now)
	if err != nil || len(entries) != 1 || !entries[0].Read || entries[0].Text != "hello" {
		t.Fatalf("entries: %+v, %v", entries, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("db mode: %v %v", info, err)
	}
	if ok, err := store.Delete(msg.ID); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if ok, err := store.Delete(msg.ID); err != nil || ok {
		t.Fatalf("repeat delete: %v %v", ok, err)
	}
}

func TestStoreDeduplicatesPendingAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms.db")
	now := time.Now()
	message := Message{Number: "+49123", Text: "hello", Timestamp: now, SourcePath: "/sms/1", SourceTimestamp: "network-time"}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	first, created, err := store.Record(message, now)
	if err != nil || !created {
		t.Fatalf("first: %v %v", created, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	again, created, err := store.Record(message, now.Add(time.Second))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("duplicate: %+v %v %v", again, created, err)
	}
	message.Text = "different"
	other, created, err := store.Record(message, now)
	if err != nil || !created || other.ID == first.ID {
		t.Fatalf("reused path: %+v %v %v", other, created, err)
	}
	if err := store.ForgetModem(message.SourcePath); err != nil {
		t.Fatal(err)
	}
	_, created, err = store.Record(message, now)
	if err != nil || !created {
		t.Fatalf("new identical message: %v %v", created, err)
	}
}

func TestStoreRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("untouched"), 0644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "sms.db")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if store, err := OpenStore(path); err == nil {
		store.Close()
		t.Fatal("opened symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "untouched" {
		t.Fatalf("target changed: %q, %v", data, err)
	}
}

func TestStoreRetention(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < inboxLimit+1; i++ {
		if _, err := store.Add(Message{Text: fmt.Sprint(i), Timestamp: now}, now); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := store.List(now)
	if err != nil || len(entries) != inboxLimit || entries[0].Text != "100" {
		t.Fatalf("entries %d, err %v", len(entries), err)
	}
	entries, err = store.List(now.Add(inboxMaxAge + time.Millisecond))
	if err != nil || len(entries) != 0 {
		t.Fatalf("expired entries %d, err %v", len(entries), err)
	}
}
