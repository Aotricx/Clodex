// Package codexstream parses Codex Responses API server-sent events.
package codexstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
	"unicode/utf8"
)

const DefaultMaxEventBytes = 16 << 20

var (
	ErrEventTooLarge      = errors.New("Codex SSE event exceeds size limit")
	ErrMalformedEvent     = errors.New("malformed Codex SSE event")
	ErrTruncatedFrame     = errors.New("Codex SSE stream ended with a truncated frame")
	ErrEOFWithoutTerminal = errors.New("Codex SSE stream ended without a terminal event")
)

const (
	TypeResponseCreated   = "response.created"
	TypeOutputTextDelta   = "response.output_text.delta"
	TypeResponseCompleted = "response.completed"
)

// Options controls parser resource limits.
type Options struct {
	MaxEventBytes int
}

// Event is one validated Codex SSE event. Raw contains an owned JSON payload.
type Event struct {
	Type  string
	Event string
	Raw   json.RawMessage
	ID    string
	Retry time.Duration
}

// Decode unmarshals the preserved payload into dst.
func (e Event) Decode(dst any) error {
	if err := json.Unmarshal(e.Raw, dst); err != nil {
		return fmt.Errorf("decode %s: %w", e.Type, err)
	}
	return nil
}

func (e Event) Known() bool {
	_, ok := knownTypes[e.Type]
	return ok
}

func (e Event) Terminal() bool {
	_, ok := terminalTypes[e.Type]
	return ok
}

// Parser incrementally reads bounded SSE events.
type Parser struct {
	ctx context.Context
	r   *bufio.Reader
	max int

	lastID      string
	retry       time.Duration
	terminal    bool
	ended       error
	frameBytes  int
	frameActive bool
	eventName   string
	data        [][]byte
}

// New constructs a parser. A non-positive limit uses DefaultMaxEventBytes.
func New(ctx context.Context, r io.Reader, opts Options) *Parser {
	if ctx == nil {
		ctx = context.Background()
	}
	maxBytes := opts.MaxEventBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxEventBytes
	}
	return &Parser{ctx: ctx, r: bufio.NewReaderSize(r, 32<<10), max: maxBytes}
}

func (p *Parser) TerminalSeen() bool { return p.terminal }

// Next returns the next data-bearing SSE event.
func (p *Parser) Next() (Event, error) {
	if p.ended != nil {
		return Event{}, p.ended
	}
	for {
		if err := p.ctx.Err(); err != nil {
			p.ended = err
			return Event{}, err
		}
		line, err := p.readLine()
		if err != nil {
			if len(line) > 0 && errors.Is(err, io.EOF) {
				p.applyLine(line)
			}
			return Event{}, p.finish(err)
		}
		if len(line) == 0 {
			event, ok, err := p.dispatch()
			if err != nil {
				p.ended = err
				return Event{}, err
			}
			if ok {
				if event.Terminal() {
					p.terminal = true
				}
				return event, nil
			}
			continue
		}
		p.applyLine(line)
	}
}

func (p *Parser) readLine() ([]byte, error) {
	var line []byte
	for {
		if err := p.ctx.Err(); err != nil {
			return nil, err
		}
		part, err := p.r.ReadSlice('\n')
		if ctxErr := p.ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		p.frameBytes += len(part)
		if p.frameBytes > p.max {
			return nil, fmt.Errorf("%w: maximum %d bytes", ErrEventTooLarge, p.max)
		}
		line = append(line, part...)
		if err == nil {
			line = line[:len(line)-1]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if !utf8.Valid(line) {
				return nil, fmt.Errorf("%w: invalid UTF-8", ErrMalformedEvent)
			}
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

func (p *Parser) finish(err error) error {
	if ctxErr := p.ctx.Err(); ctxErr != nil {
		p.ended = ctxErr
		return ctxErr
	}
	if !errors.Is(err, io.EOF) {
		p.ended = err
		return err
	}
	if p.terminal && len(p.data) == 0 {
		p.ended = io.EOF
		return p.ended
	}
	if p.frameActive || p.frameBytes != 0 {
		p.ended = ErrTruncatedFrame
		return p.ended
	}
	if p.terminal {
		p.ended = io.EOF
	} else {
		p.ended = ErrEOFWithoutTerminal
	}
	return p.ended
}

func (p *Parser) applyLine(line []byte) {
	p.frameActive = true
	if line[0] == ':' {
		return
	}
	field, value := splitField(line)
	switch field {
	case "event":
		p.eventName = string(value)
	case "data":
		p.data = append(p.data, bytes.Clone(value))
	case "id":
		if !bytes.ContainsRune(value, '\x00') {
			p.lastID = string(value)
		}
	case "retry":
		if millis, parseErr := strconv.ParseInt(string(value), 10, 64); parseErr == nil && millis >= 0 {
			p.retry = time.Duration(millis) * time.Millisecond
		}
	}
}

func (p *Parser) dispatch() (Event, bool, error) {
	defer p.resetFrame()
	if len(p.data) == 0 {
		return Event{}, false, nil
	}
	raw := bytes.Join(p.data, []byte{'\n'})
	if !json.Valid(raw) {
		return Event{}, false, fmt.Errorf("%w: invalid JSON payload", ErrMalformedEvent)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Event{}, false, fmt.Errorf("%w: JSON payload must be an object", ErrMalformedEvent)
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Event{}, false, fmt.Errorf("%w: JSON payload must be an object", ErrMalformedEvent)
	}
	eventType := envelope.Type
	if eventType == "" {
		eventType = p.eventName
	}
	if eventType == "" {
		return Event{}, false, fmt.Errorf("%w: event type is missing", ErrMalformedEvent)
	}
	return Event{
		Type:  eventType,
		Event: p.eventName,
		Raw:   bytes.Clone(raw),
		ID:    p.lastID,
		Retry: p.retry,
	}, true, nil
}

func (p *Parser) resetFrame() {
	p.frameBytes = 0
	p.frameActive = false
	p.eventName = ""
	p.data = p.data[:0]
}

func splitField(line []byte) (string, []byte) {
	field, value, found := bytes.Cut(line, []byte{':'})
	if !found {
		return string(line), nil
	}
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return string(field), value
}

var terminalTypes = map[string]struct{}{
	"response.completed":  {},
	"response.done":       {},
	"response.incomplete": {},
	"response.failed":     {},
}

var knownTypes = map[string]struct{}{
	"response.created":                       {},
	"response.in_progress":                   {},
	"response.output_item.added":             {},
	"response.output_item.done":              {},
	"response.output_text.delta":             {},
	"response.output_text.done":              {},
	"response.refusal.delta":                 {},
	"response.refusal.done":                  {},
	"response.reasoning_summary_part.added":  {},
	"response.reasoning_summary_part.done":   {},
	"response.reasoning_summary_text.delta":  {},
	"response.reasoning_summary_text.done":   {},
	"response.reasoning_text.delta":          {},
	"response.reasoning_text.done":           {},
	"response.function_call_arguments.delta": {},
	"response.function_call_arguments.done":  {},
	"response.custom_tool_call_input.delta":  {},
	"response.custom_tool_call_input.done":   {},
	"response.content_part.added":            {},
	"response.content_part.done":             {},
	"response.web_search_call.in_progress":   {},
	"response.web_search_call.searching":     {},
	"response.web_search_call.completed":     {},
	"response.web_search_call.failed":        {},
	"codex.rate_limits":                      {},
	"response.completed":                     {},
	"response.done":                          {},
	"response.incomplete":                    {},
	"response.failed":                        {},
}
