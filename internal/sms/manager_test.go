package sms

import (
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"modem-service/internal/mm"
)

const testModemPath dbus.ObjectPath = "/org/freedesktop/ModemManager1/Modem/0"

// fakeDBus records calls and returns canned data/errors.
type fakeDBus struct {
	// Send side
	createPath dbus.ObjectPath
	createErr  error
	sendErr    error

	// Receive side
	listPaths []dbus.ObjectPath
	listErr   error
	props     map[dbus.ObjectPath]mm.SMSProperties
	propsErr  map[dbus.ObjectPath]error

	// Per-path delete errors (absent entry == success)
	deleteErr map[dbus.ObjectPath]error

	// Recorded calls
	createCalls      int
	sendCalls        int
	deleted          []dbus.ObjectPath
	lastCreateNumber string
	lastCreateText   string
}

func (f *fakeDBus) ListSMS(_ dbus.ObjectPath) ([]dbus.ObjectPath, error) {
	return f.listPaths, f.listErr
}

func (f *fakeDBus) CreateSMS(_ dbus.ObjectPath, number, text string) (dbus.ObjectPath, error) {
	f.createCalls++
	f.lastCreateNumber = number
	f.lastCreateText = text
	if f.createErr != nil {
		return "", f.createErr
	}
	return f.createPath, nil
}

func (f *fakeDBus) DeleteSMS(_ dbus.ObjectPath, smsPath dbus.ObjectPath) error {
	f.deleted = append(f.deleted, smsPath)
	if f.deleteErr != nil {
		return f.deleteErr[smsPath]
	}
	return nil
}

func (f *fakeDBus) SendSMS(_ dbus.ObjectPath) error {
	f.sendCalls++
	return f.sendErr
}

func (f *fakeDBus) GetSMSProperties(smsPath dbus.ObjectPath) (mm.SMSProperties, error) {
	if f.propsErr != nil {
		if err := f.propsErr[smsPath]; err != nil {
			return mm.SMSProperties{}, err
		}
	}
	return f.props[smsPath], nil
}

func newManager(d *fakeDBus) *Manager {
	return &Manager{
		dbus:   d,
		logger: log.New(io.Discard, "", 0),
	}
}

// --- Send -------------------------------------------------------------------

func TestSend_HappyPath(t *testing.T) {
	d := &fakeDBus{createPath: "/sms/1"}
	m := newManager(d)

	out, err := m.Send(testModemPath, SendRequest{To: "+4915112345678", Text: "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != OutcomeOK {
		t.Fatalf("got %q, want %q", out, OutcomeOK)
	}
	if d.createCalls != 1 || d.sendCalls != 1 {
		t.Fatalf("expected 1 create + 1 send, got create=%d send=%d", d.createCalls, d.sendCalls)
	}
	if d.lastCreateNumber != "+4915112345678" || d.lastCreateText != "hello" {
		t.Fatalf("create got number=%q text=%q", d.lastCreateNumber, d.lastCreateText)
	}
	if len(d.deleted) != 1 || d.deleted[0] != "/sms/1" {
		t.Fatalf("expected /sms/1 deleted after send, got %v", d.deleted)
	}
}

func TestSend_NoModem(t *testing.T) {
	d := &fakeDBus{}
	m := newManager(d)

	out, err := m.Send("", SendRequest{To: "+49", Text: "x"})
	if out != OutcomeNoModem {
		t.Fatalf("got %q, want %q", out, OutcomeNoModem)
	}
	if err == nil {
		t.Fatalf("expected error")
	}
	if d.createCalls != 0 || d.sendCalls != 0 || len(d.deleted) != 0 {
		t.Fatalf("must not touch D-Bus without a modem")
	}
}

func TestSend_EmptyRecipient(t *testing.T) {
	d := &fakeDBus{}
	m := newManager(d)

	out, _ := m.Send(testModemPath, SendRequest{To: "", Text: "x"})
	if out != OutcomeError {
		t.Fatalf("got %q, want %q", out, OutcomeError)
	}
	if d.createCalls != 0 {
		t.Fatalf("must not create with empty recipient")
	}
}

func TestSend_CreateFails(t *testing.T) {
	d := &fakeDBus{createErr: errors.New("bus error")}
	m := newManager(d)

	out, err := m.Send(testModemPath, SendRequest{To: "+49", Text: "x"})
	if out != OutcomeError || err == nil {
		t.Fatalf("got %q err=%v, want error outcome", out, err)
	}
	if d.sendCalls != 0 {
		t.Fatalf("must not send when create failed")
	}
	if len(d.deleted) != 0 {
		t.Fatalf("nothing to delete when create failed, got %v", d.deleted)
	}
}

func TestSend_SendFailsStillDeletes(t *testing.T) {
	d := &fakeDBus{createPath: "/sms/1", sendErr: errors.New("send failed")}
	m := newManager(d)

	out, err := m.Send(testModemPath, SendRequest{To: "+49", Text: "x"})
	if out != OutcomeError || err == nil {
		t.Fatalf("got %q err=%v, want error outcome", out, err)
	}
	if len(d.deleted) != 1 || d.deleted[0] != "/sms/1" {
		t.Fatalf("created object must be cleaned up even on send failure, got %v", d.deleted)
	}
}

// --- DrainReceived ----------------------------------------------------------

func TestDrain_IncomingReceived(t *testing.T) {
	d := &fakeDBus{
		listPaths: []dbus.ObjectPath{"/sms/1"},
		props: map[dbus.ObjectPath]mm.SMSProperties{
			"/sms/1": {Number: "+4930", Text: "ping", State: mm.MMSmsStateReceived, PduType: mm.MMSmsPduTypeDeliver},
		},
	}
	m := newManager(d)

	msgs, err := m.DrainReceived(testModemPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Number != "+4930" || msgs[0].Text != "ping" {
		t.Fatalf("got number=%q text=%q", msgs[0].Number, msgs[0].Text)
	}
	if len(d.deleted) != 1 || d.deleted[0] != "/sms/1" {
		t.Fatalf("received message must be deleted, got %v", d.deleted)
	}
}

func TestDrain_DeletesStaleOutbound(t *testing.T) {
	// A leftover sent message (its post-send delete didn't take) must be
	// cleaned up so it doesn't occupy a slot forever.
	d := &fakeDBus{
		listPaths: []dbus.ObjectPath{"/sms/1"},
		props: map[dbus.ObjectPath]mm.SMSProperties{
			"/sms/1": {Number: "+4930", Text: "sent", State: mm.MMSmsStateSent, PduType: mm.MMSmsPduTypeSubmit},
		},
	}
	m := newManager(d)

	msgs, _ := m.DrainReceived(testModemPath)
	if len(msgs) != 0 {
		t.Fatalf("outbound message must not be delivered, got %d", len(msgs))
	}
	if len(d.deleted) != 1 || d.deleted[0] != "/sms/1" {
		t.Fatalf("stale outbound must be deleted, got %v", d.deleted)
	}
}

func TestDrain_SkipsIncomplete(t *testing.T) {
	d := &fakeDBus{
		listPaths: []dbus.ObjectPath{"/sms/1"},
		props: map[dbus.ObjectPath]mm.SMSProperties{
			"/sms/1": {Number: "+4930", Text: "part", State: mm.MMSmsStateReceiving, PduType: mm.MMSmsPduTypeDeliver},
		},
	}
	m := newManager(d)

	msgs, _ := m.DrainReceived(testModemPath)
	if len(msgs) != 0 {
		t.Fatalf("still-assembling message must be skipped, got %d", len(msgs))
	}
	if len(d.deleted) != 0 {
		t.Fatalf("incomplete message must not be deleted, got %v", d.deleted)
	}
}

func TestDrain_DeleteFailureDefersDelivery(t *testing.T) {
	d := &fakeDBus{
		listPaths: []dbus.ObjectPath{"/sms/1"},
		props: map[dbus.ObjectPath]mm.SMSProperties{
			"/sms/1": {Number: "+4930", Text: "ping", State: mm.MMSmsStateReceived, PduType: mm.MMSmsPduTypeDeliver},
		},
		deleteErr: map[dbus.ObjectPath]error{"/sms/1": errors.New("delete failed")},
	}
	m := newManager(d)

	msgs, _ := m.DrainReceived(testModemPath)
	if len(msgs) != 0 {
		t.Fatalf("must not deliver a message we couldn't delete, got %d", len(msgs))
	}
	if len(d.deleted) != 1 {
		t.Fatalf("expected one delete attempt, got %v", d.deleted)
	}
}

func TestDrain_MixedBatch(t *testing.T) {
	d := &fakeDBus{
		listPaths: []dbus.ObjectPath{"/sms/1", "/sms/2", "/sms/3"},
		props: map[dbus.ObjectPath]mm.SMSProperties{
			"/sms/1": {Number: "+1", Text: "in", State: mm.MMSmsStateReceived, PduType: mm.MMSmsPduTypeDeliver},
			"/sms/2": {Number: "+2", Text: "out", State: mm.MMSmsStateSent, PduType: mm.MMSmsPduTypeSubmit},
			"/sms/3": {Number: "+3", Text: "in2", State: mm.MMSmsStateReceived, PduType: mm.MMSmsPduTypeDeliver},
		},
	}
	m := newManager(d)

	msgs, _ := m.DrainReceived(testModemPath)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 inbound messages, got %d", len(msgs))
	}
	if msgs[0].Number != "+1" || msgs[1].Number != "+3" {
		t.Fatalf("unexpected order/content: %+v", msgs)
	}
	if len(d.deleted) != 3 {
		t.Fatalf("two inbound + one stale outbound should be deleted, got %v", d.deleted)
	}
}

func TestDrain_DeletesStatusReport(t *testing.T) {
	d := &fakeDBus{
		listPaths: []dbus.ObjectPath{"/sms/1"},
		props: map[dbus.ObjectPath]mm.SMSProperties{
			"/sms/1": {Number: "+4930", State: mm.MMSmsStateReceived, PduType: mm.MMSmsPduTypeStatusReport},
		},
	}
	m := newManager(d)

	msgs, _ := m.DrainReceived(testModemPath)
	if len(msgs) != 0 {
		t.Fatalf("status reports must not be delivered, got %d", len(msgs))
	}
	if len(d.deleted) != 1 || d.deleted[0] != "/sms/1" {
		t.Fatalf("status report must be deleted to free the slot, got %v", d.deleted)
	}
}

func TestDrain_IncomingStoredState(t *testing.T) {
	// Some modems report received messages read back from SIM storage with
	// state "stored" rather than "received"; they must still be delivered.
	d := &fakeDBus{
		listPaths: []dbus.ObjectPath{"/sms/1"},
		props: map[dbus.ObjectPath]mm.SMSProperties{
			"/sms/1": {Number: "+4930", Text: "stored msg", State: mm.MMSmsStateStored, PduType: mm.MMSmsPduTypeDeliver},
		},
	}
	m := newManager(d)

	msgs, _ := m.DrainReceived(testModemPath)
	if len(msgs) != 1 || msgs[0].Text != "stored msg" {
		t.Fatalf("DELIVER message in stored state must be delivered, got %+v", msgs)
	}
	if len(d.deleted) != 1 {
		t.Fatalf("delivered message must be deleted, got %v", d.deleted)
	}
}

func TestDrain_IncomingUnknownPduReceivedState(t *testing.T) {
	// PduType isn't always populated; state "received" alone marks it inbound.
	d := &fakeDBus{
		listPaths: []dbus.ObjectPath{"/sms/1"},
		props: map[dbus.ObjectPath]mm.SMSProperties{
			"/sms/1": {Number: "+4930", Text: "hi", State: mm.MMSmsStateReceived, PduType: mm.MMSmsPduTypeUnknown},
		},
	}
	m := newManager(d)

	msgs, _ := m.DrainReceived(testModemPath)
	if len(msgs) != 1 || msgs[0].Text != "hi" {
		t.Fatalf("received-state message must be delivered even with unknown PduType, got %+v", msgs)
	}
	if len(d.deleted) != 1 {
		t.Fatalf("delivered message must be deleted, got %v", d.deleted)
	}
}

func TestDrain_IncomingStoredUnknownPdu(t *testing.T) {
	// The hardware case Copilot flagged: a received message the modem reports
	// as state "stored" with no PduType. It must be delivered, not left behind.
	d := &fakeDBus{
		listPaths: []dbus.ObjectPath{"/sms/1"},
		props: map[dbus.ObjectPath]mm.SMSProperties{
			"/sms/1": {Number: "+4930", Text: "stored unknown", State: mm.MMSmsStateStored, PduType: mm.MMSmsPduTypeUnknown},
		},
	}
	m := newManager(d)

	msgs, _ := m.DrainReceived(testModemPath)
	if len(msgs) != 1 || msgs[0].Text != "stored unknown" {
		t.Fatalf("stored message with unknown PduType must be delivered, got %+v", msgs)
	}
	if len(d.deleted) != 1 {
		t.Fatalf("delivered message must be deleted, got %v", d.deleted)
	}
}

func TestDrain_StoredSubmitIsOutboundNotInbound(t *testing.T) {
	// A SUBMIT object must be classified as outbound (and cleaned up), never
	// delivered as inbound, even when its state is "stored".
	d := &fakeDBus{
		listPaths: []dbus.ObjectPath{"/sms/1"},
		props: map[dbus.ObjectPath]mm.SMSProperties{
			"/sms/1": {Number: "+4930", Text: "draft", State: mm.MMSmsStateStored, PduType: mm.MMSmsPduTypeSubmit},
		},
	}
	m := newManager(d)

	msgs, _ := m.DrainReceived(testModemPath)
	if len(msgs) != 0 {
		t.Fatalf("stored SUBMIT must not be delivered as inbound, got %d", len(msgs))
	}
	if len(d.deleted) != 1 {
		t.Fatalf("stored SUBMIT should be cleaned up, got %v", d.deleted)
	}
}

func TestDrain_NoModem(t *testing.T) {
	d := &fakeDBus{}
	m := newManager(d)

	if _, err := m.DrainReceived(""); err == nil {
		t.Fatalf("expected error without modem path")
	}
}

func TestParseTimestamp(t *testing.T) {
	want := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	if got := parseTimestamp("2026-01-02T15:04:05Z"); !got.Equal(want) {
		t.Fatalf("parseTimestamp got %v, want %v", got, want)
	}

	// Empty and unparseable values fall back to ~now.
	if got := parseTimestamp(""); time.Since(got) > time.Minute {
		t.Fatalf("empty timestamp should fall back to now, got %v", got)
	}
	if got := parseTimestamp("not-a-time"); time.Since(got) > time.Minute {
		t.Fatalf("garbage timestamp should fall back to now, got %v", got)
	}
}

// --- HandleAdded (signal path) ---------------------------------------------

func TestHandleAdded_DeliversInbound(t *testing.T) {
	d := &fakeDBus{
		props: map[dbus.ObjectPath]mm.SMSProperties{
			"/sms/9": {Number: "+4930", Text: "hi there", State: mm.MMSmsStateReceived, PduType: mm.MMSmsPduTypeDeliver},
		},
	}
	m := newManager(d)

	msg := m.HandleAdded(testModemPath, "/sms/9")
	if msg == nil || msg.Text != "hi there" || msg.Number != "+4930" {
		t.Fatalf("expected inbound message, got %+v", msg)
	}
	if len(d.deleted) != 1 || d.deleted[0] != "/sms/9" {
		t.Fatalf("handled message must be deleted, got %v", d.deleted)
	}
}

func TestHandleAdded_StoredUnknownPdu(t *testing.T) {
	// The signal path must also deliver a received message reported as state
	// "stored" with no PduType (the observed hardware case).
	d := &fakeDBus{
		props: map[dbus.ObjectPath]mm.SMSProperties{
			"/sms/9": {Number: "+4930", Text: "stored", State: mm.MMSmsStateStored, PduType: mm.MMSmsPduTypeUnknown},
		},
	}
	m := newManager(d)

	if msg := m.HandleAdded(testModemPath, "/sms/9"); msg == nil || msg.Text != "stored" {
		t.Fatalf("expected stored/unknown message to be delivered, got %+v", msg)
	}
}

func TestHandleAdded_StatusReportNotDelivered(t *testing.T) {
	d := &fakeDBus{
		props: map[dbus.ObjectPath]mm.SMSProperties{
			"/sms/9": {Number: "+4930", State: mm.MMSmsStateReceived, PduType: mm.MMSmsPduTypeStatusReport},
		},
	}
	m := newManager(d)

	if msg := m.HandleAdded(testModemPath, "/sms/9"); msg != nil {
		t.Fatalf("status report must not be delivered, got %+v", msg)
	}
	if len(d.deleted) != 1 {
		t.Fatalf("status report must be deleted, got %v", d.deleted)
	}
}

func TestHandleAdded_NoModemOrPath(t *testing.T) {
	m := newManager(&fakeDBus{})
	if msg := m.HandleAdded("", "/sms/1"); msg != nil {
		t.Fatalf("expected nil without modem path")
	}
	if msg := m.HandleAdded(testModemPath, ""); msg != nil {
		t.Fatalf("expected nil without sms path")
	}
}
