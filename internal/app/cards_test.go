package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"floorline/internal/bot"
	"floorline/internal/config"
	"floorline/internal/pricing"
	"floorline/internal/signal"
	"floorline/internal/tonnel"
)

// mambaCard rebuilds the 12 Aug production card: Pet Snake / Black Mamba at 6
// GRAM with a 4.21 next ask, a hole at 7.90 / 10.20, and Portals showing five
// asks at or under our own entry.
func mambaCard(t *testing.T) (string, pricing.Valuation) {
	short, _, v := mambaCards(t)
	return short, v
}

// mambaCards renders the same fixture both ways: the compact card the operator
// acts on, and the full one they open to argue with it.
func mambaCards(t *testing.T) (short, full string, v pricing.Valuation) {
	t.Helper()
	key := tonnel.ModelKey{Name: "Pet Snake", Model: "Black Mamba"}
	book := &pricing.Book{Key: key, FetchedAt: time.Now(), Asks: []pricing.Ask{
		{GiftID: 42, Price: 6}, {GiftID: 43, Price: 4.21}, {GiftID: 44, Price: 7.9}, {GiftID: 45, Price: 10.2},
	}}
	liq := pricing.Liquidity{Prints: 9, Sales: 9, DistinctGifts: 9, Turnover: 1, Median: 3.846, Median7: 3.9,
		Trend: 1.05, Velocity: .6, MADRatio: .15, LastSale: time.Now().Add(-2 * time.Hour)}
	v = pricing.Evaluate(pricing.Input{
		GiftID: 42, Key: key, Price: 6, Book: book, Liq: liq, Floor: 4.21, Supply: 13, Rarity: 1.4,
		Backdrop: "Platinum", Symbol: "Khinkali", Now: time.Now(),
		Params: pricing.Params{Fee: .005, Undercut: .01},
	})
	v = pricing.WithCrossDepth(v, pricing.CrossMarket{Support: 5, Venues: 1, Asks: []float64{4, 5, 5, 5.89, 6}})

	a := &App{cfg: &config.Config{LookbackDays: 14, Undercut: .01}}
	dec := &signal.Decision{
		Gift: tonnel.Gift{GiftID: 10380168, GiftNum: 150969, Backdrop: "Platinum", Symbol: "Khinkali"},
		Val:  v,
	}
	ctx := context.Background()
	return a.renderCard(ctx, dec, "включён shadow-режим"),
		a.renderCardFull(ctx, dec, "включён shadow-режим"), v
}

// The 12 Aug card read as an opportunity. It has to read as a warning: the book
// is gappy, and six offers across the venues are already under our entry.
func TestGappyBookCardWarnsInsteadOfTempting(t *testing.T) {
	short, full, v := mambaCards(t)
	// The full card carries every warning; the compact one carries the most
	// decisive and says how many it left behind.
	for _, want := range []string{
		"дырявый стакан: 4.21 → 10.2",
		"дешевле твоего входа стоит 6 чужих асков",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("full card is missing %q:\n%s", want, full)
		}
	}
	if !strings.Contains(short, "рынок ниже нас") {
		t.Errorf("the compact card led with something other than the upside-down trade:\n%s", short)
	}
	if !strings.Contains(short, "в /val") {
		t.Errorf("the compact card hid the remaining warnings without saying so:\n%s", short)
	}
	// And it must not read as an invitation. This is the whole complaint: the
	// headline said BUY while the numbers under it said otherwise.
	if strings.Contains(short, ">BUY<") {
		t.Errorf("a trade 36%% under water is headlined BUY:\n%s", short)
	}
	// The exit now undercuts the single real ask instead of clearing the hole
	// above it, so we genuinely would be the cheapest offer at that price.
	if v.CheaperAsks != 0 {
		t.Errorf("exit %.3f should sit under the 4.21 ask, leaving nobody cheaper; got %d",
			v.FastExit, v.CheaperAsks)
	}
}

// The card claimed "nobody is cheaper than you" two lines under competing asks
// it had printed itself. The exit now undercuts the cheapest of them, so being
// first in the queue is true by construction and no longer worth saying — what
// the card must not hide is that we paid more than the market is asking.
func TestCardNeverClaimsBestAskWhileCheaperAsksExist(t *testing.T) {
	key := tonnel.ModelKey{Name: "Pet Snake", Model: "Black Mamba"}
	// Our own 4.90 listing behind three cheaper sellers: the exit lands above
	// them, so we are demonstrably not first in the queue.
	book := &pricing.Book{Key: key, FetchedAt: time.Now(), Asks: []pricing.Ask{
		{GiftID: 43, Price: 4.0}, {GiftID: 44, Price: 4.1}, {GiftID: 45, Price: 4.2},
		{GiftID: 42, Price: 4.9}, {GiftID: 46, Price: 5.0},
	}}
	liq := pricing.Liquidity{Sales: 20, Prints: 20, DistinctGifts: 20, Turnover: 1,
		Median: 4.6, Median7: 4.6, Trend: 1, Velocity: 1.4, MADRatio: .05, LastSale: time.Now()}
	v := pricing.Evaluate(pricing.Input{
		GiftID: 42, Key: key, Price: 4.9, Book: book, Liq: liq, Floor: 4.0, Supply: 20,
		Now: time.Now(), Params: pricing.Params{Fee: .005, Undercut: .01},
	})

	a := &App{cfg: &config.Config{LookbackDays: 14, Undercut: .01}}
	card := a.renderCard(context.Background(),
		&signal.Decision{Gift: tonnel.Gift{GiftID: 10380168, GiftNum: 150969}, Val: v}, "")

	if v.AsksBelowEntry < 3 {
		t.Fatalf("fixture is wrong: three asks must sit below the %.3f entry, got %d",
			v.Cost, v.AsksBelowEntry)
	}
	if strings.Contains(card, "подрежут") {
		t.Errorf("card frames us as first in the queue with %d asks under our entry:\n%s", v.AsksBelowEntry, card)
	}
	if !strings.Contains(card, "рынок ниже нас") {
		t.Errorf("card must say the market is cheaper than we paid:\n%s", card)
	}
}

// Readability is a feature here: the operator reads these at 2am on a phone.
func TestCardIsSectionedAndUsesRussianPlurals(t *testing.T) {
	_, card, _ := mambaCards(t)
	for _, want := range []string{"📐 <b>Почему столько:</b>", "📚 Tonnel:", "📊 ", "🎨 "} {
		if !strings.Contains(card, want) {
			t.Errorf("card has no %q section:\n%s", want, card)
		}
	}
	if !strings.Contains(card, "3 аска") {
		t.Errorf("wrong plural form for the ask count:\n%s", card)
	}
	if strings.Contains(card, "(-") || strings.Contains(card, " -1") {
		t.Errorf("card mixes ASCII and typographic minus signs:\n%s", card)
	}
}

// On this market half of what people pay for is how the gift looks, and the
// only way to see it in Telegram is the collectible link, which the client
// unfurls into a picture. The Tonnel deep link cannot do that.
func TestCardLinksTheGiftSoTelegramShowsIt(t *testing.T) {
	card, _ := mambaCard(t)
	if !strings.Contains(card, `href="https://t.me/nft/PetSnake-150969"`) {
		t.Errorf("card carries no collectible link to unfurl:\n%s", card)
	}
}

// The complaint that started this: a card carrying thirty numbers cannot be
// read, so the two that decide the trade get the same glance as the rest.
//
// The Mamba fixture is close to the worst case — two risk lines, an appearance
// note and a manual-only footer all fire at once — so the bound is measured
// there rather than on a clean card that would pass trivially.
func TestCardStaysShortEnoughToRead(t *testing.T) {
	short, full, _ := mambaCards(t)
	if n := strings.Count(short, "\n"); n > 7 {
		t.Errorf("the acting card is %d lines; it is meant to be glanced at, not read:\n%s", n, short)
	}
	// The long form is still bounded — it is a reference, not a dump.
	if n := strings.Count(full, "\n"); n > 24 {
		t.Errorf("the detailed card is %d lines:\n%s", n, full)
	}
	// Everything cut from the short card has to remain reachable.
	if len(full) <= len(short) {
		t.Error("the detailed card carries no more than the compact one")
	}
}

func TestNFTPreviewURLDerivesTheSlug(t *testing.T) {
	for _, tc := range []struct {
		collection string
		num        int64
		want       string
	}{
		{"Vice Cream", 49968, "https://t.me/nft/ViceCream-49968"},
		{"Plush Pepe", 1, "https://t.me/nft/PlushPepe-1"},
		{"Xmas Stocking", 224990, "https://t.me/nft/XmasStocking-224990"},
		{"Pet Snake", 0, ""}, // no number, nothing to link to
		{"", 42, ""},
	} {
		if got := bot.NFTPreviewURL(tc.collection, tc.num); got != tc.want {
			t.Errorf("NFTPreviewURL(%q, %d) = %q, want %q", tc.collection, tc.num, got, tc.want)
		}
	}
}

func TestPluralPicksTheRussianForm(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{1, "1 аск"}, {2, "2 аска"}, {5, "5 асков"}, {11, "11 асков"}, {21, "21 аск"}, {104, "104 аска"}} {
		if got := plural(tc.n, "аск", "аска", "асков"); got != tc.want {
			t.Errorf("plural(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
