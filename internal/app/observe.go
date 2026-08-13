package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rs/zerolog/log"

	"floorline/internal/pricing"
	"floorline/internal/signal"
	"floorline/internal/store"
)

// Weights learned from floors are weights learned from what people wish their
// gifts were worth. The only honest teacher is what happened next, so the desk
// keeps a tape of its own opinions and goes back to mark them.
//
// Two things were missing before this. The journal only recorded lots that
// cleared every gate, so the record was of decisions taken and never of
// decisions declined — and a model trained on that can learn what a good trade
// looks like but never what a rejected one would have done. And the recorded
// features were the outputs (score, edge, exit) rather than the inputs the
// prediction actually rests on: the queue in front of us, the arrival rate, the
// regime, the confidence. Those are what a fill probability has to be calibrated
// against, and none of them were stored.
//
// So: every candidate that was worth pricing is written down, whether or not it
// was taken, with the features it was judged on; and the maintenance pass marks
// each one at 24h, 72h and 7d against what the market actually did. A few
// hundred of those and the hand-set numbers in fill.go — undercutShare above
// all — stop being guesses.

// observed is the action recorded for a candidate the desk priced and passed on.
// It is distinct from the empty action a signal carries before the operator
// responds, so the two never get confused when the tape is read back.
const observedAction = "observed"

// observation is everything worth knowing later about a decision made now.
type observation struct {
	Verdict    signal.Verdict         `json:"verdict"`
	Score      pricing.ScoreBreakdown `json:"score"`
	Confidence float64                `json:"confidence"`
	Regime     pricing.Regime         `json:"regime"`

	// The exit ladder as it stood, so a later mark can ask which horizon was
	// right rather than only whether the single quoted number was.
	FastExit   float64 `json:"fast_exit"`
	PatientAsk float64 `json:"patient_ask"`
	FairValue  float64 `json:"fair_value"`
	ExitIn24h  float64 `json:"exit_24h"`
	ExitIn72h  float64 `json:"exit_72h"`
	ExitIn7d   float64 `json:"exit_7d"`

	// The prediction being calibrated, and the inputs it came from.
	Fill24h      float64 `json:"fill_24h"`
	Fill72h      float64 `json:"fill_72h"`
	Fill7d       float64 `json:"fill_7d"`
	QueueAhead   int     `json:"queue_ahead"`
	Cheaper      int     `json:"cheaper"`
	Undercutters int     `json:"undercutters"`
	BuyersPerDay float64 `json:"buyers_per_day"`

	// Market shape at the moment of the call.
	Walkaway         float64 `json:"walkaway"`
	WalkawayVenue    string  `json:"walkaway_venue"`
	CrossVenues      int     `json:"cross_venues"`
	CrossUnreachable int     `json:"cross_unreachable"`
	CrossImplausible bool    `json:"cross_implausible"`
	AsksBelowEntry   int     `json:"asks_below_entry"`
	AdverseSelection bool    `json:"adverse_selection"`
	DataAgeSeconds   float64 `json:"data_age_seconds"`

	Attribute pricing.AttributeValue `json:"attribute"`
	// Blocked names the layers that held the verdict down, so a later analysis
	// can ask which objection was worth listening to.
	Blocked []string `json:"blocked,omitempty"`
}

// observationPayload serialises a decision for the journal.
func observationPayload(dec *signal.Decision) (string, error) {
	v := dec.Val
	o := observation{
		Verdict: dec.Verdict, Score: v.ScoreBreakdown, Confidence: v.Confidence, Regime: v.Regime,
		FastExit: v.FastExit, PatientAsk: v.PatientAsk, FairValue: v.FairValue,
		ExitIn24h: v.ExitIn24h, ExitIn72h: v.ExitIn72h, ExitIn7d: v.ExitIn7d,
		Fill24h: v.Fill.In24h, Fill72h: v.Fill.In72h, Fill7d: v.Fill.In7d,
		QueueAhead: v.Fill.QueueAhead, Cheaper: v.Fill.Cheaper,
		Undercutters: v.Fill.Undercutters, BuyersPerDay: v.Fill.BuyersPerDay,
		Walkaway: v.Walkaway, WalkawayVenue: v.WalkawayVenue,
		CrossVenues: v.Cross.Venues, CrossUnreachable: v.Cross.Unreachable,
		CrossImplausible: v.CrossImplausible, AsksBelowEntry: v.AsksBelowEntry,
		AdverseSelection: v.AdverseSelection, DataAgeSeconds: v.DataAge.Seconds(),
		Attribute: v.Attribute,
	}
	for _, l := range signal.Blocking(dec.Layers) {
		o.Blocked = append(o.Blocked, l.Name)
	}
	raw, err := json.Marshal(o)
	return string(raw), err
}

// observe records a candidate the desk priced and did not take.
//
// It is deliberately quiet: no notification, no cooldown, no effect on
// anything the bot does today. The value is entirely in six months of them.
func (a *App) observe(ctx context.Context, dec *signal.Decision, now time.Time) {
	if dec == nil || !dec.Val.Valid || dec.Signal {
		return // a taken signal is already journalled by handleDecision
	}
	// Only the near misses. A lot whose exit is under its entry teaches nothing
	// that the arithmetic did not already say, and writing every row the scanner
	// touches would bury the tape in noise.
	if dec.Val.Edge <= 0 {
		return
	}
	already, err := a.st.AlreadySignalled(ctx, dec.Gift.GiftID.Int(), signal.KindBuy, dec.Val.Price)
	if err != nil || already {
		return
	}
	payload, err := observationPayload(dec)
	if err != nil {
		return
	}
	if _, err := a.st.InsertSignal(ctx, store.SignalRow{
		TS: now, Kind: signal.KindBuy, GiftID: dec.Gift.GiftID.Int(), Key: dec.Val.Key,
		Price: dec.Val.Price, Exit: dec.Val.FastExit, Edge: dec.Val.Edge,
		Velocity: dec.Val.Liq.Velocity, Score: dec.Score, Payload: payload,
		Action: observedAction,
	}); err != nil {
		log.Debug().Err(err).Msg("recording a passed-over candidate failed")
	}
}
