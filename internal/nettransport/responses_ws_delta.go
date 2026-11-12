package nettransport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	responsesWSDeltaMaxBytes = 1 << 20
	responsesWSDeltaMaxItems = 256
	responsesWSDeltaMaxNodes = 16_384
	responsesWSDeltaMaxDepth = 8
	responsesWSDeltaBeta     = "responses_websockets=2026-02-06"
)

type responsesWSDeltaValue struct {
	kind   byte
	text   string
	object map[string]*responsesWSDeltaValue
	array  []*responsesWSDeltaValue
	raw    []byte
}

type responsesWSDeltaParser struct {
	data   []byte
	offset int
	nodes  int
}

type responsesWSDeltaSeed struct {
	input           [][]byte
	options         []byte
	responseID      string
	output          [][]byte
	outputCanonical [][]byte
	bytes           int
}

type responsesWSDeltaCandidate struct {
	seed          responsesWSDeltaSeed
	outputSeen    [][]byte
	outputIndexes []int
	seenBytes     int
	outputInvalid bool
	terminal      bool
}

type responsesWSDeltaState struct {
	seed          *responsesWSDeltaSeed
	pending       *responsesWSDeltaCandidate
	requireStream bool
	last          ResponsesWSFrameInfo
}

func responsesWSDeltaAllowed(protocol string, headers http.Header) bool {
	if protocol == "public-openai-responses" {
		return true
	}
	if protocol != "codex-responses" {
		return false
	}
	values := headers.Values("OpenAI-Beta")
	return len(values) == 1 && values[0] == responsesWSDeltaBeta
}

func (s *responsesWSDeltaState) clear() {
	s.seed = nil
	s.pending = nil
}

func (s *responsesWSDeltaState) prepare(frame []byte, enabled bool) ([]byte, *responsesWSDeltaCandidate) {
	s.pending = nil
	s.last = newResponsesWSFrameInfo(len(frame))
	full := func(reason string) ([]byte, *responsesWSDeltaCandidate) {
		s.seed = nil
		s.last.Reason = reason
		return frame, nil
	}
	if !enabled {
		return full(ResponsesWSFullDisabled)
	}
	if len(frame) > responsesWSDeltaMaxBytes {
		return full(ResponsesWSFullTooLarge)
	}
	root, err := parseResponsesWSDeltaJSON(frame)
	if err != nil || root.kind != 'o' {
		return full(ResponsesWSFullUnparseable)
	}
	request, ok := responsesWSDeltaRequest(root, s.requireStream)
	if !ok {
		return full(ResponsesWSFullIneligible)
	}
	options := responsesWSDeltaOptions(root)
	if options == nil {
		return full(ResponsesWSFullIneligible)
	}
	s.last.InputItems = len(request.array)
	s.last.WireItems = len(request.array)
	input := make([][]byte, len(request.array))
	requestBytes := len(options)
	for i, item := range request.array {
		canonical, canonicalErr := responsesWSDeltaCanonical(item)
		if canonicalErr != nil {
			return full(ResponsesWSFullUnparseable)
		}
		input[i] = canonical
		requestBytes += len(canonical)
	}
	plan := &responsesWSDeltaCandidate{seed: responsesWSDeltaSeed{input: input, options: options, bytes: requestBytes}}
	wireFrame := frame
	overBudget := requestBytes > responsesWSDeltaMaxBytes || len(request.array) > responsesWSDeltaMaxItems
	switch {
	case s.seed == nil:
		s.last.Reason = ResponsesWSFullNoParent
	case !bytes.Equal(options, s.seed.options):
		s.last.Reason = ResponsesWSFullOptionsChanged
	case overBudget:
		s.last.Reason = ResponsesWSFullOverBudget
	default:
		wireFrame = s.deltaFrame(root, request, input, frame)
	}
	if overBudget {
		plan = nil
	}
	s.seed = nil
	s.pending = plan
	return wireFrame, plan
}

// deltaFrame returns the chained frame when the request exactly extends the
// seed's accepted history, else the full frame with s.last explaining why.
func (s *responsesWSDeltaState) deltaFrame(root, request *responsesWSDeltaValue, input [][]byte, frame []byte) []byte {
	expected := len(s.seed.input) + len(s.seed.outputCanonical)
	if expected > len(request.array) {
		s.last.Reason = ResponsesWSFullHistoryDiverged
		s.last.DivergedAt = len(request.array)
		return frame
	}
	diverged := -1
	for i, item := range s.seed.input {
		if !bytes.Equal(item, input[i]) {
			diverged = i
			break
		}
	}
	if diverged < 0 {
		for i, item := range s.seed.outputCanonical {
			index := len(s.seed.input) + i
			canonical, canonicalErr := responsesWSDeltaCanonical(request.array[index])
			if canonicalErr != nil || !bytes.Equal(item, canonical) {
				diverged = index
				break
			}
		}
	}
	if diverged >= 0 {
		s.last.Reason = ResponsesWSFullHistoryDiverged
		s.last.DivergedAt = diverged
		return frame
	}
	delta, deltaErr := responsesWSDeltaFrame(root, request.array[expected:], s.seed.responseID)
	if deltaErr != nil || len(delta) >= len(frame) {
		s.last.Reason = ResponsesWSFullNotSmaller
		return frame
	}
	s.last.Mode = ResponsesWSFrameDelta
	s.last.WireBytes = len(delta)
	s.last.WireItems = len(request.array) - expected
	return delta
}

func (s *responsesWSDeltaState) observe(raw []byte) {
	if s.pending == nil {
		return
	}
	root, err := parseResponsesWSDeltaJSON(raw)
	if err != nil || root.kind != 'o' {
		s.pending = nil
		return
	}
	switch responsesWSDeltaString(root.object["type"]) {
	case "response.output_item.done":
		if s.pending.terminal {
			s.pending.terminal = false
			s.pending.outputInvalid = true
			return
		}
		index, item, ok := responsesWSDeltaOutputDone(root)
		if !ok {
			s.pending.outputInvalid = true
			return
		}
		canonical, canonicalErr := responsesWSDeltaCanonical(item)
		if canonicalErr != nil || len(s.pending.outputSeen)+len(s.pending.seed.input) >= responsesWSDeltaMaxItems || len(item.raw)+len(canonical)+s.pending.seed.bytes+s.pending.seenBytes > responsesWSDeltaMaxBytes {
			s.pending.outputInvalid = true
			return
		}
		for _, seen := range s.pending.outputIndexes {
			if seen == index {
				s.pending.outputInvalid = true
				return
			}
		}
		s.pending.outputSeen = append(s.pending.outputSeen, append([]byte(nil), canonical...))
		s.pending.outputIndexes = append(s.pending.outputIndexes, index)
		s.pending.seenBytes += len(item.raw) + len(canonical)
	case "response.completed":
		response := root.object["response"]
		if response == nil || response.kind != 'o' || responsesWSDeltaString(response.object["status"]) != "completed" {
			s.pending.terminal = false
			return
		}
		id, items := responsesWSDeltaString(response.object["id"]), response.object["output"]
		if id == "" || items == nil || items.kind != 'a' || len(items.array)+len(s.pending.seed.input) > responsesWSDeltaMaxItems {
			s.pending.terminal = false
			return
		}
		output, canonical, size, ok := responsesWSDeltaItems(items.array)
		if !ok || size+s.pending.seed.bytes+s.pending.seenBytes+len(id) > responsesWSDeltaMaxBytes || s.pending.outputInvalid {
			s.pending.terminal = false
			return
		}
		for i, index := range s.pending.outputIndexes {
			if index >= len(canonical) || !bytes.Equal(canonical[index], s.pending.outputSeen[i]) {
				s.pending.terminal = false
				return
			}
		}
		if len(s.pending.seed.input)+len(items.array) > responsesWSDeltaMaxItems {
			s.pending.terminal = false
			return
		}
		s.pending.seed.responseID = id
		s.pending.seed.output = output
		s.pending.seed.outputCanonical = canonical
		s.pending.seed.bytes += size + len(id)
		s.pending.terminal = true
	case "error", "response.failed", "response.incomplete":
		s.pending = nil
		s.seed = nil
	}
}

func (s *responsesWSDeltaState) commit() {
	if s.pending != nil && s.pending.terminal && s.pending.seed.responseID != "" {
		s.seed = &s.pending.seed
	} else {
		s.seed = nil
	}
	s.pending = nil
}

func responsesWSDeltaRequest(root *responsesWSDeltaValue, requireStream bool) (*responsesWSDeltaValue, bool) {
	fields := root.object
	if responsesWSDeltaString(fields["type"]) != "response.create" {
		return nil, false
	}
	input := fields["input"]
	if input == nil || input.kind != 'a' || len(input.array) > responsesWSDeltaMaxItems {
		return nil, false
	}
	if store := fields["store"]; store == nil || store.kind != 'b' || store.text != "false" {
		return nil, false
	}
	if prev := fields["previous_response_id"]; prev != nil {
		if prev.kind == 's' && prev.text != "" {
			return nil, false
		}
		if prev.kind != 'z' && prev.kind != 's' {
			return nil, false
		}
	}
	for _, key := range []string{"generate", "context_management", "stream_id", "background", "truncation", "prompt_cache_retention", "safety_identifier", "metadata"} {
		if _, exists := fields[key]; exists {
			return nil, false
		}
	}
	if stream := fields["stream"]; requireStream && (stream == nil || stream.kind != 'b' || stream.text != "true") {
		return nil, false
	}
	if stream := fields["stream"]; stream != nil && (stream.kind != 'b' || stream.text != "true") {
		return nil, false
	}
	return input, true
}

func responsesWSDeltaOptions(root *responsesWSDeltaValue) []byte {
	fields := make(map[string]*responsesWSDeltaValue, len(root.object))
	for key, value := range root.object {
		if key != "input" {
			fields[key] = value
		}
	}
	canonical, err := responsesWSDeltaCanonicalObject(fields)
	if err != nil || len(canonical) > responsesWSDeltaMaxBytes {
		return nil
	}
	return canonical
}

func responsesWSDeltaFrame(root *responsesWSDeltaValue, suffix []*responsesWSDeltaValue, responseID string) ([]byte, error) {
	fields := make(map[string]*responsesWSDeltaValue, len(root.object)+1)
	for key, value := range root.object {
		fields[key] = value
	}
	fields["input"] = &responsesWSDeltaValue{kind: 'a', array: suffix}
	idJSON, err := json.Marshal(responseID)
	if err != nil {
		return nil, err
	}
	fields["previous_response_id"] = &responsesWSDeltaValue{kind: 's', text: responseID, raw: idJSON}
	return responsesWSDeltaCanonicalObject(fields)
}

func responsesWSDeltaOutputDone(root *responsesWSDeltaValue) (int, *responsesWSDeltaValue, bool) {
	index, item := root.object["output_index"], root.object["item"]
	if index == nil || index.kind != 'n' || item == nil || index.text == "-0" || strings.HasPrefix(index.text, "-") || strings.ContainsAny(index.text, ".eE") {
		return 0, nil, false
	}
	value, err := strconv.Atoi(index.text)
	return value, item, err == nil && value >= 0 && value < responsesWSDeltaMaxItems
}

func responsesWSDeltaItems(items []*responsesWSDeltaValue) ([][]byte, [][]byte, int, bool) {
	output, canonical := make([][]byte, len(items)), make([][]byte, len(items))
	size := 0
	for i, item := range items {
		if item == nil || item.kind != 'o' {
			return nil, nil, 0, false
		}
		encoded, err := responsesWSDeltaCanonical(item)
		if err != nil {
			return nil, nil, 0, false
		}
		canonical[i], output[i] = encoded, append([]byte(nil), item.raw...)
		size += len(item.raw) + len(encoded)
	}
	if size > responsesWSDeltaMaxBytes {
		return nil, nil, 0, false
	}
	return output, canonical, size, true
}

func responsesWSDeltaString(value *responsesWSDeltaValue) string {
	if value == nil || value.kind != 's' {
		return ""
	}
	return value.text
}

func parseResponsesWSDeltaJSON(data []byte) (*responsesWSDeltaValue, error) {
	if len(data) == 0 || len(data) > responsesWSDeltaMaxBytes || !utf8.Valid(data) {
		return nil, errors.New("invalid JSON bytes")
	}
	parser := responsesWSDeltaParser{data: data}
	value, err := parser.value(0)
	if err != nil {
		return nil, err
	}
	parser.space()
	if parser.offset != len(data) {
		return nil, errors.New("trailing JSON data")
	}
	return value, nil
}

func (p *responsesWSDeltaParser) value(depth int) (*responsesWSDeltaValue, error) {
	if depth > responsesWSDeltaMaxDepth || p.nodes >= responsesWSDeltaMaxNodes {
		return nil, errors.New("JSON limit exceeded")
	}
	p.space()
	if p.offset >= len(p.data) {
		return nil, io.ErrUnexpectedEOF
	}
	p.nodes++
	start := p.offset
	var value *responsesWSDeltaValue
	switch p.data[p.offset] {
	case '{':
		p.offset++
		value = &responsesWSDeltaValue{kind: 'o', object: make(map[string]*responsesWSDeltaValue)}
		p.space()
		if p.take('}') {
			break
		}
		for {
			p.space()
			key, err := p.string()
			if err != nil {
				return nil, err
			}
			if _, exists := value.object[key]; exists {
				return nil, errors.New("duplicate JSON object key")
			}
			p.space()
			if !p.take(':') {
				return nil, errors.New("invalid JSON object")
			}
			child, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			value.object[key] = child
			p.space()
			if p.take('}') {
				break
			}
			if !p.take(',') {
				return nil, errors.New("invalid JSON object")
			}
		}
	case '[':
		p.offset++
		value = &responsesWSDeltaValue{kind: 'a'}
		p.space()
		if p.take(']') {
			break
		}
		for {
			child, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			value.array = append(value.array, child)
			p.space()
			if p.take(']') {
				break
			}
			if !p.take(',') {
				return nil, errors.New("invalid JSON array")
			}
		}
	case '"':
		text, err := p.string()
		if err != nil {
			return nil, err
		}
		value = &responsesWSDeltaValue{kind: 's', text: text}
	case 't':
		if !p.literal("true") {
			return nil, errors.New("invalid JSON literal")
		}
		value = &responsesWSDeltaValue{kind: 'b', text: "true"}
	case 'f':
		if !p.literal("false") {
			return nil, errors.New("invalid JSON literal")
		}
		value = &responsesWSDeltaValue{kind: 'b', text: "false"}
	case 'n':
		if !p.literal("null") {
			return nil, errors.New("invalid JSON literal")
		}
		value = &responsesWSDeltaValue{kind: 'z'}
	default:
		if p.data[p.offset] != '-' && (p.data[p.offset] < '0' || p.data[p.offset] > '9') {
			return nil, errors.New("invalid JSON value")
		}
		if err := p.number(); err != nil {
			return nil, err
		}
		if p.offset < len(p.data) {
			next := p.data[p.offset]
			if (next >= '0' && next <= '9') || next == '.' || next == 'e' || next == 'E' || next == '+' || next == '-' {
				return nil, errors.New("invalid JSON number")
			}
		}
		value = &responsesWSDeltaValue{kind: 'n', text: string(p.data[start:p.offset])}
	}
	value.raw = p.data[start:p.offset]
	return value, nil
}

func (p *responsesWSDeltaParser) string() (string, error) {
	if !p.take('"') {
		return "", errors.New("invalid JSON string")
	}
	start := p.offset - 1
	for p.offset < len(p.data) {
		ch := p.data[p.offset]
		if ch == '"' {
			p.offset++
			var value string
			if err := json.Unmarshal(p.data[start:p.offset], &value); err != nil {
				return "", err
			}
			return value, nil
		}
		if ch < 0x20 {
			return "", errors.New("control in JSON string")
		}
		if ch != '\\' {
			p.offset++
			continue
		}
		p.offset++
		if p.offset >= len(p.data) {
			return "", io.ErrUnexpectedEOF
		}
		escape := p.data[p.offset]
		p.offset++
		switch escape {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		case 'u':
			code, ok := p.hex4()
			if !ok {
				return "", errors.New("invalid unicode escape")
			}
			if code >= 0xd800 && code <= 0xdbff {
				if p.offset+2 > len(p.data) || p.data[p.offset] != '\\' || p.data[p.offset+1] != 'u' {
					return "", errors.New("unpaired unicode surrogate")
				}
				p.offset += 2
				low, valid := p.hex4()
				if !valid || low < 0xdc00 || low > 0xdfff {
					return "", errors.New("unpaired unicode surrogate")
				}
			} else if code >= 0xdc00 && code <= 0xdfff {
				return "", errors.New("unpaired unicode surrogate")
			}
		default:
			return "", errors.New("invalid JSON escape")
		}
	}
	return "", io.ErrUnexpectedEOF
}

func (p *responsesWSDeltaParser) hex4() (uint16, bool) {
	if p.offset+4 > len(p.data) {
		return 0, false
	}
	var value uint16
	for _, ch := range p.data[p.offset : p.offset+4] {
		value <<= 4
		switch {
		case ch >= '0' && ch <= '9':
			value += uint16(ch - '0')
		case ch >= 'a' && ch <= 'f':
			value += uint16(ch-'a') + 10
		case ch >= 'A' && ch <= 'F':
			value += uint16(ch-'A') + 10
		default:
			return 0, false
		}
	}
	p.offset += 4
	return value, true
}

func (p *responsesWSDeltaParser) number() error {
	start := p.offset
	p.take('-')
	if p.take('0') {
		if p.offset < len(p.data) && p.data[p.offset] >= '0' && p.data[p.offset] <= '9' {
			return errors.New("invalid JSON number")
		}
	} else if !p.digits(true) {
		return errors.New("invalid JSON number")
	}
	if p.take('.') && !p.digits(true) {
		return errors.New("invalid JSON number")
	}
	if p.take('e') || p.take('E') {
		if !p.take('+') {
			p.take('-')
		}
		if !p.digits(true) {
			return errors.New("invalid JSON number")
		}
	}
	if p.offset == start {
		return errors.New("invalid JSON number")
	}
	return nil
}

func (p *responsesWSDeltaParser) digits(required bool) bool {
	start := p.offset
	for p.offset < len(p.data) && p.data[p.offset] >= '0' && p.data[p.offset] <= '9' {
		p.offset++
	}
	return !required || p.offset > start
}

func (p *responsesWSDeltaParser) literal(value string) bool {
	if !bytes.HasPrefix(p.data[p.offset:], []byte(value)) {
		return false
	}
	p.offset += len(value)
	return true
}

func (p *responsesWSDeltaParser) space() {
	for p.offset < len(p.data) && strings.ContainsRune(" \t\r\n", rune(p.data[p.offset])) {
		p.offset++
	}
}

func (p *responsesWSDeltaParser) take(value byte) bool {
	if p.offset < len(p.data) && p.data[p.offset] == value {
		p.offset++
		return true
	}
	return false
}

func responsesWSDeltaCanonicalObject(fields map[string]*responsesWSDeltaValue) ([]byte, error) {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var result bytes.Buffer
	result.WriteByte('{')
	for i, key := range keys {
		if i != 0 {
			result.WriteByte(',')
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		result.Write(encodedKey)
		result.WriteByte(':')
		encodedValue, err := responsesWSDeltaCanonical(fields[key])
		if err != nil {
			return nil, err
		}
		result.Write(encodedValue)
	}
	result.WriteByte('}')
	return result.Bytes(), nil
}

func responsesWSDeltaCanonical(value *responsesWSDeltaValue) ([]byte, error) {
	if value == nil {
		return nil, errors.New("nil JSON value")
	}
	switch value.kind {
	case 's':
		return json.Marshal(value.text)
	case 'n', 'b':
		return []byte(value.text), nil
	case 'z':
		return []byte("null"), nil
	case 'o':
		return responsesWSDeltaCanonicalObject(value.object)
	case 'a':
		var result bytes.Buffer
		result.WriteByte('[')
		for i, item := range value.array {
			if i != 0 {
				result.WriteByte(',')
			}
			encoded, err := responsesWSDeltaCanonical(item)
			if err != nil {
				return nil, err
			}
			result.Write(encoded)
		}
		result.WriteByte(']')
		return result.Bytes(), nil
	default:
		return nil, errors.New("invalid JSON value")
	}
}
