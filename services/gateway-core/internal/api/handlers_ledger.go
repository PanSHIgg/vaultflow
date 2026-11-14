package api

import (
	"net/http"

	"github.com/google/uuid"

	"vaultflow/gateway-core/internal/types"
)

// ---------------------------------------------------------------------------
// GET /v1/ledger/accounts — listLedgerAccounts
// ---------------------------------------------------------------------------

func (s *Server) listLedgerAccounts(w http.ResponseWriter, r *http.Request) {
	m, _ := MerchantFromCtx(r)
	chainID, ae := parseChainIDQuery(r)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	accts, ae := parseEnumCSV[types.LedgerAccountCode](r, "account_type", func(c types.LedgerAccountCode) bool {
		return c.IsValid()
	})
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	owners := []uuid.UUID{m.ID, types.SystemMerchantID}
	if raw := r.URL.Query().Get("owner_merchant_id"); raw != "" {
		oid, ae := parseUUIDParam(raw, "owner_merchant_id")
		if ae != nil {
			writeError(w, r, ae)
			return
		}
		if oid != m.ID && oid != types.SystemMerchantID {
			writeError(w, r, forbidden("owner_merchant_id must equal the caller or the system sentinel"))
			return
		}
		owners = []uuid.UUID{oid}
	}
	if s.deps.Store == nil {
		writeError(w, r, upstream("read store not wired"))
		return
	}
	rows, err := s.deps.Store.ListLedgerAccounts(r.Context(), LedgerAccountQuery{
		Owners: owners, ChainID: chainID,
		AssetSymbol: r.URL.Query().Get("asset_symbol"), Accounts: accts,
	})
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	out := ledgerAccountListDTO{Data: make([]ledgerAccountDTO, 0, len(rows))}
	for _, a := range rows {
		out.Data = append(out.Data, ledgerAccountDTO{
			ID:              a.ID.String(),
			OwnerMerchantID: a.OwnerMerchantID.String(),
			ChainID:         int64(a.ChainID),
			AssetSymbol:     a.AssetSymbol,
			AccountType:     a.Account.String(),
			NormalSide:      a.NormalSide.String(),
			Balance:         a.Balance,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// GET /v1/ledger/transactions — listLedgerTransactions
// ---------------------------------------------------------------------------

func (s *Server) listLedgerTransactions(w http.ResponseWriter, r *http.Request) {
	limit, after, ae := parsePageParams(r)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	refTypes, ae := parseEnumCSV[types.EntryTemplateCode](r, "reference_type", func(c types.EntryTemplateCode) bool {
		return c.IsValid()
	})
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	if s.deps.Store == nil {
		writeError(w, r, upstream("read store not wired"))
		return
	}
	page, err := s.deps.Store.ListLedgerTransactions(r.Context(), LedgerTxQuery{
		RefTypes: refTypes,
		RefID:    r.URL.Query().Get("reference_id"),
		After:    after,
		Limit:    limit,
	})
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	out := ledgerTransactionPageDTO{Data: make([]ledgerTransactionDTO, 0, len(page.Data))}
	for _, t := range page.Data {
		var desc *string
		if t.Description != "" {
			d := t.Description
			desc = &d
		}
		out.Data = append(out.Data, ledgerTransactionDTO{
			ID:            t.ID.String(),
			ReferenceType: t.ReferenceType.String(),
			ReferenceID:   t.ReferenceID,
			Description:   desc,
			CreatedAt:     t.CreatedAt.UTC(),
		})
	}
	if page.HasMore && len(page.Data) > 0 {
		last := page.Data[len(page.Data)-1]
		out.Page = pageOf(true, encodeCursor(last.CreatedAt, last.ID))
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// GET /v1/ledger/entries — listLedgerEntries
// ---------------------------------------------------------------------------

func (s *Server) listLedgerEntries(w http.ResponseWriter, r *http.Request) {
	limit, after, ae := parsePageParams(r)
	if ae != nil {
		writeError(w, r, ae)
		return
	}
	var txID *uuid.UUID
	if raw := r.URL.Query().Get("transaction_id"); raw != "" {
		id, ae := parseUUIDParam(raw, "transaction_id")
		if ae != nil {
			writeError(w, r, ae)
			return
		}
		txID = &id
	}
	var acctID *uuid.UUID
	if raw := r.URL.Query().Get("account_id"); raw != "" {
		id, ae := parseUUIDParam(raw, "account_id")
		if ae != nil {
			writeError(w, r, ae)
			return
		}
		acctID = &id
	}
	var dir *types.Direction
	if raw := r.URL.Query().Get("direction"); raw != "" {
		d := types.Direction(raw)
		if !directionValid(d) {
			writeError(w, r, badRequest("invalid direction"))
			return
		}
		dir = &d
	}
	if s.deps.Store == nil {
		writeError(w, r, upstream("read store not wired"))
		return
	}
	page, err := s.deps.Store.ListLedgerEntries(r.Context(), LedgerEntryQuery{
		TransactionID: txID, AccountID: acctID, Direction: dir, After: after, Limit: limit,
	})
	if err != nil {
		writeError(w, r, mapError(err, ""))
		return
	}
	out := ledgerEntryPageDTO{Data: make([]ledgerEntryDTO, 0, len(page.Data))}
	for _, e := range page.Data {
		out.Data = append(out.Data, ledgerEntryDTO{
			ID:              e.Entry.ID.String(),
			TransactionID:   e.Entry.TransactionID.String(),
			AccountID:       e.Entry.AccountID.String(),
			AccountType:     e.AccountCode.String(),
			OwnerMerchantID: e.OwnerMerchantID.String(),
			Direction:       e.Entry.Direction.String(),
			Amount:          e.Entry.Amount,
			BalanceAfter:    e.Entry.BalanceAfter,
			CreatedAt:       e.Entry.CreatedAt.UTC(),
		})
	}
	if page.HasMore && len(page.Data) > 0 {
		last := page.Data[len(page.Data)-1]
		out.Page = pageOf(true, encodeCursor(last.Entry.CreatedAt, last.Entry.ID))
	}
	writeJSON(w, http.StatusOK, out)
}
