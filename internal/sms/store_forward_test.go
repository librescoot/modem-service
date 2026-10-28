package sms

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

const testForwardTo = "+4915112345678"
const testSender = "+4917612345678"
const testOwn = "+4917012345678"

func TestForwardBudgetSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sms.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 11; i++ {
		msg := Message{Number: testSender, Text: fmt.Sprint(i), Timestamp: now}
		if _, created, err := store.RecordWithForward(msg, now.Add(time.Duration(i)*time.Millisecond), testForwardTo); err != nil || !created {
			t.Fatalf("record %d: %v %v", i, created, err)
		}
	}
	for i := 0; i < 10; i++ {
		job, err := store.ReserveForward(now.Add(time.Minute), testForwardTo, testOwn)
		if err != nil || job == nil || job.Segments != 1 {
			t.Fatalf("job %d: %+v %v", i, job, err)
		}
		if i != 0 {
			if err := store.FinishForward(job.ID, true); err != nil {
				t.Fatal(err)
			}
			if i == 1 {
				if ok, err := store.Delete(job.ID); err != nil || !ok {
					t.Fatalf("delete sent message: %v %v", ok, err)
				}
			}
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RecoverForward(); err != nil {
		t.Fatal(err)
	}
	job, err := store.ReserveForward(now.Add(time.Hour), testForwardTo, testOwn)
	if err != nil || job != nil {
		t.Fatalf("budget allowed job: %+v %v", job, err)
	}
	var unknown int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM forward_jobs WHERE state='unknown'`).Scan(&unknown); err != nil || unknown != 1 {
		t.Fatalf("unknown=%d, %v", unknown, err)
	}
}

func TestForwardCancelAndExpire(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "sms.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	msg := Message{Number: testSender, Text: "test", Timestamp: now}
	if _, _, err := store.RecordWithForward(msg, now, testForwardTo); err != nil {
		t.Fatal(err)
	}
	if err := store.CancelForwardExcept("off"); err != nil {
		t.Fatal(err)
	}
	job, err := store.ReserveForward(now, testForwardTo, testOwn)
	if err != nil || job != nil {
		t.Fatalf("cancelled job reserved: %+v %v", job, err)
	}
	if _, _, err := store.RecordWithForward(msg, now, testForwardTo); err != nil {
		t.Fatal(err)
	}
	job, err = store.ReserveForward(now.Add(24*time.Hour), testForwardTo, testOwn)
	if err != nil || job != nil {
		t.Fatalf("expired job reserved: %+v %v", job, err)
	}
}
