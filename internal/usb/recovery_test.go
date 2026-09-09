package usb

import (
	"errors"
	"syscall"
	"testing"
)

type fakeControlFile struct {
	value    string
	writeN   int
	writeErr error
	syncErr  error
	closeErr error
	closed   bool
}

func (f *fakeControlFile) WriteString(value string) (int, error) {
	f.value = value
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.writeN > 0 {
		return f.writeN, nil
	}
	return len(value), nil
}
func (f *fakeControlFile) Sync() error { return f.syncErr }
func (f *fakeControlFile) Close() error {
	f.closed = true
	return f.closeErr
}

func TestWriteControlAcceptsUnsupportedSync(t *testing.T) {
	for _, syncErr := range []error{syscall.EINVAL, syscall.ENOTSUP, syscall.EOPNOTSUPP} {
		t.Run(syncErr.Error(), func(t *testing.T) {
			oldOpen := openControlFile
			defer func() { openControlFile = oldOpen }()

			fake := &fakeControlFile{syncErr: syncErr}
			openControlFile = func(string) (controlFile, error) { return fake, nil }
			if err := writeControl("/sys/test", "1-1"); err != nil {
				t.Fatalf("writeControl: %v", err)
			}
			if fake.value != "1-1" || !fake.closed {
				t.Fatalf("control write incomplete: %+v", fake)
			}
		})
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
	if !fake.closed {
		t.Fatal("sync failure did not close control file")
	}
}

func TestWriteControlErrorPaths(t *testing.T) {
	tests := []struct {
		name string
		fake *fakeControlFile
	}{
		{"write", &fakeControlFile{writeErr: syscall.EIO}},
		{"short-write", &fakeControlFile{writeN: 1}},
		{"close", &fakeControlFile{closeErr: syscall.EIO}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oldOpen := openControlFile
			defer func() { openControlFile = oldOpen }()
			openControlFile = func(string) (controlFile, error) { return tc.fake, nil }
			if err := writeControl("/sys/test", "1-1"); err == nil {
				t.Fatal("writeControl accepted failure")
			}
			if !tc.fake.closed {
				t.Fatal("failure path did not close control file")
			}
		})
	}
}

func TestWriteControlOpenFailure(t *testing.T) {
	oldOpen := openControlFile
	defer func() { openControlFile = oldOpen }()
	openControlFile = func(string) (controlFile, error) { return nil, syscall.EACCES }
	if err := writeControl("/sys/test", "1-1"); err == nil {
		t.Fatal("writeControl accepted open failure")
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
