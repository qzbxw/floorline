-- Floorline schema. All timestamps are unix seconds (UTC).
-- Attribute columns hold the *base* name without the "(0.4%)" rarity suffix;
-- the rarity itself lives in its own numeric column.

CREATE TABLE IF NOT EXISTS listings (
    gift_id         INTEGER PRIMARY KEY,
    gift_num        INTEGER,
    name            TEXT    NOT NULL,
    model           TEXT    NOT NULL,
    backdrop        TEXT,
    symbol          TEXT,
    price           REAL,
    asset           TEXT,
    rarity          REAL,
    model_rarity    REAL,
    backdrop_rarity REAL,
    symbol_rarity   REAL,
    seller          INTEGER,
    premarket       INTEGER NOT NULL DEFAULT 0,
    tg_marketplace  INTEGER NOT NULL DEFAULT 0,
    is_bundle       INTEGER NOT NULL DEFAULT 0,
    export_at       INTEGER,
    posted_at       INTEGER,
    first_seen      INTEGER NOT NULL,
    last_seen       INTEGER NOT NULL,
    gone_at         INTEGER
);
CREATE INDEX IF NOT EXISTS idx_listings_model ON listings(name, model, price);
CREATE INDEX IF NOT EXISTS idx_listings_seen  ON listings(first_seen);

-- Real executed trades. This is the only honest evidence of what a model is
-- worth and how fast it moves.
--
-- There are no counterparty ids in the payload, so gift_num carries the
-- wash-trading signal instead: many "trades" that are all the same physical
-- gift is one item being passed around, not a liquid market.
CREATE TABLE IF NOT EXISTS sales (
    gift_id  INTEGER NOT NULL,
    ts       INTEGER NOT NULL,
    gift_num INTEGER,
    name     TEXT    NOT NULL,
    model    TEXT    NOT NULL,
    backdrop TEXT,
    symbol   TEXT,
    price    REAL    NOT NULL,
    asset    TEXT,
    type     TEXT,
    PRIMARY KEY (gift_id, ts)
);
CREATE INDEX IF NOT EXISTS idx_sales_model ON sales(name, model, ts DESC);
CREATE INDEX IF NOT EXISTS idx_sales_ts    ON sales(ts DESC);

-- Latest full-market snapshot, refreshed wholesale from filterStats.
CREATE TABLE IF NOT EXISTS model_current (
    name   TEXT NOT NULL,
    model  TEXT NOT NULL,
    floor  REAL,
    supply INTEGER,
    rarity REAL,
    ts     INTEGER NOT NULL,
    PRIMARY KEY (name, model)
);
CREATE INDEX IF NOT EXISTS idx_model_current_name ON model_current(name);

-- Sparse history of the same, for floor-drop detection and later analysis.
CREATE TABLE IF NOT EXISTS model_history (
    ts     INTEGER NOT NULL,
    name   TEXT    NOT NULL,
    model  TEXT    NOT NULL,
    floor  REAL,
    supply INTEGER,
    PRIMARY KEY (ts, name, model)
);
CREATE INDEX IF NOT EXISTS idx_model_history_model ON model_history(name, model, ts DESC);

CREATE TABLE IF NOT EXISTS positions (
    gift_id    INTEGER PRIMARY KEY,
    gift_num   INTEGER,
    name       TEXT NOT NULL,
    model      TEXT NOT NULL,
    backdrop   TEXT,
    symbol     TEXT,
    model_rarity    REAL NOT NULL DEFAULT 0,
    backdrop_rarity REAL NOT NULL DEFAULT 0,
    symbol_rarity   REAL NOT NULL DEFAULT 0,
    buy_price  REAL    NOT NULL,
    cost_source TEXT NOT NULL DEFAULT 'unknown',
    cost_confidence REAL NOT NULL DEFAULT 0,
    bought_at  INTEGER NOT NULL,
    list_price REAL,
    listed_at  INTEGER,
    sell_price REAL,
    sold_at    INTEGER,
    missing_since INTEGER,
    status     TEXT NOT NULL,          -- open | listed | missing | sold | returned
    source     TEXT NOT NULL DEFAULT 'manual',  -- auto | manual | import
    note       TEXT
);
CREATE INDEX IF NOT EXISTS idx_positions_status ON positions(status);

-- Immutable lifecycle journal. Inventory reconciliation and bot actions both
-- write here, so manual buys/reprices are auditable exactly like bot trades.
CREATE TABLE IF NOT EXISTS position_events (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    gift_id   INTEGER NOT NULL,
    ts        INTEGER NOT NULL,
    kind      TEXT NOT NULL,
    old_price REAL,
    new_price REAL,
    detail    TEXT
);
CREATE INDEX IF NOT EXISTS idx_position_events_gift ON position_events(gift_id, ts DESC);

-- Periodic mark-to-market trail used to explain how advice changed over time.
CREATE TABLE IF NOT EXISTS position_marks (
    gift_id       INTEGER NOT NULL,
    ts            INTEGER NOT NULL,
    entry_price   REAL,
    ask_price     REAL,
    model_floor   REAL,
    recommended_exit REAL,
    external_ref  REAL,
    gram_usd      REAL,
    edge          REAL,
    expected_days REAL,
    score         REAL,
    action        TEXT,
    PRIMARY KEY (gift_id, ts)
);
CREATE INDEX IF NOT EXISTS idx_position_marks_gift ON position_marks(gift_id, ts DESC);

-- Completed ownership cycles are immutable. A physical gift may be bought,
-- sold and later reacquired; positions holds the current cycle, this table
-- preserves every prior realised PnL row.
CREATE TABLE IF NOT EXISTS position_trades (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    gift_id     INTEGER NOT NULL,
    name        TEXT NOT NULL,
    model       TEXT NOT NULL,
    buy_price   REAL,
    bought_at   INTEGER,
    sell_price  REAL,
    sold_at     INTEGER NOT NULL,
    source      TEXT,
    UNIQUE(gift_id, bought_at, sold_at)
);
CREATE INDEX IF NOT EXISTS idx_position_trades_sold ON position_trades(sold_at DESC);

-- Every signal ever produced, including ones that were only logged in shadow
-- mode. Joined against `sales` later, this table is a free backtest of the
-- detector: did the models we flagged actually trade above our entry?
CREATE TABLE IF NOT EXISTS signals (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    ts         INTEGER NOT NULL,
    kind       TEXT    NOT NULL,
    gift_id    INTEGER,
    name       TEXT,
    model      TEXT,
    price      REAL,
    exit_price REAL,
    edge       REAL,
    velocity   REAL,
    score      REAL,
    payload    TEXT,
    sent_at    INTEGER,
    action     TEXT
);
CREATE INDEX IF NOT EXISTS idx_signals_gift  ON signals(gift_id, kind);
CREATE INDEX IF NOT EXISTS idx_signals_model ON signals(name, model, ts DESC);
CREATE INDEX IF NOT EXISTS idx_signals_ts    ON signals(ts DESC);

CREATE TABLE IF NOT EXISTS watch (
    name       TEXT NOT NULL,
    model      TEXT NOT NULL,
    max_price  REAL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (name, model)
);

CREATE TABLE IF NOT EXISTS mutes (
    scope TEXT PRIMARY KEY,   -- "Collection" or "Collection|Model"
    until INTEGER NOT NULL
);

-- Daily spend ledger. Persisted so a restart cannot reset a budget that has
-- already been spent.
CREATE TABLE IF NOT EXISTS risk_state (
    day            TEXT PRIMARY KEY,   -- YYYY-MM-DD in UTC
    spent          REAL    NOT NULL DEFAULT 0,
    buys           INTEGER NOT NULL DEFAULT 0,
    disabled_until INTEGER NOT NULL DEFAULT 0
);

-- Purchase idempotency. A row is claimed before the request goes out, so a
-- crash or an ambiguous timeout can never turn into a duplicate buy.
CREATE TABLE IF NOT EXISTS buy_attempts (
    gift_id INTEGER PRIMARY KEY,
    ts      INTEGER NOT NULL,
    price   REAL    NOT NULL,
    status  TEXT    NOT NULL,   -- pending | bought | failed
    detail  TEXT
);

-- Every price change is retained so repricing is rate-limited and auditable.
CREATE TABLE IF NOT EXISTS reprices (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    gift_id   INTEGER NOT NULL,
    ts        INTEGER NOT NULL,
    old_price REAL,
    new_price REAL NOT NULL,
    reason    TEXT
);
CREATE INDEX IF NOT EXISTS idx_reprices_gift ON reprices(gift_id, ts DESC);

-- Forward outcomes make signal quality measurable instead of anecdotal.
CREATE TABLE IF NOT EXISTS signal_outcomes (
    signal_id      INTEGER NOT NULL,
    horizon_hours  INTEGER NOT NULL,
    evaluated_at   INTEGER NOT NULL,
    sold           INTEGER NOT NULL DEFAULT 0,
    sale_price     REAL,
    floor_price    REAL,
    profitable     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(signal_id, horizon_hours),
    FOREIGN KEY(signal_id) REFERENCES signals(id) ON DELETE CASCADE
);

-- GRAM/USDT history. The marketplace can keep a stale GRAM floor while the
-- native coin moves; this series lets sales and floors be compared in constant
-- current-GRAM terms.
CREATE TABLE IF NOT EXISTS gram_quotes (
    ts        INTEGER PRIMARY KEY,
    usd       REAL NOT NULL,
    bid       REAL,
    ask       REAL,
    change_24 REAL
);
CREATE INDEX IF NOT EXISTS idx_gram_quotes_ts ON gram_quotes(ts DESC);

CREATE TABLE IF NOT EXISTS kv (
    k TEXT PRIMARY KEY,
    v TEXT NOT NULL
);
