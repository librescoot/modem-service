package service

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"modem-service/internal/sms"
)

type inboxSnapshot struct {
	Version   int                 `json:"version"`
	Unread    int                 `json:"unread"`
	Messages  []sms.StoredMessage `json:"messages"`
	ForwardTo string              `json:"forward-to"`
}

type inboxCommand struct {
	Action string `json:"action"`
	ID     string `json:"id"`
}

func (s *Service) publishInbox() error {
	if s.inbox == nil {
		return nil
	}
	s.inboxMu.Lock()
	defer s.inboxMu.Unlock()
	return s.publishInboxLocked()
}

func (s *Service) publishInboxLocked() error {
	messages, err := s.inbox.List(time.Now())
	if err != nil {
		return err
	}
	forward, _ := s.forwardDestination.Load().(string)
	if forward == "" {
		forward = "off"
	}
	snapshot := inboxSnapshot{Version: 1, Messages: messages, ForwardTo: forward}
	for _, message := range messages {
		if !message.Read {
			snapshot.Unread++
		}
	}
	s.unreadSMS.Store(int64(snapshot.Unread))
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err := s.Redis.PublishSMSInbox(string(payload)); err != nil {
		return err
	}
	return s.Redis.PublishSMSState("unread-count", fmt.Sprint(snapshot.Unread))
}

func (s *Service) handleInboxCommand(payload string) error {
	if len(payload) > 256 {
		return fmt.Errorf("inbox command too long")
	}
	var command inboxCommand
	if err := json.Unmarshal([]byte(payload), &command); err != nil {
		return fmt.Errorf("invalid inbox command")
	}
	if len(command.ID) != 32 {
		return fmt.Errorf("invalid message ID")
	}
	if _, err := hex.DecodeString(command.ID); err != nil {
		return fmt.Errorf("invalid message ID")
	}
	s.inboxMu.Lock()
	defer s.inboxMu.Unlock()
	var err error
	switch command.Action {
	case "read":
		_, err = s.inbox.MarkRead(command.ID)
	case "delete":
		_, err = s.inbox.Delete(command.ID)
	default:
		return fmt.Errorf("invalid inbox action")
	}
	if err != nil {
		return err
	}
	return s.publishInboxLocked()
}
