package contracts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"bearing.example/pkg/model"
)

// ErrInvalidEvent is returned by Append and Release for an event or ID that
// breaks CheckEvent. Nothing is written.
var ErrInvalidEvent = errors.New("contracts: invalid event")

// ErrInvalidRequest is returned by Read, Commit, Committed and Trim for an
// argument outside the contract: a malformed partition or group name, a
// negative offset, a limit out of range, a commit beyond the partition's
// head or a zero cutoff.
var ErrInvalidRequest = errors.New("contracts: invalid event log request")

// The bounds on what the event log takes and returns. Raising one is
// compatible; lowering one makes an event that was accepted before refuse
// to be redelivered (docs/spec/contracts.md, "EventLog").
const (
	// MaxEventBytes bounds Event.Data. A webhook body is at most 1 MiB by
	// default (docs/security/threat-model.md, C-INGEST-4); a page of
	// observations is the largest event the core appends.
	MaxEventBytes = 8 << 20
	// MaxEventIDBytes bounds Event.ID, and MaxNameBytes a partition, a
	// group and an event type. Names are kept for as long as their events.
	MaxEventIDBytes = 512
	MaxNameBytes    = 256
	// MaxAppendEvents and MaxAppendBytes bound one Append, by the number
	// of events and by their total Data.
	MaxAppendEvents = 1000
	MaxAppendBytes  = 32 << 20
	// MaxReadEntries is the largest limit a Read takes, and MaxReadBytes
	// the Data a Read returns: it stops before the entry that would pass
	// it, but always returns at least one entry.
	MaxReadEntries = 1000
	MaxReadBytes   = 32 << 20
)

// Partition is a unit of order on the event log: events in one partition are
// read in the order they were appended, and events in different partitions
// have no order. It is the configured source the event is about; `manual`
// for people's operations; `core/<name>` for events the core appends about
// itself (docs/spec/contracts.md, "EventLog"). Source names `manual` and
// `core/...` are reserved (docs/spec/data-model.md, "Observations"), so
// they can't collide.
type Partition string

// Offset is an event's position in its partition. Offsets are positive and
// strictly increase in the order events were appended, so zero means "before
// the first". They are not promised to be consecutive: Trim leaves gaps, and
// a backend may skip numbers of its own. Offsets are comparable only within
// one partition.
type Offset int64

// Event is one input on the log: the CloudEvents attributes the log keeps,
// and the payload. The log stores it as given and does not read Data.
type Event struct {
	// ID is stable for the same input, so a redelivery repeats it:
	// "<partition>/<local ID>" (docs/spec/data-model.md, "Observations"),
	// where the local ID is the source's delivery ID or a content hash and
	// holds no slash. The partition is therefore everything before the last
	// slash, so no two partitions can produce one ID, and an ID names its
	// partition. Callers hash a delivery ID that holds a slash.
	ID string
	// Partition says which order the event belongs to.
	Partition Partition
	// Type is the CloudEvents type, for example
	// "dev.bearing.sync_requested.v1" (model.EventType).
	Type string
	// Time is when the event happened (CloudEvents time), truncated to
	// microseconds and UTC when stored. It must not be zero.
	Time time.Time
	// Data is the payload message in ProtoJSON, the data of the CloudEvent.
	// It must not be empty.
	Data []byte
	// Retain exempts the event from Trim until Release names it. Events in
	// the manual partition must set it: they are kept as long as their
	// effects are live (docs/adr/0011-identity-store-is-primary-state.md).
	Retain bool
}

// Entry is an Event as stored: where it sits in its partition, and when the
// log took it.
type Entry struct {
	Event
	// Offset is its position in Event.Partition.
	Offset Offset
	// AppendedAt is the backend's clock when the event was appended (an
	// injectable clock, not the database's transaction time), UTC and
	// truncated to microseconds. Trim goes by it and tolerates a clock that
	// is not monotonic.
	AppendedAt time.Time
}

// Appended is where one event of an Append landed.
type Appended struct {
	Partition Partition
	Offset    Offset
	// Duplicate is set when an event with the same ID was already in the
	// log: nothing was written, and Offset is the original's.
	Duplicate bool
}

// PartitionInfo describes one partition.
type PartitionInfo struct {
	Partition Partition
	// Head is the offset of the latest event appended, or zero if none. It
	// survives a Trim of every entry, and so does the partition.
	Head Offset
	// Trimmed is the highest offset Trim has removed, or zero if none. A
	// group that has committed an offset below it may have missed events:
	// the ones between its offset and Trimmed that were not Retained are
	// gone. A group with no offset yet starts at the oldest entry instead.
	Trimmed Offset
}

// EventLog is the durable, ordered, replayable log of everything that
// enters Bearing (docs/adr/0007-durable-event-log.md). It replaces the
// EventBus. A webhook is acknowledged only after Append returns for its
// event; if Append fails, the sender is told to retry.
//
// Events are ordered within a partition only. A consumer reads a partition
// after the offset its group last committed, processes the entries, and
// commits the last offset it finished. Delivery is therefore at least once,
// and the consumer's own idempotency (GraphStore.Apply's processed-event
// mark) makes a repeat harmless. The log does not hand partitions to
// consumers or lock them: two consumers of one group on one partition would
// process out of order, so the caller runs one. A new group starts at
// the oldest retained entry; a group that has committed and sits below a
// partition's Trimmed has lost events and says so instead of reading on.
// Until there is a lease, one server instance consumes.
type EventLog interface {
	// Append writes events in one atomic step, in order: either all are
	// visible to Read afterwards or none is. Events of one partition get
	// increasing offsets in the order given. Append returns only after the
	// events are durable as the backend promises (mem:// promises nothing
	// beyond the process), and a returned error means the caller may not
	// acknowledge the input. An error whose outcome is unknown, such as a
	// lost connection, may have written the events; the caller repeats the
	// Append, which then reports them as duplicates.
	//
	// An ID already in the log writes nothing and returns the original
	// position with Duplicate set, whatever else differs; an ID repeated in
	// one call is a duplicate of its first use. Duplicates are found for as
	// long as the original is retained: after Trim removes it, the same ID
	// appends again, and GraphStore.Apply's processed-event mark is what
	// stops a second apply. The result has one entry per event, in order. It
	// fails with ErrInvalidEvent if the batch or any event breaks
	// CheckEvents.
	Append(ctx context.Context, events []Event) ([]Appended, error)
	// Read returns the entries of partition with an offset greater than
	// after, oldest first, at most limit of them and at most MaxReadBytes of
	// Data (always at least one if any qualify). A partition with no
	// qualifying entry, or that doesn't exist, returns none. It does not
	// wait for new events; callers poll. An entry never becomes visible
	// before the ones with lower offsets in its partition. It fails with
	// ErrInvalidRequest (CheckRead).
	Read(ctx context.Context, partition Partition, after Offset, limit int) ([]Entry, error)
	// Commit records that group has processed partition up to and including
	// offset. It never moves a group back: a lower or equal offset changes
	// nothing and is not an error. It fails with ErrInvalidRequest for a bad
	// name or a negative offset (CheckCommit), then with ErrNotFound for a
	// partition that doesn't exist, then with ErrInvalidRequest for an
	// offset beyond the partition's head.
	Commit(ctx context.Context, group string, partition Partition, offset Offset) error
	// Committed returns the offset group last committed in partition, or
	// zero for a group or partition that has none. It fails with
	// ErrInvalidRequest for a bad name (CheckCommit).
	Committed(ctx context.Context, group string, partition Partition) (Offset, error)
	// Partitions lists every partition, ordered by name.
	Partitions(ctx context.Context) ([]PartitionInfo, error)

	// Trim removes the entries appended before the cutoff that are not
	// Retained, and returns how many. This is the retention window the data
	// model refers to ("event log retention", docs/spec/data-model.md): the
	// caller passes now minus the configured window. It does not look at
	// what groups have committed. It fails with ErrInvalidRequest for a zero
	// cutoff (CheckTrim).
	Trim(ctx context.Context, before time.Time) (int, error)
	// Release clears Retain on the events with these IDs, so a later Trim
	// can remove them. An ID the log doesn't hold, because it was never
	// appended or was already trimmed, is ignored, and an empty list does
	// nothing. The caller releases an event only after the write that ends
	// its effect has committed; a crash in between leaves it retained, which
	// is harmless. It fails with ErrInvalidEvent for more than
	// MaxAppendEvents IDs or an ID out of bounds (CheckRelease).
	Release(ctx context.Context, ids []string) error
}

// CheckEvents reports the first way events breaks the Append bounds, as an
// error wrapping ErrInvalidEvent. Every backend calls it from Append, so all
// refuse the same events. Errors quote at most 64 bytes of the input.
func CheckEvents(events []Event) error {
	if len(events) == 0 {
		return fmt.Errorf("%w: no events", ErrInvalidEvent)
	}
	if len(events) > MaxAppendEvents {
		return fmt.Errorf("%w: %d events, over the limit of %d", ErrInvalidEvent, len(events), MaxAppendEvents)
	}
	total := 0
	for i := range events {
		if err := CheckEvent(&events[i]); err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
		total += len(events[i].Data)
	}
	if total > MaxAppendBytes {
		return fmt.Errorf("%w: %d bytes of data, over the limit of %d", ErrInvalidEvent, total, MaxAppendBytes)
	}
	return nil
}

// CheckEvent reports the first way e breaks the rules of an Event, as an
// error wrapping ErrInvalidEvent.
func CheckEvent(e *Event) error {
	if err := checkText("partition", string(e.Partition), MaxNameBytes); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	if err := checkText("ID", e.ID, MaxEventIDBytes); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	local, ok := strings.CutPrefix(e.ID, string(e.Partition)+"/")
	if !ok || local == "" || strings.Contains(local, "/") {
		return fmt.Errorf("%w: ID %s is not its partition, a slash and a local ID without a slash", ErrInvalidEvent, quote(e.ID))
	}
	if e.Partition == model.SourceManual && !e.Retain {
		return fmt.Errorf("%w: an event in the manual partition must set Retain", ErrInvalidEvent)
	}
	if err := checkText("type", e.Type, MaxNameBytes); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	if e.Time.IsZero() {
		return fmt.Errorf("%w: no time", ErrInvalidEvent)
	}
	if len(e.Data) == 0 {
		return fmt.Errorf("%w: no data", ErrInvalidEvent)
	}
	if len(e.Data) > MaxEventBytes {
		return fmt.Errorf("%w: %d bytes of data, over the limit of %d", ErrInvalidEvent, len(e.Data), MaxEventBytes)
	}
	return nil
}

// CheckEventID reports whether id is a well-formed event ID, as an error
// wrapping ErrInvalidEvent. It checks the bounds only; CheckEvent also holds
// an ID to the shape "<partition>/<local ID>".
func CheckEventID(id string) error {
	if err := checkText("ID", id, MaxEventIDBytes); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	return nil
}

// CheckPartition reports whether p is a well-formed partition name: 1 to
// MaxNameBytes bytes of valid UTF-8 with no control characters. The error
// wraps ErrInvalidRequest.
func CheckPartition(p Partition) error {
	if err := checkText("partition", string(p), MaxNameBytes); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return nil
}

// CheckGroup reports whether g is a well-formed consumer group name, by the
// rule for a partition name.
func CheckGroup(g string) error {
	if err := checkText("group", g, MaxNameBytes); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	return nil
}

// CheckRead reports whether the arguments of Read are in range, as an error
// wrapping ErrInvalidRequest.
func CheckRead(partition Partition, after Offset, limit int) error {
	if err := CheckPartition(partition); err != nil {
		return err
	}
	if after < 0 {
		return fmt.Errorf("%w: offset %d is negative", ErrInvalidRequest, after)
	}
	if limit < 1 || limit > MaxReadEntries {
		return fmt.Errorf("%w: limit %d is not from 1 to %d", ErrInvalidRequest, limit, MaxReadEntries)
	}
	return nil
}

// CheckCommit reports whether the arguments of Commit and Committed are well
// formed, as an error wrapping ErrInvalidRequest. Committed passes offset 0.
func CheckCommit(group string, partition Partition, offset Offset) error {
	if err := CheckGroup(group); err != nil {
		return err
	}
	if err := CheckPartition(partition); err != nil {
		return err
	}
	if offset < 0 {
		return fmt.Errorf("%w: offset %d is negative", ErrInvalidRequest, offset)
	}
	return nil
}

// CheckTrim reports whether the cutoff of Trim is usable, as an error
// wrapping ErrInvalidRequest.
func CheckTrim(before time.Time) error {
	if before.IsZero() {
		return fmt.Errorf("%w: the cutoff is zero", ErrInvalidRequest)
	}
	return nil
}

// CheckRelease reports whether the IDs of Release are within bounds, as an
// error wrapping ErrInvalidEvent.
func CheckRelease(ids []string) error {
	if len(ids) > MaxAppendEvents {
		return fmt.Errorf("%w: %d IDs, over the limit of %d", ErrInvalidEvent, len(ids), MaxAppendEvents)
	}
	for _, id := range ids {
		if err := CheckEventID(id); err != nil {
			return err
		}
	}
	return nil
}

// checkText checks a name: non-empty, at most limit bytes, valid UTF-8, no
// control characters (a NUL would not fit a text column).
func checkText(what, s string, limit int) error {
	switch {
	case s == "":
		return fmt.Errorf("%s is empty", what)
	case len(s) > limit:
		return fmt.Errorf("%s is %d bytes, over the limit of %d", what, len(s), limit)
	case !utf8.ValidString(s):
		return fmt.Errorf("%s %s is not valid UTF-8", what, quote(s))
	case strings.IndexFunc(s, unicode.IsControl) >= 0:
		return fmt.Errorf("%s %s has a control character", what, quote(s))
	}
	return nil
}

// quote quotes at most 64 bytes of s.
func quote(s string) string {
	if len(s) > 64 {
		s = strings.ToValidUTF8(s[:64], "") + "…"
	}
	return fmt.Sprintf("%q", s)
}
