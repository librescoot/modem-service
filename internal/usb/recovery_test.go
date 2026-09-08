package usb

import (
	"errors"
	"syscall"
	"testing"
)

type fakeControlFile struct {
	value   string
	syncErr error
	closed  bool
}

func (f *fakeControlFile) WriteString(value string) (int, error) {
	f.value = value
	return len(value), nil
}
func (f *fakeControlFile) Sync() error { return f.syncErr }
func (f *fakeControlFile) Close() error {
	f.closed = true
	return nil
}

func TestWriteControlAcceptsUnsupportedSync(t *testing.T) {
	oldOpen := openControlFile
	defer func() { openControlFile = oldOpen }()

	fake := &fakeControlFile{syncErr: syscall.EINVAL}
	openControlFile = func(string) (controlFile, error) { return fake, nil }
	if err := writeControl("/sys/test", "1-1"); err != nil {
		t.Fatalf("writeControl: %v", err)
	}
	if fake.value != "1-1" || !fake.closed {
		t.Fatalf("control write incomplete: %+v", fake)
	}
}

func TestWriteControlRejectsRealSyncFailure(t *testing.T) {
	oldOpen := openControlFile
	defer func() { openControlFile = oldOpen }()

	fake := &fakeControlFile{syncErr: syscall.EIO}
	openControlFile = func(string) (controlFile, error) { return fake, nil }
	if err := writeControl("/sys/test", "1-1"); err == nil {
		t.Fatal("writeControl accepted EIO")
	}
}

func TestErrDeviceNotPresentIsSentinel(t *testing.T) {
	if !errors.Is(ErrDeviceNotPresent, ErrDeviceNotPresent) {
		t.Fatal("ErrDeviceNotPresent does not match itself")
	}
	wrapped := errors.Join(errors.New("outer"), ErrDeviceNotPresent)
	if !errors.Is(wrapped, ErrDeviceNotPresent) {
		t.Fatal("wrapped ErrDeviceNotPresent should still match")
	}
}
