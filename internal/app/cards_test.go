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

// The card claimed "nobody within 5% of your exit → you are the best ask" two
// lines under a 4.21 competing ask it had printed itself.
func TestCardNeverClaimsBestAskWhileCheaperAsksExist(t *testing.T) {
	card, v := mambaCard(t)
	if v.CheaperAsks == 0 {
		t.Fatal("fixture is wrong: the 4.21 ask must undercut the exit")
	}
	if strings.Contains(card, "Дешевле тебя никого") {
		t.Errorf("card calls us the best ask with %d cheaper asks in the book:\n%s", v.CheaperAsks, card)
	}
	for _, want := range []string{
		"Перед тобой в очереди",
		"Дырявый стакан",
		"Дешевле твоего входа по всем площадкам",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card is missing %q:\n%s", want, card)
		}
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
