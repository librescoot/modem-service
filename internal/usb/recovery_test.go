package usb

import (
	"errors"
	"testing"
)

func TestErrDeviceNotPresentIsSentinel(t *testing.T) {
	if !errors.Is(ErrDeviceNotPresent, ErrDeviceNotPresent) {
		t.Fatal("ErrDeviceNotPresent does not match itself")
	}
	wrapped := errors.Join(errors.New("outer"), ErrDeviceNotPresent)
	if !errors.Is(wrapped, ErrDeviceNotPresent) {
		t.Fatal("wrapped ErrDeviceNotPresent should still match")
	}
}
