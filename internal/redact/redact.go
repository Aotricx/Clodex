// Package redact removes credentials and account identifiers from wire data
// before it is written to logs, captures, or diagnostic dumps.
package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Marker is the only replacement value emitted for sensitive data.
const Marker = "<redacted>"

var (
	bearerPattern           = regexp.MustCompile(`(?i)(\bbearer[ \t]+)[A-Za-z0-9._~+/\-=]+`)
	jwtPattern              = regexp.MustCompile(`[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	apiKeyPattern           = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)
	credentialPathPattern   = regexp.MustCompile(`(?i)(?:~[/\\]|\$HOME[/\\]|[A-Za-z]:[/\\]|[/\\.][/\\]|[/\\])[^\s"'<>]*(?:auth\.json|models_cache\.json)`)
	textDoubleQuotedPattern = regexp.MustCompile(textSecretPrefix + `"[^"\r\n]*"`)
	textSingleQuotedPattern = regexp.MustCompile(textSecretPrefix + `'[^'\r\n]*'`)
	textBarePattern         = regexp.MustCompile(textSecretPrefix + `[^\s,;&"']+`)
)

const textSecretPrefix = `(?i)(^|[^A-Za-z0-9])((?:"|')?(?:token|auth[-_]?token|session[-_]?token|access[-_]?token|refresh[-_]?token|id[-_]?token|api[-_]?key|cookie|set[-_]?cookie|(?:(?:chatgpt|openai)[-_]?)?account[-_]?id|email|client[-_]?secret|password|code[-_]?verifier)(?:"|')?[ \t]*[:=][ \t]*)`

var sensitiveFieldNames = map[string]struct{}{
	"accesstoken":        {},
	"refreshtoken":       {},
	"idtoken":            {},
	"token":              {},
	"authtoken":          {},
	"sessiontoken":       {},
	"authorization":      {},
	"proxyauthorization": {},
	"apikey":             {},
	"clientsecret":       {},
	"codeverifier":       {},
	"authorizationcode":  {},
	"deviceauthid":       {},
	"usercode":           {},
	"cookie":             {},
	"setcookie":          {},
	"accountid":          {},
	"chatgptaccountid":   {},
	"openaiaccountid":    {},
	"email":              {},
	"password":           {},
	"encryptedcontent":   {},
}

var extraSensitiveQueryNames = map[string]struct{}{
	"code":                {},
	"state":               {},
	"oauthverifier":       {},
	"oauthconsumersecret": {},
}

// Headers returns a deep clone of source with sensitive header values replaced.
// Bearer credentials and JWTs in otherwise nonsensitive values are also removed.
func Headers(source http.Header) http.Header {
	if source == nil {
		return nil
	}

	clone := make(http.Header, len(source))
	for name, values := range source {
		clone[name] = make([]string, len(values))
		if sensitiveHeaderName(name) {
			for i := range values {
				clone[name][i] = Marker
			}
			continue
		}
		for i, value := range values {
			if urlHeaderName(name) {
				clone[name][i] = redactURLHeader(value)
				continue
			}
			clone[name][i] = Text(value)
		}
	}
	return clone
}

// URL returns a copy of source with credentials and sensitive query values
// removed. Source and its Userinfo are not mutated.
func URL(source *url.URL) *url.URL {
	if source == nil {
		return nil
	}

	clone := *source
	if source.User != nil {
		if _, hasPassword := source.User.Password(); hasPassword {
			clone.User = url.UserPassword(Marker, Marker)
		} else {
			clone.User = url.User(Marker)
		}
	}

	query := source.Query()
	for name, values := range query {
		if sensitiveQueryName(name) {
			for i := range values {
				values[i] = Marker
			}
			query[name] = values
			continue
		}
		for i, value := range values {
			values[i] = Text(value)
		}
		query[name] = values
	}
	clone.RawQuery = query.Encode()
	clone.Path = Text(clone.Path)
	clone.RawPath = Text(clone.RawPath)
	clone.Fragment = Text(clone.Fragment)
	clone.RawFragment = Text(clone.RawFragment)
	clone.Opaque = Text(clone.Opaque)
	return &clone
}

// JSON recursively redacts sensitive object fields, Bearer credentials, and
// JWTs. Malformed input returns an error and no substitute output.
func JSON(source []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON for redaction: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode JSON for redaction: trailing JSON value")
		}
		return nil, fmt.Errorf("decode JSON for redaction: trailing data: %w", err)
	}

	value = redactJSONValue(value)
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("encode redacted JSON: %w", err)
	}
	return bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), nil
}

// Text removes Bearer values and syntactically JWT-like values while leaving
// benign dotted versions, IP addresses, and ordinary dotted text unchanged.
func Text(source string) string {
	redacted := textDoubleQuotedPattern.ReplaceAllString(source, "${1}${2}\""+Marker+"\"")
	redacted = textSingleQuotedPattern.ReplaceAllString(redacted, "${1}${2}'"+Marker+"'")
	redacted = textBarePattern.ReplaceAllString(redacted, "${1}${2}"+Marker)
	redacted = bearerPattern.ReplaceAllString(redacted, "${1}"+Marker)
	redacted = apiKeyPattern.ReplaceAllString(redacted, Marker)
	redacted = credentialPathPattern.ReplaceAllString(redacted, Marker)
	return redactJWTs(redacted)
}

// SSE redacts every SSE line while preserving fields, blank event delimiters,
// and the input's exact LF or CRLF framing.
func SSE(source []byte) []byte {
	if len(source) == 0 {
		return []byte{}
	}

	var output bytes.Buffer
	output.Grow(len(source))
	for offset := 0; offset < len(source); {
		remainder := source[offset:]
		newline := bytes.IndexByte(remainder, '\n')
		if newline < 0 {
			output.Write(redactSSELine(remainder))
			break
		}

		line := remainder[:newline]
		if bytes.HasSuffix(line, []byte{'\r'}) {
			output.Write(redactSSELine(line[:len(line)-1]))
			output.WriteString("\r\n")
		} else {
			output.Write(redactSSELine(line))
			output.WriteByte('\n')
		}
		offset += newline + 1
	}
	return output.Bytes()
}

func redactJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for name, child := range typed {
			if SensitiveName(name) {
				redacted[name] = Marker
			} else {
				redacted[name] = redactJSONValue(child)
			}
		}
		return redacted
	case []any:
		redacted := make([]any, len(typed))
		for i, child := range typed {
			redacted[i] = redactJSONValue(child)
		}
		return redacted
	case string:
		return Text(typed)
	default:
		return value
	}
}

func redactSSELine(line []byte) []byte {
	colon := bytes.IndexByte(line, ':')
	if colon < 0 || string(line[:colon]) != "data" {
		return []byte(Text(string(line)))
	}

	prefixEnd := colon + 1
	if prefixEnd < len(line) && line[prefixEnd] == ' ' {
		prefixEnd++
	}
	prefix := line[:prefixEnd]
	payload := line[prefixEnd:]
	if !json.Valid(payload) {
		redacted := make([]byte, 0, len(line))
		redacted = append(redacted, prefix...)
		redacted = append(redacted, Text(string(payload))...)
		return redacted
	}

	redactedJSON, err := JSON(payload)
	if err != nil {
		return []byte(Text(string(line)))
	}
	redacted := make([]byte, 0, len(prefix)+len(redactedJSON))
	redacted = append(redacted, prefix...)
	redacted = append(redacted, redactedJSON...)
	return redacted
}

func redactJWTs(source string) string {
	indices := jwtPattern.FindAllStringIndex(source, -1)
	if len(indices) == 0 {
		return source
	}

	var output strings.Builder
	output.Grow(len(source))
	last := 0
	for _, bounds := range indices {
		candidate := source[bounds[0]:bounds[1]]
		if jwtHasAdjacentSegment(source, bounds[0], bounds[1]) || !isJWT(candidate) {
			continue
		}
		output.WriteString(source[last:bounds[0]])
		output.WriteString(Marker)
		last = bounds[1]
	}
	if last == 0 {
		return source
	}
	output.WriteString(source[last:])
	return output.String()
}

func jwtHasAdjacentSegment(source string, start, end int) bool {
	if start > 1 && source[start-1] == '.' && isBase64URLByte(source[start-2]) {
		return true
	}
	return end+1 < len(source) && source[end] == '.' && isBase64URLByte(source[end+1])
}

func isJWT(candidate string) bool {
	segments := strings.Split(candidate, ".")
	if len(segments) != 3 {
		return false
	}
	for _, encoded := range segments[:2] {
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return false
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(decoded, &object); err != nil || object == nil {
			return false
		}
	}
	return true
}

func urlHeaderName(name string) bool {
	return strings.EqualFold(name, "Location") || strings.EqualFold(name, "Referer")
}

func redactURLHeader(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return Text(value)
	}
	return URL(parsed).String()
}

func sensitiveHeaderName(name string) bool {
	normalized := normalizeName(name)
	if _, sensitive := sensitiveFieldNames[normalized]; sensitive {
		return true
	}
	return sensitiveNameSuffix(normalized)
}

// SensitiveName reports whether a structured field name identifies a
// credential or account secret. Generic "code" is intentionally excluded so
// OAuth backend error codes remain available for failure classification.
func SensitiveName(name string) bool {
	normalized := normalizeName(name)
	_, sensitive := sensitiveFieldNames[normalized]
	return sensitive || sensitiveNameSuffix(normalized)
}

func sensitiveQueryName(name string) bool {
	normalized := normalizeName(name)
	if _, sensitive := sensitiveFieldNames[normalized]; sensitive {
		return true
	}
	if _, sensitive := extraSensitiveQueryNames[normalized]; sensitive {
		return true
	}
	return sensitiveNameSuffix(normalized)
}

func sensitiveNameSuffix(normalized string) bool {
	return strings.HasSuffix(normalized, "token") ||
		strings.HasSuffix(normalized, "apikey") ||
		strings.HasSuffix(normalized, "secret") ||
		strings.HasSuffix(normalized, "authorization") ||
		strings.HasSuffix(normalized, "accountid")
}

func normalizeName(name string) string {
	var normalized strings.Builder
	normalized.Grow(len(name))
	for _, character := range name {
		switch character {
		case '-', '_':
			continue
		default:
			normalized.WriteRune(character)
		}
	}
	return strings.ToLower(normalized.String())
}

func isBase64URLByte(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' ||
		value == '_' || value == '-'
}
