package mcp

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/theoriginalaiexplorer/mcp-redfish-go/pkg/common"
	"github.com/theoriginalaiexplorer/mcp-redfish-go/pkg/config"
	"github.com/theoriginalaiexplorer/mcp-redfish-go/pkg/redfish"
)

// Server wraps the MCP server with Redfish-specific functionality
type Server struct {
	mcpServer   *mcp.Server
	config      *config.Config
	hostManager *common.HostManager
	logger      *slog.Logger

	// clients caches one authenticated Redfish client per server address to
	// avoid creating a new iDRAC session on every tool call (session exhaustion).
	clients map[string]*redfish.Client
	mu      sync.Mutex
}

// NewServer creates a new Redfish MCP server
func NewServer(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// Create MCP server
	mcpServer := mcp.NewServer(
		&mcp.Implementation{
			Name:    "redfish-mcp",
			Version: "0.4.0",
		},
		&mcp.ServerOptions{
			// Configure based on transport
		},
	)

	// Create host manager
	hostManager := common.NewHostManager(logger)

	server := &Server{
		mcpServer:   mcpServer,
		config:      cfg,
		hostManager: hostManager,
		logger:      logger,
		clients:     make(map[string]*redfish.Client),
	}

	// Register tools
	if err := server.registerTools(); err != nil {
		return nil, fmt.Errorf("failed to register tools: %w", err)
	}

	logger.Info("Redfish MCP server created successfully")
	return server, nil
}

// GetResourceInput represents input for the get_resource_data tool
type GetResourceInput struct {
	URL string `json:"url" jsonschema:"Redfish resource URL"`
}

// GetResourceOutput represents output for the get_resource_data tool
type GetResourceOutput struct {
	Headers map[string][]string `json:"headers"`
	Data    interface{}         `json:"data"`
}

// registerTools registers the MCP tools
func (s *Server) registerTools() error {
	// Register list_servers tool
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "list_servers",
		Description: "List all Redfish servers that can be accessed",
	}, s.handleListServers)

	// Register get_resource_data tool
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_resource_data",
		Description: "Fetch data from a specific Redfish resource. Returns raw Redfish JSON with HTTP headers for any valid resource URL.",
	}, s.handleGetResourceData)

	// Register get_system_health tool
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_system_health",
		Description: "Get system health summary including power state, CPU, memory, and component rollup statuses. Returns model, power state, overall health, CPU/memory summaries, and Dell OEM rollup statuses.",
	}, s.handleGetSystemHealth)

	// Register get_event_log tool
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_event_log",
		Description: "Get iDRAC System Event Log entries for hardware event investigation. Returns entries with Id, Created timestamp, Message, Severity, and MessageId. Accepts optional count parameter (default 50, max 200).",
	}, s.handleGetEventLog)

	// Register get_thermal_data tool
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_thermal_data",
		Description: "Get thermal data including temperatures and fan speeds. Returns temperature sensor readings (name, celsius, health) and fan readings (name, RPM/percent, health).",
	}, s.handleGetThermalData)

	// Register get_power_data tool
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_power_data",
		Description: "Get power supply status and power consumption data. Returns power supply details (name, output watts, health, input voltage) and power control (consumed watts, capacity watts).",
	}, s.handleGetPowerData)

	// Register get_storage_data tool
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_storage_data",
		Description: "Get storage controller and drive health data including predictive failure indicators. Returns controllers (name, health, RAID types) and drives (name, capacity, media type, health, predicted life remaining).",
	}, s.handleGetStorageData)

	// Register get_network_interfaces tool
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_network_interfaces",
		Description: "Get network interface status, MAC addresses, and link speeds. Returns each interface's Id, name, MAC address, speed in Mbps, health, link status, and IPv4 addresses.",
	}, s.handleGetNetworkInterfaces)

	// Register get_memory_data tool
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_memory_data",
		Description: "Get memory DIMM details including health status and ECC information. Returns each DIMM's name, capacity in MiB, device type, operating speed in MHz, health, and error correction type.",
	}, s.handleGetMemoryData)

	// Register get_firmware_inventory tool
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "get_firmware_inventory",
		Description: "Get firmware versions for BIOS, iDRAC, NIC, and other components. Returns each component's Id, name, version, and whether it is updateable.",
	}, s.handleGetFirmwareInventory)

	// Register discover_resources tool
	mcp.AddTool(s.mcpServer, &mcp.Tool{
		Name:        "discover_resources",
		Description: "Discover available Redfish API resource endpoints on the server. Returns all top-level resource links (Systems, Chassis, Managers, etc.) with their @odata.id paths.",
	}, s.handleDiscoverResources)

	// Register write/mutation tools (gated by REDFISH_READ_ONLY)
	s.registerWriteTools()

	s.logger.Info("MCP tools registered successfully", "count", 14)
	return nil
}

// ListServersOutput represents the output for the list_servers tool
type ListServersOutput struct {
	Servers []string `json:"servers"`
}

// handleListServers handles the list_servers tool
func (s *Server) handleListServers(ctx context.Context, req *mcp.CallToolRequest, input struct{}) (*mcp.CallToolResult, ListServersOutput, error) {
	s.logger.Info("Handling list_servers request")

	addresses := s.hostManager.GetAddresses()

	return nil, ListServersOutput{Servers: addresses}, nil
}

// getClient returns a cached, authenticated Redfish client for serverAddr.
// If no client exists yet it creates and logs in a new one.
//
// On a 401 response the caller should pass the client it received before as
// staleClient so getClient can determine whether another goroutine has already
// refreshed the session. If staleClient is nil this is a normal get-or-create.
//
// The mutex is NOT held during network I/O (Close / Login) to avoid blocking
// other goroutines while waiting for the iDRAC to respond.
func (s *Server) getClient(serverAddr string, staleClient *redfish.Client) (*redfish.Client, error) {
	// Fast path: return the cached client when no refresh is needed.
	if staleClient == nil {
		s.mu.Lock()
		existing, ok := s.clients[serverAddr]
		s.mu.Unlock()
		if ok {
			return existing, nil
		}
	} else {
		// Refresh path: check whether another goroutine already replaced the
		// stale session while we were waiting to enter this function.
		s.mu.Lock()
		current, ok := s.clients[serverAddr]
		s.mu.Unlock()
		if ok && current != staleClient {
			// Another goroutine already refreshed — return the new client.
			return current, nil
		}
	}

	// Need to create (or re-create) a client. Resolve config outside the lock.
	hostConfig, found := s.hostManager.GetHostByAddress(serverAddr)
	if !found {
		return nil, fmt.Errorf("server %s not found in configuration", serverAddr)
	}

	clientConfig := s.createClientConfig(hostConfig)
	newClient := redfish.NewClient(clientConfig, s.logger)

	// Close the stale connection outside the lock — this is a network call.
	if staleClient != nil {
		if err := staleClient.Close(); err != nil {
			s.logger.Warn("Error closing stale Redfish client", "server", serverAddr, "error", err)
		}
	}

	// Login is also a network call — do it outside the lock.
	if err := newClient.Login(); err != nil {
		return nil, fmt.Errorf("failed to login to Redfish server %s: %w", serverAddr, err)
	}

	// Re-acquire the lock to store the new client.
	// If another goroutine raced us here and already stored a fresh client,
	// prefer theirs and discard ours to avoid leaking a duplicate session.
	s.mu.Lock()
	if current, ok := s.clients[serverAddr]; ok && current != staleClient {
		s.mu.Unlock()
		// Another goroutine won the race — close the client we just created.
		if err := newClient.Close(); err != nil {
			s.logger.Warn("Error closing redundant Redfish client", "server", serverAddr, "error", err)
		}
		return current, nil
	}
	s.clients[serverAddr] = newClient
	s.mu.Unlock()

	s.logger.Info("Redfish session established", "server", serverAddr)
	return newClient, nil
}

// Close logs out all cached Redfish sessions, freeing iDRAC session slots.
// The map is cleared under the lock, then each client is logged out without
// holding the lock so network I/O does not block other goroutines.
func (s *Server) Close() {
	s.mu.Lock()
	snapshot := s.clients
	s.clients = make(map[string]*redfish.Client)
	s.mu.Unlock()

	for addr, client := range snapshot {
		if err := client.Close(); err != nil {
			s.logger.Warn("Error closing Redfish client", "server", addr, "error", err)
		} else {
			s.logger.Info("Redfish session closed", "server", addr)
		}
	}
}

// handleGetResourceData handles the get_resource_data tool
func (s *Server) handleGetResourceData(ctx context.Context, req *mcp.CallToolRequest, input GetResourceInput) (*mcp.CallToolResult, GetResourceOutput, error) {
	s.logger.Info("Handling get_resource_data request")

	// Parse the URL to extract server address and resource path
	serverAddr, resourcePath, err := s.parseRedfishURL(input.URL)
	if err != nil {
		return nil, GetResourceOutput{}, fmt.Errorf("invalid Redfish URL: %w", err)
	}

	// Reuse a cached session to avoid exhausting iDRAC session slots.
	client, err := s.getClient(serverAddr, nil)
	if err != nil {
		return nil, GetResourceOutput{}, err
	}

	// Get resource data with headers.
	response, err := client.GetWithHeaders(resourcePath)
	if err != nil {
		// On 401, the session token expired — re-login once and retry.
		// Pass the stale client so getClient can detect a concurrent refresh.
		if rfErr, ok := err.(*redfish.RedfishError); ok && rfErr.Code == 401 {
			s.logger.Warn("Session expired, re-authenticating", "server", serverAddr)
			client, err = s.getClient(serverAddr, client)
			if err != nil {
				return nil, GetResourceOutput{}, fmt.Errorf("re-login failed: %w", err)
			}
			response, err = client.GetWithHeaders(resourcePath)
		}
		if err != nil {
			return nil, GetResourceOutput{}, fmt.Errorf("failed to get resource data: %w", err)
		}
	}

	return nil, GetResourceOutput{
		Headers: response.Headers,
		Data:    response.Data,
	}, nil
}

// parseRedfishURL parses a Redfish URL to extract server address and resource path
func (s *Server) parseRedfishURL(url string) (string, string, error) {
	// This is a simplified parser - in production, use proper URL parsing
	// Expected format: https://server:port/redfish/v1/resource/path

	if len(url) < 8 || url[:8] != "https://" {
		return "", "", fmt.Errorf("URL must use HTTPS")
	}

	// Remove https:// prefix
	withoutScheme := url[8:]

	// Find the first / after the host
	hostEnd := -1
	for i, char := range withoutScheme {
		if char == '/' {
			hostEnd = i
			break
		}
	}

	if hostEnd == -1 {
		return "", "", fmt.Errorf("invalid URL format")
	}

	serverAddr := withoutScheme[:hostEnd]
	resourcePath := withoutScheme[hostEnd:]

	// Basic validation
	if serverAddr == "" {
		return "", "", fmt.Errorf("empty server address")
	}

	if resourcePath == "" {
		resourcePath = "/"
	}

	return serverAddr, resourcePath, nil
}

// createClientConfig creates a Redfish client config from host config
func (s *Server) createClientConfig(hostConfig config.HostConfig) *redfish.ClientConfig {
	config := redfish.DefaultClientConfig()

	config.Address = hostConfig.Address
	if hostConfig.Port != 0 {
		config.Port = hostConfig.Port
	} else {
		config.Port = s.config.Redfish.Port
	}

	config.Username = hostConfig.Username
	if config.Username == "" {
		config.Username = s.config.Redfish.Username
	}

	config.Password = hostConfig.Password
	if config.Password == "" {
		config.Password = s.config.Redfish.Password
	}

	config.AuthMethod = redfish.AuthMethod(hostConfig.AuthMethod)
	if config.AuthMethod == "" {
		config.AuthMethod = redfish.AuthMethod(s.config.Redfish.AuthMethod)
	}

	config.TLSServerCACert = hostConfig.TLSServerCACert
	if config.TLSServerCACert == "" {
		config.TLSServerCACert = s.config.Redfish.TLSServerCACert
	}

	config.InsecureSkipVerify = s.config.Redfish.InsecureSkipVerify

	return config
}

// Start starts the MCP server with the specified transport
func (s *Server) Start(ctx context.Context) error {
	s.logger.Info("Starting Redfish MCP server",
		"transport", s.config.MCP.Transport)

	// For now, we'll implement stdio transport
	// Other transports can be added later
	switch s.config.MCP.Transport {
	case config.MCPTransportStdio:
		return s.startStdio(ctx)
	case config.MCPTransportSSE:
		return s.startSSE(ctx)
	case config.MCPTransportStreamableHTTP:
		return s.startStreamableHTTP(ctx)
	default:
		return fmt.Errorf("unsupported transport: %s", s.config.MCP.Transport)
	}
}

// startStdio starts the server with stdio transport
func (s *Server) startStdio(ctx context.Context) error {
	transport := &mcp.StdioTransport{}
	return s.mcpServer.Run(ctx, transport)
}

// startSSE starts the server with SSE (Server-Sent Events) HTTP transport.
// The server listens on the configured MCP port and serves SSE connections,
// allowing Context Forge or other MCP clients to connect directly without
// an intermediate agentgateway process.
func (s *Server) startSSE(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.config.MCP.Port)

	handler := mcp.NewSSEHandler(func(request *http.Request) *mcp.Server {
		return s.mcpServer
	}, nil)

	// Mount on /sse so Context Forge (and other MCP clients) can connect
	// at the standard path. NewSSEHandler is a plain http.Handler — it
	// serves on whatever path it is registered under.
	mux := http.NewServeMux()
	mux.Handle("/sse", handler)
	mux.Handle("/sse/", handler)

	// Smart readiness probe: checks iDRAC reachability when a host is configured.
	mux.HandleFunc("/healthz", s.handleHealthz)

	// Prometheus metrics endpoint.
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Shut down gracefully when the context is cancelled.
	// Use Shutdown (not Close) to let active SSE connections drain.
	go func() {
		<-ctx.Done()
		s.logger.Info("Shutting down SSE server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			s.logger.Warn("SSE server shutdown error, forcing close", "error", err)
			srv.Close()
		}
	}()

	s.logger.Info("SSE server listening", "addr", addr, "path", "/sse")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("SSE server failed: %w", err)
	}
	return nil
}

// startStreamableHTTP starts the server with Streamable HTTP transport.
// This is the recommended transport per MCP spec v2025-03-26, replacing SSE.
// Clients POST JSON-RPC to /mcp and receive responses as application/json
// or text/event-stream (for streaming).
func (s *Server) startStreamableHTTP(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.config.MCP.Port)

	handler := mcp.NewStreamableHTTPHandler(func(request *http.Request) *mcp.Server {
		return s.mcpServer
	}, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.Handle("/mcp/", handler)

	// Smart readiness probe: checks iDRAC reachability.
	mux.HandleFunc("/healthz", s.handleHealthz)

	// Prometheus metrics endpoint.
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		s.logger.Info("Shutting down Streamable HTTP server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			s.logger.Warn("Streamable HTTP server shutdown error, forcing close", "error", err)
			srv.Close()
		}
	}()

	s.logger.Info("Streamable HTTP server listening", "addr", addr, "path", "/mcp")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("streamable HTTP server failed: %w", err)
	}
	return nil
}

// handleHealthz is a smart readiness probe. If at least one Redfish host is
// configured, it performs a quick GET to /redfish/v1/ on the first host. If
// that fails, it returns 503 so K8s stops routing traffic to the pod.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	addrs := s.hostManager.GetAddresses()
	if len(addrs) == 0 {
		// No hosts configured — server is healthy but has nothing to probe.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok (no hosts configured)"))
		return
	}

	// Quick connectivity check — use a short-lived HTTP client with a tight
	// timeout so the readiness probe does not block for 30 seconds.
	addr := addrs[0]
	hostCfg, found := s.hostManager.GetHostByAddress(addr)
	if !found {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	}

	port := hostCfg.Port
	if port == 0 {
		port = s.config.Redfish.Port
	}

	probeURL := fmt.Sprintf("https://%s:%d/redfish/v1/", addr, port)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: s.config.Redfish.InsecureSkipVerify,
			},
		},
	}
	resp, err := client.Get(probeURL)
	if err != nil {
		s.logger.Warn("Healthz probe failed", "server", addr, "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(fmt.Sprintf("iDRAC unreachable: %v", err)))
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 500 {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(fmt.Sprintf("iDRAC returned %d", resp.StatusCode)))
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// GetMCPServer returns the underlying MCP server
func (s *Server) GetMCPServer() *mcp.Server {
	return s.mcpServer
}
