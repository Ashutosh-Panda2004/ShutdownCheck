package timeline

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Evidence limits bound files passed to `analyze`; live recordings are much
// smaller even at the default 100k-request cap.
const (
	MaxNDJSONBytes       = 256 << 20
	MaxNDJSONRecordBytes = 1 << 20
	MaxNDJSONRecords     = 1_000_000
	MaxEvidenceDuration  = 30 * 24 * time.Hour
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
	meta.Dropped = t.Dropped
	meta.DroppedAuxiliary = t.DroppedAuxiliary
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
	return readNDJSONWithLimits(r, MaxNDJSONBytes, MaxNDJSONRecordBytes, MaxNDJSONRecords)
}

func readNDJSONWithLimits(r io.Reader, maxBytes, maxRecordBytes, maxRecords int) (Timeline, error) {
	if maxBytes < 1 || maxRecordBytes < 1 || maxRecords < 1 {
		return Timeline{}, errors.New("NDJSON limits must be positive")
	}

	limited := &io.LimitedReader{R: r, N: int64(maxBytes) + 1}
	scanner := bufio.NewScanner(limited)
	initialBuffer := min(64<<10, maxRecordBytes+1)
	scanner.Buffer(make([]byte, initialBuffer), maxRecordBytes+1)

	var (
		out    Timeline
		record int
	)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) > maxRecordBytes {
			return Timeline{}, fmt.Errorf("record %d exceeds the %d-byte record limit", record+1, maxRecordBytes)
		}
		if strings.TrimSpace(string(line)) == "" {
			continue
		}
		if record >= maxRecords {
			return Timeline{}, fmt.Errorf("evidence exceeds the %d-record limit", maxRecords)
		}

		var e Event
		if err := decodeEvent(line, &e); err != nil {
			return Timeline{}, fmt.Errorf("record %d: %w", record+1, err)
		}
		record++

		if err := e.Validate(); err != nil {
			return Timeline{}, fmt.Errorf("record %d: %w", record, err)
		}
		if e.Kind == KindMeta {
			if record != 1 {
				return Timeline{}, fmt.Errorf("record %d: meta must be the first record", record)
			}
			if e.Meta == nil {
				return Timeline{}, fmt.Errorf("record %d: meta record has no meta payload", record)
			}
			if e.Meta.Dropped < 0 || e.Meta.DroppedAuxiliary < 0 {
				return Timeline{}, fmt.Errorf("record %d: meta dropped counts cannot be negative", record)
			}
			out.Meta = *e.Meta
			out.Dropped = e.Meta.Dropped
			out.DroppedAuxiliary = e.Meta.DroppedAuxiliary
			continue
		}
		out.Events = append(out.Events, e)
	}
	consumed := int64(maxBytes+1) - limited.N
	if consumed > int64(maxBytes) {
		return Timeline{}, fmt.Errorf("evidence exceeds the %d-byte limit", maxBytes)
	}
	if err := scanner.Err(); err != nil {
		return Timeline{}, fmt.Errorf("record %d exceeds the %d-byte record limit or could not be read: %w",
			record+1, maxRecordBytes, err)
	}

	sortEvents(out.Events)
	return out, nil
}

func decodeEvent(line []byte, event *Event) error {
	if err := rejectDuplicateJSONFields(line); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(event); err != nil {
		return err
	}

	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("record contains more than one JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("record contains more than one JSON value")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	_, err = decoder.Token()
	return err
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
	return e.validateValues()
}

func (e Event) validateValues() error {
	if !validEvidenceDuration(e.Offset) {
		return fmt.Errorf("event offset must be between 0 and %s, got %s", MaxEvidenceDuration, e.Offset)
	}

	switch e.Kind {
	case KindStage:
		if !validStage(e.Stage.Stage) {
			return fmt.Errorf("unknown stage %q", e.Stage.Stage)
		}
	case KindRequest:
		r := e.Request
		if !validEvidenceDuration(r.Scheduled) || !validEvidenceDuration(r.Sent) || !validEvidenceDuration(r.Done) {
			return fmt.Errorf("request timestamps must be between 0 and %s", MaxEvidenceDuration)
		}
		if r.Done < r.Sent {
			return fmt.Errorf("request completed at %s before it was sent at %s", r.Done, r.Sent)
		}
		if e.Offset != r.Done {
			return fmt.Errorf("request event offset %s does not match completion time %s", e.Offset, r.Done)
		}
		if !validOutcome(r.Outcome, false) {
			return fmt.Errorf("unknown request outcome %q", r.Outcome)
		}
		if r.Status < 0 || r.Status > 999 {
			return fmt.Errorf("request status %d is outside the HTTP status range", r.Status)
		}
	case KindConnection:
		if !validConnPhase(e.Connection.Phase) {
			return fmt.Errorf("unknown connection phase %q", e.Connection.Phase)
		}
		if !validEvidenceDuration(e.Connection.TLSHandshake) {
			return fmt.Errorf("TLS handshake duration must be between 0 and %s", MaxEvidenceDuration)
		}
		if e.Connection.Termination != "" && !validConnTermination(e.Connection.Termination) {
			return fmt.Errorf("unknown connection termination %q", e.Connection.Termination)
		}
	case KindReadiness:
		if !validEvidenceDuration(e.Readiness.Latency) {
			return fmt.Errorf("readiness latency must be between 0 and %s", MaxEvidenceDuration)
		}
		if !validOutcome(e.Readiness.Outcome, true) {
			return fmt.Errorf("unknown readiness outcome %q", e.Readiness.Outcome)
		}
		if e.Readiness.Status < 0 || e.Readiness.Status > 999 {
			return fmt.Errorf("readiness status %d is outside the HTTP status range", e.Readiness.Status)
		}
	case KindListener:
		if !validEvidenceDuration(e.Listener.Latency) {
			return fmt.Errorf("listener latency must be between 0 and %s", MaxEvidenceDuration)
		}
		if !validOutcome(e.Listener.Outcome, true) {
			return fmt.Errorf("unknown listener outcome %q", e.Listener.Outcome)
		}
	case KindSignal:
		if !validEvidenceDuration(e.Signal.Skew) {
			return fmt.Errorf("signal skew must be between 0 and %s", MaxEvidenceDuration)
		}
		if !validSignal(e.Signal.Signal) {
			return fmt.Errorf("unknown signal %q", e.Signal.Signal)
		}
	case KindProcess:
		if !validProcPhase(e.Process.Phase) {
			return fmt.Errorf("unknown process phase %q", e.Process.Phase)
		}
	case KindLog:
		if e.Log.Stream != "stdout" && e.Log.Stream != "stderr" {
			return fmt.Errorf("unknown log stream %q", e.Log.Stream)
		}
	case KindNotice:
		if e.Notice.Level != "info" && e.Notice.Level != "warn" && e.Notice.Level != "error" {
			return fmt.Errorf("unknown notice level %q", e.Notice.Level)
		}
	}
	return nil
}

func validEvidenceDuration(value time.Duration) bool {
	return value >= 0 && value <= MaxEvidenceDuration
}

func validStage(stage Stage) bool {
	switch stage {
	case StagePreflight, StageStartTarget, StageWaitReady, StageWarmup, StageCalibrate,
		StageSteady, StagePreStop, StageSignal, StageObserve, StageSigkill,
		StagePostExit, StageComplete:
		return true
	default:
		return false
	}
}

func validOutcome(outcome Outcome, optional bool) bool {
	if optional && outcome == "" {
		return true
	}
	switch outcome {
	case OutcomeOK, OutcomeHTTPError, OutcomeRefused, OutcomeReset, OutcomeTimeout,
		OutcomeEOF, OutcomeTLSError, OutcomeDNSError, OutcomeAbandoned, OutcomeOther:
		return true
	default:
		return false
	}
}

func validConnPhase(phase ConnPhase) bool {
	return phase == ConnOpen || phase == ConnReuse || phase == ConnClose
}

func validConnTermination(termination ConnTermination) bool {
	return termination == TermFIN || termination == TermRST || termination == TermTimeout || termination == TermUnknown
}

func validSignal(signal string) bool {
	return signal == "TERM" || signal == "INT" || signal == "QUIT" || signal == "KILL"
}

func validProcPhase(phase ProcPhase) bool {
	return phase == ProcStarted || phase == ProcReady || phase == ProcExited || phase == ProcPortReleased
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
