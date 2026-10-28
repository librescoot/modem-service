package sms

import (
	"strings"
	"testing"
)

func TestSMSSegments(t *testing.T) {
	for _, tt := range []struct {
		text string
		want int
	}{
		{strings.Repeat("a", 160), 1}, {strings.Repeat("a", 161), 2},
		{strings.Repeat("^", 80), 1}, {strings.Repeat("^", 81), 2},
		{strings.Repeat("漢", 70), 1}, {strings.Repeat("漢", 71), 2},
		{strings.Repeat("😀", 35), 1}, {strings.Repeat("😀", 36), 2},
	} {
		if got := SMSSegments(tt.text); got != tt.want {
			t.Errorf("length %d: got %d want %d", len(tt.text), got, tt.want)
		}
	}
}

func TestForwardText(t *testing.T) {
	msg := Message{Number: "+4917612345678", Text: "test"}
	body, n, ok := ForwardText(msg, "+4915112345678")
	if !ok || n != 1 || !strings.Contains(body, msg.Number) {
		t.Fatalf("forward %q %d %v", body, n, ok)
	}
	for _, dest := range []string{"off", "123", msg.Number, "+19009099099"} {
		if _, _, ok := ForwardText(msg, dest); ok {
			t.Fatalf("accepted %q", dest)
		}
	}
	msg.Text = forwardMarker + " test"
	if _, _, ok := ForwardText(msg, "+4915112345678"); ok {
		t.Fatal("forwarded already-forwarded message")
	}
}
