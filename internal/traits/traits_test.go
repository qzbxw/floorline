package traits

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCatalogue serves the two endpoints the client reads, in the shape the
// live API returns them — captured from api.changes.tg/gift/spring-basket.
func fakeCatalogue(t *testing.T, hits *atomic.Int32) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/gift/spring-basket", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"gift":{"name":"Spring Basket","id":"5773725897517433693"},
			"models":[{"name":"Meme God","rarity":0.1},{"name":"Midas Bunny","rarity":1.5}],
			"backdrops":[{"name":"Old Gold","rarity":1},{"name":"Cyberpunk","rarity":1.2}],
			"symbols":[{"name":"Chest","rarity":0.4},{"name":"Money Bag","rarity":0.2}]}`)
	})
	mux.HandleFunc("/backdrops/spring-basket", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"name":"Old Gold","rarityPermille":10,"hex":{"centerColor":"#c8a951"}},
			{"name":"Cyberpunk","rarityPermille":12,"hex":{"centerColor":"#858ff3"}}]`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(strings.TrimPrefix(srv.URL, "http://"), 5*time.Second)
	// httptest speaks plain HTTP; the client speaks https to the real host.
	c.http.Transport = rewriteToHTTP{}
	return c
}

// rewriteToHTTP downgrades the scheme so the client can be pointed at httptest
// without loosening anything in production code.
type rewriteToHTTP struct{}

func (rewriteToHTTP) RoundTrip(r *http.Request) (*http.Response, error) {
	r.URL.Scheme = "http"
	return http.DefaultTransport.RoundTrip(r)
}

func TestCatalogueReadsRarityAndColour(t *testing.T) {
	var hits atomic.Int32
	c := fakeCatalogue(t, &hits)

	cat, err := c.Get(context.Background(), "Spring Basket")
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := cat.Model("Midas Bunny"); !ok || r != 1.5 {
		t.Fatalf("model rarity = %v %v", r, ok)
	}
	// Case and spacing fold, because Tonnel and this API do not agree on either.
	if r, ok := cat.Backdrop("old gold"); !ok || r != 1 {
		t.Fatalf("backdrop rarity = %v %v", r, ok)
	}
	if r, ok := cat.Symbol("Chest"); !ok || r != 0.4 {
		t.Fatalf("symbol rarity = %v %v", r, ok)
	}
	if _, ok := cat.Model("Nothing Like This"); ok {
		t.Fatal("an unknown model reported a rarity")
	}

	hex, ok := cat.Colour("Old Gold")
	if !ok || Family(hex) != FamilyWarm {
		t.Fatalf("backdrop colour %q is family %q, want gold to read as warm", hex, Family(hex))
	}
}

// The mint is what makes a specimen scarce, and no marketplace publishes it: a
// model floor is set by whichever ordinary example is cheapest and says nothing
// about one that also drew a 1% backdrop and a 0.4% symbol.
func TestCombinedRarityMultipliesWhatIsKnown(t *testing.T) {
	var hits atomic.Int32
	c := fakeCatalogue(t, &hits)
	cat, err := c.Get(context.Background(), "spring_basket")
	if err != nil {
		t.Fatal(err)
	}

	share, known := cat.Combined("Midas Bunny", "Old Gold", "Chest")
	if known != 3 {
		t.Fatalf("used %d of 3 attributes", known)
	}
	// 1.5% × 1% × 0.4%, as shares, back into percent.
	if want := Rarity(0.015 * 0.01 * 0.004 * 100); absDiff(float64(share), float64(want)) > 1e-12 {
		t.Fatalf("combined = %v, want %v", share, want)
	}
	// Which is the readable form the card actually wants.
	if n := cat.OneIn("Midas Bunny", "Old Gold", "Chest"); n < 1_600_000 || n > 1_700_000 {
		t.Fatalf("one in %.0f, want about 1.7 million", n)
	}

	// What is unknown is left out rather than assumed, so the answer is always
	// "at least this rare" instead of quietly inventing a factor.
	share2, known2 := cat.Combined("Midas Bunny", "No Such Backdrop", "Chest")
	if known2 != 2 || share2 <= share {
		t.Fatalf("unknown attribute changed the answer wrongly: %v/%d vs %v/%d", share2, known2, share, known)
	}
	if _, known3 := cat.Combined("nope", "nope", "nope"); known3 != 0 {
		t.Fatal("a specimen with nothing known reported a rarity")
	}
}

// The mint changes when Telegram issues a collection and never otherwise, so
// asking twice in a day is a request spent on an answer already held.
func TestCatalogueIsFetchedOncePerDay(t *testing.T) {
	var hits atomic.Int32
	c := fakeCatalogue(t, &hits)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if _, err := c.Get(ctx, "Spring Basket"); err != nil {
			t.Fatal(err)
		}
	}
	// Every spelling the API accepts is the same collection to us.
	for _, name := range []string{"spring basket", "SPRING BASKET", "spring-basket"} {
		if _, err := c.Get(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("fetched the catalogue %d times, want once", n)
	}
}

// Nothing in the desk depends on this being reachable, so an unknown collection
// is an absence of data rather than a failure that propagates.
func TestUnknownCollectionIsCachedAsAbsence(t *testing.T) {
	var hits atomic.Int32
	c := fakeCatalogue(t, &hits)
	ctx := context.Background()

	if _, err := c.Get(ctx, "Not A Collection"); err == nil {
		t.Fatal("an unknown collection came back without an error")
	}
	if _, err := c.Get(ctx, "Not A Collection"); err == nil {
		t.Fatal("the second ask also went to the network")
	}
	if _, err := c.Get(ctx, ""); err == nil {
		t.Fatal("an empty collection was looked up")
	}
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}
