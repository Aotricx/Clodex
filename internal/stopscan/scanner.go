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
	if result, ok := s.commitMatch(true); ok {
		return result
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
	if result, ok := s.commitMatch(false); ok {
		return result
	}
	text := s.buffer
	s.buffer = ""
	return Result{Text: text}
}

// commitMatch takes the leftmost complete stop. When holdPending, a match that
// is still a prefix of a longer stop (or sits inside one that could complete
// earlier) is retained so later bytes can agree with a one-shot Feed.
func (s *Scanner) commitMatch(holdPending bool) (Result, bool) {
	matchOffset := -1
	matchValue := ""
	matchIndex := -1
	for i, stop := range s.stops {
		offset := strings.Index(s.buffer, stop)
		if offset >= 0 && (matchOffset < 0 || offset < matchOffset) {
			matchOffset = offset
			matchValue = stop
			matchIndex = i
		}
	}
	if matchOffset < 0 {
		return Result{}, false
	}
	if holdPending && s.couldOverride(matchOffset, matchIndex) {
		return Result{}, false
	}
	text := s.buffer[:matchOffset]
	s.buffer = ""
	s.match = matchValue
	s.stopped = true
	return Result{Text: text, Matched: matchValue}, true
}

func (s *Scanner) couldOverride(matchOffset, matchIndex int) bool {
	for i, stop := range s.stops {
		for start := 0; start <= matchOffset; start++ {
			remaining := s.buffer[start:]
			if remaining == stop || !strings.HasPrefix(stop, remaining) {
				continue
			}
			if start < matchOffset || i < matchIndex {
				return true
			}
		}
	}
	return false
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
