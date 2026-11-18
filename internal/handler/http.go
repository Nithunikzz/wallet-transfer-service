// Package handler is the HTTP transport: decoding, validation of the wire
// format, mapping to service calls and mapping errors to status codes.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/service"
)

// Service is what the handler needs from the service layer.
type Service interface {
	CreateTransfer(ctx context.Context, req domain.TransferRequest) (service.TransferResult, error)
	GetTransfer(ctx context.Context, id string) (domain.Transfer, error)
	CreateWallet(ctx context.Context, id string, initialBalance int64) (domain.Wallet, error)
	GetWallet(ctx context.Context, id string) (domain.Wallet, error)
	ListLedgerEntries(ctx context.Context, walletID string) ([]domain.LedgerEntry, error)
}

const maxBodyBytes = 1 << 16

// HeaderReplayed is set on responses served from an earlier request with the
// same idempotency key.
const HeaderReplayed = "Idempotent-Replayed"

// Handler serves the HTTP API.
type Handler struct {
	svc Service
	log *slog.Logger
}

// New builds the HTTP handler with all routes registered.
func New(svc Service, log *slog.Logger) http.Handler {
	h := &Handler{svc: svc, log: log}
	mux := http.NewServeMux()
	routes := []struct {
		method, path string
		fn           http.HandlerFunc
	}{
		{http.MethodPost, "/transfers", h.createTransfer},
		{http.MethodGet, "/transfers/{id}", h.getTransfer},
		{http.MethodPost, "/wallets", h.createWallet},
		{http.MethodGet, "/wallets/{id}", h.getWallet},
		{http.MethodGet, "/wallets/{id}/ledger", h.listLedger},
		{http.MethodGet, "/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }},
	}
	for _, rt := range routes {
		mux.HandleFunc(rt.method+" "+rt.path, rt.fn)
		// Method-less fallback for the same path: a consistent JSON 405.
		allow := rt.method
		mux.HandleFunc(rt.path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Allow", allow)
			h.writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		h.writeError(w, http.StatusNotFound, "NOT_FOUND", "route not found")
	})
	// Outermost first: request id -> access log -> panic recovery -> routes.
	return withRequestID(withAccessLog(log, h.withRecover(mux)))
}

// ---- wire types ----

type createTransferRequest struct {
	IdempotencyKey string `json:"idempotencyKey"`
	FromWalletID   string `json:"fromWalletId"`
	ToWalletID     string `json:"toWalletId"`
	Amount         *int64 `json:"amount"`
}

type transferResponse struct {
	ID             string    `json:"id"`
	IdempotencyKey string    `json:"idempotencyKey,omitempty"`
	FromWalletID   string    `json:"fromWalletId"`
	ToWalletID     string    `json:"toWalletId"`
	Amount         int64     `json:"amount"`
	Status         string    `json:"status"`
	FailureReason  string    `json:"failureReason,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type createWalletRequest struct {
	ID             string `json:"id"`
	InitialBalance int64  `json:"initialBalance"`
}

type walletResponse struct {
	ID        string    `json:"id"`
	Balance   int64     `json:"balance"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type ledgerEntryResponse struct {
	ID         int64     `json:"id"`
	WalletID   string    `json:"walletId"`
	TransferID string    `json:"transferId"`
	Type       string    `json:"type"`
	Amount     int64     `json:"amount"`
	CreatedAt  time.Time `json:"createdAt"`
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func toTransferResponse(t domain.Transfer) transferResponse {
	return transferResponse{
		ID: t.ID, IdempotencyKey: t.IdempotencyKey,
		FromWalletID: t.FromWalletID, ToWalletID: t.ToWalletID, Amount: t.Amount,
		Status: string(t.Status), FailureReason: string(t.FailureReason),
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

func toWalletResponse(w domain.Wallet) walletResponse {
	return walletResponse{ID: w.ID, Balance: w.Balance, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
}

// ---- handlers ----

func (h *Handler) createTransfer(w http.ResponseWriter, r *http.Request) {
	var body createTransferRequest
	if !h.decode(w, r, &body) {
		return
	}
	if body.Amount == nil {
		h.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "amount is required")
		return
	}
	res, err := h.svc.CreateTransfer(r.Context(), domain.TransferRequest{
		IdempotencyKey: strings.TrimSpace(body.IdempotencyKey),
		FromWalletID:   body.FromWalletID,
		ToWalletID:     body.ToWalletID,
		Amount:         *body.Amount,
	})
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	if res.Replayed {
		w.Header().Set(HeaderReplayed, "true")
	}
	status := http.StatusCreated
	if res.Transfer.Status == domain.TransferFailed {
		status = http.StatusUnprocessableEntity
	}
	h.writeJSON(w, status, toTransferResponse(res.Transfer))
}

func (h *Handler) getTransfer(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.GetTransfer(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toTransferResponse(t))
}

func (h *Handler) createWallet(w http.ResponseWriter, r *http.Request) {
	var body createWalletRequest
	if !h.decode(w, r, &body) {
		return
	}
	wallet, err := h.svc.CreateWallet(r.Context(), body.ID, body.InitialBalance)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, toWalletResponse(wallet))
}

func (h *Handler) getWallet(w http.ResponseWriter, r *http.Request) {
	wallet, err := h.svc.GetWallet(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	h.writeJSON(w, http.StatusOK, toWalletResponse(wallet))
}

func (h *Handler) listLedger(w http.ResponseWriter, r *http.Request) {
	entries, err := h.svc.ListLedgerEntries(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	out := make([]ledgerEntryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, ledgerEntryResponse{
			ID: e.ID, WalletID: e.WalletID, TransferID: e.TransferID,
			Type: string(e.Type), Amount: e.Amount, CreatedAt: e.CreatedAt,
		})
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// ---- helpers ----

func (h *Handler) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		h.writeError(w, http.StatusBadRequest, "INVALID_JSON", "invalid request body: "+err.Error())
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		h.writeError(w, http.StatusBadRequest, "INVALID_JSON", "request body must contain a single JSON object")
		return false
	}
	return true
}

func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrValidation):
		h.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, domain.ErrWalletNotFound):
		h.writeError(w, http.StatusNotFound, "WALLET_NOT_FOUND", err.Error())
	case errors.Is(err, domain.ErrTransferNotFound):
		h.writeError(w, http.StatusNotFound, "TRANSFER_NOT_FOUND", err.Error())
	case errors.Is(err, domain.ErrWalletExists):
		h.writeError(w, http.StatusConflict, "WALLET_EXISTS", err.Error())
	case errors.Is(err, domain.ErrIdempotencyKeyReused):
		h.writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", err.Error())
	default:
		h.log.ErrorContext(r.Context(), "request failed", "err", err)
		h.writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, code, msg string) {
	var b errorBody
	b.Error.Code = code
	b.Error.Message = msg
	h.writeJSON(w, status, b)
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.log.Error("encode response", "err", err)
	}
}
