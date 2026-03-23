package redfish

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/avast/retry-go"
)

// Client represents a Redfish HTTP client
type Client struct {
	config       *ClientConfig
	baseURL      string
	httpClient   *http.Client
	sessionToken string
	sessionURL   string // full URL of the session resource, used for DELETE on logout
	logger       *slog.Logger
}

// NewClient creates a new Redfish client
func NewClient(config *ClientConfig, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}

	// Create HTTP client with TLS configuration
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: config.InsecureSkipVerify,
	}

	if config.TLSServerCACert != "" {
		// TODO: Load custom CA certificate
		logger.Warn("Custom CA certificate support not yet implemented")
	}

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}

	baseURL := fmt.Sprintf("https://%s:%d", config.Address, config.Port)

	return &Client{
		config:     config,
		baseURL:    baseURL,
		httpClient: httpClient,
		logger:     logger,
	}
}

// Login authenticates with the Redfish service
func (c *Client) Login() error {
	switch c.config.AuthMethod {
	case AuthMethodBasic:
		return c.loginBasic()
	case AuthMethodSession:
		return c.loginSession()
	default:
		return fmt.Errorf("unsupported auth method: %s", c.config.AuthMethod)
	}
}

// loginBasic performs basic authentication
func (c *Client) loginBasic() error {
	// Basic auth is handled per-request, no session setup needed
	c.logger.Info("Using basic authentication")
	return nil
}

// loginSession performs session-based authentication
func (c *Client) loginSession() error {
	sessionURL := c.baseURL + "/redfish/v1/SessionService/Sessions"

	loginData := map[string]interface{}{
		"UserName": c.config.Username,
		"Password": c.config.Password,
	}

	jsonData, err := json.Marshal(loginData)
	if err != nil {
		return fmt.Errorf("failed to marshal login data: %w", err)
	}

	req, err := http.NewRequest("POST", sessionURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create login request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return &RedfishError{
			Message: fmt.Sprintf("login failed with status %d: %s", resp.StatusCode, string(body)),
			Code:    resp.StatusCode,
		}
	}

	// Extract session token from response
	var sessionResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&sessionResp); err != nil {
		return fmt.Errorf("failed to decode session response: %w", err)
	}

	// Capture the session resource URL so we can DELETE it on logout.
	// iDRAC returns the URL in the Location header (preferred) or @odata.id body field.
	if loc := resp.Header.Get("Location"); loc != "" {
		// Location may be a relative path — normalise to full URL.
		if strings.HasPrefix(loc, "/") {
			c.sessionURL = c.baseURL + loc
		} else {
			c.sessionURL = loc
		}
	} else if odataID, ok := sessionResp["@odata.id"].(string); ok && odataID != "" {
		c.sessionURL = c.baseURL + odataID
	}

	// Extract X-Auth-Token from response headers
	if token := resp.Header.Get("X-Auth-Token"); token != "" {
		c.sessionToken = token
		c.logger.Info("Session authentication successful", "sessionURL", c.sessionURL)
		return nil
	}

	// Fallback: try to extract from response body
	if token, ok := sessionResp["token"].(string); ok {
		c.sessionToken = token
		c.logger.Info("Session authentication successful", "sessionURL", c.sessionURL)
		return nil
	}

	return fmt.Errorf("no session token found in response")
}

// Logout ends the session by sending HTTP DELETE to the session resource URL.
// iDRAC limits concurrent sessions to 4-6; failing to DELETE causes RAC0218 exhaustion.
// Session fields are only cleared on a successful DELETE; errors are returned to the
// caller so they can decide whether to retry or surface the leak.
func (c *Client) Logout() error {
	if c.sessionToken == "" {
		return nil // No active session
	}

	// For basic auth there is no server-side session to delete.
	if c.config.AuthMethod == AuthMethodBasic {
		c.sessionToken = ""
		return nil
	}

	if c.sessionURL == "" {
		c.logger.Warn("No session URL captured; cannot DELETE session — iDRAC slot may leak")
		// Clear local state even though we cannot free the server-side slot.
		c.sessionToken = ""
		return fmt.Errorf("logout failed: no session URL available — iDRAC session slot may leak")
	}

	// Send DELETE to the session URL to free the iDRAC session slot.
	req, err := http.NewRequest("DELETE", c.sessionURL, nil)
	if err != nil {
		// Do NOT clear state — the server-side session is still live.
		return fmt.Errorf("logout failed: could not create DELETE request: %w", err)
	}

	req.Header.Set("X-Auth-Token", c.sessionToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Do NOT clear state — we cannot confirm the session was deleted.
		return fmt.Errorf("logout DELETE request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		// Do NOT clear state — the DELETE was rejected by the server.
		c.logger.Warn("Logout returned non-success status",
			"status", resp.StatusCode, "sessionURL", c.sessionURL)
		return &RedfishError{
			Message: fmt.Sprintf("logout DELETE returned HTTP %d: %s", resp.StatusCode, string(body)),
			Code:    resp.StatusCode,
		}
	}

	// DELETE succeeded — safe to clear local session state.
	c.logger.Info("Session deleted successfully", "sessionURL", c.sessionURL)
	c.sessionToken = ""
	c.sessionURL = ""
	return nil
}

// Get performs a GET request to the Redfish API
func (c *Client) Get(resourcePath string) (*RedfishResponse, error) {
	return c.request("GET", resourcePath, nil)
}

// Post performs a POST request to the Redfish API
func (c *Client) Post(resourcePath string, data interface{}) (*RedfishResponse, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request data: %w", err)
	}
	return c.request("POST", resourcePath, jsonData)
}

// Patch performs a PATCH request to the Redfish API
func (c *Client) Patch(resourcePath string, data interface{}) (*RedfishResponse, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request data: %w", err)
	}
	return c.request("PATCH", resourcePath, jsonData)
}

// Delete performs a DELETE request to the Redfish API
func (c *Client) Delete(resourcePath string) (*RedfishResponse, error) {
	return c.request("DELETE", resourcePath, nil)
}

// GetWithHeaders performs a GET request and returns both data and headers
func (c *Client) GetWithHeaders(resourcePath string) (*RedfishResponse, error) {
	resp, err := c.request("GET", resourcePath, nil)
	if err != nil {
		return nil, err
	}

	// Extract specific headers we're interested in
	headers := make(map[string][]string)
	targetHeaders := map[string]string{
		"allow":            "Allow",
		"content-type":     "Content-Type",
		"content-encoding": "Content-Encoding",
		"etag":             "ETag",
		"link":             "Link",
	}

	for headerName, standardName := range targetHeaders {
		if values := resp.Headers[headerName]; len(values) > 0 {
			headers[standardName] = values
		}
	}

	resp.Headers = headers
	return resp, nil
}

// PostJSON sends a POST request with a JSON body and returns the response with headers.
// It follows the same pattern as GetWithHeaders — auth headers are included automatically,
// Content-Type is set to application/json, and non-2xx responses return a RedfishError.
func (c *Client) PostJSON(path string, body interface{}) (*RedfishResponse, error) {
	jsonData, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}
	return c.request("POST", path, jsonData)
}

// PatchJSON sends a PATCH request with a JSON body and returns the response with headers.
// It follows the same pattern as GetWithHeaders — auth headers are included automatically,
// Content-Type is set to application/json, and non-2xx responses return a RedfishError.
func (c *Client) PatchJSON(path string, body interface{}) (*RedfishResponse, error) {
	jsonData, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}
	return c.request("PATCH", path, jsonData)
}

// request performs an HTTP request with retry logic
func (c *Client) request(method, resourcePath string, body []byte) (*RedfishResponse, error) {
	var lastResp *RedfishResponse
	var lastErr error

	retryConfig := []retry.Option{
		retry.Attempts(uint(c.config.MaxRetries + 1)), // +1 because Attempts includes initial attempt
		retry.Delay(c.config.InitialDelay),
		retry.MaxDelay(c.config.MaxDelay),
		retry.DelayType(retry.BackOffDelay),
		retry.RetryIf(func(err error) bool {
			return IsRetryable(err)
		}),
		retry.OnRetry(func(n uint, err error) {
			c.logger.Warn("Redfish request failed, retrying",
				"attempt", n+1,
				"error", err)
		}),
	}

	err := retry.Do(
		func() error {
			resp, err := c.doRequest(method, resourcePath, body)
			if err != nil {
				lastErr = err
				return err
			}
			lastResp = resp
			return nil
		},
		retryConfig...,
	)

	if err != nil {
		return nil, lastErr
	}

	return lastResp, nil
}

// doRequest performs a single HTTP request
func (c *Client) doRequest(method, resourcePath string, body []byte) (*RedfishResponse, error) {
	fullURL := c.baseURL + resourcePath
	if !strings.HasPrefix(resourcePath, "/") {
		fullURL = c.baseURL + "/" + resourcePath
	}

	c.logger.Debug("Making Redfish request",
		"method", method,
		"url", fullURL)

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, fullURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	// Add authentication
	if err := c.addAuthHeaders(req); err != nil {
		return nil, fmt.Errorf("failed to add auth headers: %w", err)
	}

	// Make the request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &RedfishError{
			Message: fmt.Sprintf("HTTP request failed: %v", err),
			Code:    0, // Network error
		}
	}
	defer resp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Parse JSON response
	var data interface{}
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &data); err != nil {
			c.logger.Warn("Failed to parse JSON response, returning raw body",
				"error", err)
			data = string(respBody)
		}
	}

	// Check for HTTP errors
	if resp.StatusCode >= 400 {
		return nil, &RedfishError{
			Message: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(respBody)),
			Code:    resp.StatusCode,
		}
	}

	return &RedfishResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		Data:       data,
	}, nil
}

// addAuthHeaders adds authentication headers to the request
func (c *Client) addAuthHeaders(req *http.Request) error {
	switch c.config.AuthMethod {
	case AuthMethodBasic:
		if c.config.Username != "" && c.config.Password != "" {
			req.SetBasicAuth(c.config.Username, c.config.Password)
		}
	case AuthMethodSession:
		if c.sessionToken != "" {
			req.Header.Set("X-Auth-Token", c.sessionToken)
		}
	}
	return nil
}

// Close closes the client and cleans up resources
func (c *Client) Close() error {
	err := c.Logout()
	if c.httpClient != nil {
		c.httpClient.CloseIdleConnections()
	}
	return err
}
