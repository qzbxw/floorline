package app

import (
	"testing"

	"floorline/internal/tonnel"
)

// Keyboard handles must stay short: Telegram caps callback data at 64 bytes,
// which is why the names are not embedded directly.
func TestNavRefsRoundTrip(t *testing.T) {
	n := newNavRefs()
	key := tonnel.ModelKey{Name: "Plush Pepe", Model: "Pink Diamond"}

	ref := n.put(key)
	if len(ref) > 8 {
		t.Errorf("handle %q is too long to leave room in the callback payload", ref)
	}

	got, ok := n.get(ref)
	if !ok || got != key {
		t.Errorf("get(%q) = (%+v, %v), want the stored key", ref, got, ok)
	}
	if _, ok := n.get("nope"); ok {
		t.Error("an unknown handle must not resolve")
	}
}

func TestNavRefsHandlesAreDistinct(t *testing.T) {
	n := newNavRefs()
	a := n.put(tonnel.ModelKey{Name: "A", Model: "M"})
	b := n.put(tonnel.ModelKey{Name: "B", Model: "M"})
	if a == b {
		t.Fatal("two models were given the same handle")
	}
	ka, _ := n.get(a)
	kb, _ := n.get(b)
	if ka.Name != "A" || kb.Name != "B" {
		t.Errorf("handles resolved to the wrong models: %+v %+v", ka, kb)
	}
}

// The process runs for weeks, so the handle table must not grow forever.
func TestNavRefsEvictsOldest(t *testing.T) {
	n := newNavRefs()
	first := n.put(tonnel.ModelKey{Name: "first", Model: "M"})

	for i := 0; i < navRefLimit; i++ {
		n.put(tonnel.ModelKey{Name: "filler", Model: "M"})
	}

	if _, ok := n.get(first); ok {
		t.Error("the oldest handle should have been evicted")
	}
	if len(n.keys) > navRefLimit {
		t.Errorf("handle table holds %d entries, over the %d cap", len(n.keys), navRefLimit)
	}

	last := n.put(tonnel.ModelKey{Name: "last", Model: "M"})
	if k, ok := n.get(last); !ok || k.Name != "last" {
		t.Error("the newest handle must still resolve after eviction")
	}
}
