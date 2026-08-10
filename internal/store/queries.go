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
INSERT OR IGNORE INTO sales (gift_id, ts, name, model, backdrop, symbol, price, asset, type, seller, buyer)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		for i := range sales {
			sale := &sales[i]
			when := sale.When()
			if when.IsZero() || sale.Name == "" || sale.Price.Float() <= 0 {
				continue
			}
			res, err := stmt.ExecContext(ctx,
				sale.GiftID.Int(), when.Unix(), sale.Name,
				tonnel.BaseAttr(sale.Model), tonnel.BaseAttr(sale.Backdrop), tonnel.BaseAttr(sale.Symbol),
				sale.Price.Float(), sale.Asset, sale.Type,
				sale.Seller.Int(), sale.Buyer.Int(),
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
	GiftID int64
	TS     time.Time
	Price  float64
	Seller int64
	Buyer  int64
}

// SalesSince returns a model's trades newer than `since`, oldest first.
func (s *Store) SalesSince(ctx context.Context, key tonnel.ModelKey, since time.Time) ([]SaleRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT gift_id, ts, price, COALESCE(seller,0), COALESCE(buyer,0)
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
		if err := rows.Scan(&r.GiftID, &ts, &r.Price, &r.Seller, &r.Buyer); err != nil {
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
SELECT name, model, COUNT(*) AS n, MIN(price), MAX(price), COUNT(DISTINCT buyer)
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
		if err := rows.Scan(&r.Key.Name, &r.Key.Model, &r.Count, &r.MinPrice, &r.MaxPrice, &r.Buyers); err != nil {
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
	Buyers   int
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
	GiftID    int64
	GiftNum   int64
	Key       tonnel.ModelKey
	Backdrop  string
	Symbol    string
	BuyPrice  float64
	BoughtAt  time.Time
	ListPrice float64
	ListedAt  time.Time
	SellPrice float64
	SoldAt    time.Time
	Status    string
	Source    string
	Note      string
}

// Position status values.
const (
	StatusOpen     = "open"
	StatusListed   = "listed"
	StatusSold     = "sold"
	StatusReturned = "returned"
)

// UpsertPosition inserts or refreshes a position without clobbering fields that
// are already set (a later reconciliation must not erase a recorded sale).
func (s *Store) UpsertPosition(ctx context.Context, p Position) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO positions (gift_id, gift_num, name, model, backdrop, symbol,
                       buy_price, bought_at, list_price, listed_at,
                       sell_price, sold_at, status, source, note)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(gift_id) DO UPDATE SET
    list_price = COALESCE(excluded.list_price, positions.list_price),
    listed_at  = COALESCE(NULLIF(excluded.listed_at,0), positions.listed_at),
    sell_price = COALESCE(excluded.sell_price, positions.sell_price),
    sold_at    = COALESCE(NULLIF(excluded.sold_at,0), positions.sold_at),
    status     = excluded.status`,
		p.GiftID, p.GiftNum, p.Key.Name, p.Key.Model, p.Backdrop, p.Symbol,
		p.BuyPrice, unix(p.BoughtAt), nullFloat(p.ListPrice), unix(p.ListedAt),
		nullFloat(p.SellPrice), unix(p.SoldAt), p.Status, p.Source, p.Note)
	return err
}

// SetPositionListed records that a position is now on the market.
func (s *Store) SetPositionListed(ctx context.Context, giftID int64, price float64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE positions SET list_price = ?, listed_at = ?, status = ? WHERE gift_id = ?`,
		price, unix(at), StatusListed, giftID)
	return err
}

// SetPositionSold closes a position.
func (s *Store) SetPositionSold(ctx context.Context, giftID int64, price float64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE positions SET sell_price = ?, sold_at = ?, status = ? WHERE gift_id = ?`,
		price, unix(at), StatusSold, giftID)
	return err
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
       buy_price, bought_at, COALESCE(list_price,0), COALESCE(listed_at,0),
       COALESCE(sell_price,0), COALESCE(sold_at,0), status, source, COALESCE(note,'')
FROM positions`

func scanPositions(rows *sql.Rows) ([]Position, error) {
	var out []Position
	for rows.Next() {
		var p Position
		var boughtAt, listedAt, soldAt int64
		if err := rows.Scan(&p.GiftID, &p.GiftNum, &p.Key.Name, &p.Key.Model, &p.Backdrop, &p.Symbol,
			&p.BuyPrice, &boughtAt, &p.ListPrice, &listedAt,
			&p.SellPrice, &soldAt, &p.Status, &p.Source, &p.Note); err != nil {
			return nil, err
		}
		p.BoughtAt, p.ListedAt, p.SoldAt = fromUnix(boughtAt), fromUnix(listedAt), fromUnix(soldAt)
		out = append(out, p)
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
