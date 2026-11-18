package handler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/service"
)

type fakeService struct {
	gotReq domain.TransferRequest
	calls  int
	res    service.TransferResult
	err    error
}

func (f *fakeService) CreateTransfer(_ context.Context, r domain.TransferRequest) (service.TransferResult, error) {
	f.calls++
	f.gotReq = r
	return f.res, f.err
}
func (f *fakeService) GetTransfer(context.Context, string) (domain.Transfer, error) {
	return domain.Transfer{}, domain.ErrTransferNotFound
}
func (f *fakeService) CreateWallet(context.Context, string, int64) (domain.Wallet, error) {
	return domain.Wallet{}, domain.ErrWalletExists
}
func (f *fakeService) GetWallet(context.Context, string) (domain.Wallet, error) {
	return domain.Wallet{}, domain.ErrWalletNotFound
}
func (f *fakeService) ListLedgerEntries(context.Context, string) ([]domain.LedgerEntry, error) {
	return nil, nil
}

func do(t *testing.T, svc Service, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := New(svc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func TestCreateTransferRejectsBadBodies(t *testing.T) {
	cases := map[string]string{
		"not json":       `{`,
		"unknown field":  `{"fromWalletId":"a","toWalletId":"b","amount":1,"extra":1}`,
		"missing amount": `{"fromWalletId":"a","toWalletId":"b"}`,
		"float amount":   `{"fromWalletId":"a","toWalletId":"b","amount":1.5}`,
		"string amount":  `{"fromWalletId":"a","toWalletId":"b","amount":"1"}`,
		"trailing data":  `{"fromWalletId":"a","toWalletId":"b","amount":1}{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeService{}
			rec := do(t, f, http.MethodPost, "/transfers", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
			}
			if f.calls != 0 {
				t.Fatal("service must not be called for malformed input")
			}
		})
	}
}

func TestCreateTransferMapsRequest(t *testing.T) {
	f := &fakeService{res: service.TransferResult{Transfer: domain.Transfer{ID: "t1", Status: domain.TransferProcessed}}}
	rec := do(t, f, http.MethodPost, "/transfers",
		`{"idempotencyKey":" k1 ","fromWalletId":"a","toWalletId":"b","amount":100}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	want := domain.TransferRequest{IdempotencyKey: "k1", FromWalletID: "a", ToWalletID: "b", Amount: 100}
	if f.gotReq != want {
		t.Fatalf("service got %+v, want %+v", f.gotReq, want)
	}
}

func TestCreateTransferStatusMapping(t *testing.T) {
	body := `{"fromWalletId":"a","toWalletId":"b","amount":1}`
	cases := []struct {
		name     string
		res      service.TransferResult
		err      error
		code     int
		replayed bool
	}{
		{"processed", service.TransferResult{Transfer: domain.Transfer{Status: domain.TransferProcessed}}, nil, 201, false},
		{"failed", service.TransferResult{Transfer: domain.Transfer{Status: domain.TransferFailed}}, nil, 422, false},
		{"replayed", service.TransferResult{Transfer: domain.Transfer{Status: domain.TransferProcessed}, Replayed: true}, nil, 201, true},
		{"validation", service.TransferResult{}, domain.ErrValidation, 400, false},
		{"wallet missing", service.TransferResult{}, domain.ErrWalletNotFound, 404, false},
		{"key reused", service.TransferResult{}, domain.ErrIdempotencyKeyReused, 409, false},
		{"internal", service.TransferResult{}, errors.New("boom"), 500, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, &fakeService{res: c.res, err: c.err}, http.MethodPost, "/transfers", body)
			if rec.Code != c.code {
				t.Fatalf("status = %d, want %d", rec.Code, c.code)
			}
			if got := rec.Header().Get(HeaderReplayed) == "true"; got != c.replayed {
				t.Fatalf("replayed header = %v, want %v", got, c.replayed)
			}
			if c.code == 500 && strings.Contains(rec.Body.String(), "boom") {
				t.Fatal("internal error details must not leak")
			}
		})
	}
}

func TestOtherRoutesMapErrors(t *testing.T) {
	f := &fakeService{}
	if rec := do(t, f, http.MethodGet, "/transfers/x", ""); rec.Code != 404 {
		t.Fatalf("GET transfer = %d", rec.Code)
	}
	if rec := do(t, f, http.MethodGet, "/wallets/x", ""); rec.Code != 404 {
		t.Fatalf("GET wallet = %d", rec.Code)
	}
	if rec := do(t, f, http.MethodPost, "/wallets", `{"id":"x","initialBalance":1}`); rec.Code != 409 {
		t.Fatalf("POST wallet = %d", rec.Code)
	}
	if rec := do(t, f, http.MethodGet, "/transfers", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /transfers = %d", rec.Code)
	}
}

type panicService struct{ fakeService }

func (panicService) GetWallet(context.Context, string) (domain.Wallet, error) { panic("boom") }

func TestRequestIDIsPropagatedOrGenerated(t *testing.T) {
	h := New(&fakeService{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(HeaderRequestID, "abc-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get(HeaderRequestID); got != "abc-123" {
		t.Fatalf("incoming id not echoed: %q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set(HeaderRequestID, "bad id\nwith newline")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get(HeaderRequestID); got == "" || strings.ContainsAny(got, " \n") {
		t.Fatalf("malformed id must be replaced, got %q", got)
	}
}

func TestUnknownRouteReturnsJSON404(t *testing.T) {
	rec := do(t, &fakeService{}, http.MethodGet, "/nope", "")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"NOT_FOUND"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestPanicIsRecoveredAs500(t *testing.T) {
	rec := do(t, &panicService{}, http.MethodGet, "/wallets/x", "")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	big := `{"fromWalletId":"` + strings.Repeat("a", maxBodyBytes) + `","toWalletId":"b","amount":1}`
	f := &fakeService{}
	if rec := do(t, f, http.MethodPost, "/transfers", big); rec.Code != http.StatusBadRequest || f.calls != 0 {
		t.Fatalf("got %d, calls=%d", rec.Code, f.calls)
	}
}
