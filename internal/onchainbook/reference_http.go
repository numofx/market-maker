package onchainbook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"
)

// HTTPReference is the divergence guard's second price when the engine has no independent feed for the market: cNGN,
// whose only on-chain price is the index itself (engine market 1's spot feed). It reads a rate service
// (exchange services/cngn-rate, which runs cngn-rate-picker) that answers
//
//	{"usd_per_base_e18": "691…", "observed_at": 1795041138}
//
// A rate older than MaxAge, a non-200, or a body that does not parse is an error: Prices then returns a nil
// reference and Decide takes the spline down ("reference_unreadable"). It fails closed, as an unreadable feed does.
type HTTPReference struct {
	URL    string
	MaxAge time.Duration
	Client *http.Client
	now    func() time.Time
}

type httpReferenceBody struct {
	USDPerBaseE18 string `json:"usd_per_base_e18"`
	ObservedAt    int64  `json:"observed_at"`
}

func (r *HTTPReference) Price(ctx context.Context) (*big.Int, error) {
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rate service answered %d: %.200s", resp.StatusCode, body)
	}
	var b httpReferenceBody
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, fmt.Errorf("rate service body: %w", err)
	}
	price, ok := new(big.Int).SetString(b.USDPerBaseE18, 10)
	if !ok || price.Sign() <= 0 {
		return nil, fmt.Errorf("rate service price %q is not a positive integer", b.USDPerBaseE18)
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	age := now().Sub(time.Unix(b.ObservedAt, 0))
	if b.ObservedAt <= 0 || age > r.MaxAge {
		return nil, fmt.Errorf("rate service price is %s old (max %s)", age.Round(time.Second), r.MaxAge)
	}
	return price, nil
}
