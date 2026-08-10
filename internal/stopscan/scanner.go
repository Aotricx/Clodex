// Package stopscan truncates streamed text at Anthropic stop sequences when
// the concrete Codex backend cannot enforce them.
package stopscan

import (
	"errors"
	"strings"
)

// Result is text safe to emit and, on the matching feed, the stop sequence.
type Result struct {
	Text    string
	Matched string
}

// Scanner retains only the suffix that could begin a future stop sequence.
type Scanner struct {
	stops    []string
	maxStop  int
	buffer   string
	match    string
	stopped  bool
	finished bool
}

// New constructs one scanner. Empty stop sequences are ambiguous and rejected.
func New(stops []string) (*Scanner, error) {
	scanner := &Scanner{stops: append([]string(nil), stops...)}
	for _, stop := range scanner.stops {
		if stop == "" {
			return nil, errors.New("stop sequence must not be empty")
		}
		if len(stop) > scanner.maxStop {
			scanner.maxStop = len(stop)
		}
	}
	return scanner, nil
}

// Feed adds one upstream text delta and returns only bytes proven not to be
// part of a stop sequence. Input order breaks ties for identical match offsets.
func (s *Scanner) Feed(delta string) Result {
	if s == nil || s.stopped || s.finished {
		return Result{}
	}
	if len(s.stops) == 0 {
		return Result{Text: delta}
	}
	s.buffer += delta

	matchOffset := -1
	matchValue := ""
	for _, stop := range s.stops {
		offset := strings.Index(s.buffer, stop)
		if offset >= 0 && (matchOffset < 0 || offset < matchOffset) {
			matchOffset = offset
			matchValue = stop
		}
	}
	if matchOffset >= 0 {
		text := s.buffer[:matchOffset]
		s.buffer = ""
		s.match = matchValue
		s.stopped = true
		return Result{Text: text, Matched: matchValue}
	}

	retained := s.longestPossiblePrefixSuffix()
	safe := len(s.buffer) - retained
	text := s.buffer[:safe]
	s.buffer = s.buffer[safe:]
	return Result{Text: text}
}

// Finish flushes a nonmatching retained suffix. A matched scanner emits none.
func (s *Scanner) Finish() Result {
	if s == nil || s.stopped || s.finished {
		return Result{}
	}
	s.finished = true
	text := s.buffer
	s.buffer = ""
	return Result{Text: text}
}

func (s *Scanner) Stopped() bool {
	return s != nil && s.stopped
}

func (s *Scanner) Match() string {
	if s == nil {
		return ""
	}
	return s.match
}

func (s *Scanner) longestPossiblePrefixSuffix() int {
	limit := min(len(s.buffer), s.maxStop-1)
	for length := limit; length > 0; length-- {
		suffix := s.buffer[len(s.buffer)-length:]
		for _, stop := range s.stops {
			if strings.HasPrefix(stop, suffix) {
				return length
			}
		}
	}
	return 0
}
