package marketdata

import (
	"context"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/numofx/market-maker/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestZeroExExternalAnchorRejectsWildDeviation(t *testing.T) {
	var hits atomic.Int32
	provider := &ZeroExUSDCCNGNSpotExternalAnchor{
		cfg: config.USDCCNGNSpotExternalAnchorConfig{
			Provider:        "0x",
			BaseURL:         "https://example.invalid/price",
			ChainID:         8453,
			SellToken:       "0xsell",
			BuyToken:        "0xbuy",
			Amount:          "1000000",
			Timeout:         time.Second,
			MaxAge:          time.Minute,
			MaxDeviationBPS: 500,
		},
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			n := hits.Add(1)
			body := `{"price":"1500"}`
			if n > 1 {
				body = `{"price":"3000"}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		})},
	}

	first := provider.Fetch(context.Background())
	second := provider.Fetch(context.Background())
	if !first.Present || !second.Present {
		t.Fatalf("expected cached external anchor, got first=%#v second=%#v", first, second)
	}
	if second.Price != first.Price {
		t.Fatalf("wild deviation should keep cached price, got %v want %v", second.Price, first.Price)
	}
	if !second.RefreshFailed {
		t.Fatal("expected deviation rejection to mark refresh failed")
	}
}

func TestZeroExExternalAnchorExpiresStaleCache(t *testing.T) {
	var hits atomic.Int32
	provider := &ZeroExUSDCCNGNSpotExternalAnchor{
		cfg: config.USDCCNGNSpotExternalAnchorConfig{
			Provider:        "0x",
			BaseURL:         "https://example.invalid/price",
			ChainID:         8453,
			SellToken:       "0xsell",
			BuyToken:        "0xbuy",
			Amount:          "1000000",
			Timeout:         time.Second,
			MaxAge:          10 * time.Millisecond,
			MaxDeviationBPS: 500,
		},
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			n := hits.Add(1)
			if n == 1 {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"price":"1500"}`)),
					Header:     make(http.Header),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Body:       io.NopCloser(strings.NewReader(`{"error":"down"}`)),
				Header:     make(http.Header),
			}, nil
		})},
	}

	first := provider.Fetch(context.Background())
	if !first.Present {
		t.Fatalf("expected fresh price, got %#v", first)
	}
	time.Sleep(20 * time.Millisecond)
	second := provider.Fetch(context.Background())
	if second.Present {
		t.Fatalf("expected stale cached anchor to be rejected, got %#v", second)
	}
	if !second.RefreshFailed {
		t.Fatal("expected failed refresh when stale cache cannot be reused")
	}
}

func TestCNGNOracleExternalAnchorFetchesOnChainPrice(t *testing.T) {
	var hits atomic.Int32
	now := time.Now().UTC()
	provider := &ZeroExUSDCCNGNSpotExternalAnchor{
		cfg: config.USDCCNGNSpotExternalAnchorConfig{
			Provider:        "cngn-price-oracle",
			RPCURL:          "https://example.invalid/rpc",
			Timeout:         time.Second,
			MaxAge:          time.Minute,
			MaxDeviationBPS: 500,
		},
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			hit := hits.Add(1)
			var body string
			switch hit {
			case 1:
				body = rpcResultJSON(t, "decimals", uint8(8))
			default:
				body = rpcResultJSON(t, "latestRoundData", big.NewInt(1), big.NewInt(80_000), big.NewInt(0), big.NewInt(now.Unix()), big.NewInt(1))
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		})},
	}

	quote := provider.Fetch(context.Background())
	if !quote.Present {
		t.Fatalf("expected present quote, got %#v", quote)
	}
	if quote.Price != 1250 {
		t.Fatalf("price = %v want 1250", quote.Price)
	}
	if quote.FetchedAt.Unix() != now.Unix() {
		t.Fatalf("fetchedAt = %v want unix %d", quote.FetchedAt, now.Unix())
	}
	if hits.Load() != 2 {
		t.Fatalf("rpc calls = %d want 2", hits.Load())
	}
}

func TestCNGNOracleExternalAnchorRejectsStaleOnChainPrice(t *testing.T) {
	provider := &ZeroExUSDCCNGNSpotExternalAnchor{
		cfg: config.USDCCNGNSpotExternalAnchorConfig{
			Provider:        "cngn-price-oracle",
			RPCURL:          "https://example.invalid/rpc",
			Timeout:         time.Second,
			MaxAge:          time.Second,
			MaxDeviationBPS: 500,
		},
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			payload, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			body := rpcResultJSON(t, "latestRoundData", big.NewInt(1), big.NewInt(80_000), big.NewInt(0), big.NewInt(1), big.NewInt(1))
			if strings.Contains(string(payload), "0x313ce567") {
				body = rpcResultJSON(t, "decimals", uint8(8))
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		})},
	}

	quote := provider.Fetch(context.Background())
	if quote.Present {
		t.Fatalf("expected stale quote to be rejected, got %#v", quote)
	}
	if !quote.RefreshFailed {
		t.Fatal("expected stale quote rejection to mark refresh failed")
	}
}

func rpcResultJSON(t *testing.T, method string, values ...any) string {
	t.Helper()
	packed, err := cngnOracleABI.Methods[method].Outputs.Pack(values...)
	if err != nil {
		t.Fatalf("pack %s: %v", method, err)
	}
	return `{"jsonrpc":"2.0","id":1,"result":"` + hexutil.Encode(packed) + `"}`
}
