// Package anthropicstream renders neutral reducer events as Anthropic Messages
// SSE and buffered JSON responses.
package anthropicstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Aotricx/Clodex/internal/reducer"
	"github.com/Aotricx/Clodex/internal/stopscan"
)

var (
	ErrStreamFinished = errors.New("Anthropic stream is finished")
	ErrInvalidEvent   = errors.New("invalid Anthropic semantic event")
)

// Options configures one response stream.
type Options struct {
	MessageID     string
	Model         string
	StopSequences []string
	Flush         func() error
}

type openBlock struct {
	kind    reducer.BlockType
	scanner *stopscan.Scanner
}

// Encoder writes exact event/data SSE frames synchronously. It owns no
// goroutines; caller cancellation interrupts through the supplied writer.
type Encoder struct {
	writer io.Writer
	flush  func() error
	id     string
	model  string
	stops  []string

	started  bool
	finished bool
	matched  string
	matchAt  int
	open     map[int]*openBlock
}

// New constructs a stream encoder without writing headers or frames.
func New(writer io.Writer, options Options) (*Encoder, error) {
	if writer == nil {
		return nil, errors.New("create Anthropic stream: nil writer")
	}
	if options.MessageID == "" || options.Model == "" {
		return nil, errors.New("create Anthropic stream: message ID and model are required")
	}
	if _, err := stopscan.New(options.StopSequences); err != nil {
		return nil, fmt.Errorf("create Anthropic stream: %w", err)
	}
	return &Encoder{
		writer: writer,
		flush:  options.Flush,
		id:     options.MessageID,
		model:  options.Model,
		stops:  append([]string(nil), options.StopSequences...),
		open:   make(map[int]*openBlock),
	}, nil
}

// Encode renders one reducer event. Warning and rate-limit events are telemetry
// for the caller and intentionally produce no Anthropic content frame.
func (e *Encoder) Encode(event reducer.Event) error {
	if e.finished {
		return ErrStreamFinished
	}
	switch event.Kind {
	case reducer.KindWarning, reducer.KindRateLimits:
		return nil
	case reducer.KindContentStart:
		return e.contentStart(event)
	case reducer.KindContentDelta:
		return e.contentDelta(event)
	case reducer.KindContentStop:
		return e.contentStop(event)
	case reducer.KindTerminal:
		return e.terminal(event.Terminal)
	default:
		return fmt.Errorf("%w: unknown kind %d", ErrInvalidEvent, event.Kind)
	}
}

// Ping emits Anthropic's idle heartbeat frame.
func (e *Encoder) Ping() error {
	if e.finished {
		return ErrStreamFinished
	}
	return e.emit("ping", struct {
		Type string `json:"type"`
	}{Type: "ping"})
}

func (e *Encoder) MatchedStop() string { return e.matched }

func (e *Encoder) ensureStart() error {
	if e.started {
		return nil
	}
	e.started = true
	payload := struct {
		Type    string `json:"type"`
		Message struct {
			ID           string            `json:"id"`
			Type         string            `json:"type"`
			Role         string            `json:"role"`
			Model        string            `json:"model"`
			Content      []json.RawMessage `json:"content"`
			StopReason   *string           `json:"stop_reason"`
			StopSequence *string           `json:"stop_sequence"`
			Usage        streamStartUsage  `json:"usage"`
		} `json:"message"`
	}{Type: "message_start"}
	payload.Message.ID = e.id
	payload.Message.Type = "message"
	payload.Message.Role = "assistant"
	payload.Message.Model = e.model
	payload.Message.Content = []json.RawMessage{}
	// Responses supplies authoritative usage only in its terminal object.
	// Final message_delta carries exact totals; start remains protocol zero.
	return e.emit("message_start", payload)
}

type streamStartUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func (e *Encoder) contentStart(event reducer.Event) error {
	if e.matched != "" {
		return nil
	}
	if _, exists := e.open[event.Index]; exists {
		return fmt.Errorf("%w: content index %d opened twice", ErrInvalidEvent, event.Index)
	}
	if err := e.ensureStart(); err != nil {
		return err
	}
	var block any
	state := &openBlock{kind: event.Block.Type}
	switch event.Block.Type {
	case reducer.BlockThinking:
		block = struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{Type: "thinking"}
	case reducer.BlockText:
		block = struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text"}
		scanner, err := stopscan.New(e.stops)
		if err != nil {
			return err
		}
		state.scanner = scanner
	case reducer.BlockToolUse, reducer.BlockServerToolUse:
		block = struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		}{Type: string(event.Block.Type), ID: event.Block.ID, Name: event.Block.Name, Input: map[string]any{}}
	default:
		return fmt.Errorf("%w: unsupported block type %q", ErrInvalidEvent, event.Block.Type)
	}
	raw, err := json.Marshal(block)
	if err != nil {
		return err
	}
	e.open[event.Index] = state
	return e.emit("content_block_start", struct {
		Type         string          `json:"type"`
		Index        int             `json:"index"`
		ContentBlock json.RawMessage `json:"content_block"`
	}{Type: "content_block_start", Index: event.Index, ContentBlock: raw})
}

func (e *Encoder) contentDelta(event reducer.Event) error {
	state := e.open[event.Index]
	if state == nil {
		if e.matched != "" {
			return nil
		}
		return fmt.Errorf("%w: delta for unopened index %d", ErrInvalidEvent, event.Index)
	}
	if e.matched != "" {
		return nil
	}
	text := event.Delta.Text
	if event.Delta.Type == reducer.DeltaText {
		if state.kind != reducer.BlockText || state.scanner == nil {
			return fmt.Errorf("%w: text delta for non-text index %d", ErrInvalidEvent, event.Index)
		}
		result := state.scanner.Feed(text)
		text = result.Text
		if result.Matched != "" {
			e.matched = result.Matched
			e.matchAt = event.Index
		}
	}
	if text == "" {
		return nil
	}
	return e.emitDelta(event.Index, event.Delta.Type, text)
}

func (e *Encoder) emitDelta(index int, deltaType reducer.DeltaType, text string) error {
	var delta any
	switch deltaType {
	case reducer.DeltaThinking:
		delta = struct {
			Type     string `json:"type"`
			Thinking string `json:"thinking"`
		}{Type: "thinking_delta", Thinking: text}
	case reducer.DeltaSignature:
		delta = struct {
			Type      string `json:"type"`
			Signature string `json:"signature"`
		}{Type: "signature_delta", Signature: text}
	case reducer.DeltaText:
		delta = struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text_delta", Text: text}
	case reducer.DeltaInputJSON:
		delta = struct {
			Type        string `json:"type"`
			PartialJSON string `json:"partial_json"`
		}{Type: "input_json_delta", PartialJSON: text}
	default:
		return fmt.Errorf("%w: unsupported delta type %q", ErrInvalidEvent, deltaType)
	}
	raw, err := json.Marshal(delta)
	if err != nil {
		return err
	}
	return e.emit("content_block_delta", struct {
		Type  string          `json:"type"`
		Index int             `json:"index"`
		Delta json.RawMessage `json:"delta"`
	}{Type: "content_block_delta", Index: index, Delta: raw})
}

func (e *Encoder) contentStop(event reducer.Event) error {
	state := e.open[event.Index]
	if state == nil {
		if e.matched != "" {
			return nil
		}
		return fmt.Errorf("%w: stop for unopened index %d", ErrInvalidEvent, event.Index)
	}
	if e.matched != "" && event.Index > e.matchAt {
		delete(e.open, event.Index)
		return nil
	}
	if state.kind == reducer.BlockText && e.matched == "" {
		result := state.scanner.Finish()
		if result.Text != "" {
			if err := e.emitDelta(event.Index, reducer.DeltaText, result.Text); err != nil {
				return err
			}
		}
	}
	delete(e.open, event.Index)
	return e.emit("content_block_stop", struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
	}{Type: "content_block_stop", Index: event.Index})
}

func (e *Encoder) terminal(terminal *reducer.Terminal) error {
	if terminal == nil {
		return fmt.Errorf("%w: nil terminal", ErrInvalidEvent)
	}
	if len(e.open) != 0 {
		return fmt.Errorf("%w: terminal with %d open blocks", ErrInvalidEvent, len(e.open))
	}
	if err := e.ensureStart(); err != nil {
		return err
	}
	reason := terminal.StopReason
	sequence := terminal.StopSequence
	if e.matched != "" {
		reason = reducer.StopStopSequence
		sequence = e.matched
	}
	var sequencePtr *string
	if sequence != "" {
		sequenceCopy := sequence
		sequencePtr = &sequenceCopy
	}
	payload := struct {
		Type  string `json:"type"`
		Delta struct {
			StopReason   reducer.StopReason `json:"stop_reason"`
			StopSequence *string            `json:"stop_sequence"`
		} `json:"delta"`
		Usage bufferedUsage `json:"usage"`
	}{Type: "message_delta", Usage: mapUsage(terminal.Usage)}
	payload.Delta.StopReason = reason
	payload.Delta.StopSequence = sequencePtr
	if err := e.emit("message_delta", payload); err != nil {
		return err
	}
	if err := e.emit("message_stop", struct {
		Type string `json:"type"`
	}{Type: "message_stop"}); err != nil {
		return err
	}
	e.finished = true
	return nil
}

func (e *Encoder) emit(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Anthropic %s: %w", event, err)
	}
	frame := make([]byte, 0, len(event)+len(data)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, '\n')
	frame = append(frame, "data: "...)
	frame = append(frame, data...)
	frame = append(frame, '\n', '\n')
	if err := writeAll(e.writer, frame); err != nil {
		return fmt.Errorf("write Anthropic %s: %w", event, err)
	}
	if e.flush != nil {
		if err := e.flush(); err != nil {
			return fmt.Errorf("flush Anthropic %s: %w", event, err)
		}
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
