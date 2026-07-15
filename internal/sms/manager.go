// Package sms sends and receives SMS messages through ModemManager. It owns
// the create/send/delete dance for outbound messages and the read/delete dance
// for inbound ones, deleting every message from the modem as soon as it has
// been handled so the modem's small message store (typically a few dozen slots
// on SIMCom hardware) never fills up.
//
// The manager is deliberately I/O-narrow: it talks to ModemManager only through
// the MessagingDBus interface (satisfied by mm.Client), so it can be unit
// tested with a recorder. Service-level concerns — Redis publishing, resolving
// the modem path, arming the Added-signal watch — live in internal/service,
// mirroring how internal/sim splits its decision logic from the wiring. See the
// design doc at docs/superpowers/specs/2026-06-15-sms-support-design.md.
package sms

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"modem-service/internal/mm"
)

// MessagingDBus is the narrow ModemManager surface the manager needs. mm.Client
// satisfies it; tests substitute a recorder.
type MessagingDBus interface {
	ListSMS(modemPath dbus.ObjectPath) ([]dbus.ObjectPath, error)
	CreateSMS(modemPath dbus.ObjectPath, number, text string) (dbus.ObjectPath, error)
	DeleteSMS(modemPath, smsPath dbus.ObjectPath) error
	SendSMS(smsPath dbus.ObjectPath) error
	GetSMSProperties(smsPath dbus.ObjectPath) (mm.SMSProperties, error)
}

// SendRequest is the JSON payload accepted on the scooter:sms command queue.
type SendRequest struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

// Message is a parsed inbound SMS handed back to the service for publication.
type Message struct {
	Number    string    // sender's number
	Text      string    // message body
	Timestamp time.Time // network timestamp, or receive time if unavailable
}

// Outcome is the result of a Send. The service maps it to the sms.state field.
type Outcome string

const (
	OutcomeOK      Outcome = "ok"
	OutcomeError   Outcome = "error"
	OutcomeNoModem Outcome = "no-modem"
)

// Manager performs SMS send/receive over a MessagingDBus.
type Manager struct {
	dbus   MessagingDBus
	logger *log.Logger

	// mu serializes Send with the drain/handle paths so they never race on the
	// modem's shared message store. Because Send holds mu for its whole
	// create→send→delete, any outbound object a drain encounters while holding
	// mu is a leftover (a previous send whose delete failed) and is therefore
	// safe to clean up.
	mu sync.Mutex

	// deliveredUndeleted tracks inbound messages that were handed to the
	// service but whose delete failed. Guarded by mu. Later drains only retry
	// the delete for these instead of re-publishing the message. Keyed by
	// object path with the message identity as the value, so a path that gets
	// reused for a different message (e.g. after a ModemManager restart) is
	// still delivered.
	deliveredUndeleted map[dbus.ObjectPath]messageIdentity
}

// messageIdentity distinguishes "same SMS object still lingering" from "a new
// message under a reused D-Bus path".
type messageIdentity struct {
	number, text, timestamp string
}

// New returns a Manager bound to the given D-Bus surface and logger.
func New(d MessagingDBus, logger *log.Logger) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	return &Manager{
		dbus:               d,
		logger:             logger,
		deliveredUndeleted: make(map[dbus.ObjectPath]messageIdentity),
	}
}

// Send creates an outbound SMS, transmits it, and deletes the object afterwards
// (whether or not the send succeeded) so it doesn't linger in modem storage.
func (m *Manager) Send(modemPath dbus.ObjectPath, req SendRequest) (Outcome, error) {
	if modemPath == "" {
		return OutcomeNoModem, fmt.Errorf("no modem available")
	}
	if req.To == "" {
		return OutcomeError, fmt.Errorf("empty recipient")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	smsPath, err := m.dbus.CreateSMS(modemPath, req.To, req.Text)
	if err != nil {
		m.logger.Printf("sms: create failed: %v", err)
		return OutcomeError, fmt.Errorf("create sms: %w", err)
	}

	// Always clean up the created object — a sent message left in storage
	// counts against the modem's slot limit just like a received one. The
	// deferred delete runs before mu is released, so a concurrent drain can't
	// observe this object mid-send.
	defer func() {
		if err := m.dbus.DeleteSMS(modemPath, smsPath); err != nil {
			m.logger.Printf("sms: delete after send failed for %s: %v", smsPath, err)
		}
	}()

	if err := m.dbus.SendSMS(smsPath); err != nil {
		m.logger.Printf("sms: send to %s failed: %v", req.To, err)
		return OutcomeError, fmt.Errorf("send sms: %w", err)
	}

	m.logger.Printf("sms: sent to %s (%d chars)", req.To, len(req.Text))
	return OutcomeOK, nil
}

// HandleAdded processes a single SMS object reported by a Messaging.Added
// signal, returning the parsed message if it was inbound (and deleting it) or
// nil otherwise. Processing the signal's path directly (rather than only
// re-listing storage) matters because some modems deliver a received message as
// a transient object that never appears in Messaging.List().
func (m *Manager) HandleAdded(modemPath, smsPath dbus.ObjectPath) *Message {
	if modemPath == "" || smsPath == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.processOne(modemPath, smsPath)
}

// DrainReceived processes every message currently in modem storage: inbound
// messages are captured, deleted, and returned; status reports and stale
// outbound objects are deleted; multipart messages still assembling are left
// for a later drain. Deleting handled messages keeps the modem's small store
// from filling up.
//
// It is safe to call at startup (to pick up messages received while offline)
// and on a periodic tick (a reliable fallback for modems that don't emit
// Added for stored messages).
func (m *Manager) DrainReceived(modemPath dbus.ObjectPath) ([]*Message, error) {
	if modemPath == "" {
		return nil, fmt.Errorf("no modem available")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	paths, err := m.dbus.ListSMS(modemPath)
	if err != nil {
		return nil, fmt.Errorf("list sms: %w", err)
	}
	if len(paths) > 0 {
		m.logger.Printf("sms: drain found %d message(s) in modem storage", len(paths))
	}

	var msgs []*Message
	for _, p := range paths {
		if msg := m.processOne(modemPath, p); msg != nil {
			msgs = append(msgs, msg)
		}
	}
	return msgs, nil
}

// processOne classifies a single SMS object and acts on it. It assumes m.mu is
// held. Classification is by elimination so that received messages a modem
// reports oddly (state "stored", no PduType) are still treated as inbound:
//
//   - assembling (state=receiving)        → left for a later drain
//   - status report                       → deleted (delivery report, nothing to publish)
//   - outbound (SUBMIT / sending / sent)  → deleted as a stale leftover (mu means
//     no send is in flight, so it can't be a live one)
//   - everything else                     → treated as inbound: deleted and returned
//
// An inbound message is delivered even when its delete fails: some modems
// report a delete error but drop the object anyway (e.g. when the QMI WMS and
// AT steps disagree), and skipping delivery there would silently lose mail.
// To keep that from turning into a duplicate on every later drain while the
// object lingers, delivered-but-undeleted objects are remembered and only
// have their delete retried.
func (m *Manager) processOne(modemPath, smsPath dbus.ObjectPath) *Message {
	props, err := m.dbus.GetSMSProperties(smsPath)
	if err != nil {
		m.logger.Printf("sms: reading %s failed: %v", smsPath, err)
		return nil
	}

	switch {
	case props.State == mm.MMSmsStateReceiving:
		m.logger.Printf("sms: %s still assembling (state=receiving), leaving for later", smsPath)
		return nil

	case props.PduType == mm.MMSmsPduTypeStatusReport:
		if err := m.dbus.DeleteSMS(modemPath, smsPath); err != nil {
			m.logger.Printf("sms: delete of status-report %s failed: %v", smsPath, err)
		} else {
			m.logger.Printf("sms: deleted status-report %s", smsPath)
		}
		return nil

	case props.PduType == mm.MMSmsPduTypeSubmit ||
		props.State == mm.MMSmsStateSending ||
		props.State == mm.MMSmsStateSent:
		// Stale outbound: a previous send whose delete didn't take. Safe to
		// remove because holding mu means no send is in flight.
		if err := m.dbus.DeleteSMS(modemPath, smsPath); err != nil {
			m.logger.Printf("sms: delete of stale outbound %s failed: %v", smsPath, err)
		} else {
			m.logger.Printf("sms: deleted stale outbound %s (pdu=%s state=%s)",
				smsPath, mm.SmsPduTypeToString(props.PduType), mm.SmsStateToString(props.State))
		}
		return nil

	default:
		identity := messageIdentity{number: props.Number, text: props.Text, timestamp: props.Timestamp}
		if m.deliveredUndeleted[smsPath] == identity {
			// Already published in an earlier round; the object just wouldn't
			// delete. Retry the delete, never re-deliver.
			if err := m.dbus.DeleteSMS(modemPath, smsPath); err != nil {
				m.logger.Printf("sms: delete retry for already-delivered %s failed: %v", smsPath, err)
			} else {
				delete(m.deliveredUndeleted, smsPath)
				m.logger.Printf("sms: delete retry for already-delivered %s succeeded", smsPath)
			}
			return nil
		}

		msg := &Message{
			Number:    props.Number,
			Text:      props.Text,
			Timestamp: parseTimestamp(props.Timestamp),
		}
		m.logger.Printf("sms: received from %s (%d chars, pdu=%s state=%s)",
			msg.Number, len(msg.Text), mm.SmsPduTypeToString(props.PduType), mm.SmsStateToString(props.State))
		if err := m.dbus.DeleteSMS(modemPath, smsPath); err != nil {
			// MM sometimes reports a delete error but cleans up the object anyway
			// (e.g. when QMI WMS and AT steps disagree). Deliver regardless so the
			// message is never silently lost, and remember it so later drains only
			// retry the delete instead of publishing a duplicate.
			m.logger.Printf("sms: delete of %s failed (delivering anyway): %v", smsPath, err)
			m.deliveredUndeleted[smsPath] = identity
		} else {
			delete(m.deliveredUndeleted, smsPath)
		}
		return msg
	}
}

// parseTimestamp converts ModemManager's ISO 8601 SMS timestamp into a
// time.Time, falling back to the current time when it's absent or unparseable
// (not every modem populates it for every message).
func parseTimestamp(s string) time.Time {
	if s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return time.Now()
}
