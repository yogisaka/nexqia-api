package server

// Locked stock ledger (plan 2026-10-07-inventory-17a Task 2, spec §4.2–4.5).
// All balance mutations run through applyStockMove/issueStock inside the
// caller's transaction; no other handler may write inventory.stock_balance or
// inventory.stock_move directly.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

var (
	errStockPeriodLocked            = errors.New("stock period is locked")
	errInsufficientStock            = errors.New("insufficient stock")
	errOverrideReason               = errors.New("override needs a reason")
	errConsignmentPriceMissing      = errors.New("consignment price missing")
	errConsignmentSupplierAmbiguous = errors.New("consignment supplier ambiguous")
)

// respondStockError maps ledger sentinel errors to their fixed client
// responses; it returns true when err was one of them and the response is
// written, so callers can fall through to respondInternalError otherwise.
func respondStockError(c *gin.Context, err error) bool {
	switch {
	case errors.Is(err, errStockPeriodLocked):
		c.JSON(409, gin.H{"error": "stock period is locked"})
		return true
	case errors.Is(err, errInsufficientStock):
		c.JSON(409, gin.H{"error": "insufficient stock"})
		return true
	case errors.Is(err, errOverrideReason):
		c.JSON(400, gin.H{"error": "override needs a reason"})
		return true
	case errors.Is(err, errConsignmentPriceMissing):
		c.JSON(409, gin.H{"error": "consignment price missing"})
		return true
	case errors.Is(err, errConsignmentSupplierAmbiguous):
		c.JSON(409, gin.H{"error": "consignment supplier ambiguous"})
		return true
	}
	return false
}

type stockKey struct {
	LocationID pgtype.UUID
	LotID      pgtype.UUID
	Source     string
	Status     string
	// Consignment dimension (spec 17b §6.1): the supplier of consignment
	// balances; zero value = NULL for every other source.
	SupplierID pgtype.UUID
}

// stockRef names the business document that caused a move.
type stockRef struct {
	RefType string
	RefID   pgtype.UUID
}

type stockMoveInput struct {
	Key            stockKey
	ItemID         pgtype.UUID
	Qty            pgtype.Numeric // signed; negative = stock leaves the balance
	UnitCost       pgtype.Numeric
	MoveType       string
	RefType        string
	Reason         string
	OverrideReason string
	RefID          pgtype.UUID
	ReversalOf     pgtype.UUID // set on reversal moves (spec §4.1.7)
	MoveDate       time.Time
	ReceivedAt     time.Time // only used when the balance row is created here
}

// applyStockMove writes one move and its balance delta inside the caller's
// transaction: it enforces the merchant's stock period lock, locks the balance
// row (creating it when absent, with received_first_at only on creation), and
// rejects moves that would take the balance below zero.
func applyStockMove(ctx context.Context, q *sqlcgen.Queries, merchantID, companyID, userID pgtype.UUID, in stockMoveInput) (pgtype.UUID, error) {
	lockedUntil, err := q.UpsertMerchantStockSetting(ctx, sqlcgen.UpsertMerchantStockSettingParams{
		MerchantID: merchantID,
		CompanyID:  companyID,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("read stock setting: %w", err)
	}
	if lockedUntil.Valid && !dayAfterDate(in.MoveDate, lockedUntil.Time) {
		return pgtype.UUID{}, errStockPeriodLocked
	}

	// Create the balance row if absent, then lock it. received_first_at is
	// only set on creation; later moves never touch it.
	received := pgtype.Timestamptz{Time: in.ReceivedAt, Valid: !in.ReceivedAt.IsZero()}
	if _, err := q.InsertStockBalanceIfAbsent(ctx, sqlcgen.InsertStockBalanceIfAbsentParams{
		CompanyID:  companyID,
		MerchantID: merchantID,
		LocationID: in.Key.LocationID,
		ItemID:     in.ItemID,
		LotID:      in.Key.LotID,
		Source:     in.Key.Source,
		Status:     in.Key.Status,
		SupplierID: in.Key.SupplierID,
		ReceivedAt: received,
	}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, fmt.Errorf("ensure balance row: %w", err)
	}
	row, err := q.LockStockBalance(ctx, sqlcgen.LockStockBalanceParams{
		MerchantID: merchantID,
		LocationID: in.Key.LocationID,
		LotID:      in.Key.LotID,
		Source:     in.Key.Source,
		Status:     in.Key.Status,
		SupplierID: in.Key.SupplierID,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("lock balance: %w", err)
	}

	balMilli, errBal := numericToMilli(row.Quantity)
	qtyMilli, errQty := numericToMilli(in.Qty)
	if errBal != nil || errQty != nil {
		return pgtype.UUID{}, fmt.Errorf("balance quantity outside numeric(14,3) scale")
	}
	if balMilli+qtyMilli < 0 {
		return pgtype.UUID{}, errInsufficientStock
	}

	moveID, err := q.InsertStockMove(ctx, sqlcgen.InsertStockMoveParams{
		CompanyID:      companyID,
		MerchantID:     merchantID,
		MoveDate:       pgDate(in.MoveDate),
		MoveType:       in.MoveType,
		LocationID:     in.Key.LocationID,
		ItemID:         in.ItemID,
		LotID:          in.Key.LotID,
		Source:         in.Key.Source,
		Status:         in.Key.Status,
		Quantity:       in.Qty,
		UnitCost:       in.UnitCost,
		RefType:        pgText(in.RefType),
		RefID:          in.RefID,
		ReversalOf:     in.ReversalOf,
		Reason:         pgText(in.Reason),
		OverrideReason: pgText(in.OverrideReason),
		CreatedBy:      userID,
	})
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("write stock move: %w", err)
	}
	if err := q.AddStockBalanceQuantity(ctx, sqlcgen.AddStockBalanceQuantityParams{
		Quantity: in.Qty,
		ID:       row.ID,
	}); err != nil {
		return pgtype.UUID{}, fmt.Errorf("update balance: %w", err)
	}
	return moveID, nil
}

// issuedPart is one allocation of an issue: which balance was drawn and at
// what cost (spec §4.4 snapshot for #13).
type issuedPart struct {
	LotID      pgtype.UUID
	Source     string
	SupplierID pgtype.UUID
	Qty        pgtype.Numeric
	UnitCost   pgtype.Numeric
}

type stockCandidate struct {
	row   sqlcgen.LockIssuableStockRow
	milli int64
}

// issueStock allocates qty across eligible balances at one location (spec
// §4.4): available status, lot not recalled, ED NULL or >= onDate, ordered
// FEFO (ED ASC NULLS LAST) then FIFO (received_first_at) then consignment
// last then id. All candidates are locked FOR UPDATE in id order before any
// number is computed. An override lot is drawn first and requires a reason.
// If the candidates cannot cover qty nothing is written.
func issueStock(ctx context.Context, q *sqlcgen.Queries, merchantID, companyID, userID pgtype.UUID, locationID, itemID pgtype.UUID, qty pgtype.Numeric, onDate time.Time, ref stockRef, overrideLotID pgtype.UUID, overrideReason string) ([]issuedPart, error) {
	qtyMilli, errQty := numericToMilli(qty)
	if errQty != nil || qtyMilli <= 0 {
		return nil, errInsufficientStock
	}
	if overrideLotID.Valid && strings.TrimSpace(overrideReason) == "" {
		return nil, errOverrideReason
	}

	rows, err := q.LockIssuableStock(ctx, sqlcgen.LockIssuableStockParams{
		MerchantID: merchantID,
		LocationID: locationID,
		ItemID:     itemID,
		ExpiryDate: pgDate(onDate),
	})
	if err != nil {
		return nil, fmt.Errorf("lock issuables: %w", err)
	}
	cands := make([]stockCandidate, 0, len(rows))
	var total int64
	for _, row := range rows {
		m, err := numericToMilli(row.Quantity)
		if err != nil {
			return nil, fmt.Errorf("balance quantity outside numeric(14,3) scale")
		}
		cands = append(cands, stockCandidate{row: row, milli: m})
		total += m
	}
	if total < qtyMilli {
		return nil, errInsufficientStock
	}

	sort.SliceStable(cands, func(i, j int) bool { return lessStockCandidate(cands[i], cands[j]) })
	if overrideLotID.Valid {
		idx := -1
		for i, cand := range cands {
			if cand.row.LotID == overrideLotID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, errInsufficientStock
		}
		picked := cands[idx]
		cands = append(cands[:idx], cands[idx+1:]...)
		cands = append([]stockCandidate{picked}, cands...)
	}

	avg, err := q.GetItemAvgCost(ctx, sqlcgen.GetItemAvgCostParams{MerchantID: merchantID, ItemID: itemID})
	if err != nil {
		return nil, fmt.Errorf("read avg cost: %w", err)
	}
	zero := pgtype.Numeric{Valid: true}

	// Phase 1 — pure allocation: how much each locked balance contributes.
	type stockTake struct {
		cand stockCandidate
		take int64
	}
	remaining := qtyMilli
	takes := make([]stockTake, 0, len(cands))
	for _, cand := range cands {
		if remaining == 0 {
			break
		}
		take := cand.milli
		if take > remaining {
			take = remaining
		}
		remaining -= take
		takes = append(takes, stockTake{cand: cand, take: take})
	}

	// Phase 2 — pre-write consignment validation (spec 17b §6.2 #1): every
	// consignment part needs a consignment-kind contract price valid on the
	// issue date; a single missing price rejects the WHOLE allocation before
	// the first move is written.
	consignmentPrices := make(map[pgtype.UUID]pgtype.Numeric)
	for _, tk := range takes {
		if tk.cand.row.Source != "consignment" {
			continue
		}
		supplier := tk.cand.row.SupplierID
		if _, seen := consignmentPrices[supplier]; seen {
			continue
		}
		item, err := q.GetInventoryItemByID(ctx, itemID)
		if err != nil {
			return nil, fmt.Errorf("read item: %w", err)
		}
		// The contract keys its price to an inventory.item_unit row, not the
		// inventory.unit row the item's base_unit_id names (Amandemen 2
		// 2026-10-21): resolve the base unit's packaging row and price
		// against it (its factor is the pack_content).
		units, err := q.ListInventoryItemUnits(ctx, itemID)
		if err != nil {
			return nil, fmt.Errorf("read item units: %w", err)
		}
		var baseItemUnitID pgtype.UUID
		for _, u := range units {
			if u.UnitID == item.BaseUnitID {
				baseItemUnitID = u.ID
				break
			}
		}
		if !baseItemUnitID.Valid {
			return nil, errConsignmentPriceMissing
		}
		contract, err := q.GetEffectiveContractPrice(ctx, sqlcgen.GetEffectiveContractPriceParams{
			CompanyID:  companyID,
			SupplierID: supplier,
			ItemID:     itemID,
			ItemUnitID: baseItemUnitID,
			Kind:       "consignment",
			Column6:    pgDate(onDate),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errConsignmentPriceMissing
		}
		if err != nil {
			return nil, fmt.Errorf("read consignment contract price: %w", err)
		}
		// The contract prices one pack of its item unit; the base-unit
		// equivalent divides by that unit's packaging factor
		// (unit_price = pack_price ÷ pack_content, spec 17b §6.2 #1).
		unit, err := q.GetInventoryItemUnitByID(ctx, contract.ItemUnitID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!unit.Factor.Valid || unit.Factor.Int == nil || unit.Factor.Int.Sign() == 0)) {
			return nil, errConsignmentPriceMissing
		}
		if err != nil {
			return nil, fmt.Errorf("read contract item unit: %w", err)
		}
		unitPrice := roundRatHalfUp(new(big.Rat).Quo(numericToRat(contract.PackPrice), numericToRat(unit.Factor)), 6)
		price, errP := parseNumeric(unitPrice.FloatString(6))
		if errP != nil {
			return nil, fmt.Errorf("normalize consignment unit price: %w", errP)
		}
		consignmentPrices[supplier] = price
	}
	var loc *time.Location
	if len(consignmentPrices) > 0 {
		if loc, err = merchantLocation(ctx, q, merchantID); err != nil {
			return nil, fmt.Errorf("read merchant timezone: %w", err)
		}
	}

	// Phase 3 — writes: one negative move per part, then the consignment
	// usage row for consignment parts (spec 17b §6.2 #1).
	parts := make([]issuedPart, 0, len(takes))
	for _, tk := range takes {
		cand := tk.cand
		unit := zero
		if cand.row.Source == "purchase" || cand.row.Source == "consignment" {
			unit = avg
		}
		if _, err := applyStockMove(ctx, q, merchantID, companyID, userID, stockMoveInput{
			Key:      stockKey{LocationID: locationID, LotID: cand.row.LotID, Source: cand.row.Source, Status: "available", SupplierID: cand.row.SupplierID},
			ItemID:   itemID,
			Qty:      numericFromMilli(-tk.take),
			UnitCost: unit,
			MoveType: "issue",
			RefType:  ref.RefType,
			RefID:    ref.RefID,
			MoveDate: onDate,
		}); err != nil {
			return nil, err
		}
		parts = append(parts, issuedPart{
			LotID:      cand.row.LotID,
			Source:     cand.row.Source,
			SupplierID: cand.row.SupplierID,
			Qty:        numericFromMilli(tk.take),
			UnitCost:   unit,
		})
		if cand.row.Source != "consignment" {
			continue
		}
		if _, err := q.InsertConsignmentUsage(ctx, sqlcgen.InsertConsignmentUsageParams{
			CompanyID:  companyID,
			MerchantID: merchantID,
			SupplierID: cand.row.SupplierID,
			ItemID:     itemID,
			LotID:      cand.row.LotID,
			BaseQty:    numericFromMilli(tk.take),
			UnitPrice:  consignmentPrices[cand.row.SupplierID],
			UsedOn:     pgDate(localDayStart(onDate, loc)),
			RefType:    pgText(ref.RefType),
			RefID:      ref.RefID,
			CreatedBy:  userID,
		}); err != nil {
			return nil, fmt.Errorf("write consignment usage: %w", err)
		}
	}
	return parts, nil
}

// lessStockCandidate implements the spec §4.4 pick order: ED ASC NULLS LAST,
// then received_first_at, then consignment last, then id.
func lessStockCandidate(a, b stockCandidate) bool {
	if a.row.ExpiryDate.Valid != b.row.ExpiryDate.Valid {
		return a.row.ExpiryDate.Valid
	}
	if a.row.ExpiryDate.Valid {
		ad, bd := a.row.ExpiryDate.Time, b.row.ExpiryDate.Time
		if !ad.Equal(bd) {
			return ad.Before(bd)
		}
	}
	at, bt := a.row.ReceivedFirstAt.Time, b.row.ReceivedFirstAt.Time
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	ac, bc := a.row.Source == "consignment", b.row.Source == "consignment"
	if ac != bc {
		return bc // non-consignment is picked before consignment
	}
	return strings.Compare(a.row.ID.String(), b.row.ID.String()) < 0
}

// dayAfterDate reports whether t's calendar day is strictly after ref's.
func dayAfterDate(t, ref time.Time) bool {
	ty, tm, td := t.Date()
	ry, rm, rd := ref.Date()
	return ty > ry || (ty == ry && (tm > rm || (tm == rm && td > rd)))
}

// pgDate normalizes t to midnight UTC of its own calendar day.
func pgDate(t time.Time) pgtype.Date {
	y, m, d := t.Date()
	return pgtype.Date{Time: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), Valid: true}
}

func pgText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

// numericFromMilli builds a numeric(14,3)-compatible value from thousandths.
// numericToMilli (drug_price.go) is the exact inverse for values at this scale.
func numericFromMilli(milli int64) pgtype.Numeric {
	return pgtype.Numeric{Int: big.NewInt(milli), Exp: -3, Valid: true}
}

// balanceSupplierAtSlot resolves the supplier dimension carried by the
// merchant's balance rows at one slot (location, lot, source, status —
// amendment 2026-10-21 / spec 17b §6.1): zero UUID when the rows carry no
// supplier, errConsignmentSupplierAmbiguous when they disagree.
func balanceSupplierAtSlot(ctx context.Context, tx pgx.Tx, merchantID, locationID, lotID pgtype.UUID, source, status string) (pgtype.UUID, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT COALESCE(supplier_id, '00000000-0000-0000-0000-000000000000'::uuid)
		FROM inventory.stock_balance
		WHERE merchant_id = $1 AND location_id = $2 AND lot_id = $3 AND source = $4 AND status = $5`,
		merchantID, locationID, lotID, source, status)
	if err != nil {
		return pgtype.UUID{}, err
	}
	defer rows.Close()
	var found []pgtype.UUID
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			return pgtype.UUID{}, err
		}
		found = append(found, id)
	}
	if err := rows.Err(); err != nil {
		return pgtype.UUID{}, err
	}
	switch len(found) {
	case 0:
		return pgtype.UUID{}, nil
	case 1:
		return found[0], nil
	default:
		return pgtype.UUID{}, errConsignmentSupplierAmbiguous
	}
}

// balanceSupplierByID resolves the supplier dimension of one balance row by
// its id (zero UUID when NULL; unknown ids read as NULL).
func balanceSupplierByID(ctx context.Context, tx pgx.Tx, merchantID, balanceID pgtype.UUID) (pgtype.UUID, error) {
	var id pgtype.UUID
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(supplier_id, '00000000-0000-0000-0000-000000000000'::uuid)
		FROM inventory.stock_balance WHERE merchant_id = $1 AND id = $2`,
		merchantID, balanceID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil
	}
	return id, err
}
