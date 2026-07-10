package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func strailsStub(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			http.Error(w, `{"message":"Missing API key"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func strailsHandler(url string) *handler {
	return &handler{
		cfg: config{
			StrailsAPIURL: url,
			StrailsAPIKey: "test-key",
			StrailsPair:   "CNGN-USDC",
			Timeout:       2 * time.Second,
		},
		client: &http.Client{Timeout: 2 * time.Second},
	}
}

func TestFetchStrailsMid(t *testing.T) {
	// Unordered quote board with a paused entry: best active bid 1388.77, best ask 1389.33.
	stub := strailsStub(t, `{"data":{
		"buyOrders":[
			{"price":"1380.00","status":"active"},
			{"price":"1395.00","status":"paused"},
			{"price":"1388.77","status":"active"}
		],
		"sellOrders":[
			{"price":"1392.00","status":"active"},
			{"price":"1389.33","status":"active"}
		]}}`)
	defer stub.Close()

	mid, err := strailsHandler(stub.URL).fetchStrailsMid(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := (1388.77 + 1389.33) / 2.0
	if mid != want {
		t.Fatalf("mid = %v, want %v", mid, want)
	}
}

func TestFetchStrailsMidRejectsBrokenBooks(t *testing.T) {
	cases := map[string]string{
		"crossed":     `{"data":{"buyOrders":[{"price":"1400","status":"active"}],"sellOrders":[{"price":"700.5","status":"active"}]}}`,
		"empty side":  `{"data":{"buyOrders":[],"sellOrders":[{"price":"1389.33","status":"active"}]}}`,
		"implausible": `{"data":{"buyOrders":[{"price":"0.00062","status":"active"}],"sellOrders":[{"price":"0.00063","status":"active"}]}}`,
	}
	for name, body := range cases {
		stub := strailsStub(t, body)
		if _, err := strailsHandler(stub.URL).fetchStrailsMid(context.Background()); err == nil {
			t.Fatalf("%s book must not produce an anchor", name)
		}
		stub.Close()
	}
}

func TestSpotAnchorNeverFallsBackToOwnBook(t *testing.T) {
	// Broken strails + own-book fallback disallowed must error, not anchor circularly.
	stub := strailsStub(t, `{"data":{"buyOrders":[],"sellOrders":[]}}`)
	defer stub.Close()

	h := strailsHandler(stub.URL)
	if _, _, err := h.fetchSpot(context.Background(), false); err == nil {
		t.Fatal("spot anchor must fail rather than fall back to our own book")
	}

	h.cfg.StrailsAPIURL = ""
	if _, _, err := h.fetchSpot(context.Background(), false); err == nil {
		t.Fatal("unconfigured strails must fail the spot anchor")
	}
}

func TestFetchStrailsMidRequiresAuth(t *testing.T) {
	stub := strailsStub(t, `{}`)
	defer stub.Close()

	h := strailsHandler(stub.URL)
	h.cfg.StrailsAPIKey = "wrong-key"
	if _, err := h.fetchStrailsMid(context.Background()); err == nil {
		t.Fatal("expected auth failure to surface as an error")
	}
}
