package service

import (
	"context"
	"time"

	"modem-service/internal/mm"
	"modem-service/internal/sms"
)

func (s *Service) forwardLoop(ctx context.Context) {
	defer close(s.forwardDone)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		s.forwardOne(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) forwardOne(ctx context.Context) {
	if ctx.Err() != nil || !s.modemEnabled.Load() || !s.smsSIMPresent.Load() {
		return
	}
	s.forwardMu.Lock()
	defer s.forwardMu.Unlock()
	destination, _ := s.forwardDestination.Load().(string)
	if destination == "" || destination == "off" {
		return
	}
	path, err := s.Modem.FindModem()
	if err != nil {
		return
	}
	registration, err := s.MMClient.GetProperty(path, mm.Modem3gppInterface, "RegistrationState")
	if err != nil {
		return
	}
	state, ok := registration.Value().(uint32)
	if !ok || state != mm.MMModem3gppRegistrationStateHome {
		return
	}
	job, err := s.inbox.ReserveForward(time.Now(), destination, s.ownNumber())
	if err != nil {
		s.Logger.Printf("sms: forward reservation failed: %v", err)
		return
	}
	if job == nil {
		return
	}
	if err := s.publishInbox(); err != nil {
		s.Logger.Printf("sms: forward status projection failed: %v", err)
	}
	if ctx.Err() != nil {
		_ = s.inbox.FinishForward(job.ID, false)
		return
	}
	req := sms.SendRequest{ID: "forward-" + job.ID, To: job.To, Text: job.Text}
	outcome, sendErr := s.SMS.Send(path, req)
	if err := s.inbox.FinishForward(job.ID, outcome == sms.OutcomeOK); err != nil {
		s.Logger.Printf("sms: recording forward outcome failed: %v", err)
	}
	if err := s.publishInbox(); err != nil {
		s.Logger.Printf("sms: forward status projection failed: %v", err)
	}
	if sendErr != nil {
		s.Logger.Printf("sms: forward outcome unknown: %v", sendErr)
		s.recordSendResult(req, "forward outcome unknown")
		return
	}
	s.touchCSActivity()
	s.recordSendResult(req, "")
}
