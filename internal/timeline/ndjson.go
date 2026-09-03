package timeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// NDJSON is the raw evidence format: one JSON object per line, meta first.
//
// It exists so a run can be re-analysed offline, including under a different
// shutdown profile, without repeating the experiment. That separation of
// measurement from interpretation is what makes profiles workable at all; see
// docs/adr/0005-shutdown-profiles.md.

// WriteNDJSON serialises a timeline. The stream is a meta record followed by
// every event in timeline order.
func WriteNDJSON(w io.Writer, t Timeline) error {
	enc := json.NewEncoder(w)

	meta := t.Meta
	header := Event{Kind: KindMeta, Meta: &meta}
	if err := enc.Encode(header); err != nil {
		return fmt.Errorf("write meta record: %w", err)
	}

	for _, e := range t.Events {
		if e.Kind == KindMeta {
			continue // meta is written from Timeline.Meta, never duplicated
		}
		if err := enc.Encode(e); err != nil {
			return fmt.Errorf("write event %d: %w", e.Seq, err)
		}
	}
	return nil
}

// ReadNDJSON parses a timeline. A meta record is optional, but if present it
// must come first.
//
// Decoding is strict about payload/kind agreement: a record whose kind says
// "request" but carries no request payload would otherwise disappear silently
// from analysis, which is exactly the class of bug that produces a wrong
// verdict with no visible cause.
func ReadNDJSON(r io.Reader) (Timeline, error) {
	dec := json.NewDecoder(r)

	var (
		out    Timeline
		record int
	)
	for {
		var e Event
		err := dec.Decode(&e)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Timeline{}, fmt.Errorf("record %d: %w", record+1, err)
		}
		record++

		if e.Kind == KindMeta {
			if record != 1 {
				return Timeline{}, fmt.Errorf("record %d: meta must be the first record", record)
			}
			if e.Meta == nil {
				return Timeline{}, fmt.Errorf("record %d: meta record has no meta payload", record)
			}
			out.Meta = *e.Meta
			continue
		}

		if err := e.Validate(); err != nil {
			return Timeline{}, fmt.Errorf("record %d: %w", record, err)
		}
		out.Events = append(out.Events, e)
	}

	sortEvents(out.Events)
	return out, nil
}

// ErrPayloadMismatch reports an event whose kind and payload disagree.
var ErrPayloadMismatch = errors.New("event payload does not match its kind")

// Validate checks that an event carries exactly the payload its kind implies.
func (e Event) Validate() error {
	present := e.payloadCount()
	if present == 0 {
		return fmt.Errorf("%w: kind %q has no payload", ErrPayloadMismatch, e.Kind)
	}
	if present > 1 {
		return fmt.Errorf("%w: kind %q has %d payloads", ErrPayloadMismatch, e.Kind, present)
	}
	if !e.payloadMatchesKind() {
		return fmt.Errorf("%w: kind %q", ErrPayloadMismatch, e.Kind)
	}
	return nil
}

func (e Event) payloadCount() int {
	n := 0
	for _, set := range []bool{
		e.Meta != nil,
		e.Stage != nil,
		e.Request != nil,
		e.Connection != nil,
		e.Readiness != nil,
		e.Listener != nil,
		e.Signal != nil,
		e.Process != nil,
		e.Log != nil,
		e.Notice != nil,
	} {
		if set {
			n++
		}
	}
	return n
}

func (e Event) payloadMatchesKind() bool {
	switch e.Kind {
	case KindMeta:
		return e.Meta != nil
	case KindStage:
		return e.Stage != nil
	case KindRequest:
		return e.Request != nil
	case KindConnection:
		return e.Connection != nil
	case KindReadiness:
		return e.Readiness != nil
	case KindListener:
		return e.Listener != nil
	case KindSignal:
		return e.Signal != nil
	case KindProcess:
		return e.Process != nil
	case KindLog:
		return e.Log != nil
	case KindNotice:
		return e.Notice != nil
	default:
		return false
	}
}
