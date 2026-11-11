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

func TestValidForwardDestination(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"off", true}, {"+4915112345678", true}, {"+12345678", true}, {"+19009099099", true},
		{"+123456789012345", true}, {"", false}, {"123456789", false}, {"+012345678", false},
		{"+1234567", false}, {"+1234567890123456", false}, {"+49123 45678", false}, {"+49123x5678", false},
	} {
		if got := ValidForwardDestination(tc.value); got != tc.valid {
			t.Errorf("ValidForwardDestination(%q) = %v, want %v", tc.value, got, tc.valid)
		}
	}
}

func TestForwardText(t *testing.T) {
	msg := Message{Number: "+4917612345678", Text: "test"}
	body, n, ok := ForwardText(msg, "+4915112345678")
	if !ok || n != 1 || !strings.Contains(body, msg.Number) {
		t.Fatalf("forward %q %d %v", body, n, ok)
	}
	for _, dest := range []string{"off", "123", msg.Number, "+012345678"} {
		if _, _, ok := ForwardText(msg, dest); ok {
			t.Fatalf("accepted %q", dest)
		}
	}
	msg.Text = forwardMarker + " test"
	if _, _, ok := ForwardText(msg, "+4915112345678"); ok {
		t.Fatal("forwarded already-forwarded message")
	}
}
