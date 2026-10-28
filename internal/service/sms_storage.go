package service

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"modem-service/internal/mm"
)

type smsStorageClient interface {
	SendCommand(dbus.ObjectPath, string, time.Duration) (string, error)
	GetProperty(dbus.ObjectPath, string, string) (dbus.Variant, error)
	SetSMSDefaultStorage(dbus.ObjectPath, uint32) error
}

type smsStorage struct {
	name           string
	used, capacity int
}

var cpmsStoragePattern = regexp.MustCompile(`"(SM|ME|MT|SR)"\s*,\s*(\d+)\s*,\s*(\d+)`)
var cpmsCountPattern = regexp.MustCompile(`\+CPMS:\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)`)

func parseSMSStorage(response string) ([]smsStorage, bool) {
	matches := cpmsStoragePattern.FindAllStringSubmatch(response, -1)
	if len(matches) != 3 {
		return nil, false
	}
	result := make([]smsStorage, 3)
	for i, match := range matches {
		used, err1 := strconv.Atoi(match[2])
		capacity, err2 := strconv.Atoi(match[3])
		if err1 != nil || err2 != nil || used > capacity {
			return nil, false
		}
		result[i] = smsStorage{match[1], used, capacity}
	}
	return result, true
}

func cpmsCommand(read, write, receive string) string {
	return fmt.Sprintf("AT+CPMS=\"%s\",\"%s\",\"%s\"", read, write, receive)
}

// recoverFullSMSStorage changes stores only when the active receive store is
// full and the other ModemManager-supported store has free space. It never
// deletes stored messages.
func recoverFullSMSStorage(client smsStorageClient, path dbus.ObjectPath, logger *log.Logger) {
	response, err := client.SendCommand(path, "AT+CPMS?", 5*time.Second)
	if err != nil {
		return
	}
	stores, ok := parseSMSStorage(response)
	if !ok || stores[2].capacity == 0 || stores[2].used < stores[2].capacity {
		return
	}
	alternate, storageID := "SM", uint32(1)
	if stores[2].name == "SM" {
		alternate, storageID = "ME", 2
	} else if stores[2].name != "ME" {
		return
	}
	supported, err := client.GetProperty(path, mm.ModemMessagingInterface, "SupportedStorages")
	if err != nil {
		return
	}
	ids, ok := supported.Value().([]uint32)
	if !ok {
		return
	}
	found := false
	for _, id := range ids {
		if id == storageID {
			found = true
		}
	}
	if !found {
		return
	}
	previous, err := client.GetProperty(path, mm.ModemMessagingInterface, "DefaultStorage")
	if err != nil {
		return
	}
	previousID, ok := previous.Value().(uint32)
	if !ok {
		return
	}
	restore := func() {
		_, restoreErr := client.SendCommand(path, cpmsCommand(stores[0].name, stores[1].name, stores[2].name), 5*time.Second)
		if restoreErr != nil {
			logger.Printf("sms: failed to restore storage selection: %v", restoreErr)
		}
	}
	probe, err := client.SendCommand(path, cpmsCommand(alternate, stores[1].name, stores[2].name), 5*time.Second)
	if err != nil {
		restore()
		return
	}
	counts := cpmsCountPattern.FindStringSubmatch(strings.TrimSpace(probe))
	if len(counts) != 7 {
		restore()
		return
	}
	used, err1 := strconv.Atoi(counts[1])
	capacity, err2 := strconv.Atoi(counts[2])
	if err1 != nil || err2 != nil || capacity <= 0 || used >= capacity {
		restore()
		return
	}
	if err = client.SetSMSDefaultStorage(path, storageID); err != nil {
		_ = client.SetSMSDefaultStorage(path, previousID)
		restore()
		return
	}
	if _, err = client.SendCommand(path, cpmsCommand(alternate, alternate, alternate), 5*time.Second); err != nil {
		_ = client.SetSMSDefaultStorage(path, previousID)
		restore()
		return
	}
	logger.Printf("sms: receive storage full; switched to %s", alternate)
}
