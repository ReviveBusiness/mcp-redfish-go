package mcp

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/theoriginalaiexplorer/mcp-redfish-go/pkg/redfish"
)

// toMapData safely converts an interface{} value to map[string]interface{}.
// We use map[string]interface{} instead of interface{} for output Data fields because
// the Go MCP SDK's jsonschema-go generates empty schema {} for interface{}, which strict
// MCP clients (e.g., dot-ai's Zod validator) reject. map[string]interface{} generates
// {"type": "object"} which is MCP-spec compliant. Redfish API always returns JSON objects.
func toMapData(v interface{}) map[string]interface{} {
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		log.Printf("WARNING: toMapData received %T instead of map[string]interface{}, data will be nil in MCP response", v)
		return nil
	}
	return m
}

// validateSubscriptionID rejects IDs that contain path-traversal or reserved
// characters and returns a path-escaped segment safe for URL interpolation.
func validateSubscriptionID(id string) (string, error) {
	if id == "." || id == ".." || strings.Contains(id, "/") || strings.Contains(id, "\\") {
		return "", fmt.Errorf("invalid subscription_id: %q", id)
	}
	return url.PathEscape(id), nil
}

// redactURL strips path, query, fragment and userinfo from a URL string,
// returning only "scheme://host". This prevents sensitive data (credentials
// embedded in SNMP/syslog URLs, API keys in paths) from being written to logs.
// If the URL cannot be parsed, the original string is replaced with "[REDACTED]".
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[REDACTED]"
	}
	redacted := &url.URL{
		Scheme: u.Scheme,
		Host:   u.Host,
	}
	return redacted.String()
}

// ---------------------------------------------------------------------------
// Write safety: per-server mutex + rate limiting
// ---------------------------------------------------------------------------

// writeLimiter tracks per-server write mutex and rate limiting.
type writeLimiter struct {
	mu       sync.Mutex
	lastCall map[string]time.Time // per-server last write timestamp
	writeMu  map[string]*sync.Mutex // per-server write serialization
}

// limiter is the global write rate limiter. It is in-memory and does not
// persist across process restarts. It also does not coordinate across multiple
// replicas — each instance maintains its own independent state. For single-
// instance deployments (the intended use case) this is sufficient. Multi-
// replica or persistent rate limiting would require an external store (e.g.
// Redis) and is out of scope for this implementation.
var limiter = &writeLimiter{
	lastCall: make(map[string]time.Time),
	writeMu:  make(map[string]*sync.Mutex),
}

const writeRateLimitSeconds = 10 // minimum seconds between write ops per server

// acquireWrite locks the per-server write mutex and enforces rate limiting.
// Returns (release, recordSuccess, error). release MUST be called when the
// write attempt is complete (whether success or failure). recordSuccess MUST be
// called only after the write completes successfully — this updates the rate
// limit timestamp so failed writes do not consume quota.
func (l *writeLimiter) acquireWrite(serverAddr string) (release func(), recordSuccess func(), err error) {
	l.mu.Lock()
	if _, ok := l.writeMu[serverAddr]; !ok {
		l.writeMu[serverAddr] = &sync.Mutex{}
	}
	serverMu := l.writeMu[serverAddr]
	l.mu.Unlock()

	// Serialize writes per server — prevents concurrent boot override + power action races.
	serverMu.Lock()

	// Check rate limit
	l.mu.Lock()
	last, ok := l.lastCall[serverAddr]
	l.mu.Unlock()

	if ok {
		elapsed := time.Since(last)
		if elapsed < time.Duration(writeRateLimitSeconds)*time.Second {
			serverMu.Unlock()
			return nil, nil, fmt.Errorf("rate limited: write operations on %s allowed every %ds (last call %s ago)", serverAddr, writeRateLimitSeconds, elapsed.Round(time.Second))
		}
	}

	// Rate limit timestamp is recorded by recordSuccess() only after the write
	// completes successfully — failed writes must not consume quota.
	return func() { serverMu.Unlock() }, func() {
		l.mu.Lock()
		l.lastCall[serverAddr] = time.Now()
		l.mu.Unlock()
	}, nil
}

// ---------------------------------------------------------------------------
// Input / Output structs
// ---------------------------------------------------------------------------

// PowerActionInput represents input for the power_action tool.
type PowerActionInput struct {
	ServerAddressInput
	ResetType string `json:"reset_type" jsonschema:"Reset type: On, ForceOff, ForceRestart, GracefulRestart, GracefulShutdown, PushPowerButton, Nmi, PowerCycle"`
}

// PowerActionOutput represents the result of a power action.
type PowerActionOutput struct {
	StatusCode int                    `json:"status_code"`
	Message    string                 `json:"message"`
	Data       map[string]interface{} `json:"data,omitempty"`
}

// SetBootOverrideInput represents input for the set_boot_override tool.
type SetBootOverrideInput struct {
	ServerAddressInput
	Target  string `json:"target" jsonschema:"Boot target: None, Pxe, Cd, Hdd, BiosSetup, Utilities, UefiTarget, SDCard, UefiHttp"`
	Enabled string `json:"enabled" jsonschema:"Override mode: Once, Continuous, Disabled"`
}

// SetBootOverrideOutput represents the result of a boot override change.
type SetBootOverrideOutput struct {
	StatusCode int                    `json:"status_code"`
	Message    string                 `json:"message"`
	Data       map[string]interface{} `json:"data,omitempty"`
}

// ClearEventLogInput accepts an optional server address override.
type ClearEventLogInput struct {
	ServerAddressInput
}

// ClearEventLogOutput represents the result of clearing the event log.
type ClearEventLogOutput struct {
	StatusCode int                    `json:"status_code"`
	Message    string                 `json:"message"`
	Data       map[string]interface{} `json:"data,omitempty"`
}

// SetBiosSettingInput represents input for the set_bios_setting tool.
type SetBiosSettingInput struct {
	ServerAddressInput
	Attribute string `json:"attribute" jsonschema:"BIOS attribute name to change, e.g. 'ProcVirtualization' (required)"`
	Value     string `json:"value" jsonschema:"New value for the BIOS attribute, e.g. 'Enabled' (required)"`
}

// SetBiosSettingOutput represents the result of staging a BIOS attribute change.
type SetBiosSettingOutput struct {
	StatusCode int                    `json:"status_code,omitempty"`
	Message    string                 `json:"message"`
	Warning    string                 `json:"warning"`
	Data       map[string]interface{} `json:"data,omitempty"`
}

// SetAlertConfigInput represents input for the set_alert_config tool.
//
// For action=update, Context, EventTypes and Severity are pointer types so the
// handler can distinguish "field omitted" (nil) from "field explicitly set to
// empty" (non-nil pointing to a zero value). This makes it possible to clear a
// field that was previously set — passing nil leaves it unchanged on the server,
// passing a non-nil pointer to an empty value clears it.
type SetAlertConfigInput struct {
	ServerAddressInput
	Action         string    `json:"action" jsonschema:"Action to perform: list|add|remove|update"`
	SubscriptionID string    `json:"subscription_id,omitempty" jsonschema:"Subscription ID, required for remove/update (e.g. 'SubscriptionId1')"`
	Destination    string    `json:"destination,omitempty" jsonschema:"Target URL, required for add (e.g. 'snmp://10.0.0.1:162' or 'syslog://10.0.0.2:514')"`
	Protocol       string    `json:"protocol,omitempty" jsonschema:"Alert protocol, required for add: SNMPv1|SNMPv2c|SNMPv3|Syslog|Redfish|SMTP"`
	EventTypes     *[]string `json:"event_types,omitempty" jsonschema:"Event type filter: Alert, StatusChange, ResourceUpdated, ResourceAdded, ResourceRemoved"`
	Severity       *[]string `json:"severity,omitempty" jsonschema:"Severity filter: OK, Warning, Critical"`
	Context        *string   `json:"context,omitempty" jsonschema:"Human-readable description of this subscription"`
}

// SetAlertConfigOutput represents the result of an alert configuration operation.
type SetAlertConfigOutput struct {
	StatusCode int                    `json:"status_code,omitempty"`
	Message    string                 `json:"message"`
	Data       map[string]interface{} `json:"data,omitempty"`
}

// ---------------------------------------------------------------------------
// Allowed values
// ---------------------------------------------------------------------------

var validResetTypes = []string{
	"On", "ForceOff", "ForceRestart", "GracefulRestart",
	"GracefulShutdown", "PushPowerButton", "Nmi", "PowerCycle",
}

var validBootTargets = []string{
	"None", "Pxe", "Cd", "Hdd", "BiosSetup",
	"Utilities", "UefiTarget", "SDCard", "UefiHttp",
}

var validBootEnabled = []string{
	"Once", "Continuous", "Disabled",
}

var validAlertActions = []string{"list", "add", "remove", "update"}

var validAlertProtocols = []string{
	"SNMPv1", "SNMPv2c", "SNMPv3", "Syslog", "Redfish", "SMTP",
}

var validEventTypes = []string{
	"Alert", "StatusChange", "ResourceUpdated", "ResourceAdded", "ResourceRemoved",
}

var validSeverities = []string{"OK", "Warning", "Critical"}

// ---------------------------------------------------------------------------
// Registration (called from registerTools in server.go)
// ---------------------------------------------------------------------------

// registerWriteTools registers all write/mutation MCP tools.
func (s *Server) registerWriteTools() {
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "power_action",
		Description: "[WRITE] Execute a power action on the server (requires REDFISH_READ_ONLY=false). Supports: On, ForceOff, ForceRestart, GracefulRestart, GracefulShutdown, PushPowerButton, Nmi, PowerCycle",
	}, s.handlePowerAction)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "set_boot_override",
		Description: "[WRITE] Set one-time or persistent boot source override (requires REDFISH_READ_ONLY=false). Use with power_action GracefulRestart to boot from the override target.",
	}, s.handleSetBootOverride)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "clear_event_log",
		Description: "[WRITE] Clear the iDRAC System Event Log (requires REDFISH_READ_ONLY=false). Use after investigating and resolving hardware events.",
	}, s.handleClearEventLog)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "set_alert_config",
		Description: "Manage iDRAC alert destinations and filtering via EventService subscriptions. action=list is read-only and works without REDFISH_READ_ONLY=false. All other actions (add, remove, update) are writes and require REDFISH_READ_ONLY=false. Actions: list (show current subscriptions), add (create new with destination URL, protocol, event type and severity filters), remove (delete by ID), update (patch existing). Supports protocols: SNMPv1, SNMPv2c, SNMPv3, Syslog, Redfish, SMTP.",
	}, s.handleSetAlertConfig)

	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "set_bios_setting",
		Description: "[WRITE] Stage a BIOS attribute change on the server (requires REDFISH_READ_ONLY=false). Changes are staged and will only take effect after the next reboot — use power_action with GracefulRestart to apply. Does NOT auto-reboot. Example attributes: ProcVirtualization, BootMode, SysProfile.",
	}, s.handleSetBiosSetting)
}

// ---------------------------------------------------------------------------
// Safety gate
// ---------------------------------------------------------------------------

// checkReadOnly returns an error when REDFISH_READ_ONLY is true (the default).
func (s *Server) checkReadOnly(operation string) error {
	if s.config.Redfish.ReadOnlyValue() {
		return fmt.Errorf("write operations disabled: REDFISH_READ_ONLY=true (default). Set REDFISH_READ_ONLY=false to enable %s", operation)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 401-retry helper
// ---------------------------------------------------------------------------

// withRetryOn401 executes fn with client. On a 401 RedfishError it refreshes
// the session via getClient and retries fn exactly once.
func (s *Server) withRetryOn401(serverAddr string, fn func(c *redfish.Client) (*redfish.RedfishResponse, error)) (*redfish.RedfishResponse, error) {
	client, err := s.getClient(serverAddr, nil)
	if err != nil {
		return nil, err
	}

	response, err := fn(client)
	if err != nil {
		if rfErr, ok := err.(*redfish.RedfishError); ok && rfErr.Code == 401 {
			s.logger.Warn("Session expired, re-authenticating", "server", serverAddr)
			client, err = s.getClient(serverAddr, client)
			if err != nil {
				return nil, fmt.Errorf("re-login failed: %w", err)
			}
			response, err = fn(client)
		}
		if err != nil {
			return nil, err
		}
	}

	return response, nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// getCurrentPowerState fetches the current power state of the server.
// NOTE: /redfish/v1/Systems/System.Embedded.1 is a Dell iDRAC-specific path.
// Other BMC implementations may use a different Systems member identifier.
// This path could be made configurable in a future release.
func (s *Server) getCurrentPowerState(serverAddr string) (string, error) {
	response, err := s.withRetryOn401(serverAddr, func(c *redfish.Client) (*redfish.RedfishResponse, error) {
		return c.GetWithHeaders("/redfish/v1/Systems/System.Embedded.1")
	})
	if err != nil {
		return "", err
	}

	if data, ok := response.Data.(map[string]interface{}); ok {
		if ps, ok := data["PowerState"].(string); ok {
			return ps, nil
		}
	}
	return "Unknown", nil
}

// handlePowerAction executes a ComputerSystem.Reset action on the first
// configured server.
func (s *Server) handlePowerAction(ctx context.Context, req *mcp.CallToolRequest, input PowerActionInput) (*mcp.CallToolResult, PowerActionOutput, error) {
	// Safety gate
	if err := s.checkReadOnly("power actions"); err != nil {
		return nil, PowerActionOutput{}, err
	}

	// Validate input
	if !slices.Contains(validResetTypes, input.ResetType) {
		return nil, PowerActionOutput{}, fmt.Errorf("invalid reset type: %s. Must be one of: %v", input.ResetType, validResetTypes)
	}

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		return nil, PowerActionOutput{}, fmt.Errorf("no servers configured")
	}

	// Rate limit + serialize writes per server
	release, recordSuccess, err := limiter.acquireWrite(serverAddr)
	if err != nil {
		return nil, PowerActionOutput{}, err
	}
	defer release()

	// Pre-check: get current power state to warn on no-ops
	currentState, err := s.getCurrentPowerState(serverAddr)
	if err != nil {
		s.logger.Warn("Could not determine current power state, proceeding anyway", "server", serverAddr, "error", err)
	} else {
		// Warn on redundant actions
		if currentState == "On" && input.ResetType == "On" {
			return nil, PowerActionOutput{
				Message: fmt.Sprintf("Server is already powered On — no action taken"),
			}, nil
		}
		if currentState == "Off" && (input.ResetType == "ForceOff" || input.ResetType == "GracefulShutdown") {
			return nil, PowerActionOutput{
				Message: fmt.Sprintf("Server is already powered Off — no action taken"),
			}, nil
		}
	}

	// Log at Warn — all write operations are auditable.
	s.logger.Warn("Executing power action", "server", serverAddr, "reset_type", input.ResetType, "current_state", currentState)

	// NOTE: /redfish/v1/Systems/System.Embedded.1/Actions/ComputerSystem.Reset is a
	// Dell iDRAC-specific path. Other BMC implementations may use a different Systems
	// member identifier. This path could be made configurable in a future release.
	response, err := s.withRetryOn401(serverAddr, func(c *redfish.Client) (*redfish.RedfishResponse, error) {
		return c.Post(
			"/redfish/v1/Systems/System.Embedded.1/Actions/ComputerSystem.Reset",
			map[string]string{"ResetType": input.ResetType},
		)
	})
	if err != nil {
		return nil, PowerActionOutput{}, fmt.Errorf("power action failed: %w", err)
	}
	recordSuccess()

	return nil, PowerActionOutput{
		StatusCode: response.StatusCode,
		Message:    fmt.Sprintf("Power action %s executed successfully (was %s)", input.ResetType, currentState),
		Data:       toMapData(response.Data),
	}, nil
}

// handleSetBootOverride patches the Boot source override on the first
// configured server.
func (s *Server) handleSetBootOverride(ctx context.Context, req *mcp.CallToolRequest, input SetBootOverrideInput) (*mcp.CallToolResult, SetBootOverrideOutput, error) {
	// Safety gate
	if err := s.checkReadOnly("boot override"); err != nil {
		return nil, SetBootOverrideOutput{}, err
	}

	// Validate inputs
	if !slices.Contains(validBootTargets, input.Target) {
		return nil, SetBootOverrideOutput{}, fmt.Errorf("invalid boot target: %s. Must be one of: %v", input.Target, validBootTargets)
	}
	if !slices.Contains(validBootEnabled, input.Enabled) {
		return nil, SetBootOverrideOutput{}, fmt.Errorf("invalid boot override mode: %s. Must be one of: %v", input.Enabled, validBootEnabled)
	}

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		return nil, SetBootOverrideOutput{}, fmt.Errorf("no servers configured")
	}

	// Rate limit + serialize writes per server
	release, recordSuccess, err := limiter.acquireWrite(serverAddr)
	if err != nil {
		return nil, SetBootOverrideOutput{}, err
	}
	defer release()

	s.logger.Warn("Setting boot override", "server", serverAddr, "target", input.Target, "enabled", input.Enabled)

	body := map[string]interface{}{
		"Boot": map[string]string{
			"BootSourceOverrideTarget":  input.Target,
			"BootSourceOverrideEnabled": input.Enabled,
		},
	}

	// NOTE: /redfish/v1/Systems/System.Embedded.1 is a Dell iDRAC-specific path.
	// Other BMC implementations may use a different Systems member identifier.
	// This path could be made configurable in a future release.
	response, err := s.withRetryOn401(serverAddr, func(c *redfish.Client) (*redfish.RedfishResponse, error) {
		return c.Patch("/redfish/v1/Systems/System.Embedded.1", body)
	})
	if err != nil {
		return nil, SetBootOverrideOutput{}, fmt.Errorf("set boot override failed: %w", err)
	}
	recordSuccess()

	return nil, SetBootOverrideOutput{
		StatusCode: response.StatusCode,
		Message:    fmt.Sprintf("Boot override set to %s (%s)", input.Target, input.Enabled),
		Data:       toMapData(response.Data),
	}, nil
}

// handleClearEventLog clears the iDRAC System Event Log on the first
// configured server.
func (s *Server) handleClearEventLog(ctx context.Context, req *mcp.CallToolRequest, input ClearEventLogInput) (*mcp.CallToolResult, ClearEventLogOutput, error) {
	// Safety gate
	if err := s.checkReadOnly("event log clearing"); err != nil {
		return nil, ClearEventLogOutput{}, err
	}

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		return nil, ClearEventLogOutput{}, fmt.Errorf("no servers configured")
	}

	// Rate limit + serialize writes per server
	release, recordSuccess, err := limiter.acquireWrite(serverAddr)
	if err != nil {
		return nil, ClearEventLogOutput{}, err
	}
	defer release()

	s.logger.Warn("Clearing iDRAC System Event Log", "server", serverAddr)

	// NOTE: /redfish/v1/Managers/iDRAC.Embedded.1/LogServices/Sel/Actions/LogService.ClearLog
	// is a Dell iDRAC-specific path. Other BMC implementations may expose a different
	// Manager identifier or log service path. This could be made configurable in a future release.
	response, err := s.withRetryOn401(serverAddr, func(c *redfish.Client) (*redfish.RedfishResponse, error) {
		return c.Post(
			"/redfish/v1/Managers/iDRAC.Embedded.1/LogServices/Sel/Actions/LogService.ClearLog",
			map[string]string{},
		)
	})
	if err != nil {
		return nil, ClearEventLogOutput{}, fmt.Errorf("clear event log failed: %w", err)
	}
	recordSuccess()

	return nil, ClearEventLogOutput{
		StatusCode: response.StatusCode,
		Message:    "iDRAC System Event Log cleared successfully",
		Data:       toMapData(response.Data),
	}, nil
}

// handleSetAlertConfig manages iDRAC alert destinations and filtering via the
// Redfish EventService. Supports list/add/remove/update of subscriptions.
//
// NOTE: The EventService paths below are standard Redfish paths, but Dell iDRAC
// may return additional OEM fields in subscription responses. Specifically:
//   - /redfish/v1/EventService — service root with SMTP and SNMP settings
//   - /redfish/v1/EventService/Subscriptions — subscription collection
//   - /redfish/v1/EventService/Subscriptions/{id} — individual subscription
//
// These paths are consistent across iDRAC 8/9 firmware releases.
func (s *Server) handleSetAlertConfig(ctx context.Context, req *mcp.CallToolRequest, input SetAlertConfigInput) (*mcp.CallToolResult, SetAlertConfigOutput, error) {
	// Safety gate — list is read-only; all other actions require write access.
	if input.Action != "list" {
		if err := s.checkReadOnly("alert configuration"); err != nil {
			return nil, SetAlertConfigOutput{}, err
		}
	}

	// Validate action
	if !slices.Contains(validAlertActions, input.Action) {
		return nil, SetAlertConfigOutput{}, fmt.Errorf("invalid action: %s. Must be one of: %v", input.Action, validAlertActions)
	}

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		return nil, SetAlertConfigOutput{}, fmt.Errorf("no servers configured")
	}

	switch input.Action {
	case "list":
		return s.handleAlertList(serverAddr)
	case "add":
		return s.handleAlertAdd(serverAddr, input)
	case "remove":
		return s.handleAlertRemove(serverAddr, input)
	case "update":
		return s.handleAlertUpdate(serverAddr, input)
	}

	// Unreachable — all cases handled above after validation.
	return nil, SetAlertConfigOutput{}, fmt.Errorf("unhandled action: %s", input.Action)
}

// handleAlertList fetches the EventService subscriptions collection.
func (s *Server) handleAlertList(serverAddr string) (*mcp.CallToolResult, SetAlertConfigOutput, error) {
	// NOTE: /redfish/v1/EventService/Subscriptions is the standard Redfish path
	// for the alert subscription collection. Dell iDRAC uses this path verbatim.
	response, err := s.withRetryOn401(serverAddr, func(c *redfish.Client) (*redfish.RedfishResponse, error) {
		return c.Get("/redfish/v1/EventService/Subscriptions")
	})
	if err != nil {
		return nil, SetAlertConfigOutput{}, fmt.Errorf("failed to list alert subscriptions: %w", err)
	}

	return nil, SetAlertConfigOutput{
		StatusCode: response.StatusCode,
		Message:    "Alert subscriptions retrieved successfully",
		Data:       toMapData(response.Data),
	}, nil
}

// handleAlertAdd creates a new EventService subscription.
func (s *Server) handleAlertAdd(serverAddr string, input SetAlertConfigInput) (*mcp.CallToolResult, SetAlertConfigOutput, error) {
	// Validate required fields for add
	if input.Destination == "" {
		return nil, SetAlertConfigOutput{}, fmt.Errorf("destination is required for action=add")
	}
	if input.Protocol == "" {
		return nil, SetAlertConfigOutput{}, fmt.Errorf("protocol is required for action=add")
	}
	if !slices.Contains(validAlertProtocols, input.Protocol) {
		return nil, SetAlertConfigOutput{}, fmt.Errorf("invalid protocol: %s. Must be one of: %v", input.Protocol, validAlertProtocols)
	}

	// Validate optional event type filter
	if input.EventTypes != nil {
		for _, et := range *input.EventTypes {
			if !slices.Contains(validEventTypes, et) {
				return nil, SetAlertConfigOutput{}, fmt.Errorf("invalid event type: %s. Must be one of: %v", et, validEventTypes)
			}
		}
	}

	// Validate optional severity filter — Redfish MessageSeverity is a single
	// enum value; only one severity may be specified per subscription.
	if input.Severity != nil {
		if len(*input.Severity) > 1 {
			return nil, SetAlertConfigOutput{}, fmt.Errorf("only one severity value is supported per subscription (got %d); Redfish MessageSeverity is a single enum, not an array", len(*input.Severity))
		}
		for _, sev := range *input.Severity {
			if !slices.Contains(validSeverities, sev) {
				return nil, SetAlertConfigOutput{}, fmt.Errorf("invalid severity: %s. Must be one of: %v", sev, validSeverities)
			}
		}
	}

	// Rate limit + serialize writes per server
	release, recordSuccess, err := limiter.acquireWrite(serverAddr)
	if err != nil {
		return nil, SetAlertConfigOutput{}, err
	}
	defer release()

	// Build subscription body
	body := map[string]interface{}{
		"Destination": input.Destination,
		"Protocol":    input.Protocol,
	}
	if input.EventTypes != nil && len(*input.EventTypes) > 0 {
		body["EventTypes"] = *input.EventTypes
	}
	if input.Severity != nil && len(*input.Severity) > 0 {
		// Redfish MessageSeverity is a single enum string, not an array.
		body["MessageSeverity"] = (*input.Severity)[0]
	}
	if input.Context != nil {
		body["Context"] = *input.Context
	}

	// Redact destination URL before logging — strip path/query/credentials to
	// avoid leaking sensitive config (e.g. SNMP community strings in URLs).
	logDestination := redactURL(input.Destination)
	s.logger.Warn("Adding alert subscription", "server", serverAddr, "destination", logDestination, "protocol", input.Protocol)

	// NOTE: POST to /redfish/v1/EventService/Subscriptions creates a new subscription.
	// Dell iDRAC returns 201 Created with the new subscription URL in the Location header.
	response, err := s.withRetryOn401(serverAddr, func(c *redfish.Client) (*redfish.RedfishResponse, error) {
		return c.Post("/redfish/v1/EventService/Subscriptions", body)
	})
	if err != nil {
		return nil, SetAlertConfigOutput{}, fmt.Errorf("failed to add alert subscription: %w", err)
	}
	recordSuccess()

	return nil, SetAlertConfigOutput{
		StatusCode: response.StatusCode,
		Message:    fmt.Sprintf("Alert subscription added for %s (protocol: %s)", input.Destination, input.Protocol),
		Data:       toMapData(response.Data),
	}, nil
}

// handleAlertRemove deletes an EventService subscription by ID.
func (s *Server) handleAlertRemove(serverAddr string, input SetAlertConfigInput) (*mcp.CallToolResult, SetAlertConfigOutput, error) {
	if input.SubscriptionID == "" {
		return nil, SetAlertConfigOutput{}, fmt.Errorf("subscription_id is required for action=remove")
	}
	escapedID, err := validateSubscriptionID(input.SubscriptionID)
	if err != nil {
		return nil, SetAlertConfigOutput{}, err
	}

	// Rate limit + serialize writes per server
	release, recordSuccess, err := limiter.acquireWrite(serverAddr)
	if err != nil {
		return nil, SetAlertConfigOutput{}, err
	}
	defer release()

	s.logger.Warn("Removing alert subscription", "server", serverAddr, "subscription_id", input.SubscriptionID)

	// NOTE: DELETE /redfish/v1/EventService/Subscriptions/{id} removes the subscription.
	// Dell iDRAC returns 200 OK on successful deletion.
	path := fmt.Sprintf("/redfish/v1/EventService/Subscriptions/%s", escapedID)
	response, err := s.withRetryOn401(serverAddr, func(c *redfish.Client) (*redfish.RedfishResponse, error) {
		return c.Delete(path)
	})
	if err != nil {
		return nil, SetAlertConfigOutput{}, fmt.Errorf("failed to remove alert subscription %s: %w", input.SubscriptionID, err)
	}
	recordSuccess()

	return nil, SetAlertConfigOutput{
		StatusCode: response.StatusCode,
		Message:    fmt.Sprintf("Alert subscription %s removed successfully", input.SubscriptionID),
		Data:       toMapData(response.Data),
	}, nil
}

// handleSetBiosSetting stages a BIOS attribute change via the Redfish Bios/Settings
// pending-change resource. The change is not applied until the next system reboot.
//
// NOTE: /redfish/v1/Systems/System.Embedded.1/Bios/Settings is the Dell iDRAC-specific
// path for staging BIOS attribute changes. The parent resource
// /redfish/v1/Systems/System.Embedded.1/Bios reflects the currently active values;
// /Bios/Settings holds the pending (not-yet-applied) changes. This two-resource
// pattern is standard Redfish DSP0268 but the Systems member identifier
// ("System.Embedded.1") is Dell-specific and may differ on other BMC implementations.
func (s *Server) handleSetBiosSetting(ctx context.Context, req *mcp.CallToolRequest, input SetBiosSettingInput) (*mcp.CallToolResult, SetBiosSettingOutput, error) {
	// Safety gate
	if err := s.checkReadOnly("BIOS settings"); err != nil {
		return nil, SetBiosSettingOutput{}, err
	}

	// Validate required inputs
	if input.Attribute == "" {
		return nil, SetBiosSettingOutput{}, fmt.Errorf("attribute is required")
	}
	if input.Value == "" {
		return nil, SetBiosSettingOutput{}, fmt.Errorf("value is required")
	}

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		return nil, SetBiosSettingOutput{}, fmt.Errorf("no servers configured")
	}

	// Rate limit + serialize writes per server
	release, recordSuccess, err := limiter.acquireWrite(serverAddr)
	if err != nil {
		return nil, SetBiosSettingOutput{}, err
	}
	defer release()

	// Log attribute name only — value is omitted because BIOS attributes can hold
	// sensitive data (e.g. passwords, keys) and must not be sent to centralized logs.
	s.logger.Warn("Staging BIOS attribute change", "server", serverAddr, "attribute", input.Attribute, "value", "[REDACTED]")

	body := map[string]interface{}{
		"Attributes": map[string]interface{}{
			input.Attribute: input.Value,
		},
	}

	// NOTE: PATCH to /Bios/Settings stages the change in the pending-values resource.
	// The change does not take effect until the next system reboot. Dell iDRAC returns
	// 200 OK on success; the updated pending value can be confirmed by GET /Bios/Settings.
	response, err := s.withRetryOn401(serverAddr, func(c *redfish.Client) (*redfish.RedfishResponse, error) {
		return c.Patch("/redfish/v1/Systems/System.Embedded.1/Bios/Settings", body)
	})
	if err != nil {
		return nil, SetBiosSettingOutput{}, fmt.Errorf("failed to stage BIOS setting: %w", err)
	}
	recordSuccess()

	return nil, SetBiosSettingOutput{
		StatusCode: response.StatusCode,
		Message:    fmt.Sprintf("BIOS attribute '%s' staged to '%s' on %s", input.Attribute, input.Value, serverAddr),
		Warning:    "Change is staged only — a system reboot is required for the new value to take effect. Use power_action with GracefulRestart to apply.",
		Data:       toMapData(response.Data),
	}, nil
}

// handleAlertUpdate patches an existing EventService subscription.
func (s *Server) handleAlertUpdate(serverAddr string, input SetAlertConfigInput) (*mcp.CallToolResult, SetAlertConfigOutput, error) {
	if input.SubscriptionID == "" {
		return nil, SetAlertConfigOutput{}, fmt.Errorf("subscription_id is required for action=update")
	}
	escapedID, err := validateSubscriptionID(input.SubscriptionID)
	if err != nil {
		return nil, SetAlertConfigOutput{}, err
	}

	// Validate optional protocol
	if input.Protocol != "" && !slices.Contains(validAlertProtocols, input.Protocol) {
		return nil, SetAlertConfigOutput{}, fmt.Errorf("invalid protocol: %s. Must be one of: %v", input.Protocol, validAlertProtocols)
	}

	// Validate optional event type filter
	if input.EventTypes != nil {
		for _, et := range *input.EventTypes {
			if !slices.Contains(validEventTypes, et) {
				return nil, SetAlertConfigOutput{}, fmt.Errorf("invalid event type: %s. Must be one of: %v", et, validEventTypes)
			}
		}
	}

	// Validate optional severity filter — Redfish MessageSeverity is a single
	// enum value; only one severity may be specified per subscription.
	if input.Severity != nil {
		if len(*input.Severity) > 1 {
			return nil, SetAlertConfigOutput{}, fmt.Errorf("only one severity value is supported per subscription (got %d); Redfish MessageSeverity is a single enum, not an array", len(*input.Severity))
		}
		for _, sev := range *input.Severity {
			if !slices.Contains(validSeverities, sev) {
				return nil, SetAlertConfigOutput{}, fmt.Errorf("invalid severity: %s. Must be one of: %v", sev, validSeverities)
			}
		}
	}

	// Build patch body — only include fields that were explicitly provided.
	// Pointer fields (EventTypes, Severity, Context): nil = omitted, non-nil = set
	// (including non-nil pointer to empty value, which clears the field on the server).
	body := map[string]interface{}{}
	if input.Destination != "" {
		body["Destination"] = input.Destination
	}
	if input.Protocol != "" {
		body["Protocol"] = input.Protocol
	}
	if input.EventTypes != nil {
		body["EventTypes"] = *input.EventTypes
	}
	if input.Severity != nil && len(*input.Severity) > 0 {
		// Redfish MessageSeverity is a single enum string, not an array.
		body["MessageSeverity"] = (*input.Severity)[0]
	}
	if input.Context != nil {
		body["Context"] = *input.Context
	}

	if len(body) == 0 {
		return nil, SetAlertConfigOutput{}, fmt.Errorf("no fields provided for update — supply at least one of: destination, protocol, event_types, severity, context")
	}

	// Rate limit + serialize writes per server
	release, recordSuccess, err := limiter.acquireWrite(serverAddr)
	if err != nil {
		return nil, SetAlertConfigOutput{}, err
	}
	defer release()

	s.logger.Warn("Updating alert subscription", "server", serverAddr, "subscription_id", input.SubscriptionID)

	// NOTE: PATCH /redfish/v1/EventService/Subscriptions/{id} modifies an existing
	// subscription. Dell iDRAC returns 200 OK with the updated subscription body.
	path := fmt.Sprintf("/redfish/v1/EventService/Subscriptions/%s", escapedID)
	response, err := s.withRetryOn401(serverAddr, func(c *redfish.Client) (*redfish.RedfishResponse, error) {
		return c.Patch(path, body)
	})
	if err != nil {
		return nil, SetAlertConfigOutput{}, fmt.Errorf("failed to update alert subscription %s: %w", input.SubscriptionID, err)
	}
	recordSuccess()

	return nil, SetAlertConfigOutput{
		StatusCode: response.StatusCode,
		Message:    fmt.Sprintf("Alert subscription %s updated successfully", input.SubscriptionID),
		Data:       toMapData(response.Data),
	}, nil
}
