package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/numofx/market-maker/internal/config"
)

type AnchorSource interface {
	Name() string
	GetAnchorPrice(ctx context.Context, market string) (float64, error)
}

type NoopAnchorSource struct{}

func (NoopAnchorSource) Name() string { return "none" }
func (NoopAnchorSource) GetAnchorPrice(context.Context, string) (float64, error) {
	return 0, nil
}

type FixedAnchorSource struct {
	price float64
}

func (s FixedAnchorSource) Name() string { return "fixed" }
func (s FixedAnchorSource) GetAnchorPrice(context.Context, string) (float64, error) {
	return s.price, nil
}

type HTTPAnchorSource struct {
	baseURL string
	client  *http.Client
}

func (s HTTPAnchorSource) Name() string { return "http" }
func (s HTTPAnchorSource) GetAnchorPrice(ctx context.Context, market string) (float64, error) {
	u, err := url.Parse(s.baseURL)
	if err != nil {
		return 0, err
	}
	query := u.Query()
	query.Set("market", market)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("anchor endpoint returned %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	var body struct {
		Price float64 `json:"price"`
	}
	if err := json.Unmarshal(raw, &body); err == nil && body.Price > 0 {
		return body.Price, nil
	}
	if price, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64); err == nil {
		return price, nil
	}
	return 0, fmt.Errorf("anchor response missing parseable price")
}

func NewAnchorSource(cfg config.Config) AnchorSource {
	switch cfg.AnchorSourceType {
	case "fixed":
		return FixedAnchorSource{price: cfg.AnchorFixedPrice}
	case "http":
		return HTTPAnchorSource{
			baseURL: cfg.AnchorURL,
			client:  &http.Client{Timeout: 5 * time.Second},
		}
	default:
		return NoopAnchorSource{}
	}
}
