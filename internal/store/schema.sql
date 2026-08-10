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
CREATE TABLE IF NOT EXISTS sales (
    gift_id  INTEGER NOT NULL,
    ts       INTEGER NOT NULL,
    name     TEXT    NOT NULL,
    model    TEXT    NOT NULL,
    backdrop TEXT,
    symbol   TEXT,
    price    REAL    NOT NULL,
    asset    TEXT,
    type     TEXT,
    seller   INTEGER,
    buyer    INTEGER,
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
    buy_price  REAL    NOT NULL,
    bought_at  INTEGER NOT NULL,
    list_price REAL,
    listed_at  INTEGER,
    sell_price REAL,
    sold_at    INTEGER,
    status     TEXT NOT NULL,          -- open | listed | sold | returned
    source     TEXT NOT NULL DEFAULT 'manual',  -- auto | manual | import
    note       TEXT
);
CREATE INDEX IF NOT EXISTS idx_positions_status ON positions(status);

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

CREATE TABLE IF NOT EXISTS kv (
    k TEXT PRIMARY KEY,
    v TEXT NOT NULL
);
