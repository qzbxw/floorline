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

// The gift picture is half of what a buyer is paying for on this market, and
// the only way to put it in a Telegram message is the collectible link, which
// the client unfurls. Two things have to be true at once for that to happen and
// they live in different packages, so both are pinned here.
func TestSignalCardCarriesTheGiftLinkAsItsFirstLink(t *testing.T) {
	key := tonnel.ModelKey{Name: "Liberty Figure", Model: "Peridot"}
	book := &pricing.Book{Key: key, FetchedAt: time.Now(), Asks: []pricing.Ask{
		{GiftID: 42, Price: 4}, {GiftID: 43, Price: 4.2}, {GiftID: 44, Price: 4.3},
	}}
	liq := pricing.Liquidity{Prints: 58, Sales: 36, DistinctGifts: 36, Turnover: .62,
		Median: 3.984, Median7: 4, Trend: 1.01, Velocity: 2.57, MADRatio: .04, LastSale: time.Now()}
	v := pricing.Evaluate(pricing.Input{
		GiftID: 42, GiftNum: 235831, Key: key, Price: 4, Book: book, Liq: liq,
		Floor: 4.2, Supply: 46, Backdrop: "Feldgrau", Symbol: "Geisha Fan",
		Now: time.Now(), Params: pricing.Params{Fee: .005, Undercut: .01},
	})

	a := &App{cfg: &config.Config{LookbackDays: 14, Undercut: .01}}
	card := a.renderCard(context.Background(), &signal.Decision{
		Gift: tonnel.Gift{GiftID: 10384560, GiftNum: 235831, Backdrop: "Feldgrau", Symbol: "Geisha Fan"},
		Val:  v,
	}, "")

	const want = `https://t.me/nft/LibertyFigure-235831`
	if !strings.Contains(card, want) {
		t.Fatalf("card has no collectible link:\n%s", card)
	}
	// Telegram previews the *first* link in the message, so the gift has to win
	// that race against anything else the card mentions.
	if i := strings.Index(card, "https://"); i < 0 || !strings.HasPrefix(card[i:], want) {
		t.Errorf("the gift link is not the first link in the card:\n%s", card)
	}
}
