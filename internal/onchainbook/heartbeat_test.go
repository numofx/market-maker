package onchainbook

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestShouldBeatOnlyWhileTheBookIsQuoted(t *testing.T) {
	for _, c := range []struct {
		name   string
		d      Decision
		err    error
		dryRun bool
		want   bool
	}{
		{"an update landed", Decision{Action: ActionUpdate, Reason: "repriced"}, nil, false, true},
		{"live and unchanged", Decision{Action: ActionNone, Reason: "unchanged"}, nil, false, true},
		{"stopped on schedule before last trade", Decision{Action: ActionNone, Reason: "cutoff_before_last_trade"}, nil, false, true},
		{"the update failed", Decision{Action: ActionUpdate, Reason: "refresh"}, errors.New("rpc"), false, false},
		{"dry run", Decision{Action: ActionUpdate, Reason: "down"}, nil, true, false},
		{"taking it down", Decision{Action: ActionCancel, Reason: "index_unreadable"}, nil, false, false},
		{"held down: a feed is unreadable", Decision{Action: ActionNone, Reason: "forward_unreadable"}, nil, false, false},
		{"held down: index and reference diverge", Decision{Action: ActionNone, Reason: "index_reference_divergence"}, nil, false, false},
		{"superseded", Decision{}, ErrSuperseded, false, false},
	} {
		if got := ShouldBeat(c.d, c.err, c.dryRun); got != c.want {
			t.Errorf("%s: ShouldBeat = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHeartbeatPingsAtMostOncePerInterval(t *testing.T) {
	var pings atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { pings.Add(1) }))
	defer srv.Close()
	now := time.Unix(1_800_000_000, 0)
	h := &Heartbeat{URL: srv.URL + "/ping/secret-uuid", Every: time.Minute, now: func() time.Time { return now }}
	ctx := context.Background()
	for _, step := range []time.Duration{0, 5 * time.Second, 30 * time.Second, 25 * time.Second} {
		now = now.Add(step)
		if err := h.Beat(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if pings.Load() != 2 {
		t.Fatalf("%d pings over 60 s at a 1 min interval, want 2 (at 0 s and 60 s)", pings.Load())
	}
	if err := (*Heartbeat)(nil).Beat(ctx); err != nil {
		t.Fatal("no heartbeat configured is not an error")
	}
}

func TestHeartbeatFailureRetriesNextCycleAndNeverLeaksTheURL(t *testing.T) {
	var pings atomic.Int64
	status := http.StatusServiceUnavailable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pings.Add(1)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	h := &Heartbeat{URL: srv.URL + "/ping/secret-uuid", Every: time.Minute}
	err := h.Beat(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret-uuid") {
		t.Fatalf("a refused ping is an error, without the URL: %v", err)
	}
	status = http.StatusOK
	if err := h.Beat(context.Background()); err != nil || pings.Load() != 2 {
		t.Fatalf("a failed ping must not count as the interval's beat: %v, %d pings", err, pings.Load())
	}
	dead := &Heartbeat{URL: "http://127.0.0.1:1/ping/secret-uuid", Every: time.Minute}
	if err := dead.Beat(context.Background()); err == nil || strings.Contains(err.Error(), "secret-uuid") {
		t.Fatalf("an unreachable ping is an error, without the URL: %v", err)
	}
}
