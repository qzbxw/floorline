package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"floorline/internal/tonnel"
)

// ---- listings -----------------------------------------------------------

// ListingChanges reports what a batch of order-book rows changed.
type ListingChanges struct {
	// New are gift ids seen for the very first time.
	New []int64
	// Cheaper are gift ids that were already known but have just been repriced
	// downwards. A relist at a lower price is a fresh opportunity even though
	// the gift id has not changed, so the feed must not treat it as old news.
	Cheaper []int64
}

// Candidates is New and Cheaper combined, in that order.
func (c ListingChanges) Candidates() []int64 {
	out := make([]int64, 0, len(c.New)+len(c.Cheaper))
	out = append(out, c.New...)
	return append(out, c.Cheaper...)
}

// UpsertListings writes a batch of order-book rows and reports what changed.
func (s *Store) UpsertListings(ctx context.Context, gifts []tonnel.Gift, now time.Time) (ListingChanges, error) {
	var changes ListingChanges
	if len(gifts) == 0 {
		return changes, nil
	}

	ids := make([]int64, 0, len(gifts))
	for i := range gifts {
		ids = append(ids, gifts[i].GiftID.Int())
	}
	known, err := s.existingListingIDs(ctx, ids)
	if err != nil {
		return changes, err
	}

	err = s.tx(ctx, func(t *sql.Tx) error {
		stmt, err := t.PrepareContext(ctx, `
INSERT INTO listings (
    gift_id, gift_num, name, model, backdrop, symbol, price, asset,
    rarity, model_rarity, backdrop_rarity, symbol_rarity, seller,
    premarket, tg_marketplace, is_bundle, export_at, posted_at,
    first_seen, last_seen, gone_at
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)
ON CONFLICT(gift_id) DO UPDATE SET
    price     = excluded.price,
    seller    = excluded.seller,
    last_seen = excluded.last_seen,
    gone_at   = NULL`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		ts := unix(now)
		for i := range gifts {
			g := &gifts[i]
			id := g.GiftID.Int()
			modelBase, modelRarity := tonnel.SplitAttr(g.Model)
			backdropBase, backdropRarity := tonnel.SplitAttr(g.Backdrop)
			symbolBase, symbolRarity := tonnel.SplitAttr(g.Symbol)

			// filterStats carries rarity for models; individual rows sometimes
			// omit the numeric fields, so fall back to the suffix we parsed.
			mr := g.ModelRarity.Float()
			if mr == 0 {
				mr = modelRarity
			}
			br := g.BackdropRarity.Float()
			if br == 0 {
				br = backdropRarity
			}
			sr := g.SymbolRarity.Float()
			if sr == 0 {
				sr = symbolRarity
			}

			if _, err := stmt.ExecContext(ctx,
				id, g.GiftNum.Int(), g.Name, modelBase, backdropBase, symbolBase,
				g.Price.Float(), g.Asset,
				g.Rarity.Float(), mr, br, sr, g.Seller.Int(),
				boolInt(g.Premarket.Bool()), boolInt(g.TelegramMarketplace.Bool()), boolInt(g.IsBundle()),
				unix(g.ExportAt.Time), unix(g.MessagePostTime.Time),
				ts, ts,
			); err != nil {
				return fmt.Errorf("upsert listing %d: %w", id, err)
			}
			prev, seen := known[id]
			switch {
			case !seen:
				changes.New = append(changes.New, id)
			case g.Price.Float() > 0 && prev > 0 && g.Price.Float() < prev:
				changes.Cheaper = append(changes.Cheaper, id)
			}
		}
		return nil
	})
	return changes, err
}

// existingListingIDs returns the last known price of each listing we already
// have, so a repricing can be told apart from a brand new ask.
func (s *Store) existingListingIDs(ctx context.Context, ids []int64) (map[int64]float64, error) {
	if len(ids) == 0 {
		return map[int64]float64{}, nil
	}
	q := `SELECT gift_id, COALESCE(price,0) FROM listings WHERE gift_id IN (` + placeholders(len(ids)) + `)`
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]float64, len(ids))
	for rows.Next() {
		var id int64
		var price float64
		if err := rows.Scan(&id, &price); err != nil {
			return nil, err
		}
		out[id] = price
	}
	return out, rows.Err()
}

// MarkGoneForModel records that listings of a model which were not in the latest
// book snapshot have left the market. Combined with first_seen this yields how
// long asks survive — the empirical basis for "how fast does this model sell".
func (s *Store) MarkGoneForModel(ctx context.Context, key tonnel.ModelKey, present []int64, now time.Time) error {
	q := `UPDATE listings SET gone_at = ?
	      WHERE name = ? AND model = ? AND gone_at IS NULL`
	args := []any{unix(now), key.Name, key.Model}
	if len(present) > 0 {
		q += ` AND gift_id NOT IN (` + placeholders(len(present)) + `)`
		for _, id := range present {
			args = append(args, id)
		}
	}
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}

// ListingFirstSeen returns when a listing was first observed, or zero.
func (s *Store) ListingFirstSeen(ctx context.Context, giftID int64) (time.Time, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT first_seen FROM listings WHERE gift_id = ?`, giftID).Scan(&n)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	return fromUnix(n), err
}

// ---- sales --------------------------------------------------------------

// InsertSales stores executed trades, ignoring ones already recorded, and
// reports how many rows were genuinely new. The pollers use that count to know
// when they have caught up with the tail of the history.
func (s *Store) InsertSales(ctx context.Context, sales []tonnel.Sale) (int, error) {
	if len(sales) == 0 {
		return 0, nil
	}
	var inserted int
	err := s.tx(ctx, func(t *sql.Tx) error {
		stmt, err := t.PrepareContext(ctx, `
INSERT OR IGNORE INTO sales (gift_id, ts, gift_num, name, model, backdrop, symbol, price, asset, type)
VALUES (?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		for i := range sales {
			sale := &sales[i]
			when := sale.When()
			if when.IsZero() || sale.Name() == "" || sale.Price.Float() <= 0 {
				continue
			}
			res, err := stmt.ExecContext(ctx,
				sale.GiftID.Int(), when.Unix(), sale.GiftNum.Int(), sale.Name(),
				tonnel.BaseAttr(sale.Model), tonnel.BaseAttr(sale.Backdrop), tonnel.BaseAttr(sale.Symbol),
				sale.Price.Float(), sale.Asset, sale.Type,
			)
			if err != nil {
				return fmt.Errorf("insert sale %d: %w", sale.GiftID.Int(), err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				inserted++
			}
		}
		return nil
	})
	return inserted, err
}

// SaleRow is one stored trade.
type SaleRow struct {
	GiftID   int64
	GiftNum  int64
	TS       time.Time
	Price    float64
	Backdrop string
	Symbol   string
}

type GramQuote struct {
	TS       time.Time
	USD      float64
	Bid      float64
	Ask      float64
	Change24 float64
}

func (s *Store) InsertGramQuotes(ctx context.Context, quotes []GramQuote) error {
	if len(quotes) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO gram_quotes(ts,usd,bid,ask,change_24) VALUES(?,?,?,?,?) ON CONFLICT(ts) DO UPDATE SET usd=excluded.usd,bid=excluded.bid,ask=excluded.ask,change_24=excluded.change_24`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, q := range quotes {
			if q.USD <= 0 || q.TS.IsZero() {
				continue
			}
			if _, err := stmt.ExecContext(ctx, unix(q.TS), q.USD, nullFloat(q.Bid), nullFloat(q.Ask), q.Change24); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) LatestGramQuote(ctx context.Context) (GramQuote, bool, error) {
	return s.gramQuote(ctx, `SELECT ts,usd,COALESCE(bid,0),COALESCE(ask,0),COALESCE(change_24,0) FROM gram_quotes ORDER BY ts DESC LIMIT 1`)
}

func (s *Store) GramQuoteAt(ctx context.Context, at time.Time) (GramQuote, bool, error) {
	return s.gramQuote(ctx, `SELECT ts,usd,COALESCE(bid,0),COALESCE(ask,0),COALESCE(change_24,0) FROM gram_quotes WHERE ts<=? ORDER BY ts DESC LIMIT 1`, unix(at))
}

func (s *Store) gramQuote(ctx context.Context, q string, args ...any) (GramQuote, bool, error) {
	var out GramQuote
	var ts int64
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&ts, &out.USD, &out.Bid, &out.Ask, &out.Change24)
	if err == sql.ErrNoRows {
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	out.TS = fromUnix(ts)
	return out, true, nil
}

func (s *Store) GramQuotesSince(ctx context.Context, since time.Time) ([]GramQuote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts,usd,COALESCE(bid,0),COALESCE(ask,0),COALESCE(change_24,0) FROM gram_quotes WHERE ts>=? ORDER BY ts`, unix(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GramQuote
	for rows.Next() {
		var q GramQuote
		var ts int64
		if err := rows.Scan(&ts, &q.USD, &q.Bid, &q.Ask, &q.Change24); err != nil {
			return nil, err
		}
		q.TS = fromUnix(ts)
		out = append(out, q)
	}
	return out, rows.Err()
}

// SalesSince returns a model's trades newer than `since`, oldest first.
func (s *Store) SalesSince(ctx context.Context, key tonnel.ModelKey, since time.Time) ([]SaleRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT gift_id, COALESCE(gift_num,0), ts, price, COALESCE(backdrop,''), COALESCE(symbol,'')
FROM sales
WHERE name = ? AND model = ? AND ts >= ? AND price > 0
ORDER BY ts ASC`, key.Name, key.Model, unix(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SaleRow
	for rows.Next() {
		var r SaleRow
		var ts int64
		if err := rows.Scan(&r.GiftID, &r.GiftNum, &ts, &r.Price, &r.Backdrop, &r.Symbol); err != nil {
			return nil, err
		}
		r.TS = fromUnix(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentSalesForCollection returns the newest trades in a collection.
func (s *Store) RecentSalesForCollection(ctx context.Context, name string, limit int) ([]CollectionSale, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT model, ts, price FROM sales
WHERE name = ? AND price > 0
ORDER BY ts DESC LIMIT ?`, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CollectionSale
	for rows.Next() {
		var c CollectionSale
		var ts int64
		if err := rows.Scan(&c.Model, &ts, &c.Price); err != nil {
			return nil, err
		}
		c.TS = fromUnix(ts)
		out = append(out, c)
	}
	return out, rows.Err()
}

// CollectionSale is a trade summarised for collection-level views.
type CollectionSale struct {
	Model string
	TS    time.Time
	Price float64
}

// OldestSaleTime and NewestSaleTime bound the history we hold, which is how the
// warm-up progress is reported.
func (s *Store) OldestSaleTime(ctx context.Context) (time.Time, error) {
	return s.scalarTime(ctx, `SELECT COALESCE(MIN(ts),0) FROM sales`)
}

// NewestSaleTime returns the most recent stored trade time.
func (s *Store) NewestSaleTime(ctx context.Context) (time.Time, error) {
	return s.scalarTime(ctx, `SELECT COALESCE(MAX(ts),0) FROM sales`)
}

func (s *Store) scalarTime(ctx context.Context, q string) (time.Time, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return time.Time{}, err
	}
	return fromUnix(n), nil
}

// CountSales returns the total number of stored trades.
func (s *Store) CountSales(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sales`).Scan(&n)
	return n, err
}

// SweepCandidates finds models with an unusual burst of trades in a short
// window — the fingerprint of someone sweeping a collection.
func (s *Store) SweepCandidates(ctx context.Context, since time.Time, minSales int) ([]SweepRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT name, model, COUNT(*) AS n, MIN(price), MAX(price), COUNT(DISTINCT gift_num)
FROM sales
WHERE ts >= ?
GROUP BY name, model
HAVING n >= ?
ORDER BY n DESC
LIMIT 20`, unix(since), minSales)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SweepRow
	for rows.Next() {
		var r SweepRow
		if err := rows.Scan(&r.Key.Name, &r.Key.Model, &r.Count, &r.MinPrice, &r.MaxPrice, &r.Gifts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SweepRow describes a burst of trades on one model.
type SweepRow struct {
	Key      tonnel.ModelKey
	Count    int
	MinPrice float64
	MaxPrice float64
	// Gifts is the number of distinct physical gifts involved. One gift traded
	// five times is not a sweep.
	Gifts int
}

// ---- model stats --------------------------------------------------------

// ModelStatRow is the current market state of one model.
type ModelStatRow struct {
	Key    tonnel.ModelKey
	Floor  float64
	Supply int
	Rarity float64
	TS     time.Time
}

// ReplaceModelStats overwrites the current full-market snapshot.
func (s *Store) ReplaceModelStats(ctx context.Context, stats []tonnel.ModelStat, now time.Time) error {
	if len(stats) == 0 {
		return nil
	}
	return s.tx(ctx, func(t *sql.Tx) error {
		stmt, err := t.PrepareContext(ctx, `
INSERT INTO model_current (name, model, floor, supply, rarity, ts)
VALUES (?,?,?,?,?,?)
ON CONFLICT(name, model) DO UPDATE SET
    floor = excluded.floor, supply = excluded.supply,
    rarity = excluded.rarity, ts = excluded.ts`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		ts := unix(now)
		for _, st := range stats {
			if _, err := stmt.ExecContext(ctx, st.Key.Name, st.Key.Model, st.Floor, st.Supply, st.Rarity, ts); err != nil {
				return err
			}
		}
		return nil
	})
}

// SnapshotModelHistory appends the current snapshot to the sparse history table.
func (s *Store) SnapshotModelHistory(ctx context.Context, stats []tonnel.ModelStat, now time.Time) error {
	if len(stats) == 0 {
		return nil
	}
	return s.tx(ctx, func(t *sql.Tx) error {
		stmt, err := t.PrepareContext(ctx, `
INSERT OR REPLACE INTO model_history (ts, name, model, floor, supply) VALUES (?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		ts := unix(now)
		for _, st := range stats {
			if st.Floor <= 0 {
				continue
			}
			if _, err := stmt.ExecContext(ctx, ts, st.Key.Name, st.Key.Model, st.Floor, st.Supply); err != nil {
				return err
			}
		}
		return nil
	})
}

// ModelStat returns the current snapshot for one model.
func (s *Store) ModelStat(ctx context.Context, key tonnel.ModelKey) (*ModelStatRow, error) {
	var r ModelStatRow
	var ts int64
	err := s.db.QueryRowContext(ctx, `
SELECT name, model, COALESCE(floor,0), COALESCE(supply,0), COALESCE(rarity,0), ts
FROM model_current WHERE name = ? AND model = ?`, key.Name, key.Model).
		Scan(&r.Key.Name, &r.Key.Model, &r.Floor, &r.Supply, &r.Rarity, &ts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.TS = fromUnix(ts)
	return &r, nil
}

// ModelStats returns the whole current market snapshot.
//
// The scanner needs every model at once: it ranks the market by how much each
// one trades and then walks the busiest slice, which cannot be done a
// collection at a time.
func (s *Store) ModelStats(ctx context.Context) ([]ModelStatRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT name, model, COALESCE(floor,0), COALESCE(supply,0), COALESCE(rarity,0), ts
FROM model_current WHERE floor > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ModelStatRow
	for rows.Next() {
		var r ModelStatRow
		var ts int64
		if err := rows.Scan(&r.Key.Name, &r.Key.Model, &r.Floor, &r.Supply, &r.Rarity, &ts); err != nil {
			return nil, err
		}
		r.TS = fromUnix(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ModelsForCollection lists a collection's models, cheapest floor first.
func (s *Store) ModelsForCollection(ctx context.Context, name string) ([]ModelStatRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT name, model, COALESCE(floor,0), COALESCE(supply,0), COALESCE(rarity,0), ts
FROM model_current WHERE name = ? ORDER BY floor ASC`, tonnel.TitleCase(name))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ModelStatRow
	for rows.Next() {
		var r ModelStatRow
		var ts int64
		if err := rows.Scan(&r.Key.Name, &r.Key.Model, &r.Floor, &r.Supply, &r.Rarity, &ts); err != nil {
			return nil, err
		}
		r.TS = fromUnix(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CollectionNames lists every known collection.
func (s *Store) CollectionNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT name FROM model_current ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// SearchCollections finds collections whose name contains the query.
func (s *Store) SearchCollections(ctx context.Context, q string, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT name FROM model_current WHERE name LIKE ? ORDER BY name LIMIT ?`,
		"%"+q+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// FloorAt returns the newest recorded floor at or before `at`, used to measure
// how far a floor has moved over a window.
func (s *Store) FloorAt(ctx context.Context, key tonnel.ModelKey, at time.Time) (float64, bool, error) {
	var f float64
	err := s.db.QueryRowContext(ctx, `
SELECT floor FROM model_history
WHERE name = ? AND model = ? AND ts <= ?
ORDER BY ts DESC LIMIT 1`, key.Name, key.Model, unix(at)).Scan(&f)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return f, true, nil
}

// ---- positions ----------------------------------------------------------

// Position is an owned gift tracked from purchase to sale.
type Position struct {
	GiftID         int64
	GiftNum        int64
	Key            tonnel.ModelKey
	Backdrop       string
	Symbol         string
	ModelRarity    float64
	BackdropRarity float64
	SymbolRarity   float64
	BuyPrice       float64
	CostSource     string
	CostConfidence float64
	BoughtAt       time.Time
	ListPrice      float64
	ListedAt       time.Time
	SellPrice      float64
	SoldAt         time.Time
	MissingSince   time.Time
	Status         string
	Source         string
	Note           string
}

// Position status values.
const (
	StatusOpen     = "open"
	StatusListed   = "listed"
	StatusSold     = "sold"
	StatusReturned = "returned"
	StatusMissing  = "missing"
)

// UpsertPosition inserts or refreshes a position without clobbering fields that
// are already set (a later reconciliation must not erase a recorded sale).
func (s *Store) UpsertPosition(ctx context.Context, p Position) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO positions (gift_id, gift_num, name, model, backdrop, symbol,
                       model_rarity, backdrop_rarity, symbol_rarity,
                       buy_price, cost_source, cost_confidence, bought_at, list_price, listed_at,
					   sell_price, sold_at, missing_since, status, source, note)
	VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(gift_id) DO UPDATE SET
	backdrop = excluded.backdrop,
	symbol = excluded.symbol,
	model_rarity = excluded.model_rarity,
	backdrop_rarity = excluded.backdrop_rarity,
	symbol_rarity = excluded.symbol_rarity,
	buy_price = CASE WHEN positions.status IN ('sold','returned') THEN excluded.buy_price WHEN positions.buy_price > 0 THEN positions.buy_price ELSE excluded.buy_price END,
	cost_source = CASE WHEN positions.status IN ('sold','returned') THEN excluded.cost_source WHEN positions.buy_price > 0 THEN positions.cost_source ELSE excluded.cost_source END,
	cost_confidence = CASE WHEN positions.status IN ('sold','returned') THEN excluded.cost_confidence ELSE MAX(positions.cost_confidence, excluded.cost_confidence) END,
	bought_at = CASE WHEN positions.status IN ('sold','returned') THEN excluded.bought_at ELSE positions.bought_at END,
	    list_price = CASE WHEN positions.status IN ('sold','returned') THEN excluded.list_price ELSE COALESCE(excluded.list_price, positions.list_price) END,
	    listed_at  = CASE WHEN positions.status IN ('sold','returned') THEN excluded.listed_at ELSE COALESCE(NULLIF(excluded.listed_at,0), positions.listed_at) END,
	    sell_price = CASE WHEN positions.status IN ('sold','returned') THEN excluded.sell_price ELSE COALESCE(excluded.sell_price, positions.sell_price) END,
	    sold_at    = CASE WHEN positions.status IN ('sold','returned') THEN excluded.sold_at ELSE COALESCE(NULLIF(excluded.sold_at,0), positions.sold_at) END,
	missing_since = CASE WHEN positions.status IN ('sold','returned') THEN excluded.missing_since WHEN excluded.missing_since > 0 THEN excluded.missing_since ELSE positions.missing_since END,
	    status     = excluded.status,
	source = CASE WHEN positions.status IN ('sold','returned') THEN excluded.source ELSE positions.source END,
	note = CASE WHEN positions.status IN ('sold','returned') THEN excluded.note ELSE positions.note END`,
		p.GiftID, p.GiftNum, p.Key.Name, p.Key.Model, p.Backdrop, p.Symbol,
		p.ModelRarity, p.BackdropRarity, p.SymbolRarity,
		p.BuyPrice, p.CostSource, p.CostConfidence, unix(p.BoughtAt), nullFloat(p.ListPrice), unix(p.ListedAt),
		nullFloat(p.SellPrice), unix(p.SoldAt), unix(p.MissingSince), p.Status, p.Source, p.Note)
	return err
}

// SetPositionListed records that a position is now on the market.
func (s *Store) SetPositionListed(ctx context.Context, giftID int64, price float64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE positions SET list_price = ?, listed_at = ?, missing_since = NULL, status = ? WHERE gift_id = ?`,
		price, unix(at), StatusListed, giftID)
	return err
}

func (s *Store) SetPositionUnlisted(ctx context.Context, giftID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE positions SET list_price=NULL, listed_at=NULL, missing_since=NULL, status=? WHERE gift_id=?`, StatusOpen, giftID)
	return err
}

func (s *Store) SetPositionMissing(ctx context.Context, giftID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE positions SET missing_since=CASE WHEN missing_since IS NULL OR missing_since=0 THEN ? ELSE missing_since END, status=? WHERE gift_id=?`, unix(at), StatusMissing, giftID)
	return err
}

// SetPositionSold closes a position.
func (s *Store) SetPositionSold(ctx context.Context, giftID int64, price float64, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO position_trades(gift_id,name,model,buy_price,bought_at,sell_price,sold_at,source) SELECT gift_id,name,model,buy_price,bought_at,?,?,source FROM positions WHERE gift_id=?`, price, unix(at), giftID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE positions SET sell_price=?,sold_at=?,missing_since=NULL,status=? WHERE gift_id=?`, price, unix(at), StatusSold, giftID)
		return err
	})
}

// GetPosition loads one position.
func (s *Store) GetPosition(ctx context.Context, giftID int64) (*Position, error) {
	rows, err := s.db.QueryContext(ctx, positionSelect+` WHERE gift_id = ?`, giftID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanPositions(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// OpenPositions returns everything not yet sold, oldest purchase first.
func (s *Store) OpenPositions(ctx context.Context) ([]Position, error) {
	rows, err := s.db.QueryContext(ctx,
		positionSelect+` WHERE status IN ('open','listed') ORDER BY bought_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPositions(rows)
}

// TrackedPositions includes unresolved inventory disappearances so later sale
// history can close them without falsely booking the last ask as proceeds.
func (s *Store) TrackedPositions(ctx context.Context) ([]Position, error) {
	rows, err := s.db.QueryContext(ctx, positionSelect+` WHERE status IN ('open','listed','missing') ORDER BY bought_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPositions(rows)
}

// ClosedPositions returns sold positions, newest first.
func (s *Store) ClosedPositions(ctx context.Context, limit int) ([]Position, error) {
	rows, err := s.db.QueryContext(ctx,
		positionSelect+` WHERE status = 'sold' ORDER BY sold_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPositions(rows)
}

type PositionTrade struct {
	GiftID              int64
	Key                 tonnel.ModelKey
	BuyPrice, SellPrice float64
	BoughtAt, SoldAt    time.Time
	Source              string
}

func (s *Store) PositionTrades(ctx context.Context, limit int) ([]PositionTrade, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT gift_id,name,model,COALESCE(buy_price,0),COALESCE(bought_at,0),COALESCE(sell_price,0),sold_at,COALESCE(source,'') FROM position_trades ORDER BY sold_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PositionTrade
	for rows.Next() {
		var r PositionTrade
		var bought, sold int64
		if err := rows.Scan(&r.GiftID, &r.Key.Name, &r.Key.Model, &r.BuyPrice, &bought, &r.SellPrice, &sold, &r.Source); err != nil {
			return nil, err
		}
		r.BoughtAt, r.SoldAt = fromUnix(bought), fromUnix(sold)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) PositionTradesForGift(ctx context.Context, giftID int64) ([]PositionTrade, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT gift_id,name,model,COALESCE(buy_price,0),COALESCE(bought_at,0),COALESCE(sell_price,0),sold_at,COALESCE(source,'') FROM position_trades WHERE gift_id=? ORDER BY sold_at DESC`, giftID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PositionTrade
	for rows.Next() {
		var r PositionTrade
		var bought, sold int64
		if err := rows.Scan(&r.GiftID, &r.Key.Name, &r.Key.Model, &r.BuyPrice, &bought, &r.SellPrice, &sold, &r.Source); err != nil {
			return nil, err
		}
		r.BoughtAt, r.SoldAt = fromUnix(bought), fromUnix(sold)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountOpenPositions is a risk-limit input.
func (s *Store) CountOpenPositions(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM positions WHERE status IN ('open','listed')`).Scan(&n)
	return n, err
}

// LastBuyForModel powers the per-model cooldown, which stops the bot from
// accidentally accumulating five of the same thing in one minute.
func (s *Store) LastBuyForModel(ctx context.Context, key tonnel.ModelKey) (time.Time, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(bought_at),0) FROM positions WHERE name = ? AND model = ?`,
		key.Name, key.Model).Scan(&n)
	return fromUnix(n), err
}

const positionSelect = `
SELECT gift_id, COALESCE(gift_num,0), name, model, COALESCE(backdrop,''), COALESCE(symbol,''),
       COALESCE(model_rarity,0), COALESCE(backdrop_rarity,0), COALESCE(symbol_rarity,0),
       buy_price, COALESCE(cost_source,'unknown'), COALESCE(cost_confidence,0), bought_at,
       COALESCE(list_price,0), COALESCE(listed_at,0),
	       COALESCE(sell_price,0), COALESCE(sold_at,0), COALESCE(missing_since,0), status, source, COALESCE(note,'')
FROM positions`

func scanPositions(rows *sql.Rows) ([]Position, error) {
	var out []Position
	for rows.Next() {
		var p Position
		var boughtAt, listedAt, soldAt, missingSince int64
		if err := rows.Scan(&p.GiftID, &p.GiftNum, &p.Key.Name, &p.Key.Model, &p.Backdrop, &p.Symbol,
			&p.ModelRarity, &p.BackdropRarity, &p.SymbolRarity,
			&p.BuyPrice, &p.CostSource, &p.CostConfidence, &boughtAt, &p.ListPrice, &listedAt,
			&p.SellPrice, &soldAt, &missingSince, &p.Status, &p.Source, &p.Note); err != nil {
			return nil, err
		}
		p.BoughtAt, p.ListedAt, p.SoldAt, p.MissingSince = fromUnix(boughtAt), fromUnix(listedAt), fromUnix(soldAt), fromUnix(missingSince)
		out = append(out, p)
	}
	return out, rows.Err()
}

// AcquisitionForGift returns the newest executed trade for a physical gift.
// It is the best available cost basis for inventory acquired outside Floorline.
func (s *Store) AcquisitionForGift(ctx context.Context, giftID int64) (float64, time.Time, bool, error) {
	return s.AcquisitionForGiftAfter(ctx, giftID, time.Time{})
}

func (s *Store) AcquisitionForGiftAfter(ctx context.Context, giftID int64, after time.Time) (float64, time.Time, bool, error) {
	var price float64
	var ts int64
	err := s.db.QueryRowContext(ctx, `SELECT price, ts FROM sales WHERE gift_id = ? AND price > 0 AND ts>? ORDER BY ts DESC LIMIT 1`, giftID, unix(after)).Scan(&price, &ts)
	if err == sql.ErrNoRows {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, err
	}
	return price, fromUnix(ts), true, nil
}

// SetCostBasis records a manually supplied or recovered acquisition price.
func (s *Store) SetCostBasis(ctx context.Context, giftID int64, price float64, source string, confidence float64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE positions SET buy_price=?, cost_source=?, cost_confidence=? WHERE gift_id=?`, price, source, confidence, giftID)
	return err
}

func (s *Store) SetRecoveredCostBasis(ctx context.Context, giftID int64, price float64, boughtAt time.Time, source string, confidence float64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE positions SET buy_price=?, bought_at=?, cost_source=?, cost_confidence=? WHERE gift_id=?`, price, unix(boughtAt), source, confidence, giftID)
	return err
}

// SaleForGiftAfter returns the newest execution after acquisition/listing. It
// cannot accidentally reuse the acquisition trade as sale proceeds.
func (s *Store) SaleForGiftAfter(ctx context.Context, giftID int64, after time.Time) (float64, time.Time, bool, error) {
	var price float64
	var ts int64
	err := s.db.QueryRowContext(ctx, `SELECT price,ts FROM sales WHERE gift_id=? AND price>0 AND ts>? ORDER BY ts DESC LIMIT 1`, giftID, unix(after)).Scan(&price, &ts)
	if err == sql.ErrNoRows {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, err
	}
	return price, fromUnix(ts), true, nil
}

// PositionExposure returns invested cost and counts for concentration checks.
func (s *Store) PositionExposure(ctx context.Context, key tonnel.ModelKey) (modelValue, collectionValue float64, modelCount int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN name=? AND model=? THEN buy_price ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN name=? THEN buy_price ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN name=? AND model=? THEN 1 ELSE 0 END),0)
		FROM positions WHERE status IN ('open','listed')`, key.Name, key.Model, key.Name, key.Name, key.Model).
		Scan(&modelValue, &collectionValue, &modelCount)
	return
}

func (s *Store) RecordReprice(ctx context.Context, giftID int64, oldPrice, newPrice float64, reason string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO reprices(gift_id,ts,old_price,new_price,reason) VALUES(?,?,?,?,?)`, giftID, unix(at), nullFloat(oldPrice), newPrice, reason)
	return err
}

func (s *Store) LastReprice(ctx context.Context, giftID int64) (time.Time, error) {
	var ts int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts),0) FROM reprices WHERE gift_id=?`, giftID).Scan(&ts)
	return fromUnix(ts), err
}

type PositionEvent struct {
	TS                 time.Time
	Kind               string
	OldPrice, NewPrice float64
	Detail             string
}

func (s *Store) RecordPositionEvent(ctx context.Context, giftID int64, kind string, oldPrice, newPrice float64, detail string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO position_events(gift_id,ts,kind,old_price,new_price,detail) VALUES(?,?,?,?,?,?)`, giftID, unix(at), kind, nullFloat(oldPrice), nullFloat(newPrice), detail)
	return err
}

func (s *Store) PositionEvents(ctx context.Context, giftID int64, limit int) ([]PositionEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ts,kind,COALESCE(old_price,0),COALESCE(new_price,0),COALESCE(detail,'') FROM position_events WHERE gift_id=? ORDER BY ts DESC,id DESC LIMIT ?`, giftID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PositionEvent
	for rows.Next() {
		var e PositionEvent
		var ts int64
		if err := rows.Scan(&ts, &e.Kind, &e.OldPrice, &e.NewPrice, &e.Detail); err != nil {
			return nil, err
		}
		e.TS = fromUnix(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

type PositionMark struct {
	TS                                                                      time.Time
	EntryPrice, AskPrice, ModelFloor, RecommendedExit, ExternalRef, GramUSD float64
	Edge, ExpectedDays, Score                                               float64
	Action                                                                  string
}

func (s *Store) InsertPositionMark(ctx context.Context, giftID int64, m PositionMark) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO position_marks(gift_id,ts,entry_price,ask_price,model_floor,recommended_exit,external_ref,gram_usd,edge,expected_days,score,action) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, giftID, unix(m.TS), nullFloat(m.EntryPrice), nullFloat(m.AskPrice), nullFloat(m.ModelFloor), nullFloat(m.RecommendedExit), nullFloat(m.ExternalRef), nullFloat(m.GramUSD), m.Edge, nullFloat(m.ExpectedDays), m.Score, m.Action)
	return err
}

func (s *Store) PositionMarks(ctx context.Context, giftID int64, limit int) ([]PositionMark, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ts,COALESCE(entry_price,0),COALESCE(ask_price,0),COALESCE(model_floor,0),COALESCE(recommended_exit,0),COALESCE(external_ref,0),COALESCE(gram_usd,0),COALESCE(edge,0),COALESCE(expected_days,0),COALESCE(score,0),COALESCE(action,'') FROM position_marks WHERE gift_id=? ORDER BY ts DESC LIMIT ?`, giftID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PositionMark
	for rows.Next() {
		var m PositionMark
		var ts int64
		if err := rows.Scan(&ts, &m.EntryPrice, &m.AskPrice, &m.ModelFloor, &m.RecommendedExit, &m.ExternalRef, &m.GramUSD, &m.Edge, &m.ExpectedDays, &m.Score, &m.Action); err != nil {
			return nil, err
		}
		m.TS = fromUnix(ts)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- signals ------------------------------------------------------------

// SignalRow is a recorded detector output.
type SignalRow struct {
	ID       int64
	TS       time.Time
	Kind     string
	GiftID   int64
	Key      tonnel.ModelKey
	Price    float64
	Exit     float64
	Edge     float64
	Velocity float64
	Score    float64
	Payload  string
	SentAt   time.Time
	Action   string
}

// InsertSignal records a detector output and returns its id.
func (s *Store) InsertSignal(ctx context.Context, sig SignalRow) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
INSERT INTO signals (ts, kind, gift_id, name, model, price, exit_price, edge, velocity, score, payload, sent_at, action)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		unix(sig.TS), sig.Kind, sig.GiftID, sig.Key.Name, sig.Key.Model,
		sig.Price, sig.Exit, sig.Edge, sig.Velocity, sig.Score,
		sig.Payload, unix(sig.SentAt), sig.Action)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetSignalAction records what the trader (or the bot) did with a signal.
func (s *Store) SetSignalAction(ctx context.Context, id int64, action string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE signals SET action = ? WHERE id = ?`, action, id)
	return err
}

// HasSignal reports whether this exact listing already produced this signal kind.
func (s *Store) HasSignal(ctx context.Context, giftID int64, kind string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM signals WHERE gift_id = ? AND kind = ?`, giftID, kind).Scan(&n)
	return n > 0, err
}

// AlreadySignalled reports whether we have already alerted on this listing at
// the same price or cheaper. Plain gift-id dedupe would silence a relist that
// dropped its price, which is exactly the event worth hearing about.
func (s *Store) AlreadySignalled(ctx context.Context, giftID int64, kind string, price float64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM signals WHERE gift_id = ? AND kind = ? AND price <= ?`,
		giftID, kind, price).Scan(&n)
	return n > 0, err
}

// GetSignal loads one recorded signal.
func (s *Store) GetSignal(ctx context.Context, id int64) (*SignalRow, error) {
	var r SignalRow
	var ts, sentAt int64
	err := s.db.QueryRowContext(ctx, `
SELECT id, ts, kind, COALESCE(gift_id,0), COALESCE(name,''), COALESCE(model,''),
       COALESCE(price,0), COALESCE(exit_price,0), COALESCE(edge,0), COALESCE(velocity,0),
       COALESCE(score,0), COALESCE(payload,''), COALESCE(sent_at,0), COALESCE(action,'')
FROM signals WHERE id = ?`, id).
		Scan(&r.ID, &ts, &r.Kind, &r.GiftID, &r.Key.Name, &r.Key.Model,
			&r.Price, &r.Exit, &r.Edge, &r.Velocity, &r.Score, &r.Payload, &sentAt, &r.Action)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.TS, r.SentAt = fromUnix(ts), fromUnix(sentAt)
	return &r, nil
}

// MarkSignalSent records that the card reached Telegram.
func (s *Store) MarkSignalSent(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE signals SET sent_at = ? WHERE id = ?`, unix(at), id)
	return err
}

// SignalsNeedingOutcome returns mature signals without an evaluation at the
// requested horizon.
func (s *Store) SignalsNeedingOutcome(ctx context.Context, horizonHours int, now time.Time) ([]SignalRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.id,s.ts,s.kind,COALESCE(s.gift_id,0),COALESCE(s.name,''),COALESCE(s.model,''),COALESCE(s.price,0),COALESCE(s.exit_price,0),COALESCE(s.edge,0),COALESCE(s.velocity,0),COALESCE(s.score,0),COALESCE(s.payload,''),COALESCE(s.sent_at,0),COALESCE(s.action,'') FROM signals s LEFT JOIN signal_outcomes o ON o.signal_id=s.id AND o.horizon_hours=? WHERE s.kind='buy' AND s.ts<=? AND o.signal_id IS NULL ORDER BY s.ts LIMIT 500`, horizonHours, unix(now.Add(-time.Duration(horizonHours)*time.Hour)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SignalRow
	for rows.Next() {
		var r SignalRow
		var ts, sent int64
		if err := rows.Scan(&r.ID, &ts, &r.Kind, &r.GiftID, &r.Key.Name, &r.Key.Model, &r.Price, &r.Exit, &r.Edge, &r.Velocity, &r.Score, &r.Payload, &sent, &r.Action); err != nil {
			return nil, err
		}
		r.TS = fromUnix(ts)
		r.SentAt = fromUnix(sent)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SaleForGiftBetween(ctx context.Context, giftID int64, from, to time.Time) (float64, bool, error) {
	var p float64
	err := s.db.QueryRowContext(ctx, `SELECT price FROM sales WHERE gift_id=? AND ts>? AND ts<=? ORDER BY ts LIMIT 1`, giftID, unix(from), unix(to)).Scan(&p)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return p, err == nil, err
}

func (s *Store) PutSignalOutcome(ctx context.Context, signalID int64, horizon int, at time.Time, sold bool, salePrice, floor float64, profitable bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO signal_outcomes(signal_id,horizon_hours,evaluated_at,sold,sale_price,floor_price,profitable) VALUES(?,?,?,?,?,?,?)`, signalID, horizon, unix(at), boolInt(sold), nullFloat(salePrice), nullFloat(floor), boolInt(profitable))
	return err
}

// CalibrationStats reports how much *scored* evidence the desk has: buy signals
// whose forward outcome has actually been measured, and how far back they go.
//
// Counting raw signal rows instead — which is what this used to do — makes it a
// volume gate wearing the name of a calibration gate. Two hundred signals that
// nobody ever checked the result of say nothing about whether the scoring works,
// and that is the one question standing between shadow mode and real money.
func (s *Store) CalibrationStats(ctx context.Context) (int, time.Time, error) {
	var n int
	var ts int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT s.id), COALESCE(MIN(s.ts),0)
		FROM signals s
		JOIN signal_outcomes o ON o.signal_id = s.id
		WHERE s.kind='buy'`).Scan(&n, &ts)
	return n, fromUnix(ts), err
}

// LastSignalForModel powers the per-model alert cooldown.
func (s *Store) LastSignalForModel(ctx context.Context, key tonnel.ModelKey, kind string) (time.Time, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(ts),0) FROM signals WHERE name = ? AND model = ? AND kind = ?`,
		key.Name, key.Model, kind).Scan(&n)
	return fromUnix(n), err
}

// SignalStats summarises detector output over a window, for /status.
type SignalStats struct {
	Total  int
	Sent   int
	Bought int
}

// SignalStatsSince aggregates signals produced after `since`.
func (s *Store) SignalStatsSince(ctx context.Context, since time.Time) (SignalStats, error) {
	var st SignalStats
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       SUM(CASE WHEN sent_at > 0 THEN 1 ELSE 0 END),
       SUM(CASE WHEN action = 'bought' THEN 1 ELSE 0 END)
FROM signals WHERE ts >= ? AND kind = 'buy'`, unix(since)).
		Scan(&st.Total, nullableInt{&st.Sent}, nullableInt{&st.Bought})
	return st, err
}

// nullableInt lets SUM() return NULL on an empty set without failing the scan.
type nullableInt struct{ dst *int }

func (n nullableInt) Scan(v any) error {
	switch t := v.(type) {
	case nil:
		*n.dst = 0
	case int64:
		*n.dst = int(t)
	case float64:
		*n.dst = int(t)
	}
	return nil
}

// ---- watchlist ----------------------------------------------------------

// Watch is a user subscription to a model.
type Watch struct {
	Key      tonnel.ModelKey
	MaxPrice float64
}

// AddWatch subscribes to a model.
func (s *Store) AddWatch(ctx context.Context, key tonnel.ModelKey, maxPrice float64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO watch (name, model, max_price, created_at) VALUES (?,?,?,?)
ON CONFLICT(name, model) DO UPDATE SET max_price = excluded.max_price`,
		key.Name, key.Model, maxPrice, unix(now))
	return err
}

// RemoveWatch unsubscribes.
func (s *Store) RemoveWatch(ctx context.Context, key tonnel.ModelKey) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM watch WHERE name = ? AND model = ?`, key.Name, key.Model)
	return err
}

// Watches lists all subscriptions.
func (s *Store) Watches(ctx context.Context) ([]Watch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, model, COALESCE(max_price,0) FROM watch ORDER BY name, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Watch
	for rows.Next() {
		var w Watch
		if err := rows.Scan(&w.Key.Name, &w.Key.Model, &w.MaxPrice); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ---- mutes --------------------------------------------------------------

// SetMute silences a collection ("Name") or a single model ("Name|Model").
func (s *Store) SetMute(ctx context.Context, scope string, until time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mutes (scope, until) VALUES (?,?) ON CONFLICT(scope) DO UPDATE SET until = excluded.until`,
		scope, unix(until))
	return err
}

// ClearMute removes a mute.
func (s *Store) ClearMute(ctx context.Context, scope string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mutes WHERE scope = ?`, scope)
	return err
}

// IsMuted reports whether alerts for this model are currently silenced, by
// either a collection-wide or a model-specific mute.
func (s *Store) IsMuted(ctx context.Context, key tonnel.ModelKey, now time.Time) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mutes WHERE scope IN (?, ?) AND until > ?`,
		key.Name, key.ID(), unix(now)).Scan(&n)
	return n > 0, err
}

// MuteRow is an active mute.
type MuteRow struct {
	Scope string
	Until time.Time
}

// ActiveMutes lists mutes that have not expired.
func (s *Store) ActiveMutes(ctx context.Context, now time.Time) ([]MuteRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope, until FROM mutes WHERE until > ? ORDER BY scope`, unix(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MuteRow
	for rows.Next() {
		var m MuteRow
		var u int64
		if err := rows.Scan(&m.Scope, &u); err != nil {
			return nil, err
		}
		m.Until = fromUnix(u)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- risk ledger --------------------------------------------------------

// DaySpend is the money already committed today.
type DaySpend struct {
	Spent float64
	Buys  int
}

// SpendToday reads the ledger for a UTC day key (YYYY-MM-DD).
func (s *Store) SpendToday(ctx context.Context, day string) (DaySpend, error) {
	var d DaySpend
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(spent,0), COALESCE(buys,0) FROM risk_state WHERE day = ?`, day).
		Scan(&d.Spent, &d.Buys)
	if err == sql.ErrNoRows {
		return DaySpend{}, nil
	}
	return d, err
}

// AddSpend commits an amount against the daily budget.
func (s *Store) AddSpend(ctx context.Context, day string, amount float64) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO risk_state (day, spent, buys) VALUES (?,?,1)
ON CONFLICT(day) DO UPDATE SET spent = risk_state.spent + excluded.spent, buys = risk_state.buys + 1`,
		day, amount)
	return err
}

// ---- purchase idempotency ----------------------------------------------

// ClaimBuy reserves the right to purchase a listing. It returns false when
// another attempt already owns this gift id, which is what makes a retry or a
// double-tap on the Buy button safe.
func (s *Store) ClaimBuy(ctx context.Context, giftID int64, price float64, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO buy_attempts (gift_id, ts, price, status) VALUES (?,?,?,'pending')`,
		giftID, unix(now), price)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// FinishBuy records the outcome of a claimed purchase.
func (s *Store) FinishBuy(ctx context.Context, giftID int64, status, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE buy_attempts SET status = ?, detail = ? WHERE gift_id = ?`, status, detail, giftID)
	return err
}

// ReleaseBuy drops a claim so the listing can be attempted again later. Used
// only when the request never reached the server.
func (s *Store) ReleaseBuy(ctx context.Context, giftID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM buy_attempts WHERE gift_id = ?`, giftID)
	return err
}

// BuysSince counts purchase attempts that resulted in ownership.
func (s *Store) BuysSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM buy_attempts WHERE ts >= ? AND status = 'bought'`, unix(since)).Scan(&n)
	return n, err
}

// ---- maintenance --------------------------------------------------------

// Prune drops data that no longer affects any decision.
func (s *Store) Prune(ctx context.Context, now time.Time, keepDays int) error {
	cutoff := unix(now.AddDate(0, 0, -keepDays))
	return s.tx(ctx, func(t *sql.Tx) error {
		if _, err := t.ExecContext(ctx, `DELETE FROM model_history WHERE ts < ?`, cutoff); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx,
			`DELETE FROM listings WHERE gone_at IS NOT NULL AND gone_at < ?`, cutoff); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM gram_quotes WHERE ts < ?`, cutoff); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, `DELETE FROM position_marks WHERE ts < ?`, cutoff); err != nil {
			return err
		}
		_, err := t.ExecContext(ctx, `DELETE FROM mutes WHERE until < ?`, unix(now))
		return err
	})
}

// ---- helpers ------------------------------------------------------------

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}
