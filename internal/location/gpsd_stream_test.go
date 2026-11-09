package location

import (
	"bufio"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stratoberry/go-gpsd"
)

func TestReportClass(t *testing.T) {
	for line, want := range map[string]string{
		`{"class":"TPV","device":"/dev/x","mode":3}`:   "TPV",
		`{"device":"x","class":"SKY","satellites":[]}`: "SKY",
		`{"class":"WATCH","enable":true}`:              "WATCH",
		`{"nothing":1}`:                                "",
		`{"class":"unterminated`:                       "",
	} {
		if got := reportClass([]byte(line)); got != want {
			t.Errorf("reportClass(%s) = %q, want %q", line, got, want)
		}
	}
}

// fakeGPSD sends the greeting and, once the client has asked to watch, a burst
// of reports, then holds the connection open.
func fakeGPSD(t *testing.T, tpv, sky int) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintln(conn, `{"class":"VERSION","release":"3.25"}`)
		buf := make([]byte, 256)
		_, _ = bufio.NewReader(conn).Read(buf) // the ?WATCH command
		for i := 0; i < tpv; i++ {
			fmt.Fprintf(conn, "{\"class\":\"TPV\",\"mode\":3,\"lat\":52.5,\"lon\":13.4,\"speed\":%d}\n", i)
		}
		for i := 0; i < sky; i++ {
			fmt.Fprintf(conn, "{\"class\":\"SKY\",\"satellites\":[{\"PRN\":%d,\"used\":true,\"ss\":40.5}]}\n", i)
		}
		fmt.Fprintln(conn, `{"class":"DEVICES","devices":[]}`)
		time.Sleep(2 * time.Second)
	}()
	return ln.Addr().String()
}

type streamCounts struct{ tpv, sky, skipped atomic.Int32 }

func runStream(t *testing.T, addr string, interval time.Duration) *streamCounts {
	t.Helper()
	var c streamCounts
	s, err := dialGPSD(addr, func() time.Duration { return interval }, func(string) { c.skipped.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	s.AddFilter("TPV", func(r interface{}) {
		if rep, ok := r.(*gpsd.TPVReport); !ok || rep.Mode != 3 || rep.Lat != 52.5 {
			t.Errorf("bad TPV report %#v", r)
		}
		c.tpv.Add(1)
	})
	s.AddFilter("SKY", func(r interface{}) {
		if rep, ok := r.(*gpsd.SKYReport); !ok || len(rep.Satellites) != 1 || !rep.Satellites[0].Used {
			t.Errorf("bad SKY report %#v", r)
		}
		c.sky.Add(1)
	})
	done := s.Watch()
	time.Sleep(400 * time.Millisecond)
	_ = s.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not stop after Close")
	}
	return &c
}

func TestEveryReportIsDecodedWithoutAnInterval(t *testing.T) {
	c := runStream(t, fakeGPSD(t, 5, 5), 0)
	if c.tpv.Load() != 5 || c.sky.Load() != 5 || c.skipped.Load() != 0 {
		t.Fatalf("tpv=%d sky=%d skipped=%d, want 5/5/0", c.tpv.Load(), c.sky.Load(), c.skipped.Load())
	}
}

func TestSKYReportsInsideTheIntervalAreSkippedButCounted(t *testing.T) {
	c := runStream(t, fakeGPSD(t, 5, 5), time.Hour)
	if c.tpv.Load() != 5 || c.sky.Load() != 1 {
		t.Fatalf("tpv=%d sky=%d, want every TPV and one SKY decoded", c.tpv.Load(), c.sky.Load())
	}
	if c.skipped.Load() != 4 {
		t.Fatalf("skipped=%d, want the other 4 SKY reports counted as activity", c.skipped.Load())
	}
}
