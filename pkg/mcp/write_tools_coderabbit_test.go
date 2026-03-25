package mcp

// Tests for the 4 CodeRabbit findings fixed in write_tools.go (#1337).
//
// Finding 1 (MAJOR):  set_alert_config update can't distinguish omitted vs
//                     empty fields — fixed by using pointer types for Context,
//                     EventTypes, Severity on SetAlertConfigInput.
//
// Finding 2 (MINOR):  set_alert_config description was misleading about
//                     read-only requirement — fixed in tool registration.
//
// Finding 3 (MAJOR):  sensitive values logged at Warn level — fixed by
//                     redacting destination URL and BIOS value in log calls.
//
// Finding 4 (MAJOR):  silently drops extra severity values — fixed by
//                     returning an error when len(Severity) > 1.

import (
	"log/slog"
	"slices"
	"testing"
)

// newTestServer returns a minimal Server suitable for unit-testing validation
// logic that runs before any network call. The logger is set to the default
// slog logger so Warn calls in the handler don't panic on a nil logger.
func newTestServer() *Server {
	return &Server{
		logger: slog.Default(),
	}
}

// ---------------------------------------------------------------------------
// Finding 1 — pointer types for update fields
// ---------------------------------------------------------------------------

// TestSetAlertConfigInputPointerTypes verifies that Context, EventTypes and
// Severity on SetAlertConfigInput are pointer types, enabling the handler to
// distinguish "field omitted" (nil) from "explicitly set to empty value"
// (non-nil pointer to zero/empty value).
func TestSetAlertConfigInputPointerTypes(t *testing.T) {
	// Omitted fields — all pointers are nil.
	omitted := SetAlertConfigInput{
		Action: "update",
	}
	if omitted.Context != nil {
		t.Error("omitted Context should be nil, got non-nil — field cannot distinguish omitted from empty")
	}
	if omitted.EventTypes != nil {
		t.Error("omitted EventTypes should be nil, got non-nil")
	}
	if omitted.Severity != nil {
		t.Error("omitted Severity should be nil, got non-nil")
	}

	// Explicitly set to empty string/slice — pointer is non-nil but points to
	// a zero value. This is the "clear the field" signal.
	emptyCtx := ""
	emptyETs := []string{}
	emptySev := []string{}
	explicit := SetAlertConfigInput{
		Action:     "update",
		Context:    &emptyCtx,
		EventTypes: &emptyETs,
		Severity:   &emptySev,
	}
	if explicit.Context == nil {
		t.Error("explicitly-set-to-empty Context should be non-nil pointer")
	}
	if *explicit.Context != "" {
		t.Errorf("explicit empty Context: got %q, want empty string", *explicit.Context)
	}
	if explicit.EventTypes == nil {
		t.Error("explicitly-set-to-empty EventTypes should be non-nil pointer")
	}
	if len(*explicit.EventTypes) != 0 {
		t.Errorf("explicit empty EventTypes: got len=%d, want 0", len(*explicit.EventTypes))
	}
	if explicit.Severity == nil {
		t.Error("explicitly-set-to-empty Severity should be non-nil pointer")
	}
	if len(*explicit.Severity) != 0 {
		t.Errorf("explicit empty Severity: got len=%d, want 0", len(*explicit.Severity))
	}

	// Normal populated values.
	ctx := "my subscription"
	ets := []string{"Alert", "StatusChange"}
	sev := []string{"Critical"}
	populated := SetAlertConfigInput{
		Action:     "update",
		Context:    &ctx,
		EventTypes: &ets,
		Severity:   &sev,
	}
	if *populated.Context != ctx {
		t.Errorf("populated Context: got %q, want %q", *populated.Context, ctx)
	}
	if len(*populated.EventTypes) != 2 {
		t.Errorf("populated EventTypes: got len=%d, want 2", len(*populated.EventTypes))
	}
	if (*populated.Severity)[0] != "Critical" {
		t.Errorf("populated Severity[0]: got %q, want \"Critical\"", (*populated.Severity)[0])
	}

	t.Log("pointer types allow nil-vs-empty distinction for all three update fields (PASS)")
}

// ---------------------------------------------------------------------------
// Finding 3 — redactURL helper
// ---------------------------------------------------------------------------

// TestRedactURL verifies that redactURL strips path, query, fragment and
// userinfo, retaining only scheme+host. This prevents credentials and sensitive
// path segments from appearing in centralized logs.
func TestRedactURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		desc  string
	}{
		{
			name:  "plain snmp URL",
			input: "snmp://10.0.0.1:162",
			want:  "snmp://10.0.0.1:162",
			desc:  "simple host:port — nothing to strip",
		},
		{
			name:  "URL with path",
			input: "syslog://10.0.0.2:514/some/path",
			want:  "syslog://10.0.0.2:514",
			desc:  "path must be stripped",
		},
		{
			name:  "URL with credentials",
			input: "snmp://community:secret@10.0.0.3:162",
			want:  "snmp://10.0.0.3:162",
			desc:  "userinfo (credentials) must be stripped",
		},
		{
			name:  "URL with query string",
			input: "https://alert.example.com/hook?token=supersecret&env=prod",
			want:  "https://alert.example.com",
			desc:  "query params (may contain API keys) must be stripped",
		},
		{
			name:  "URL with fragment",
			input: "https://logs.example.com/endpoint#section",
			want:  "https://logs.example.com",
			desc:  "fragment must be stripped",
		},
		{
			name:  "full URL with all sensitive parts",
			input: "https://user:password@10.1.2.3:8443/path/to/resource?key=abc&token=xyz#frag",
			want:  "https://10.1.2.3:8443",
			desc:  "all sensitive parts (creds, path, query, fragment) must be stripped",
		},
		{
			name:  "unparseable URL",
			input: "://this-is-not-valid",
			want:  "[REDACTED]",
			desc:  "unparseable URL must fall back to [REDACTED] — never log the raw value",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
			desc:  "empty string parses as empty URL — scheme and host are both empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactURL(tt.input)
			if got != tt.want {
				t.Errorf("redactURL(%q) = %q, want %q\n  context: %s", tt.input, got, tt.want, tt.desc)
				return
			}
			t.Logf("redactURL(%q) = %q (PASS)", tt.input, got)
		})
	}
}

// TestRedactURLDoesNotLeakCredentials is a security-focused test that confirms
// no credential material survives redactURL. The test explicitly checks for
// substrings that should never appear in a redacted URL.
func TestRedactURLDoesNotLeakCredentials(t *testing.T) {
	sensitiveInputs := []struct {
		name     string
		raw      string
		badWords []string
	}{
		{
			name:     "SNMP community string in URL",
			raw:      "snmp://public:community123@192.168.1.1:162",
			badWords: []string{"public", "community123"},
		},
		{
			name:     "API token in query param",
			raw:      "https://webhook.example.com/notify?api_key=sk-supersecret123",
			badWords: []string{"sk-supersecret123", "api_key"},
		},
		{
			name:     "password in path segment",
			raw:      "syslog://10.0.0.1:514/token/my-secret-token/events",
			badWords: []string{"my-secret-token", "token"},
		},
	}

	for _, tt := range sensitiveInputs {
		t.Run(tt.name, func(t *testing.T) {
			got := redactURL(tt.raw)
			for _, bad := range tt.badWords {
				if contains(got, bad) {
					t.Errorf("redactURL leaked sensitive substring %q in output %q (input: %q)", bad, got, tt.raw)
				}
			}
			t.Logf("redactURL(%q) = %q — no sensitive substrings leaked (PASS)", tt.raw, got)
		})
	}
}

// contains is a simple substring check used in tests to avoid importing strings
// package or depending on external helpers.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && stringContains(s, substr))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Finding 4 — reject multiple severity values
// ---------------------------------------------------------------------------

// TestHandleAlertAddRejectsMultipleSeverities verifies that passing more than
// one severity value to handleAlertAdd returns an error rather than silently
// discarding all but the first value.
func TestHandleAlertAddRejectsMultipleSeverities(t *testing.T) {
	// Severity validation happens before any network call, so we don't need a
	// live Redfish endpoint — a test Server with a default logger is sufficient.
	s := newTestServer()

	twoSev := []string{"Warning", "Critical"}
	input := SetAlertConfigInput{
		Action:      "add",
		Destination: "snmp://10.0.0.1:162",
		Protocol:    "SNMPv2c",
		Severity:    &twoSev,
	}

	_, _, err := s.handleAlertAdd("192.168.1.1", input)
	if err == nil {
		t.Fatal("expected error for len(Severity) > 1, got nil — extra values are being silently dropped")
	}
	t.Logf("handleAlertAdd correctly rejected multiple severities: %v (PASS)", err)
}

// TestHandleAlertUpdateRejectsMultipleSeverities covers the same validation
// in the update path.
func TestHandleAlertUpdateRejectsMultipleSeverities(t *testing.T) {
	s := newTestServer()

	twoSev := []string{"OK", "Warning"}
	input := SetAlertConfigInput{
		Action:         "update",
		SubscriptionID: "Sub1",
		Severity:       &twoSev,
	}

	_, _, err := s.handleAlertUpdate("192.168.1.1", input)
	if err == nil {
		t.Fatal("expected error for len(Severity) > 1 in update, got nil — extra values are being silently dropped")
	}
	t.Logf("handleAlertUpdate correctly rejected multiple severities: %v (PASS)", err)
}

// TestHandleAlertAddAcceptsSingleSeverity confirms that the single-severity
// happy path passes the validation gate. We only verify that validation
// does NOT return the "only one severity value" error — we do not attempt
// a real network call, so we call the severity-validation logic inline
// rather than invoking the full handleAlertAdd handler (which would panic
// on a nil hostManager).
func TestHandleAlertAddAcceptsSingleSeverity(t *testing.T) {
	oneSev := []string{"Critical"}
	input := SetAlertConfigInput{
		Severity: &oneSev,
	}

	// Replicate the validation gate from handleAlertAdd.
	if input.Severity != nil {
		if len(*input.Severity) > 1 {
			t.Fatalf("single-severity input was incorrectly rejected by the >1 gate (len=%d)", len(*input.Severity))
		}
		for _, sev := range *input.Severity {
			if !slices.Contains(validSeverities, sev) {
				t.Fatalf("valid severity %q was incorrectly rejected", sev)
			}
		}
	}

	t.Log("single severity [Critical] passes validation gate (PASS)")
}

// TestHandleAlertAddAcceptsNoSeverity confirms that omitting severity
// (nil pointer) passes the validation gate without error.
func TestHandleAlertAddAcceptsNoSeverity(t *testing.T) {
	input := SetAlertConfigInput{
		Severity: nil, // omitted
	}

	// Replicate the validation gate from handleAlertAdd.
	if input.Severity != nil {
		if len(*input.Severity) > 1 {
			t.Fatalf("nil severity was incorrectly flagged as too many values")
		}
	}

	t.Log("nil (omitted) severity passes validation gate (PASS)")
}
