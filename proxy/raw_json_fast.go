package proxy

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sozercan/vekil/models"
)

type rawJSONObjectScanner struct {
	data   []byte
	pos    int
	first  bool
	done   bool
	strict bool
}

// rawJSONObjectCursor exposes each value start without first walking to its
// end. Specialized inspectors can consume known nested values directly and
// then advance the cursor, avoiding the generic skip-then-rescan pattern used
// by rawJSONObjectScanner.
type rawJSONObjectCursor struct {
	data   []byte
	pos    int
	first  bool
	done   bool
	strict bool
}

func newRawJSONObjectCursor(data []byte, pos int) (rawJSONObjectCursor, bool) {
	pos = skipRawJSONSpace(data, pos)
	if pos >= len(data) || data[pos] != '{' {
		return rawJSONObjectCursor{}, false
	}
	return rawJSONObjectCursor{data: data, pos: pos + 1, first: true}, true
}

func (s *rawJSONObjectCursor) next() (key []byte, valueStart int, done, ok bool) {
	if s == nil || s.done {
		return nil, 0, true, s != nil
	}
	pos := skipRawJSONSpace(s.data, s.pos)
	if pos >= len(s.data) {
		return nil, 0, false, false
	}
	if s.data[pos] == '}' {
		s.done = true
		s.pos = pos + 1
		return nil, 0, true, true
	}
	if !s.first {
		if s.data[pos] != ',' {
			return nil, 0, false, false
		}
		pos = skipRawJSONSpace(s.data, pos+1)
	}
	var contentStart, contentEnd, afterKey int
	var escaped, scanOK bool
	if s.strict {
		contentStart, contentEnd, afterKey, escaped, scanOK = scanStrictRawJSONString(s.data, pos)
	} else {
		contentStart, contentEnd, afterKey, escaped, scanOK = scanRawJSONString(s.data, pos)
	}
	if !scanOK || escaped {
		return nil, 0, false, false
	}
	pos = skipRawJSONSpace(s.data, afterKey)
	if pos >= len(s.data) || s.data[pos] != ':' {
		return nil, 0, false, false
	}
	return s.data[contentStart:contentEnd], skipRawJSONSpace(s.data, pos+1), false, true
}

func (s *rawJSONObjectCursor) advance(valueEnd int) bool {
	if s == nil || s.done || valueEnd <= s.pos || valueEnd > len(s.data) {
		return false
	}
	s.pos = valueEnd
	s.first = false
	return true
}

type rawJSONArrayCursor struct {
	data  []byte
	pos   int
	first bool
	done  bool
}

func newRawJSONArrayCursor(data []byte, pos int) (rawJSONArrayCursor, bool) {
	pos = skipRawJSONSpace(data, pos)
	if pos >= len(data) || data[pos] != '[' {
		return rawJSONArrayCursor{}, false
	}
	return rawJSONArrayCursor{data: data, pos: pos + 1, first: true}, true
}

func (s *rawJSONArrayCursor) next() (valueStart int, done, ok bool) {
	if s == nil || s.done {
		return 0, true, s != nil
	}
	pos := skipRawJSONSpace(s.data, s.pos)
	if pos >= len(s.data) {
		return 0, false, false
	}
	if s.data[pos] == ']' {
		s.done = true
		s.pos = pos + 1
		return 0, true, true
	}
	if !s.first {
		if s.data[pos] != ',' {
			return 0, false, false
		}
		pos = skipRawJSONSpace(s.data, pos+1)
	}
	return pos, false, true
}

func (s *rawJSONArrayCursor) advance(valueEnd int) bool {
	if s == nil || s.done || valueEnd <= s.pos || valueEnd > len(s.data) {
		return false
	}
	s.pos = valueEnd
	s.first = false
	return true
}

type rawJSONSingleWalkMode struct {
	strict bool
	depth  int
}

func (m rawJSONSingleWalkMode) object(data []byte, pos int) (rawJSONObjectCursor, bool) {
	object, ok := newRawJSONObjectCursor(data, pos)
	if ok {
		object.strict = m.strict
	}
	return object, ok
}

func (m rawJSONSingleWalkMode) array(data []byte, pos int) (rawJSONArrayCursor, bool) {
	return newRawJSONArrayCursor(data, pos)
}

func (m rawJSONSingleWalkMode) valueEnd(data []byte, pos int) (int, bool) {
	if m.strict {
		return scanStrictRawJSONValue(data, pos, m.depth)
	}
	return skipRawJSONValue(data, pos)
}

func (m rawJSONSingleWalkMode) child() (rawJSONSingleWalkMode, bool) {
	if m.strict && m.depth >= maxFastRawJSONNestingDepth {
		return rawJSONSingleWalkMode{}, false
	}
	m.depth++
	return m, true
}

func newRawJSONObjectScanner(data []byte) (rawJSONObjectScanner, bool) {
	pos := skipRawJSONSpace(data, 0)
	if pos >= len(data) || data[pos] != '{' {
		return rawJSONObjectScanner{}, false
	}
	return rawJSONObjectScanner{data: data, pos: pos + 1, first: true}, true
}

func newStrictRawJSONObjectScanner(data []byte) (rawJSONObjectScanner, bool) {
	scanner, ok := newRawJSONObjectScanner(data)
	if !ok {
		return rawJSONObjectScanner{}, false
	}
	scanner.strict = true
	return scanner, true
}

func (s *rawJSONObjectScanner) next() (key []byte, valueStart, valueEnd int, done, ok bool) {
	if s == nil || s.done {
		return nil, 0, 0, true, s != nil
	}
	pos := skipRawJSONSpace(s.data, s.pos)
	if pos >= len(s.data) {
		return nil, 0, 0, false, false
	}
	if s.data[pos] == '}' {
		pos = skipRawJSONSpace(s.data, pos+1)
		if pos != len(s.data) {
			return nil, 0, 0, false, false
		}
		s.done = true
		s.pos = pos
		return nil, 0, 0, true, true
	}
	if !s.first {
		if s.data[pos] != ',' {
			return nil, 0, 0, false, false
		}
		pos = skipRawJSONSpace(s.data, pos+1)
	}
	var contentStart, contentEnd, afterKey int
	var escaped, scanOK bool
	if s.strict {
		contentStart, contentEnd, afterKey, escaped, scanOK = scanStrictRawJSONString(s.data, pos)
	} else {
		contentStart, contentEnd, afterKey, escaped, scanOK = scanRawJSONString(s.data, pos)
	}
	if !scanOK || escaped {
		return nil, 0, 0, false, false
	}
	pos = skipRawJSONSpace(s.data, afterKey)
	if pos >= len(s.data) || s.data[pos] != ':' {
		return nil, 0, 0, false, false
	}
	valueStart = skipRawJSONSpace(s.data, pos+1)
	if s.strict {
		valueEnd, ok = scanStrictRawJSONValue(s.data, valueStart, 1)
	} else {
		valueEnd, ok = skipRawJSONValue(s.data, valueStart)
	}
	if !ok {
		return nil, 0, 0, false, false
	}
	s.pos = valueEnd
	s.first = false
	return s.data[contentStart:contentEnd], valueStart, valueEnd, false, true
}

type rawJSONArrayScanner struct {
	data  []byte
	pos   int
	first bool
	done  bool
}

func newRawJSONArrayScanner(data []byte) (rawJSONArrayScanner, bool) {
	pos := skipRawJSONSpace(data, 0)
	if pos >= len(data) || data[pos] != '[' {
		return rawJSONArrayScanner{}, false
	}
	return rawJSONArrayScanner{data: data, pos: pos + 1, first: true}, true
}

func (s *rawJSONArrayScanner) next() (valueStart, valueEnd int, done, ok bool) {
	if s == nil || s.done {
		return 0, 0, true, s != nil
	}
	pos := skipRawJSONSpace(s.data, s.pos)
	if pos >= len(s.data) {
		return 0, 0, false, false
	}
	if s.data[pos] == ']' {
		pos = skipRawJSONSpace(s.data, pos+1)
		if pos != len(s.data) {
			return 0, 0, false, false
		}
		s.done = true
		s.pos = pos
		return 0, 0, true, true
	}
	if !s.first {
		if s.data[pos] != ',' {
			return 0, 0, false, false
		}
		pos = skipRawJSONSpace(s.data, pos+1)
	}
	valueStart = pos
	valueEnd, ok = skipRawJSONValue(s.data, valueStart)
	if !ok {
		return 0, 0, false, false
	}
	s.pos = valueEnd
	s.first = false
	return valueStart, valueEnd, false, true
}

func skipRawJSONSpace(data []byte, pos int) int {
	for pos < len(data) {
		switch data[pos] {
		case ' ', '\t', '\n', '\r':
			pos++
		default:
			return pos
		}
	}
	return pos
}

func scanRawJSONString(data []byte, pos int) (contentStart, contentEnd, end int, escaped, ok bool) {
	if pos >= len(data) || data[pos] != '"' {
		return 0, 0, 0, false, false
	}
	contentStart = pos + 1
	nonASCII := false
	for pos = contentStart; pos < len(data); pos++ {
		switch data[pos] {
		case '\\':
			escaped = true
			pos++
			if pos >= len(data) {
				return 0, 0, 0, false, false
			}
			nonASCII = nonASCII || data[pos] >= utf8.RuneSelf
		case '"':
			if nonASCII && !utf8.Valid(data[contentStart:pos]) {
				return 0, 0, 0, false, false
			}
			return contentStart, pos, pos + 1, escaped, true
		default:
			nonASCII = nonASCII || data[pos] >= utf8.RuneSelf
		}
	}
	return 0, 0, 0, false, false
}

const maxFastRawJSONNestingDepth = 64

func scanStrictRawJSONString(data []byte, pos int) (contentStart, contentEnd, end int, escaped, ok bool) {
	if pos >= len(data) || data[pos] != '"' {
		return 0, 0, 0, false, false
	}
	contentStart = pos + 1
	nonASCII := false
	for pos = contentStart; pos < len(data); pos++ {
		switch data[pos] {
		case '\\':
			escaped = true
			pos++
			if pos >= len(data) {
				return 0, 0, 0, false, false
			}
			nonASCII = nonASCII || data[pos] >= utf8.RuneSelf
			switch data[pos] {
			case 'b', 'f', 'n', 'r', 't', '\\', '/', '"':
			case 'u':
				if pos+4 >= len(data) {
					return 0, 0, 0, false, false
				}
				for i := 1; i <= 4; i++ {
					if !isRawJSONHex(data[pos+i]) {
						return 0, 0, 0, false, false
					}
				}
				pos += 4
			default:
				return 0, 0, 0, false, false
			}
		case '"':
			if nonASCII && !utf8.Valid(data[contentStart:pos]) {
				return 0, 0, 0, false, false
			}
			return contentStart, pos, pos + 1, escaped, true
		default:
			if data[pos] < 0x20 {
				return 0, 0, 0, false, false
			}
			nonASCII = nonASCII || data[pos] >= utf8.RuneSelf
		}
	}
	return 0, 0, 0, false, false
}

func isRawJSONHex(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

// scanStrictRawJSONValue validates one JSON value without allocating. It is
// deliberately bounded more tightly than encoding/json: unusually deep valid
// inputs fall back to the standard decoder instead of growing the Go stack on
// the request hot path.
func scanStrictRawJSONValue(data []byte, pos, depth int) (int, bool) {
	pos = skipRawJSONSpace(data, pos)
	if pos >= len(data) {
		return 0, false
	}
	switch data[pos] {
	case '"':
		_, _, end, _, ok := scanStrictRawJSONString(data, pos)
		return end, ok
	case '{':
		if depth >= maxFastRawJSONNestingDepth {
			return 0, false
		}
		return scanStrictRawJSONObject(data, pos, depth+1)
	case '[':
		if depth >= maxFastRawJSONNestingDepth {
			return 0, false
		}
		return scanStrictRawJSONArray(data, pos, depth+1)
	case 't':
		return scanStrictRawJSONLiteral(data, pos, "true")
	case 'f':
		return scanStrictRawJSONLiteral(data, pos, "false")
	case 'n':
		return scanStrictRawJSONLiteral(data, pos, "null")
	default:
		return scanStrictRawJSONNumber(data, pos)
	}
}

func scanStrictRawJSONObject(data []byte, pos, depth int) (int, bool) {
	pos = skipRawJSONSpace(data, pos+1)
	if pos >= len(data) {
		return 0, false
	}
	if data[pos] == '}' {
		return pos + 1, true
	}
	for {
		_, _, afterKey, _, ok := scanStrictRawJSONString(data, pos)
		if !ok {
			return 0, false
		}
		pos = skipRawJSONSpace(data, afterKey)
		if pos >= len(data) || data[pos] != ':' {
			return 0, false
		}
		pos, ok = scanStrictRawJSONValue(data, pos+1, depth)
		if !ok {
			return 0, false
		}
		pos = skipRawJSONSpace(data, pos)
		if pos >= len(data) {
			return 0, false
		}
		switch data[pos] {
		case '}':
			return pos + 1, true
		case ',':
			pos = skipRawJSONSpace(data, pos+1)
		default:
			return 0, false
		}
	}
}

func scanStrictRawJSONArray(data []byte, pos, depth int) (int, bool) {
	pos = skipRawJSONSpace(data, pos+1)
	if pos >= len(data) {
		return 0, false
	}
	if data[pos] == ']' {
		return pos + 1, true
	}
	for {
		var ok bool
		pos, ok = scanStrictRawJSONValue(data, pos, depth)
		if !ok {
			return 0, false
		}
		pos = skipRawJSONSpace(data, pos)
		if pos >= len(data) {
			return 0, false
		}
		switch data[pos] {
		case ']':
			return pos + 1, true
		case ',':
			pos = skipRawJSONSpace(data, pos+1)
		default:
			return 0, false
		}
	}
}

func scanStrictRawJSONLiteral(data []byte, pos int, literal string) (int, bool) {
	if len(data)-pos < len(literal) {
		return 0, false
	}
	for i := range literal {
		if data[pos+i] != literal[i] {
			return 0, false
		}
	}
	return pos + len(literal), true
}

func scanStrictRawJSONNumber(data []byte, pos int) (int, bool) {
	if pos >= len(data) {
		return 0, false
	}
	if data[pos] == '-' {
		pos++
		if pos >= len(data) {
			return 0, false
		}
	}
	switch {
	case data[pos] == '0':
		pos++
	case data[pos] >= '1' && data[pos] <= '9':
		for pos < len(data) && data[pos] >= '0' && data[pos] <= '9' {
			pos++
		}
	default:
		return 0, false
	}
	if pos < len(data) && data[pos] == '.' {
		pos++
		start := pos
		for pos < len(data) && data[pos] >= '0' && data[pos] <= '9' {
			pos++
		}
		if pos == start {
			return 0, false
		}
	}
	if pos < len(data) && (data[pos] == 'e' || data[pos] == 'E') {
		pos++
		if pos < len(data) && (data[pos] == '+' || data[pos] == '-') {
			pos++
		}
		start := pos
		for pos < len(data) && data[pos] >= '0' && data[pos] <= '9' {
			pos++
		}
		if pos == start {
			return 0, false
		}
	}
	return pos, true
}

func skipRawJSONValue(data []byte, pos int) (int, bool) {
	pos = skipRawJSONSpace(data, pos)
	if pos >= len(data) {
		return 0, false
	}
	switch data[pos] {
	case '"':
		_, _, end, _, ok := scanRawJSONString(data, pos)
		return end, ok
	case '{', '[':
		depth := 0
		for i := pos; i < len(data); i++ {
			switch data[i] {
			case '"':
				_, _, end, _, ok := scanRawJSONString(data, i)
				if !ok {
					return 0, false
				}
				i = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
		}
		return 0, false
	default:
		for i := pos; i < len(data); i++ {
			switch data[i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				return i, i > pos
			}
		}
		return len(data), true
	}
}

func rawJSONKeyEqual(key []byte, want string) bool {
	if len(key) != len(want) {
		return false
	}
	for i := range key {
		if key[i] != want[i] {
			return false
		}
	}
	return true
}

func rawJSONKeyEqualFold(key []byte, want string) bool {
	if len(key) != len(want) {
		return false
	}
	for i := range key {
		left := key[i]
		right := want[i]
		if left >= 'A' && left <= 'Z' {
			left += 'a' - 'A'
		}
		if right >= 'A' && right <= 'Z' {
			right += 'a' - 'A'
		}
		if left != right {
			return false
		}
	}
	return true
}

func rawJSONNonEmptyString(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	contentStart, contentEnd, end, escaped, ok := scanRawJSONString(raw, 0)
	if !ok || escaped || end != len(raw) {
		return false
	}
	return len(bytes.TrimSpace(raw[contentStart:contentEnd])) > 0
}

func rawJSONNonNull(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

func rawJSONHasNonEmptyArrayFast(raw []byte) (bool, bool) {
	array, ok := newRawJSONArrayScanner(raw)
	if !ok {
		return false, false
	}
	_, _, done, ok := array.next()
	if !ok {
		return false, false
	}
	return !done, true
}

func rawJSONInt(raw []byte) (int, bool) {
	raw = bytes.TrimSpace(raw)
	value, err := strconv.Atoi(string(raw))
	if err != nil {
		return 0, false
	}
	return value, true
}

func rawJSONCreatedUnchangedByNormalizer(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	if !rawJSONNonNull(raw) {
		return false
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	return err != nil || value != 0
}

// extractTopLevelJSONStringFast returns an exact top-level string field without
// allocating a map for the entire object. lastMatch mirrors encoding/json's
// duplicate-key behavior when true; false preserves the generic request
// extractor's historical first-match behavior. ok is false when the fast
// scanner cannot safely preserve encoding/json semantics, so callers can fall
// back to their existing decoder.
func extractTopLevelJSONStringFast(body []byte, field string, lastMatch bool) (value string, found, ok bool) {
	if !json.Valid(body) {
		return "", false, false
	}
	return extractTopLevelJSONStringFastValidated(body, field, lastMatch)
}

func extractTopLevelJSONStringFastValidated(body []byte, field string, lastMatch bool) (value string, found, ok bool) {
	object, ok := newRawJSONObjectScanner(body)
	if !ok {
		return "", false, false
	}
	var raw []byte
	for {
		key, start, end, done, scanOK := object.next()
		if !scanOK {
			return "", false, false
		}
		if done {
			break
		}
		if !rawJSONKeyEqual(key, field) {
			continue
		}
		raw = body[start:end]
		found = true
		if !lastMatch {
			break
		}
	}
	if !found {
		return "", false, true
	}

	raw = bytes.TrimSpace(raw)
	contentStart, contentEnd, end, escaped, stringOK := scanRawJSONString(raw, 0)
	if !stringOK || end != len(raw) {
		return "", true, true
	}
	if !escaped {
		return strings.TrimSpace(string(raw[contentStart:contentEnd])), true, true
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", true, true
	}
	return strings.TrimSpace(value), true, true
}

func inspectCanonicalOpenAIChatCompletionResponseFast(body []byte, requestedModel string) (models.OpenAIUsage, bool) {
	usage, end, ok := inspectCanonicalOpenAIChatCompletionObjectSingleWalk(body, 0, requestedModel, rawJSONSingleWalkMode{
		strict: true,
		depth:  1,
	})
	if !ok || skipRawJSONSpace(body, end) != len(body) {
		return models.OpenAIUsage{}, false
	}
	return usage, true
}

func inspectCanonicalOpenAIChatCompletionResponseFastValidated(body []byte, requestedModel string) (models.OpenAIUsage, bool) {
	usage, end, ok := inspectCanonicalOpenAIChatCompletionObjectSingleWalk(body, 0, requestedModel, rawJSONSingleWalkMode{depth: 1})
	if !ok || skipRawJSONSpace(body, end) != len(body) {
		return models.OpenAIUsage{}, false
	}
	return usage, true
}

func inspectCanonicalOpenAIChatCompletionObjectSingleWalk(data []byte, pos int, requestedModel string, walk rawJSONSingleWalkMode) (models.OpenAIUsage, int, bool) {
	object, ok := walk.object(data, pos)
	if !ok {
		return models.OpenAIUsage{}, 0, false
	}

	idOK := false
	objectOK := false
	createdOK := false
	modelOK := strings.TrimSpace(requestedModel) == ""
	choicesOK := false
	usageOK := false
	var usage models.OpenAIUsage

	for {
		key, valueStart, done, scanOK := object.next()
		if !scanOK {
			return models.OpenAIUsage{}, 0, false
		}
		if done {
			break
		}

		var valueEnd int
		switch {
		case rawJSONKeyEqual(key, "error"):
			return models.OpenAIUsage{}, 0, false
		case rawJSONKeyEqual(key, "id"):
			valueEnd, ok = walk.valueEnd(data, valueStart)
			idOK = ok && rawJSONNonEmptyString(data[valueStart:valueEnd])
			ok = idOK
		case rawJSONKeyEqual(key, "object"):
			valueEnd, ok = walk.valueEnd(data, valueStart)
			objectOK = ok && rawJSONNonEmptyString(data[valueStart:valueEnd])
			ok = objectOK
		case rawJSONKeyEqual(key, "created"):
			valueEnd, ok = walk.valueEnd(data, valueStart)
			createdOK = ok && rawJSONCreatedUnchangedByNormalizer(data[valueStart:valueEnd])
			ok = createdOK
		case rawJSONKeyEqual(key, "model"):
			valueEnd, ok = walk.valueEnd(data, valueStart)
			modelOK = ok && rawJSONNonEmptyString(data[valueStart:valueEnd])
			ok = modelOK
		case rawJSONKeyEqual(key, "choices"):
			choicesWalk, childOK := walk.child()
			if !childOK {
				return models.OpenAIUsage{}, 0, false
			}
			valueEnd, choicesOK = inspectCanonicalOpenAIChatChoicesSingleWalk(data, valueStart, choicesWalk)
			ok = choicesOK
		case rawJSONKeyEqual(key, "usage"):
			usageWalk, childOK := walk.child()
			if !childOK {
				return models.OpenAIUsage{}, 0, false
			}
			usage, valueEnd, usageOK = inspectCanonicalOpenAIUsageSingleWalk(data, valueStart, usageWalk)
			ok = usageOK
		default:
			valueEnd, ok = walk.valueEnd(data, valueStart)
		}
		if !ok || !object.advance(valueEnd) {
			return models.OpenAIUsage{}, 0, false
		}
	}

	if !idOK || !objectOK || !createdOK || !modelOK || !choicesOK || !usageOK {
		return models.OpenAIUsage{}, 0, false
	}
	return usage, object.pos, true
}

func inspectCanonicalOpenAIUsageSingleWalk(data []byte, pos int, walk rawJSONSingleWalkMode) (models.OpenAIUsage, int, bool) {
	object, ok := walk.object(data, pos)
	if !ok {
		return models.OpenAIUsage{}, 0, false
	}
	usage := models.OpenAIUsage{}
	promptOK := false
	completionOK := false
	totalOK := false

	for {
		key, valueStart, done, scanOK := object.next()
		if !scanOK {
			return models.OpenAIUsage{}, 0, false
		}
		if done {
			break
		}

		valueEnd, valueOK := walk.valueEnd(data, valueStart)
		if !valueOK {
			return models.OpenAIUsage{}, 0, false
		}
		switch {
		case rawJSONKeyEqual(key, "prompt_tokens"):
			usage.PromptTokens, promptOK = rawJSONInt(data[valueStart:valueEnd])
			valueOK = promptOK
		case rawJSONKeyEqual(key, "completion_tokens"):
			usage.CompletionTokens, completionOK = rawJSONInt(data[valueStart:valueEnd])
			valueOK = completionOK
		case rawJSONKeyEqual(key, "total_tokens"):
			usage.TotalTokens, totalOK = rawJSONInt(data[valueStart:valueEnd])
			valueOK = totalOK
		case rawJSONKeyEqual(key, "prompt_tokens_details"), rawJSONKeyEqual(key, "completion_tokens_details"):
			return models.OpenAIUsage{}, 0, false
		}
		if !valueOK || !object.advance(valueEnd) {
			return models.OpenAIUsage{}, 0, false
		}
	}

	if !promptOK || !completionOK || !totalOK || usage.TotalTokens == 0 && (usage.PromptTokens != 0 || usage.CompletionTokens != 0) {
		return models.OpenAIUsage{}, 0, false
	}
	return usage, object.pos, true
}

func inspectCanonicalOpenAIChatChoicesSingleWalk(data []byte, pos int, walk rawJSONSingleWalkMode) (int, bool) {
	array, ok := walk.array(data, pos)
	if !ok {
		return 0, false
	}
	for {
		valueStart, done, scanOK := array.next()
		if !scanOK {
			return 0, false
		}
		if done {
			return array.pos, true
		}
		choiceWalk, childOK := walk.child()
		if !childOK {
			return 0, false
		}
		valueEnd, valueOK := inspectCanonicalOpenAIChatChoiceSingleWalk(data, valueStart, choiceWalk)
		if !valueOK || !array.advance(valueEnd) {
			return 0, false
		}
	}
}

func inspectCanonicalOpenAIChatChoiceSingleWalk(data []byte, pos int, walk rawJSONSingleWalkMode) (int, bool) {
	choice, ok := walk.object(data, pos)
	if !ok {
		return 0, false
	}
	indexOK := false
	finishOK := false
	messageOK := false

	for {
		key, valueStart, done, scanOK := choice.next()
		if !scanOK {
			return 0, false
		}
		if done {
			break
		}

		var valueEnd int
		switch {
		case rawJSONKeyEqual(key, "index"):
			valueEnd, ok = walk.valueEnd(data, valueStart)
			indexOK = ok && rawJSONNonNull(data[valueStart:valueEnd])
			ok = indexOK
		case rawJSONKeyEqual(key, "finish_reason"):
			valueEnd, ok = walk.valueEnd(data, valueStart)
			finishOK = ok && rawJSONNonEmptyString(data[valueStart:valueEnd])
			ok = finishOK
		case rawJSONKeyEqual(key, "message"):
			messageWalk, childOK := walk.child()
			if !childOK {
				return 0, false
			}
			valueEnd, messageOK = inspectCanonicalOpenAIChatMessageSingleWalk(data, valueStart, messageWalk)
			ok = messageOK
		case rawJSONKeyEqual(key, "tool_calls"), rawJSONKeyEqual(key, "function_call"):
			return 0, false
		default:
			valueEnd, ok = walk.valueEnd(data, valueStart)
		}
		if !ok || !choice.advance(valueEnd) {
			return 0, false
		}
	}

	if !indexOK || !finishOK || !messageOK {
		return 0, false
	}
	return choice.pos, true
}

func inspectCanonicalOpenAIChatMessageSingleWalk(data []byte, pos int, walk rawJSONSingleWalkMode) (int, bool) {
	message, ok := walk.object(data, pos)
	if !ok {
		return 0, false
	}
	roleOK := false
	contentOK := false

	for {
		key, valueStart, done, scanOK := message.next()
		if !scanOK {
			return 0, false
		}
		if done {
			break
		}

		valueEnd, valueOK := walk.valueEnd(data, valueStart)
		if !valueOK {
			return 0, false
		}
		switch {
		case rawJSONKeyEqual(key, "role"):
			roleOK = rawJSONNonEmptyString(data[valueStart:valueEnd])
			valueOK = roleOK
		case rawJSONKeyEqual(key, "content"):
			contentOK = rawJSONNonNull(data[valueStart:valueEnd])
			valueOK = contentOK
		}
		if !valueOK || !message.advance(valueEnd) {
			return 0, false
		}
	}

	if !roleOK || !contentOK {
		return 0, false
	}
	return message.pos, true
}

func replaceSingleTopLevelRawJSONField(body []byte, field string, replacement json.RawMessage) ([]byte, bool) {
	if !json.Valid(body) || !json.Valid(replacement) {
		return body, false
	}
	object, ok := newRawJSONObjectScanner(body)
	if !ok {
		return body, false
	}
	matchStart := -1
	matchEnd := -1
	for {
		key, start, end, done, ok := object.next()
		if !ok {
			return body, false
		}
		if done {
			break
		}
		if !rawJSONKeyEqual(key, field) {
			continue
		}
		if matchStart >= 0 {
			return body, false
		}
		matchStart = start
		matchEnd = end
	}
	if matchStart < 0 {
		return body, false
	}
	if matchEnd < matchStart || matchEnd > len(body) {
		return body, false
	}
	retainedLength := len(body) - (matchEnd - matchStart)
	maxInt := int(^uint(0) >> 1)
	if len(replacement) > maxInt-retainedLength {
		return body, false
	}
	out := make([]byte, retainedLength+len(replacement))
	copy(out, body[:matchStart])
	n := matchStart + copy(out[matchStart:], replacement)
	copy(out[n:], body[matchEnd:])
	return out, true
}
