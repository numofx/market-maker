package state

import "sync"

// DeltaBook is the net delta of the strikes one options process quotes, in base units (cNGN): each strike's cycle sets
// its own position's delta, and every strike skews against the total. Shared by the strike bots of one process.
type DeltaBook struct {
	mu     sync.Mutex
	deltas map[string]float64
}

func NewDeltaBook() *DeltaBook { return &DeltaBook{deltas: map[string]float64{}} }

// Set records `market`'s position delta and returns the total across every market.
func (b *DeltaBook) Set(market string, delta float64) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deltas[market] = delta
	total := 0.0
	for _, d := range b.deltas {
		total += d
	}
	return total
}
