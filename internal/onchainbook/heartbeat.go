package onchainbook

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// ShouldBeat is whether a cycle leaves the book quoted as it should be: an update that landed, or a live spline that
// needed nothing. The deliberate stop before the series' last trade counts too, so expiry does not page. Anything else
// (an error, dry run, a cancel, or the spline held down because a feed is unreadable or diverges) does not, and a
// dead-man check on the beats fires when that lasts.
func ShouldBeat(d Decision, err error, dryRun bool) bool {
	if err != nil || dryRun {
		return false
	}
	switch d.Action {
	case ActionUpdate:
		return true
	case ActionNone:
		return d.Reason == "unchanged" || d.Reason == "cutoff_before_last_trade"
	default:
		return false
	}
}

// Heartbeat pings a dead-man check (healthchecks.io or similar) at most once per Every.
type Heartbeat struct {
	URL    string
	Every  time.Duration
	Client *http.Client
	now    func() time.Time
	last   time.Time
}

// Beat pings unless the last ping was under Every ago. The URL is a secret (whoever has it can keep the check green), so
// errors never include it.
func (h *Heartbeat) Beat(ctx context.Context) error {
	if h == nil || h.URL == "" {
		return nil
	}
	now := time.Now
	if h.now != nil {
		now = h.now
	}
	if !h.last.IsZero() && now().Sub(h.last) < h.Every {
		return nil
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return fmt.Errorf("heartbeat request: invalid URL")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("heartbeat ping failed (transport)")
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("heartbeat ping answered %d", resp.StatusCode)
	}
	h.last = now()
	return nil
}
