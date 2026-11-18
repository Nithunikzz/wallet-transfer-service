package integration

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wallet-transfer-service/internal/handler"
)

func TestHTTPEndToEnd(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(handler.New(e.svc, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer srv.Close()

	post := func(path, body string) (*http.Response, map[string]any) {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp, m
	}

	for _, w := range []string{`{"id":"wallet_1","initialBalance":1000}`, `{"id":"wallet_2","initialBalance":0}`} {
		if resp, _ := post("/wallets", w); resp.StatusCode != http.StatusCreated {
			t.Fatalf("create wallet: %d", resp.StatusCode)
		}
	}

	body := `{"idempotencyKey":"abc123","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":100}`
	resp1, t1 := post("/transfers", body)
	if resp1.StatusCode != http.StatusCreated || t1["status"] != "PROCESSED" {
		t.Fatalf("first: %d %v", resp1.StatusCode, t1)
	}
	if resp1.Header.Get(handler.HeaderReplayed) != "" {
		t.Fatal("first response must not be marked as replayed")
	}

	resp2, t2 := post("/transfers", body)
	if resp2.StatusCode != http.StatusCreated || resp2.Header.Get(handler.HeaderReplayed) != "true" {
		t.Fatalf("replay: %d header=%q", resp2.StatusCode, resp2.Header.Get(handler.HeaderReplayed))
	}
	if t1["id"] != t2["id"] || t1["updatedAt"] != t2["updatedAt"] {
		t.Fatalf("replay body differs: %v vs %v", t1, t2)
	}

	resp3, e3 := post("/transfers", `{"idempotencyKey":"abc123","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":5}`)
	if resp3.StatusCode != http.StatusConflict {
		t.Fatalf("key reuse: %d %v", resp3.StatusCode, e3)
	}

	resp4, t4 := post("/transfers", `{"idempotencyKey":"big","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":100000}`)
	if resp4.StatusCode != http.StatusUnprocessableEntity || t4["failureReason"] != "INSUFFICIENT_FUNDS" {
		t.Fatalf("insufficient: %d %v", resp4.StatusCode, t4)
	}

	r, err := http.Get(srv.URL + "/wallets/wallet_1")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var w map[string]any
	_ = json.NewDecoder(r.Body).Decode(&w)
	if w["balance"].(float64) != 900 {
		t.Fatalf("wallet_1 = %v", w)
	}

	g, err := http.Get(srv.URL + "/transfers/" + t1["id"].(string))
	if err != nil || g.StatusCode != http.StatusOK {
		t.Fatalf("get transfer: %v %v", g, err)
	}
	g.Body.Close()
	e.assertLedgerInvariants()
}
