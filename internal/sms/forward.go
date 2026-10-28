package sms

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/nyaruka/phonenumbers"
)

var e164 = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

const forwardMarker = "[Librescoot SMS]"

func ValidForwardDestination(value string) bool {
	if value == "off" {
		return true
	}
	if !e164.MatchString(value) {
		return false
	}
	number, err := phonenumbers.Parse(value, "")
	if err != nil || !phonenumbers.IsValidNumber(number) {
		return false
	}
	switch phonenumbers.GetNumberType(number) {
	case phonenumbers.MOBILE, phonenumbers.FIXED_LINE, phonenumbers.FIXED_LINE_OR_MOBILE:
		return true
	default:
		return false
	}
}

// ForwardText uses a marker to avoid forwarding our own forwarded messages.
func ForwardText(message Message, destination string) (string, int, bool) {
	if !ValidForwardDestination(destination) || destination == "off" || message.Number == destination || strings.Contains(message.Text, forwardMarker) {
		return "", 0, false
	}
	body := fmt.Sprintf("%s From: %s\n%s\n%s", forwardMarker, message.Number, message.Timestamp.Format(time.RFC3339), message.Text)
	segments := SMSSegments(body)
	if segments > 10 {
		return "", 0, false
	}
	return body, segments, true
}

// SMSSegments returns the number of GSM-7 or UCS-2 SMS parts, accounting for
// concatenation headers. Extension-table GSM characters consume two septets.
func SMSSegments(text string) int {
	const basic = "@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà"
	const extension = "\f^{}\\[~]|€"
	units := 0
	gsm := true
	for _, r := range text {
		switch {
		case strings.ContainsRune(basic, r):
			units++
		case strings.ContainsRune(extension, r):
			units += 2
		default:
			gsm = false
		}
	}
	if gsm {
		if units <= 160 {
			return 1
		}
		return (units + 152) / 153
	}
	units = len(utf16.Encode([]rune(text)))
	if units <= 70 {
		return 1
	}
	return (units + 66) / 67
}
