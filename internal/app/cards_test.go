package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"floorline/internal/config"
	"floorline/internal/pricing"
	"floorline/internal/signal"
	"floorline/internal/tonnel"
)

// mambaCard rebuilds the 12 Aug production card: Pet Snake / Black Mamba at 6
// GRAM with a 4.21 next ask, a hole at 7.90 / 10.20, and Portals showing five
// asks at or under our own entry.
func mambaCard(t *testing.T) (string, pricing.Valuation) {
	t.Helper()
	key := tonnel.ModelKey{Name: "Pet Snake", Model: "Black Mamba"}
	book := &pricing.Book{Key: key, FetchedAt: time.Now(), Asks: []pricing.Ask{
		{GiftID: 42, Price: 6}, {GiftID: 43, Price: 4.21}, {GiftID: 44, Price: 7.9}, {GiftID: 45, Price: 10.2},
	}}
	liq := pricing.Liquidity{Sales: 9, DistinctGifts: 9, Turnover: 1, Median: 3.846, Median7: 3.9,
		Trend: 1.05, Velocity: .6, MADRatio: .15, LastSale: time.Now().Add(-2 * time.Hour)}
	v := pricing.Evaluate(pricing.Input{
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
	return a.renderCard(context.Background(), dec, "включён shadow-режим"), v
}

// The 12 Aug card read as an opportunity. It has to read as a warning: the book
// is gappy, and six offers across the venues are already under our entry.
func TestGappyBookCardWarnsInsteadOfTempting(t *testing.T) {
	card, v := mambaCard(t)
	for _, want := range []string{
		"Дырявый стакан",
		"Дешевле твоего входа по всем площадкам",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card is missing %q:\n%s", want, card)
		}
	}
	// The exit now undercuts the single real ask instead of clearing the hole
	// above it, so we genuinely would be the cheapest offer at that price.
	if v.CheaperAsks != 0 {
		t.Errorf("exit %.3f should sit under the 4.21 ask, leaving nobody cheaper; got %d",
			v.FastExit, v.CheaperAsks)
	}
}

// The card claimed "nobody within 5% of your exit → you are the best ask" two
// lines under a competing ask it had printed itself. Whenever real asks sit
// below the exit, the queue line has to say so.
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

	if v.CheaperAsks == 0 {
		t.Fatalf("fixture is wrong: three asks must undercut the %.3f exit", v.FastExit)
	}
	if strings.Contains(card, "Дешевле тебя никого") {
		t.Errorf("card calls us the best ask with %d cheaper asks in the book:\n%s", v.CheaperAsks, card)
	}
	if !strings.Contains(card, "Перед тобой в очереди") {
		t.Errorf("card must say we are behind a queue:\n%s", card)
	}
}

// Readability is a feature here: the operator reads these at 2am on a phone.
func TestCardIsSectionedAndUsesRussianPlurals(t *testing.T) {
	card, _ := mambaCard(t)
	for _, want := range []string{"💵 <b>Вход", "🎯 <b>Выход</b>", "📚 <b>Стакан Tonnel</b>", "📊 <b>История 14д</b>", "⚖️ <b>Из чего цена</b>"} {
		if !strings.Contains(card, want) {
			t.Errorf("card has no %q section:\n%s", want, card)
		}
	}
	if !strings.Contains(card, "всего 3 аска") {
		t.Errorf("wrong plural form for the ask count:\n%s", card)
	}
	if strings.Contains(card, "(-") || strings.Contains(card, " -1") {
		t.Errorf("card mixes ASCII and typographic minus signs:\n%s", card)
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
