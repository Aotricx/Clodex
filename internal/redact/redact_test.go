package redact

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

const syntheticJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjMiLCJuYW1lIjoiSmFuZSJ9.c2lnbmF0dXJl"

func TestHeadersReturnsRedactedClone(t *testing.T) {
	sensitiveNames := []string{
		"Authorization",
		"proxy-authorization",
		"COOKIE",
		"Set-Cookie",
		"X-API-Key",
		"API-Key",
		"X-Auth-Token",
		"X-Access-Token",
		"Refresh-Token",
		"ID-Token",
		"ChatGPT-Account-ID",
		"OpenAI-Account-ID",
		"X-OpenAI-Account-ID",
		"X-Client-Secret",
	}
	source := make(http.Header)
	for _, name := range sensitiveNames {
		source[name] = []string{"synthetic-secret-one", "synthetic-secret-two"}
	}
	source["X-Trace"] = []string{"trace-value"}
	source["X-Safe-JWT"] = []string{syntheticJWT}
	source["X-Authorization-Mode"] = []string{"oauth"}
	source["Cookie-Policy"] = []string{"strict"}
	source["X-API-Key-Hint"] = []string{"configured"}
	original := cloneHeader(source)

	got := Headers(source)

	if !reflect.DeepEqual(source, original) {
		t.Fatalf("Headers mutated source:\n got  %#v\n want %#v", source, original)
	}
	if got == nil || reflect.ValueOf(got).Pointer() == reflect.ValueOf(source).Pointer() {
		t.Fatal("Headers did not return a clone")
	}
	for _, name := range sensitiveNames {
		if values := got[name]; !reflect.DeepEqual(values, []string{Marker, Marker}) {
			t.Errorf("header %q = %#v, want markers preserving value count", name, values)
		}
	}
	if got["X-Trace"][0] != "trace-value" || got["X-Authorization-Mode"][0] != "oauth" ||
		got["Cookie-Policy"][0] != "strict" || got["X-API-Key-Hint"][0] != "configured" {
		t.Errorf("benign headers changed: %#v", got)
	}
	if got["X-Safe-JWT"][0] != Marker {
		t.Errorf("JWT under nonsensitive header = %q, want marker", got["X-Safe-JWT"][0])
	}

	got["X-Trace"][0] = "changed"
	if source["X-Trace"][0] != "trace-value" {
		t.Fatal("Headers shares a value slice with source")
	}
}

func TestHeadersNilPreserved(t *testing.T) {
	if got := Headers(nil); got != nil {
		t.Fatalf("Headers(nil) = %#v, want nil", got)
	}
}

func TestURLReturnsRedactedClone(t *testing.T) {
	values := url.Values{
		"access_token":           {"access-secret"},
		"REFRESH-TOKEN":          {"refresh-secret"},
		"id_token":               {"id-secret"},
		"token":                  {"generic-secret"},
		"code":                   {"oauth-code"},
		"state":                  {"oauth-state"},
		"api_key":                {"api-secret"},
		"account-id":             {"account-secret"},
		"chatgpt_account_id":     {"chatgpt-secret"},
		"openai-account-id":      {"openai-secret"},
		"safe_bearer":            {"Bearer plaintext-secret"},
		"safe_jwt":               {syntheticJWT},
		"token_count":            {"42"},
		"state_name":             {"Ohio"},
		"code_model":             {"gpt"},
		"account_id_hint":        {"configured"},
		"benign_dotted_versions": {"1.2.3", "127.0.0.1", "alpha.beta.gamma"},
	}
	source := &url.URL{
		Scheme:   "https",
		Host:     "example.test",
		Path:     "/responses",
		RawQuery: values.Encode(),
		Fragment: "trace",
		User:     url.UserPassword("client", "password-secret"),
	}
	original := source.String()

	got := URL(source)

	if got == nil || got == source {
		t.Fatal("URL did not return a distinct URL")
	}
	if source.String() != original || source.Query().Get("access_token") != "access-secret" {
		t.Fatalf("URL mutated source: got %q, want %q", source.String(), original)
	}
	for _, key := range []string{
		"access_token", "REFRESH-TOKEN", "id_token", "token", "code", "state",
		"api_key", "account-id", "chatgpt_account_id", "openai-account-id",
	} {
		if value := got.Query().Get(key); value != Marker {
			t.Errorf("query %q = %q, want marker", key, value)
		}
	}
	if got.Query().Get("safe_bearer") != "Bearer "+Marker || got.Query().Get("safe_jwt") != Marker {
		t.Errorf("embedded secret values not redacted: %q %q", got.Query().Get("safe_bearer"), got.Query().Get("safe_jwt"))
	}
	for key, want := range map[string]string{
		"token_count": "42", "state_name": "Ohio", "code_model": "gpt", "account_id_hint": "configured",
	} {
		if value := got.Query().Get(key); value != want {
			t.Errorf("benign query %q = %q, want %q", key, value, want)
		}
	}
	if values := got.Query()["benign_dotted_versions"]; !reflect.DeepEqual(values, []string{"1.2.3", "127.0.0.1", "alpha.beta.gamma"}) {
		t.Errorf("benign dotted values changed: %#v", values)
	}
	if password, ok := got.User.Password(); !ok || password != Marker {
		t.Errorf("URL password = %q, %v; want marker", password, ok)
	}
	if username := got.User.Username(); username != Marker {
		t.Errorf("URL username = %q, want marker", username)
	}
}

func TestURLNilPreserved(t *testing.T) {
	if got := URL(nil); got != nil {
		t.Fatalf("URL(nil) = %#v, want nil", got)
	}
}

func TestJSONRedactsSensitiveFieldsRecursively(t *testing.T) {
	input := []byte(`{
		"access_token":"access-secret",
		"REFRESH-TOKEN":"refresh-secret",
		"Id_Token":"id-secret",
		"api-key":["one",{"two":"three"}],
		"cookie":{"session":"cookie-secret"},
		"nested":[
			{"Authorization":"Bearer auth-secret","account-id":{"deep":"account-secret"}},
			{"e-mail":"人@example.test","safe":null}
		],
		"safe":{
			"email_verified":true,
			"api_key_hint":"configured",
			"authorization_mode":"oauth",
			"account_identity":"public",
			"client_secret_hint":"configured",
			"token_count":3,
			"message":"Hello, 世界",
			"jwt":"` + syntheticJWT + `",
			"bearer":"prefix Bearer plaintext-secret suffix",
			"version":"1.2.3",
			"ip":"127.0.0.1",
			"dotted":"alpha.beta.gamma"
		}
	}`)
	original := append([]byte(nil), input...)

	got, err := JSON(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(input, original) {
		t.Fatal("JSON mutated input")
	}
	if !json.Valid(got) {
		t.Fatalf("JSON returned invalid JSON: %q", got)
	}
	for _, secret := range []string{
		"access-secret", "refresh-secret", "id-secret", "cookie-secret", "auth-secret",
		"account-secret", "人@example.test", syntheticJWT, "plaintext-secret",
	} {
		if bytes.Contains(got, []byte(secret)) {
			t.Errorf("JSON output contains synthetic secret %q: %s", secret, got)
		}
	}

	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"access_token", "REFRESH-TOKEN", "Id_Token", "api-key", "cookie"} {
		if decoded[key] != Marker {
			t.Errorf("field %q = %#v, want marker", key, decoded[key])
		}
	}
	nested := decoded["nested"].([]any)
	if nested[0].(map[string]any)["Authorization"] != Marker || nested[0].(map[string]any)["account-id"] != Marker ||
		nested[1].(map[string]any)["e-mail"] != Marker {
		t.Errorf("nested sensitive values not redacted: %#v", nested)
	}
	safe := decoded["safe"].(map[string]any)
	for key, want := range map[string]any{
		"email_verified":     true,
		"api_key_hint":       "configured",
		"authorization_mode": "oauth",
		"account_identity":   "public",
		"client_secret_hint": "configured",
		"token_count":        float64(3),
		"message":            "Hello, 世界",
		"jwt":                Marker,
		"bearer":             "prefix Bearer " + Marker + " suffix",
		"version":            "1.2.3",
		"ip":                 "127.0.0.1",
		"dotted":             "alpha.beta.gamma",
	} {
		if !reflect.DeepEqual(safe[key], want) {
			t.Errorf("safe[%q] = %#v, want %#v", key, safe[key], want)
		}
	}
}

func TestJSONRedactsEverySensitiveValueType(t *testing.T) {
	input := []byte(`{"access-token":null,"refresh_token":false,"id-token":123,"auth_token":"auth-secret","session_token":"session-secret","token":"generic-secret","authorization":[1,2],"api_key":{"nested":true},"client_secret":"client-secret","cookie":"","account_id":0,"email":null}`)
	got, err := JSON(input)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	for key, value := range decoded {
		if value != Marker {
			t.Errorf("field %q = %#v, want marker", key, value)
		}
	}
}

func TestSensitiveNameCoversOAuthCredentialsButNotBackendErrorCode(t *testing.T) {
	for _, name := range []string{"code_verifier", "authorization_code", "device_auth_id", "user_code", "openai_access_token", "client-secret"} {
		if !SensitiveName(name) {
			t.Errorf("SensitiveName(%q) = false", name)
		}
	}
	for _, name := range []string{"code", "error_code", "token_count", "client_secret_hint"} {
		if SensitiveName(name) {
			t.Errorf("SensitiveName(%q) = true", name)
		}
	}
}

func TestJSONRedactsOAuthCredentialNamesAndPreservesBackendErrorCode(t *testing.T) {
	input := []byte(`{"code_verifier":"verifier-secret","authorization_code":"authorization-secret","device_auth_id":"device-secret","user_code":"user-secret","error":{"code":"refresh_token_expired"}}`)
	got, err := JSON(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"verifier-secret", "authorization-secret", "device-secret", "user-secret"} {
		if bytes.Contains(got, []byte(secret)) {
			t.Errorf("JSON output contains %q: %s", secret, got)
		}
	}
	if !bytes.Contains(got, []byte(`"code":"refresh_token_expired"`)) {
		t.Fatalf("backend error code lost: %s", got)
	}
}

func TestJSONRedactsCompoundSensitiveFieldNames(t *testing.T) {
	input := []byte(`{
		"openai_client_secret":"client-secret",
		"x_chatgpt_account_id":{"nested":"account-secret"},
		"x_api_key":["api-secret"],
		"x_authorization":"opaque-secret",
		"safe":{
			"openai_client_secret_hint":"configured",
			"x_chatgpt_account_id_hint":"configured",
			"x_api_key_hint":"configured",
			"x_authorization_mode":"oauth",
			"x_token_count":4
		}
	}`)

	got, err := JSON(input)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"openai_client_secret", "x_chatgpt_account_id", "x_api_key", "x_authorization"} {
		if decoded[key] != Marker {
			t.Errorf("field %q = %#v, want marker", key, decoded[key])
		}
	}
	wantSafe := map[string]any{
		"openai_client_secret_hint": "configured",
		"x_chatgpt_account_id_hint": "configured",
		"x_api_key_hint":            "configured",
		"x_authorization_mode":      "oauth",
		"x_token_count":             float64(4),
	}
	if safe := decoded["safe"]; !reflect.DeepEqual(safe, wantSafe) {
		t.Errorf("benign compound fields changed: got %#v want %#v", safe, wantSafe)
	}
}

func TestURLRedactsCompoundSensitiveQueryNames(t *testing.T) {
	values := url.Values{
		"openai_client_secret":      {"client-secret"},
		"x_chatgpt_account_id":      {"account-secret"},
		"x_api_key":                 {"api-secret"},
		"x_authorization":           {"opaque-secret"},
		"openai_client_secret_hint": {"configured"},
		"x_chatgpt_account_id_hint": {"configured"},
		"x_api_key_hint":            {"configured"},
		"x_authorization_mode":      {"oauth"},
		"x_token_count":             {"4"},
	}
	source := &url.URL{Scheme: "https", Host: "example.test", RawQuery: values.Encode()}
	original := source.String()

	got := URL(source)

	if source.String() != original {
		t.Fatalf("URL mutated source: got %q want %q", source.String(), original)
	}
	for _, key := range []string{"openai_client_secret", "x_chatgpt_account_id", "x_api_key", "x_authorization"} {
		if value := got.Query().Get(key); value != Marker {
			t.Errorf("query %q = %q, want marker", key, value)
		}
	}
	for key, want := range map[string]string{
		"openai_client_secret_hint": "configured",
		"x_chatgpt_account_id_hint": "configured",
		"x_api_key_hint":            "configured",
		"x_authorization_mode":      "oauth",
		"x_token_count":             "4",
	} {
		if value := got.Query().Get(key); value != want {
			t.Errorf("benign query %q = %q, want %q", key, value, want)
		}
	}
}

func TestJSONRejectsMalformedInputWithoutFakeOutput(t *testing.T) {
	for _, input := range [][]byte{
		[]byte(`{"access_token":"synthetic"`),
		[]byte(`{"safe":true} trailing`),
		[]byte{},
	} {
		got, err := JSON(input)
		if err == nil {
			t.Errorf("JSON(%q) error = nil", input)
		}
		if len(got) != 0 {
			t.Errorf("JSON(%q) returned fake output %q with error %v", input, got, err)
		}
	}
}

func TestTextRedactsBearersAndJWTsWithoutCorruptingBenignDots(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"bearer", "Authorization: Bearer synthetic-token", "Authorization: Bearer " + Marker},
		{"case insensitive bearer", "bEaReR\tsynthetic-token", "bEaReR\t" + Marker},
		{"JWT", "token=" + syntheticJWT, "token=" + Marker},
		{"multiple", syntheticJWT + " then Bearer other-secret", Marker + " then Bearer " + Marker},
		{"structured plaintext", "access_token=synthetic account-id: account-secret", "access_token=" + Marker + " account-id: " + Marker},
		{"generic token plaintext", "token=synthetic token_count=12", "token=" + Marker + " token_count=12"},
		{"quoted structured plaintext", `{"api_key":"synthetic","message":"safe"}`, `{"api_key":"` + Marker + `","message":"safe"}`},
		{"versions and IPs", "versions 1.2.3 and 10.20.30.40", "versions 1.2.3 and 10.20.30.40"},
		{"ordinary dotted words", "alpha.beta.gamma package.name and bearer", "alpha.beta.gamma package.name and bearer"},
		{"similarly named text", "token_count=12 authorization_mode=oauth api_key_hint=set client_secret_hint=set", "token_count=12 authorization_mode=oauth api_key_hint=set client_secret_hint=set"},
		{"unicode", "Hello 世界", "Hello 世界"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(tt.input); got != tt.want {
				t.Fatalf("Text() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSSERedactsJSONAndPlaintextWhileRetainingFraming(t *testing.T) {
	input := []byte("event: response.output_text.delta\r\n" +
		"id: 7\r\n" +
		"data: {\"delta\":\"Hello 世界\",\"access_token\":\"json-secret\",\"nested\":{\"email\":\"人@example.test\"}}\r\n" +
		"\r\n" +
		": comment Bearer comment-secret\n" +
		"data: Bearer plaintext-secret\n" +
		"\n" +
		"data: access_token=structured-secret\n" +
		"\n" +
		"data: " + syntheticJWT + "\n" +
		"\n")
	original := append([]byte(nil), input...)

	got := SSE(input)

	if !bytes.Equal(input, original) {
		t.Fatal("SSE mutated input")
	}
	for _, secret := range []string{"json-secret", "人@example.test", "comment-secret", "plaintext-secret", "structured-secret", syntheticJWT} {
		if bytes.Contains(got, []byte(secret)) {
			t.Errorf("SSE contains synthetic secret %q: %s", secret, got)
		}
	}
	if !bytes.Contains(got, []byte("event: response.output_text.delta\r\nid: 7\r\n")) ||
		!bytes.Contains(got, []byte("\r\n\r\n: comment Bearer "+Marker+"\ndata: Bearer "+Marker+"\n\n")) ||
		!bytes.Contains(got, []byte("data: access_token="+Marker+"\n\n")) ||
		!bytes.HasSuffix(got, []byte("data: "+Marker+"\n\n")) {
		t.Errorf("SSE framing changed:\n%s", got)
	}
	dataLine := firstLineWithPrefix(got, []byte("data: {"))
	if dataLine == nil || !json.Valid(bytes.TrimPrefix(dataLine, []byte("data: "))) {
		t.Errorf("SSE JSON data is invalid: %q", dataLine)
	}
	if !reflect.DeepEqual(lineEndings(got), lineEndings(input)) {
		t.Errorf("SSE line endings changed: got %q want %q", lineEndings(got), lineEndings(input))
	}
}

func FuzzJSON(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`null`),
		[]byte(`{"access_token":"synthetic","nested":[{"email":"人@example.test"}]}`),
		[]byte(`{"token":"synthetic","auth_token":"synthetic","session_token":"synthetic","client_secret":"synthetic"}`),
		[]byte(`{"safe":"` + syntheticJWT + `"}`),
		[]byte(`{"safe":"Bearer synthetic"}`),
		[]byte(`{"malformed":`),
		[]byte("\xff\x00"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		original := append([]byte(nil), input...)
		got, err := JSON(input)
		if !bytes.Equal(input, original) {
			t.Fatal("JSON mutated fuzz input")
		}
		if err != nil {
			if json.Valid(input) {
				t.Fatalf("JSON rejected valid input %q: %v", input, err)
			}
			if len(got) != 0 {
				t.Fatalf("JSON returned output %q with error %v", got, err)
			}
			return
		}
		if !json.Valid(got) {
			t.Fatalf("JSON returned invalid output %q for %q", got, input)
		}
	})
}

func FuzzText(f *testing.F) {
	for _, seed := range []string{
		"",
		"Hello 世界",
		"Bearer synthetic-token",
		"token=synthetic client_secret=synthetic",
		syntheticJWT,
		"1.2.3 127.0.0.1 alpha.beta.gamma",
		"two " + syntheticJWT + " Bearer synthetic-token",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		got := Text(input)
		if again := Text(got); again != got {
			t.Fatalf("Text is not idempotent: once %q twice %q", got, again)
		}
	})
}

func cloneHeader(source http.Header) http.Header {
	clone := make(http.Header, len(source))
	for key, values := range source {
		clone[key] = append([]string(nil), values...)
	}
	return clone
}

func firstLineWithPrefix(input, prefix []byte) []byte {
	for _, line := range bytes.Split(input, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, prefix) {
			return line
		}
	}
	return nil
}

func lineEndings(input []byte) string {
	var endings strings.Builder
	for i, value := range input {
		if value != '\n' {
			continue
		}
		if i > 0 && input[i-1] == '\r' {
			endings.WriteString("R")
		} else {
			endings.WriteString("N")
		}
	}
	return endings.String()
}
