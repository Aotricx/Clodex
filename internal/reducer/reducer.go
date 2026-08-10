package reducer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"github.com/Aotricx/Clodex/internal/codexstream"
	"github.com/Aotricx/Clodex/internal/translate"
)

type contentKey struct {
	output  int
	content int
	refusal bool
}

type outputContentKey struct {
	output  int
	content int
}

type blockState struct {
	block     ContentBlock
	index     int
	itemID    string
	closed    bool
	encrypted string
}

type Reducer struct {
	blocks         []*blockState
	byOutput       map[int]*blockState
	byContent      map[contentKey]*blockState
	byCallID       map[string]*blockState
	skipped        map[int]struct{}
	skippedContent map[outputContentKey]struct{}
	warnings       []Warning
	warningPos     map[string]int
	rateLimits     []json.RawMessage

	semantic bool
	toolUse  bool
	refusal  bool
	terminal *Terminal
	termRaw  json.RawMessage
	termType string
	finalErr error
}

func New() *Reducer {
	return &Reducer{
		byOutput:       make(map[int]*blockState),
		byContent:      make(map[contentKey]*blockState),
		byCallID:       make(map[string]*blockState),
		skipped:        make(map[int]struct{}),
		skippedContent: make(map[outputContentKey]struct{}),
		warningPos:     make(map[string]int),
	}
}

func (r *Reducer) Push(upstream codexstream.Event) ([]Event, error) {
	if r.terminal != nil || r.finalErr != nil {
		if upstream.Terminal() {
			return r.duplicateTerminal(upstream)
		}
		return nil, ErrEventAfterTerminal
	}

	switch upstream.Type {
	case "response.created", "response.in_progress":
		return nil, nil
	case "response.output_item.added":
		return r.outputItem(upstream, false)
	case "response.output_item.done":
		return r.outputItem(upstream, true)
	case "response.output_text.delta":
		return r.textEvent(upstream, false, false)
	case "response.output_text.done":
		return r.textEvent(upstream, true, false)
	case "response.refusal.delta":
		return r.textEvent(upstream, false, true)
	case "response.refusal.done":
		return r.textEvent(upstream, true, true)
	case "response.reasoning_summary_part.added":
		return r.reasoningPart(upstream)
	case "response.reasoning_summary_part.done":
		return nil, nil
	case "response.reasoning_summary_text.delta":
		return r.reasoningText(upstream, false)
	case "response.reasoning_summary_text.done":
		return r.reasoningText(upstream, true)
	case "response.reasoning_text.delta", "response.reasoning_text.done":
		return r.reasoningText(upstream, strings.HasSuffix(upstream.Type, ".done"))
	case "response.function_call_arguments.delta":
		return r.functionArguments(upstream, false)
	case "response.function_call_arguments.done":
		return r.functionArguments(upstream, true)
	case "response.custom_tool_call_input.delta":
		return r.functionArguments(upstream, false)
	case "response.custom_tool_call_input.done":
		return r.functionArguments(upstream, true)
	case "response.content_part.added":
		return r.contentPart(upstream, false)
	case "response.content_part.done":
		return r.contentPart(upstream, true)
	case "response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed":
		return nil, nil
	case "response.web_search_call.failed":
		return r.addWarning(WarningWebSearchAction), nil
	case "codex.rate_limits":
		raw := bytes.Clone(upstream.Raw)
		r.rateLimits = append(r.rateLimits, raw)
		return []Event{{Kind: KindRateLimits, RateLimits: &RateLimitSnapshot{Raw: bytes.Clone(raw)}}}, nil
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		return r.finish(upstream)
	default:
		return r.addWarning(WarningUnknownEvent), nil
	}
}

func (r *Reducer) Result() (Result, error) {
	if r.finalErr != nil {
		return Result{}, r.finalErr
	}
	if r.terminal == nil {
		return Result{}, ErrNotTerminal
	}
	content := make([]ContentBlock, len(r.blocks))
	for i, state := range r.blocks {
		content[i] = cloneBlock(state.block)
	}
	warnings := append([]Warning(nil), r.warnings...)
	snapshots := make([]json.RawMessage, len(r.rateLimits))
	for i := range r.rateLimits {
		snapshots[i] = bytes.Clone(r.rateLimits[i])
	}
	return Result{
		Content:            content,
		StopReason:         r.terminal.StopReason,
		StopSequence:       r.terminal.StopSequence,
		Usage:              r.terminal.Usage,
		ResponseID:         r.terminal.ResponseID,
		Warnings:           warnings,
		RateLimitSnapshots: snapshots,
	}, nil
}

type outputItemPayload struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Arguments        string          `json:"arguments"`
	EncryptedContent string          `json:"encrypted_content"`
	Summary          []summaryPart   `json:"summary"`
	Content          []messagePart   `json:"content"`
	Action           json.RawMessage `json:"action"`
}

type summaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type messagePart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Refusal string `json:"refusal"`
}

func (r *Reducer) outputItem(upstream codexstream.Event, done bool) ([]Event, error) {
	decoded, err := upstream.DecodeOutputItem()
	if err != nil {
		return nil, err
	}
	if decoded.OutputIndex == nil || len(decoded.Item) == 0 {
		return nil, fmt.Errorf("%w: output item lacks index or item", ErrMalformedContent)
	}
	var item outputItemPayload
	if err := json.Unmarshal(decoded.Item, &item); err != nil || item.Type == "" {
		return nil, fmt.Errorf("%w: invalid output item", ErrMalformedContent)
	}
	output := *decoded.OutputIndex
	if _, skipped := r.skipped[output]; skipped {
		return nil, nil
	}

	switch item.Type {
	case "reasoning":
		state, events := r.ensureReasoning(output, item.ID)
		if item.EncryptedContent != "" {
			state.encrypted = item.EncryptedContent
		}
		if !done {
			return events, nil
		}
		if state.block.Thinking == "" {
			for _, summary := range item.Summary {
				if summary.Text == "" {
					continue
				}
				if state.block.Thinking != "" {
					const separator = "\n\n"
					state.block.Thinking += separator
					events = append(events, contentDelta(state, DeltaThinking, separator))
				}
				state.block.Thinking += summary.Text
				r.semantic = true
				events = append(events, contentDelta(state, DeltaThinking, summary.Text))
			}
		}
		closed, err := r.close(state)
		return append(events, closed...), err

	case "function_call":
		state, events, err := r.ensureFunction(output, item.CallID, item.Name)
		if err != nil {
			return nil, err
		}
		if !done {
			if item.Arguments != "" {
				state.block.Input = append(state.block.Input, item.Arguments...)
				events = append(events, contentDelta(state, DeltaInputJSON, item.Arguments))
			}
			return events, nil
		}
		if len(state.block.Input) == 0 && item.Arguments != "" {
			state.block.Input = append(state.block.Input, item.Arguments...)
			events = append(events, contentDelta(state, DeltaInputJSON, item.Arguments))
		}
		closed, err := r.close(state)
		return append(events, closed...), err

	case "message":
		if !done {
			return nil, nil
		}
		var events []Event
		for index, part := range item.Content {
			refusal := part.Type == "refusal"
			if part.Type != "output_text" && !refusal {
				identity := outputContentKey{output: output, content: index}
				if _, skipped := r.skippedContent[identity]; skipped {
					continue
				}
				r.skippedContent[identity] = struct{}{}
				events = append(events, r.addWarning(WarningUnknownServerTool)...)
				continue
			}
			state, opened := r.ensureText(output, index, refusal)
			events = append(events, opened...)
			value := part.Text
			if refusal {
				value = part.Refusal
			}
			if state.block.Text == "" && value != "" {
				state.block.Text = value
				r.semantic = true
				events = append(events, contentDelta(state, DeltaText, value))
			}
			closed, err := r.close(state)
			if err != nil {
				return nil, err
			}
			events = append(events, closed...)
		}
		for key, state := range r.byContent {
			if key.output != output || state.closed {
				continue
			}
			closed, err := r.close(state)
			if err != nil {
				return nil, err
			}
			events = append(events, closed...)
		}
		return events, nil

	case "web_search_call":
		state, events := r.ensureWebSearch(output, item.ID)
		if !done {
			return events, nil
		}
		input, mapped, lossy := webSearchInput(item.Action)
		if mapped {
			state.block.Input = bytes.Clone(input)
			events = append(events, contentDelta(state, DeltaInputJSON, string(input)))
			if lossy {
				events = append(events, r.addWarning(WarningWebSearchAction)...)
			}
		} else {
			events = append(events, r.addWarning(WarningWebSearchAction)...)
			state.block.Input = json.RawMessage(`{}`)
		}
		closed, err := r.close(state)
		return append(events, closed...), err

	default:
		r.skipped[output] = struct{}{}
		return r.addWarning(WarningUnknownServerTool), nil
	}
}

func (r *Reducer) ensureReasoning(output int, itemID string) (*blockState, []Event) {
	if state := r.byOutput[output]; state != nil {
		if state.itemID == "" {
			state.itemID = itemID
		}
		return state, nil
	}
	state, events := r.open(ContentBlock{Type: BlockThinking})
	state.itemID = itemID
	r.byOutput[output] = state
	return state, events
}

func (r *Reducer) ensureFunction(output int, callID, name string) (*blockState, []Event, error) {
	if state := r.byOutput[output]; state != nil {
		if state.block.ID == "" {
			state.block.ID = callID
		}
		if state.block.Name == "" {
			state.block.Name = name
		}
		return state, nil, nil
	}
	if callID == "" || name == "" {
		return nil, nil, fmt.Errorf("%w: function call lacks id or name", ErrMalformedContent)
	}
	state, events := r.open(ContentBlock{Type: BlockToolUse, ID: callID, Name: name})
	r.byOutput[output] = state
	r.byCallID[callID] = state
	r.semantic = true
	r.toolUse = true
	return state, events, nil
}

func (r *Reducer) ensureWebSearch(output int, id string) (*blockState, []Event) {
	if state := r.byOutput[output]; state != nil {
		return state, nil
	}
	if id == "" {
		id = fmt.Sprintf("web_search_%d", output)
	}
	id = serverToolUseID(id)
	state, events := r.open(ContentBlock{Type: BlockServerToolUse, ID: id, Name: "web_search"})
	state.itemID = id
	r.byOutput[output] = state
	r.semantic = true
	return state, events
}

func (r *Reducer) reasoningText(upstream codexstream.Event, done bool) ([]Event, error) {
	decoded, err := upstream.DecodeReasoning()
	if err != nil {
		return nil, err
	}
	if decoded.OutputIndex == nil {
		return nil, fmt.Errorf("%w: reasoning delta lacks output index", ErrMalformedContent)
	}
	state, events := r.ensureReasoning(*decoded.OutputIndex, decoded.ItemID)
	text := decoded.Delta
	if done {
		text = decoded.Text
	}
	if text != "" && (!done || state.block.Thinking == "") {
		state.block.Thinking += text
		r.semantic = true
		events = append(events, contentDelta(state, DeltaThinking, text))
	}
	return events, nil
}

func (r *Reducer) reasoningPart(upstream codexstream.Event) ([]Event, error) {
	decoded, err := upstream.DecodeReasoning()
	if err != nil {
		return nil, err
	}
	if decoded.OutputIndex == nil {
		return nil, fmt.Errorf("%w: reasoning summary part lacks output index", ErrMalformedContent)
	}
	state, events := r.ensureReasoning(*decoded.OutputIndex, decoded.ItemID)
	if state.block.Thinking == "" {
		return events, nil
	}
	const separator = "\n\n"
	state.block.Thinking += separator
	r.semantic = true
	return append(events, contentDelta(state, DeltaThinking, separator)), nil
}

func (r *Reducer) functionArguments(upstream codexstream.Event, done bool) ([]Event, error) {
	decoded, err := upstream.DecodeFunctionArguments()
	if err != nil {
		return nil, err
	}
	var state *blockState
	if decoded.OutputIndex != nil {
		state = r.byOutput[*decoded.OutputIndex]
	}
	if state == nil && decoded.CallID != "" {
		state = r.byCallID[decoded.CallID]
	}
	if state == nil || state.block.Type != BlockToolUse {
		return nil, fmt.Errorf("%w: arguments lack a function-call item", ErrMalformedContent)
	}
	value := decoded.Delta
	if done {
		value = decoded.Arguments
	}
	if value == "" || (done && len(state.block.Input) != 0) {
		return nil, nil
	}
	state.block.Input = append(state.block.Input, value...)
	return []Event{contentDelta(state, DeltaInputJSON, value)}, nil
}

func (r *Reducer) textEvent(upstream codexstream.Event, done, refusal bool) ([]Event, error) {
	var payload struct {
		OutputIndex  *int   `json:"output_index"`
		ContentIndex *int   `json:"content_index"`
		Delta        string `json:"delta"`
		Text         string `json:"text"`
		Refusal      string `json:"refusal"`
	}
	if err := upstream.Decode(&payload); err != nil || payload.OutputIndex == nil {
		return nil, fmt.Errorf("%w: text delta lacks output index", ErrMalformedContent)
	}
	content := 0
	if payload.ContentIndex != nil {
		content = *payload.ContentIndex
	}
	state, events := r.ensureText(*payload.OutputIndex, content, refusal)
	value := payload.Delta
	if done {
		value = payload.Text
		if refusal {
			value = payload.Refusal
		}
	}
	if value != "" && (!done || state.block.Text == "") {
		state.block.Text += value
		r.semantic = true
		events = append(events, contentDelta(state, DeltaText, value))
	}
	return events, nil
}

func (r *Reducer) ensureText(output, content int, refusal bool) (*blockState, []Event) {
	key := contentKey{output: output, content: content, refusal: refusal}
	if state := r.byContent[key]; state != nil {
		return state, nil
	}
	state, events := r.open(ContentBlock{Type: BlockText})
	r.byContent[key] = state
	if refusal {
		r.refusal = true
	}
	return state, events
}

func (r *Reducer) contentPart(upstream codexstream.Event, done bool) ([]Event, error) {
	decoded, err := upstream.DecodeContentPart()
	if err != nil {
		return nil, err
	}
	if decoded.OutputIndex == nil || decoded.ContentIndex == nil {
		return nil, fmt.Errorf("%w: content part lacks indexes", ErrMalformedContent)
	}
	identity := outputContentKey{output: *decoded.OutputIndex, content: *decoded.ContentIndex}
	if _, skipped := r.skippedContent[identity]; skipped {
		return nil, nil
	}
	var part messagePart
	if len(decoded.Part) > 0 {
		if err := json.Unmarshal(decoded.Part, &part); err != nil {
			return nil, fmt.Errorf("%w: invalid content part", ErrMalformedContent)
		}
	}
	if part.Type != "output_text" && part.Type != "refusal" {
		if done && part.Type == "" {
			for _, refusal := range []bool{false, true} {
				if state := r.byContent[contentKey{output: identity.output, content: identity.content, refusal: refusal}]; state != nil {
					return r.close(state)
				}
			}
		}
		r.skippedContent[identity] = struct{}{}
		return r.addWarning(WarningUnknownServerTool), nil
	}
	refusal := part.Type == "refusal"
	state, events := r.ensureText(*decoded.OutputIndex, *decoded.ContentIndex, refusal)
	value := part.Text
	if refusal {
		value = part.Refusal
	}
	if value != "" && state.block.Text == "" {
		state.block.Text = value
		r.semantic = true
		events = append(events, contentDelta(state, DeltaText, value))
	}
	if done {
		closed, err := r.close(state)
		return append(events, closed...), err
	}
	return events, nil
}

func (r *Reducer) open(block ContentBlock) (*blockState, []Event) {
	state := &blockState{block: block, index: len(r.blocks)}
	r.blocks = append(r.blocks, state)
	start := block
	start.Thinking = ""
	start.Signature = ""
	start.Text = ""
	start.Input = nil
	return state, []Event{{Kind: KindContentStart, Index: state.index, Block: start}}
}

func contentDelta(state *blockState, kind DeltaType, text string) Event {
	return Event{Kind: KindContentDelta, Index: state.index, Delta: Delta{Type: kind, Text: text}}
}

func (r *Reducer) close(state *blockState) ([]Event, error) {
	if state == nil || state.closed {
		return nil, nil
	}
	var events []Event
	switch state.block.Type {
	case BlockThinking:
		if state.encrypted == "" {
			return nil, ErrMissingReasoningSignature
		}
		signature, ok := translate.EncodeReasoningSignature(translate.ReasoningReplay{
			ID: state.itemID, EncryptedContent: state.encrypted,
		})
		if !ok {
			return nil, ErrMissingReasoningSignature
		}
		state.block.Signature = signature
		r.semantic = true
		events = append(events, contentDelta(state, DeltaSignature, signature))
	case BlockToolUse:
		if len(state.block.Input) == 0 {
			state.block.Input = json.RawMessage(`{}`)
		}
		if !validJSONObject(state.block.Input) {
			return nil, fmt.Errorf("%w: function arguments are not a JSON object", ErrMalformedContent)
		}
	case BlockServerToolUse:
		if len(state.block.Input) == 0 {
			state.block.Input = json.RawMessage(`{}`)
		}
		if !validJSONObject(state.block.Input) {
			return nil, fmt.Errorf("%w: server-tool input is not a JSON object", ErrMalformedContent)
		}
	}
	state.closed = true
	events = append(events, Event{Kind: KindContentStop, Index: state.index})
	return events, nil
}

func validJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 1 && trimmed[0] == '{' && json.Valid(trimmed)
}

func webSearchInput(raw json.RawMessage) (json.RawMessage, bool, bool) {
	if len(raw) == 0 {
		return nil, false, false
	}
	var action struct {
		Type    string   `json:"type"`
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
	}
	if json.Unmarshal(raw, &action) != nil || action.Type != "search" {
		return nil, false, false
	}
	if action.Query == "" {
		for _, query := range action.Queries {
			if query != "" {
				action.Query = query
				break
			}
		}
	}
	if action.Query == "" {
		return nil, false, false
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	lossy := len(action.Queries) > 1
	for key := range fields {
		if key != "type" && key != "query" && key != "queries" {
			lossy = true
		}
	}
	encoded, err := json.Marshal(struct {
		Query string `json:"query"`
	}{Query: action.Query})
	return encoded, err == nil, lossy
}

func serverToolUseID(source string) string {
	var suffix strings.Builder
	for _, char := range source {
		if unicode.IsLetter(char) || unicode.IsNumber(char) || char == '_' {
			suffix.WriteRune(char)
		} else {
			suffix.WriteByte('_')
		}
	}
	return "srvtoolu_" + suffix.String()
}

func (r *Reducer) addWarning(kind string) []Event {
	if index, ok := r.warningPos[kind]; ok {
		r.warnings[index].Count++
		warning := r.warnings[index]
		return []Event{{Kind: KindWarning, Warning: &warning}}
	}
	r.warningPos[kind] = len(r.warnings)
	r.warnings = append(r.warnings, Warning{Kind: kind, Count: 1})
	warning := r.warnings[len(r.warnings)-1]
	return []Event{{Kind: KindWarning, Warning: &warning}}
}

type terminalResponse struct {
	ID                string             `json:"id"`
	Usage             *terminalUsage     `json:"usage"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details"`
	Error             *terminalError     `json:"error"`
}

type terminalUsage struct {
	InputTokens        *int64              `json:"input_tokens"`
	InputTokenDetails  *inputTokenDetails  `json:"input_tokens_details"`
	OutputTokens       *int64              `json:"output_tokens"`
	OutputTokenDetails *outputTokenDetails `json:"output_tokens_details"`
}

type inputTokenDetails struct {
	CachedTokens     int64 `json:"cached_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

type outputTokenDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type incompleteDetails struct {
	Reason string `json:"reason"`
}

type terminalError struct {
	Message string `json:"message"`
}

func (r *Reducer) finish(upstream codexstream.Event) ([]Event, error) {
	decoded, err := upstream.DecodeResponse()
	if err != nil {
		return nil, err
	}
	var response terminalResponse
	if len(decoded.Response) == 0 || json.Unmarshal(decoded.Response, &response) != nil {
		return nil, fmt.Errorf("%w: invalid terminal response", ErrMalformedContent)
	}
	r.termRaw = bytes.Clone(decoded.Response)
	r.termType = upstream.Type

	var events []Event
	states := append([]*blockState(nil), r.blocks...)
	sort.Slice(states, func(i, j int) bool { return states[i].index < states[j].index })
	if upstream.Type == "response.failed" {
		for _, state := range states {
			if state.closed {
				continue
			}
			state.closed = true
			events = append(events, Event{Kind: KindContentStop, Index: state.index})
		}
		message := "upstream response failed"
		if response.Error != nil && response.Error.Message != "" {
			message = response.Error.Message
		}
		r.finalErr = fmt.Errorf("%w: %s", ErrUpstreamFailed, message)
		return events, r.finalErr
	}
	for _, state := range states {
		closed, closeErr := r.close(state)
		if closeErr != nil {
			r.finalErr = closeErr
			return events, closeErr
		}
		events = append(events, closed...)
	}

	if !r.semantic && upstream.Type != "response.incomplete" {
		r.finalErr = ErrEmptyCompletion
		return events, r.finalErr
	}
	usage, err := parseUsage(response.Usage)
	if err != nil {
		r.finalErr = err
		return events, err
	}
	stop := StopEndTurn
	if upstream.Type == "response.incomplete" {
		stop = StopMaxTokens
	} else if r.refusal {
		stop = StopRefusal
	} else if r.toolUse {
		stop = StopToolUse
	}
	terminal := &Terminal{StopReason: stop, ResponseID: response.ID, Usage: usage}
	r.terminal = terminal
	events = append(events, Event{Kind: KindTerminal, Terminal: cloneTerminal(terminal)})
	return events, nil
}

func parseUsage(source *terminalUsage) (Usage, error) {
	if source == nil || source.InputTokens == nil || source.OutputTokens == nil ||
		*source.InputTokens < 0 || *source.OutputTokens < 0 {
		return Usage{}, ErrMissingUsage
	}
	usage := Usage{InputTokens: *source.InputTokens, OutputTokens: *source.OutputTokens}
	if source.InputTokenDetails != nil {
		if source.InputTokenDetails.CachedTokens < 0 || source.InputTokenDetails.CacheWriteTokens < 0 {
			return Usage{}, ErrMissingUsage
		}
		if source.InputTokenDetails.CachedTokens > usage.InputTokens {
			return Usage{}, ErrMissingUsage
		}
		usage.InputTokens -= source.InputTokenDetails.CachedTokens
		usage.CacheReadInputTokens = source.InputTokenDetails.CachedTokens
		usage.CacheCreationInputTokens = source.InputTokenDetails.CacheWriteTokens
	}
	return usage, nil
}

func (r *Reducer) duplicateTerminal(upstream codexstream.Event) ([]Event, error) {
	decoded, err := upstream.DecodeResponse()
	if err != nil {
		return nil, err
	}
	equivalentKinds := upstream.Type == r.termType ||
		(upstream.Type == "response.done" && r.termType == "response.completed") ||
		(upstream.Type == "response.completed" && r.termType == "response.done")
	if equivalentKinds && jsonEqual(decoded.Response, r.termRaw) {
		return nil, nil
	}
	return nil, ErrContradictoryTerminal
}

func jsonEqual(left, right []byte) bool {
	var a, b any
	if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}

func cloneBlock(source ContentBlock) ContentBlock {
	source.Input = bytes.Clone(source.Input)
	return source
}

func cloneTerminal(source *Terminal) *Terminal {
	if source == nil {
		return nil
	}
	copy := *source
	return &copy
}
