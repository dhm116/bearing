package memstore

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"bearing.example/pkg/contracts"
)

var _ contracts.EventLog = (*Store)(nil)

// eventLog is the store's event log. It has a lock of its own so that
// appending and reading never wait for an apply.
type eventLog struct {
	mu         sync.Mutex
	partitions map[contracts.Partition]*logPartition
	groups     map[groupKey]contracts.Offset
	// byID finds the entry of an event ID. An ID names its partition, so
	// one map serves them all.
	byID map[string]idEntry
}

type idEntry struct {
	partition contracts.Partition
	offset    contracts.Offset
}

type groupKey struct {
	group     string
	partition contracts.Partition
}

// logPartition holds one partition's entries in offset order. Trim leaves
// gaps in the offsets, so entries are found by binary search.
type logPartition struct {
	entries []contracts.Entry
	head    contracts.Offset
	trimmed contracts.Offset
}

func newEventLog() *eventLog {
	return &eventLog{partitions: map[contracts.Partition]*logPartition{}, groups: map[groupKey]contracts.Offset{}, byID: map[string]idEntry{}}
}

// Append implements contracts.EventLog.
func (s *Store) Append(ctx context.Context, events []contracts.Event) ([]contracts.Appended, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := contracts.CheckEvents(events); err != nil {
		return nil, err
	}
	at := s.Now().UTC().Truncate(time.Microsecond)
	l := s.log
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]contracts.Appended, len(events))
	for i := range events {
		e := events[i]
		if at, ok := l.byID[e.ID]; ok {
			out[i] = contracts.Appended{Partition: at.partition, Offset: at.offset, Duplicate: true}
			continue
		}
		p := l.partitions[e.Partition]
		if p == nil {
			p = &logPartition{}
			l.partitions[e.Partition] = p
		}
		p.head++
		e.Time = e.Time.UTC().Truncate(time.Microsecond)
		e.Data = bytes.Clone(e.Data)
		p.entries = append(p.entries, contracts.Entry{Event: e, Offset: p.head, AppendedAt: at})
		l.byID[e.ID] = idEntry{e.Partition, p.head}
		out[i] = contracts.Appended{Partition: e.Partition, Offset: p.head}
	}
	return out, nil
}

// Read implements contracts.EventLog.
func (s *Store) Read(ctx context.Context, partition contracts.Partition, after contracts.Offset, limit int) ([]contracts.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := contracts.CheckRead(partition, after, limit); err != nil {
		return nil, err
	}
	l := s.log
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.partitions[partition]
	if p == nil {
		return nil, nil
	}
	i := sort.Search(len(p.entries), func(i int) bool { return p.entries[i].Offset > after })
	var out []contracts.Entry
	bytesRead := 0
	for ; i < len(p.entries) && len(out) < limit; i++ {
		e := p.entries[i]
		if len(out) > 0 && bytesRead+len(e.Data) > contracts.MaxReadBytes {
			break
		}
		bytesRead += len(e.Data)
		e.Data = bytes.Clone(e.Data)
		out = append(out, e)
	}
	return out, nil
}

// Commit implements contracts.EventLog.
func (s *Store) Commit(ctx context.Context, group string, partition contracts.Partition, offset contracts.Offset) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := contracts.CheckCommit(group, partition, offset); err != nil {
		return err
	}
	l := s.log
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.partitions[partition]
	if p == nil {
		return fmt.Errorf("partition %s: %w", partition, contracts.ErrNotFound)
	}
	if offset > p.head {
		return fmt.Errorf("%w: offset %d is beyond the head, %d", contracts.ErrInvalidRequest, offset, p.head)
	}
	k := groupKey{group, partition}
	if offset > l.groups[k] {
		l.groups[k] = offset
	}
	return nil
}

// Committed implements contracts.EventLog.
func (s *Store) Committed(ctx context.Context, group string, partition contracts.Partition) (contracts.Offset, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := contracts.CheckCommit(group, partition, 0); err != nil {
		return 0, err
	}
	l := s.log
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.groups[groupKey{group, partition}], nil
}

// Partitions implements contracts.EventLog.
func (s *Store) Partitions(ctx context.Context) ([]contracts.PartitionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l := s.log
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]contracts.PartitionInfo, 0, len(l.partitions))
	for name, p := range l.partitions {
		out = append(out, contracts.PartitionInfo{Partition: name, Head: p.head, Trimmed: p.trimmed})
	}
	slices.SortFunc(out, func(a, b contracts.PartitionInfo) int {
		return strings.Compare(string(a.Partition), string(b.Partition))
	})
	return out, nil
}

// Trim implements contracts.EventLog.
func (s *Store) Trim(ctx context.Context, before time.Time, groups []string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := contracts.CheckTrim(before, groups); err != nil {
		return 0, err
	}
	l := s.log
	l.mu.Lock()
	defer l.mu.Unlock()
	removed := 0
	for name, p := range l.partitions {
		// Entries above the lowest offset a required group committed stay.
		applied := contracts.Offset(math.MaxInt64)
		for _, g := range groups {
			applied = min(applied, l.groups[groupKey{g, name}])
		}
		kept := p.entries[:0]
		for _, e := range p.entries {
			if e.Retain || !e.AppendedAt.Before(before) || e.Offset > applied {
				kept = append(kept, e)
				continue
			}
			delete(l.byID, e.ID)
			p.trimmed = max(p.trimmed, e.Offset)
			removed++
		}
		clear(p.entries[len(kept):])
		p.entries = kept
	}
	return removed, nil
}

// Release implements contracts.EventLog.
func (s *Store) Release(ctx context.Context, ids []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := contracts.CheckRelease(ids); err != nil {
		return err
	}
	l := s.log
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		at, ok := l.byID[id]
		if !ok {
			continue
		}
		es := l.partitions[at.partition].entries
		i := sort.Search(len(es), func(i int) bool { return es[i].Offset >= at.offset })
		es[i].Retain = false
	}
	return nil
}
