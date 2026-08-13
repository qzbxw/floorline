package signal

import (
	"fmt"

	"floorline/internal/pricing"
)

// A verdict is the weakest of its layers, never the strongest.
//
// The bot used to print "✅ BUY" next to "скор 3/100 · данные 3%", and that is
// not a display problem, it is a broken sentence. BUY is read as "the model is
// confident", so a BUY that rests on three percent confidence is the interface
// telling the operator something the engine does not believe. The gates that
// produced it were all about *price* — edge, net, exit days — and price is only
// one of the three things that have to be true.
//
// So the verdict is assembled from three independent layers, and the answer is
// the weakest one:
//
//	price       is there money in it, after fees and the risk buffer
//	liquidity   can it actually be sold, and inside what horizon
//	data        do we know enough for either of the above to mean anything
//
// A trade that clears price but not data is not a buy, it is a speculation, and
// naming it that costs nothing and prevents the one mistake this bot can make
// that the operator cannot undo.
type Verdict string

const (
	// VerdictBuy is reserved for trades where all three layers hold. It is the
	// only verdict unattended buying is allowed to act on.
	VerdictBuy Verdict = "BUY"
	// VerdictSpeculative is a real edge on evidence too thin to stand behind.
	// Worth showing, worth taking by hand, never worth taking automatically.
	VerdictSpeculative Verdict = "SPEC"
	// VerdictManual is a real edge the desk cannot get out of quickly enough for
	// the machine to own the decision.
	VerdictManual Verdict = "РУКАМИ"
	// VerdictPass is no edge, or an edge the arithmetic does not support.
	VerdictPass Verdict = "PASS"
)

// Mark is the light the card leads with.
func (v Verdict) Mark() string {
	switch v {
	case VerdictBuy:
		return "🟢"
	case VerdictSpeculative:
		return "🟡"
	case VerdictManual:
		return "🟠"
	default:
		return "🚫"
	}
}

// Actionable reports whether the verdict is worth a notification at all.
func (v Verdict) Actionable() bool { return v != VerdictPass }

// The bars each layer has to clear before it stops holding the verdict down.
const (
	// minBuyConfidence is the trade history a BUY needs behind it. Below this the
	// exit price is an extrapolation from a handful of prints, whatever the
	// arithmetic on top of it says.
	minBuyConfidence = 0.35
	// minBuyScore keeps the headline and the number underneath it in agreement.
	// A 3/100 BUY is the card contradicting itself in two adjacent lines.
	minBuyScore = 12
	// minBuyFill72h is the liquidity bar: a flip has to be more likely than not
	// to be gone inside three days. Below that the capital is committed for a
	// week to earn a flip's margin, which is the trade the desk keeps regretting.
	minBuyFill72h = 0.5
)

// Layer is one of the three independent things that have to be true, and why it
// is or is not.
type Layer struct {
	Name string
	OK   bool
	Note string
}

// Grade assembles the verdict from the three layers, returning it with the
// layers themselves so the card can show which one is holding it down.
//
// signalFails is what the price gates said; it decides the price layer, because
// those gates are precisely the arithmetic of the round trip.
func Grade(v pricing.Valuation, signalFails []string) (Verdict, []Layer) {
	price := Layer{Name: "цена", OK: len(signalFails) == 0}
	if !price.OK {
		price.Note = signalFails[0]
	}

	liquidity := Layer{Name: "ликвидность", OK: true}
	switch {
	case v.Fill.BuyersPerDay <= 0:
		liquidity.OK = false
		liquidity.Note = "модель не торгуется — выйти не из чего"
	case v.Fill.In72h < minBuyFill72h:
		liquidity.OK = false
		liquidity.Note = fmt.Sprintf("шанс продать за 72ч всего %.0f%% — впереди %s",
			v.Fill.In72h*100, plural(v.Fill.QueueAhead, "оффер", "оффера", "офферов"))
	}

	data := Layer{Name: "данные", OK: true}
	switch {
	case v.Confidence < minBuyConfidence:
		data.OK = false
		data.Note = fmt.Sprintf("доверие данным %.0f%% — на такой истории цена выхода это догадка", v.Confidence*100)
	case v.ExitInvented:
		data.OK = false
		data.Note = "цену выхода пришлось выдумать между стаканом и лентой"
	case v.CrossImplausible:
		data.OK = false
		data.Note = "чужие площадки показывают цену, которой не бывает — сравнить не с чем"
	case v.Cross.Unreachable > 0:
		data.OK = false
		data.Note = "площадка не ответила — сравнить не с чем"
	case v.ScoreBreakdown.Total < minBuyScore:
		data.OK = false
		data.Note = fmt.Sprintf("скор %.0f/100 — заголовок не должен быть увереннее числа под ним", v.ScoreBreakdown.Total)
	}

	layers := []Layer{price, liquidity, data}
	switch {
	case !price.OK:
		return VerdictPass, layers
	case !liquidity.OK:
		return VerdictManual, layers
	case !data.OK:
		return VerdictSpeculative, layers
	default:
		return VerdictBuy, layers
	}
}

// Blocking returns the layers holding the verdict down, weakest first.
func Blocking(layers []Layer) []Layer {
	var out []Layer
	for _, l := range layers {
		if !l.OK {
			out = append(out, l)
		}
	}
	return out
}
