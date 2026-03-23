package mcp

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/theoriginalaiexplorer/mcp-redfish-go/pkg/redfish"
)

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
	ResetType string `json:"reset_type" jsonschema:"Reset type: On, ForceOff, ForceRestart, GracefulRestart, GracefulShutdown, PushPowerButton, Nmi, PowerCycle"`
}

// PowerActionOutput represents the result of a power action.
type PowerActionOutput struct {
	StatusCode int         `json:"status_code"`
	Message    string      `json:"message"`
	Data       interface{} `json:"data,omitempty"`
}

// SetBootOverrideInput represents input for the set_boot_override tool.
type SetBootOverrideInput struct {
	Target  string `json:"target" jsonschema:"Boot target: None, Pxe, Cd, Hdd, BiosSetup, Utilities, UefiTarget, SDCard, UefiHttp"`
	Enabled string `json:"enabled" jsonschema:"Override mode: Once, Continuous, Disabled"`
}

// SetBootOverrideOutput represents the result of a boot override change.
type SetBootOverrideOutput struct {
	StatusCode int         `json:"status_code"`
	Message    string      `json:"message"`
	Data       interface{} `json:"data,omitempty"`
}

// ClearEventLogInput is an empty struct — the tool takes no parameters.
type ClearEventLogInput struct{}

// ClearEventLogOutput represents the result of clearing the event log.
type ClearEventLogOutput struct {
	StatusCode int         `json:"status_code"`
	Message    string      `json:"message"`
	Data       interface{} `json:"data,omitempty"`
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

	serverAddr := s.hostManager.GetAddresses()[0]

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
		Data:       response.Data,
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

	serverAddr := s.hostManager.GetAddresses()[0]

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
		Data:       response.Data,
	}, nil
}

// handleClearEventLog clears the iDRAC System Event Log on the first
// configured server.
func (s *Server) handleClearEventLog(ctx context.Context, req *mcp.CallToolRequest, input ClearEventLogInput) (*mcp.CallToolResult, ClearEventLogOutput, error) {
	// Safety gate
	if err := s.checkReadOnly("event log clearing"); err != nil {
		return nil, ClearEventLogOutput{}, err
	}

	serverAddr := s.hostManager.GetAddresses()[0]

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
		Data:       response.Data,
	}, nil
}
