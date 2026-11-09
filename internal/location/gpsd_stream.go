package location

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/stratoberry/go-gpsd"
)

// gpsdSession is a trimmed replacement for go-gpsd's Session. The library
// decodes every line twice (once to find the class, once into the report) and
// does so for each report gpsd sends, about once a second per class. Here the
// class is read without decoding and a report is only decoded when its class is
// due, so a slow consumer pays for one decode per interval instead of one per
// report. Skipped reports still count as receiver activity.
type gpsdSession struct {
	mu      sync.Mutex
	conn    net.Conn
	reader  *bufio.Reader
	filters map[string][]func(interface{})

	// interval returns the minimum time between decoded reports of one class;
	// zero decodes every report. It is read per report so a change applies at
	// once.
	interval func() time.Duration
	skipped  func(class string)

	lastDecoded map[string]time.Time
}

func dialGPSD(address string, interval func() time.Duration, skipped func(class string)) (*gpsdSession, error) {
	conn, err := net.Dial("tcp4", address)
	if err != nil {
		return nil, err
	}
	s := &gpsdSession{
		conn:        conn,
		reader:      bufio.NewReader(conn),
		filters:     make(map[string][]func(interface{})),
		interval:    interval,
		skipped:     skipped,
		lastDecoded: make(map[string]time.Time),
	}
	// gpsd greets with a VERSION line.
	_, _ = s.reader.ReadString('\n')
	return s, nil
}

func (s *gpsdSession) AddFilter(class string, f func(interface{})) {
	s.filters[class] = append(s.filters[class], f)
}

func (s *gpsdSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return errors.New("gpsd socket is already closed")
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}

// Watch starts streaming reports in a goroutine and returns a channel that
// receives once when the stream ends.
func (s *gpsdSession) Watch() chan bool {
	done := make(chan bool)
	s.mu.Lock()
	if s.conn != nil {
		fmt.Fprintf(s.conn, "?WATCH={\"enable\":true,\"json\":true}")
	}
	s.mu.Unlock()
	go s.run(done)
	return done
}

var classKey = []byte(`"class":"`)

// reportClass returns the class of a gpsd JSON line without decoding it.
func reportClass(line []byte) string {
	i := bytes.Index(line, classKey)
	if i < 0 {
		return ""
	}
	rest := line[i+len(classKey):]
	j := bytes.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}

// due reports whether a report of this class should be decoded now. Only SKY
// is thinned out: it carries the whole satellite list and is the expensive one
// to decode, while a TPV report is small and the GPS clock submission needs the
// receipt of a position report no more than two seconds old.
func (s *gpsdSession) due(class string, now time.Time) bool {
	if class != "SKY" {
		return true
	}
	var interval time.Duration
	if s.interval != nil {
		interval = s.interval()
	}
	last, seen := s.lastDecoded[class]
	if interval > 0 && seen && now.Sub(last) < interval {
		return false
	}
	s.lastDecoded[class] = now
	return true
}

func (s *gpsdSession) run(done chan bool) {
	for {
		line, err := s.reader.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				fmt.Println("Stream reader error (is gpsd running?):", err)
			}
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				break
			}
			continue
		}
		class := reportClass(line)
		if len(s.filters[class]) == 0 {
			continue
		}
		if !s.due(class, time.Now()) {
			if s.skipped != nil {
				s.skipped(class)
			}
			continue
		}
		var report interface{}
		switch class {
		case "TPV":
			var r *gpsd.TPVReport
			err = json.Unmarshal(line, &r)
			report = r
		case "SKY":
			var r *gpsd.SKYReport
			err = json.Unmarshal(line, &r)
			report = r
		default:
			continue
		}
		if err != nil {
			fmt.Println("JSON parsing error:", err)
			continue
		}
		for _, f := range s.filters[class] {
			f(report)
		}
	}
	done <- true
}
