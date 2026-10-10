package contracts

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	modelv1alpha1 "bearing.example/gen/go/bearing/model/v1alpha1"
)

// The page sizes of GraphStore.ChangesPage.
const (
	// DefaultChangesLimit is the page size when a request sets none.
	DefaultChangesLimit = 100
	// MaxChangesLimit is the largest page a request may ask for.
	MaxChangesLimit = 1000
)

// ChangesRequest asks GraphStore.ChangesPage for the newest changes in a
// window (docs/spec/data-model.md, "What changed").
type ChangesRequest struct {
	// Filter selects the facts, as for Changes.
	Filter FactFilter
	// T1 and T2 are the window's ends, as for Changes: zero is now, and T1
	// must not be after T2.
	T1, T2 time.Time
	// Axis is the axis the window is read on.
	Axis Axis
	// Limit is the most changes to return: DefaultChangesLimit when zero, an
	// error when negative or over MaxChangesLimit.
	Limit int
	// After continues a listing: the Next of the page before it, asked of
	// the T1 and T2 that page returned. Nil starts at the newest change.
	After *ChangeCursor
}

// ChangeCursor is a position in a listing of changes: the one a page ended
// on. Changes are ordered newest first, then by fact ID.
type ChangeCursor struct {
	// ChangedAt is the change's changed_at.
	ChangedAt time.Time
	// FactID is the change's fact_id.
	FactID string
}

// ChangesPage is one page of a listing of changes.
type ChangesPage struct {
	// Changes are the page's changes, newest first, then by fact ID, each
	// with changed_at set.
	Changes []*modelv1alpha1.FactChange
	// Next is the cursor of the page after this one, or nil on the last page.
	Next *ChangeCursor
	// T1 and T2 are the window the page was read on, with a zero time
	// replaced by the time it meant. Pass them back with Next, so that every
	// page of a listing is read on the same window.
	T1, T2 time.Time
}

// Check reports whether r asks for a page the store can give: a limit in
// range and a cursor that names a fact. The window is the store's to check.
func (r ChangesRequest) Check() error {
	if r.Limit < 0 || r.Limit > MaxChangesLimit {
		return fmt.Errorf("limit %d: want 1 to %d, or 0 for %d", r.Limit, MaxChangesLimit, DefaultChangesLimit)
	}
	if r.After != nil && r.After.FactID == "" {
		return errors.New("cursor has no fact ID")
	}
	return nil
}

// PageSize is the number of changes r asks for, with the default applied.
func (r ChangesRequest) PageSize() int {
	if r.Limit == 0 {
		return DefaultChangesLimit
	}
	return r.Limit
}

// CompareChanges orders changes as a listing does: newest first, then by
// fact ID.
func CompareChanges(a, b *modelv1alpha1.FactChange) int {
	if c := b.GetChangedAt().AsTime().Compare(a.GetChangedAt().AsTime()); c != 0 {
		return c
	}
	return strings.Compare(a.GetFactId(), b.GetFactId())
}

// Precedes reports whether c is ahead of ch in a listing, so that a page
// after c includes ch.
func (c ChangeCursor) Precedes(ch *modelv1alpha1.FactChange) bool {
	at := ch.GetChangedAt().AsTime()
	return at.Before(c.ChangedAt) || at.Equal(c.ChangedAt) && ch.GetFactId() > c.FactID
}

// CursorOf is the cursor that continues a listing after ch.
func CursorOf(ch *modelv1alpha1.FactChange) *ChangeCursor {
	return &ChangeCursor{ChangedAt: ch.GetChangedAt().AsTime(), FactID: ch.GetFactId()}
}

// PageChanges cuts the page r asks for out of all the changes in its window:
// it sorts them as a listing is, drops those up to r.After and keeps the
// first r.PageSize(). The cursor is that of the page's last change, or nil
// when nothing follows. Backends share it so that a listing reads the same
// in each. all is reordered.
func PageChanges(all []*modelv1alpha1.FactChange, r ChangesRequest) ([]*modelv1alpha1.FactChange, *ChangeCursor) {
	slices.SortFunc(all, CompareChanges)
	if r.After != nil {
		i, _ := slices.BinarySearchFunc(all, r.After, func(c *modelv1alpha1.FactChange, cur *ChangeCursor) int {
			if cur.Precedes(c) {
				return 1
			}
			return -1
		})
		all = all[i:]
	}
	n := r.PageSize()
	if len(all) <= n {
		return all, nil
	}
	return all[:n], CursorOf(all[n-1])
}
