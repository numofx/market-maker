package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	APIBaseURL               string
	ListenAddr               string
	Timeout                  time.Duration
	SpotSymbol               string
	FuturesSymbol            string
	Expiry                   time.Time
	RateAPR                  float64
	CarryAbsolute            float64
	CarryBPS                 float64
	MaxSpotAge               time.Duration
	AllowAnyMarket           bool
	GuardedFallbackEnabled   bool
	GuardedFallbackSpotPrice float64
}

type bookResponse struct {
	Bids []bookLevel `json:"bids"`
	Asks []bookLevel `json:"asks"`
}

type bookLevel struct {
	LimitPrice   string        `json:"limit_price"`
	SpotContract *spotContract `json:"spot_contract"`
}

type spotContract struct {
	UIIntent *spotUIIntent `json:"ui_intent"`
}

type spotUIIntent struct {
	Price string `json:"price"`
}

type tradesResponse struct {
	Trades []trade `json:"trades"`
}

type trade struct {
	Price     string    `json:"price"`
	CreatedAt time.Time `json:"created_at"`
}

type anchorResponse struct {
	Price       float64 `json:"price"`
	Spot        float64 `json:"spot"`
	Fair        float64 `json:"fair"`
	Source      string  `json:"source"`
	Market      string  `json:"market"`
	RateAPR     float64 `json:"rate_apr"`
	CarryAbs    float64 `json:"carry_abs"`
	CarryBPS    float64 `json:"carry_bps"`
	TYears      float64 `json:"t_years"`
	ExpiryUTC   string  `json:"expiry_utc"`
	GeneratedAt string  `json:"generated_at"`
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	h := &handler{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.healthz)
	mux.HandleFunc("/price", h.price)

	server := &http.Server{Addr: cfg.ListenAddr, Handler: mux}
	logger.Info("fair-anchor listening", "addr", cfg.ListenAddr, "spot", cfg.SpotSymbol, "futures", cfg.FuturesSymbol, "expiry", cfg.Expiry.UTC().Format(time.RFC3339), "rate_apr", cfg.RateAPR)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
}

type handler struct {
	cfg    config
	client *http.Client
}

func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.Timeout)
	defer cancel()

	spot, source, usedFallback, err := h.resolveSpot(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	_, fair := h.computeFair(spot)
	if !(fair > 0) || math.IsNaN(fair) || math.IsInf(fair, 0) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "computed invalid fair value",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"spot":             spot,
		"fair":             fair,
		"source":           source,
		"market":           h.cfg.FuturesSymbol,
		"expiry":           h.cfg.Expiry.UTC().Format(time.RFC3339),
		"checked":          time.Now().UTC().Format(time.RFC3339Nano),
		"degraded":         usedFallback,
		"fallback_active":  usedFallback,
		"fallback_enabled": h.cfg.GuardedFallbackEnabled,
	})
}

func (h *handler) price(w http.ResponseWriter, r *http.Request) {
	market := strings.TrimSpace(r.URL.Query().Get("market"))
	if market == "" {
		market = h.cfg.FuturesSymbol
	}
	if !h.cfg.AllowAnyMarket && market != h.cfg.FuturesSymbol {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unsupported market %q", market)})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()

	spot, source, _, err := h.resolveSpot(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}

	tYears, fair := h.computeFair(spot)
	if !(fair > 0) || math.IsNaN(fair) || math.IsInf(fair, 0) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "computed invalid fair value"})
		return
	}

	resp := anchorResponse{
		Price:       fair,
		Spot:        spot,
		Fair:        fair,
		Source:      source,
		Market:      market,
		RateAPR:     h.cfg.RateAPR,
		CarryAbs:    h.cfg.CarryAbsolute,
		CarryBPS:    h.cfg.CarryBPS,
		TYears:      tYears,
		ExpiryUTC:   h.cfg.Expiry.UTC().Format(time.RFC3339),
		GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) computeFair(spot float64) (float64, float64) {
	tYears := time.Until(h.cfg.Expiry).Seconds() / (365.0 * 24.0 * 3600.0)
	if tYears < 0 {
		tYears = 0
	}
	return tYears, spot*math.Exp(h.cfg.RateAPR*tYears) + h.cfg.CarryAbsolute + spot*(h.cfg.CarryBPS/10000.0)
}

func (h *handler) resolveSpot(ctx context.Context) (float64, string, bool, error) {
	spot, source, err := h.fetchSpot(ctx)
	if err == nil {
		return spot, source, false, nil
	}
	if h.cfg.GuardedFallbackEnabled && h.cfg.GuardedFallbackSpotPrice > 0 {
		slog.Warn("using guarded fallback spot price", "spot_symbol", h.cfg.SpotSymbol, "fallback_price", h.cfg.GuardedFallbackSpotPrice, "reason", err.Error())
		return h.cfg.GuardedFallbackSpotPrice, "guarded_fallback", true, nil
	}
	return 0, "", false, err
}

func (h *handler) fetchSpot(ctx context.Context) (float64, string, error) {
	book, err := h.getBook(ctx, h.cfg.SpotSymbol)
	if err == nil {
		bestBid, bidOK := topBookPrice(book.Bids)
		bestAsk, askOK := topBookPrice(book.Asks)
		if bidOK && askOK && bestAsk > bestBid {
			return (bestBid + bestAsk) / 2.0, "spot_book_mid", nil
		}
	}

	trades, err := h.getTrades(ctx, h.cfg.SpotSymbol)
	if err != nil {
		return 0, "", fmt.Errorf("fetch spot trades: %w", err)
	}
	if len(trades) == 0 {
		return 0, "", fmt.Errorf("spot source unavailable: empty spot book and no trades")
	}
	spotPrice, err := strconv.ParseFloat(strings.TrimSpace(trades[0].Price), 64)
	if err != nil || spotPrice <= 0 {
		return 0, "", fmt.Errorf("invalid spot trade price: %q", trades[0].Price)
	}
	if h.cfg.MaxSpotAge > 0 && !trades[0].CreatedAt.IsZero() && time.Since(trades[0].CreatedAt) > h.cfg.MaxSpotAge {
		return 0, "", fmt.Errorf("latest spot trade too old: %s", time.Since(trades[0].CreatedAt).Round(time.Second))
	}
	return spotPrice, "spot_last_trade", nil
}

func topBookPrice(levels []bookLevel) (float64, bool) {
	if len(levels) == 0 {
		return 0, false
	}
	for _, lv := range levels {
		if p, ok := parsePresentedBookPrice(lv); ok {
			return p, true
		}
	}
	return 0, false
}

func parsePresentedBookPrice(level bookLevel) (float64, bool) {
	if level.SpotContract != nil && level.SpotContract.UIIntent != nil {
		if p, err := strconv.ParseFloat(strings.TrimSpace(level.SpotContract.UIIntent.Price), 64); err == nil && p > 0 {
			return p, true
		}
	}
	if p, err := strconv.ParseFloat(strings.TrimSpace(level.LimitPrice), 64); err == nil && p > 0 {
		return p, true
	}
	return 0, false
}

func (h *handler) getBook(ctx context.Context, symbol string) (bookResponse, error) {
	u, err := url.Parse(strings.TrimRight(h.cfg.APIBaseURL, "/") + "/v1/book")
	if err != nil {
		return bookResponse{}, err
	}
	q := u.Query()
	q.Set("symbol", symbol)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return bookResponse{}, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return bookResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return bookResponse{}, fmt.Errorf("book endpoint returned %d", resp.StatusCode)
	}
	var body bookResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return bookResponse{}, err
	}
	return body, nil
}

func (h *handler) getTrades(ctx context.Context, symbol string) ([]trade, error) {
	u, err := url.Parse(strings.TrimRight(h.cfg.APIBaseURL, "/") + "/v1/trades")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("symbol", symbol)
	q.Set("limit", "1")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("trades endpoint returned %d", resp.StatusCode)
	}
	var body tradesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Trades, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func loadConfig() (config, error) {
	listenAddr := strings.TrimSpace(os.Getenv("FAIR_ANCHOR_LISTEN_ADDR"))
	if listenAddr == "" {
		if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
			listenAddr = ":" + port
		} else {
			listenAddr = ":8090"
		}
	}

	cfg := config{
		APIBaseURL:               strings.TrimSpace(os.Getenv("FAIR_ANCHOR_API_BASE_URL")),
		ListenAddr:               listenAddr,
		Timeout:                  time.Duration(envInt("FAIR_ANCHOR_TIMEOUT_MS", 1500)) * time.Millisecond,
		SpotSymbol:               envString("FAIR_ANCHOR_SPOT_SYMBOL", "USDCcNGN-SPOT"),
		FuturesSymbol:            envString("FAIR_ANCHOR_FUTURES_SYMBOL", "USDCcNGN-APR30-2026"),
		RateAPR:                  envFloat("FAIR_ANCHOR_RATE_APR", 0),
		CarryAbsolute:            envFloat("FAIR_ANCHOR_CARRY_ABS", 0),
		CarryBPS:                 envFloat("FAIR_ANCHOR_CARRY_BPS", 0),
		MaxSpotAge:               time.Duration(envInt("FAIR_ANCHOR_MAX_SPOT_AGE_SECONDS", 300)) * time.Second,
		AllowAnyMarket:           envBool("FAIR_ANCHOR_ALLOW_ANY_MARKET", false),
		GuardedFallbackEnabled:   envBool("FAIR_ANCHOR_GUARDED_FALLBACK_ENABLED", false),
		GuardedFallbackSpotPrice: envFloat("FAIR_ANCHOR_GUARDED_FALLBACK_SPOT_PRICE", 0),
	}
	if cfg.APIBaseURL == "" {
		return config{}, fmt.Errorf("FAIR_ANCHOR_API_BASE_URL is required")
	}
	expiryRaw := strings.TrimSpace(os.Getenv("FAIR_ANCHOR_EXPIRY_UTC"))
	if expiryRaw == "" {
		expiryRaw = "2026-04-30T00:00:00Z"
	}
	expiry, err := time.Parse(time.RFC3339, expiryRaw)
	if err != nil {
		return config{}, fmt.Errorf("invalid FAIR_ANCHOR_EXPIRY_UTC: %w", err)
	}
	cfg.Expiry = expiry
	if cfg.Timeout <= 0 {
		return config{}, fmt.Errorf("FAIR_ANCHOR_TIMEOUT_MS must be > 0")
	}
	if cfg.MaxSpotAge < 0 {
		return config{}, fmt.Errorf("FAIR_ANCHOR_MAX_SPOT_AGE_SECONDS must be >= 0")
	}
	if cfg.GuardedFallbackEnabled && cfg.GuardedFallbackSpotPrice <= 0 {
		return config{}, fmt.Errorf("FAIR_ANCHOR_GUARDED_FALLBACK_SPOT_PRICE must be > 0 when FAIR_ANCHOR_GUARDED_FALLBACK_ENABLED=true")
	}
	return cfg, nil
}

func envString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		panic(fmt.Sprintf("%s must be an integer: %v", key, err))
	}
	return v
}

func envFloat(key string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		panic(fmt.Sprintf("%s must be a float: %v", key, err))
	}
	return v
}

func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		panic(fmt.Sprintf("%s must be true/false: %v", key, err))
	}
	return v
}
