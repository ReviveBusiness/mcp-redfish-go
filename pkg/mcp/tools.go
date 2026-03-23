package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/theoriginalaiexplorer/mcp-redfish-go/pkg/redfish"
)

// ---------------------------------------------------------------------------
// Prometheus metrics
// ---------------------------------------------------------------------------

// Prometheus metrics are registered via promauto to avoid duplicate registration
// panics (e.g. when tests import this package multiple times).
var (
	toolCallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "redfish_mcp_tool_calls_total",
		Help: "Total number of MCP tool calls",
	}, []string{"tool", "status"})

	toolDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "redfish_mcp_tool_duration_seconds",
		Help:    "Duration of MCP tool calls in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"tool"})
)

// observeTool records metrics for a tool call.
func observeTool(tool string, start time.Time, err error) {
	status := "ok"
	if err != nil {
		status = "error"
	}
	toolCallsTotal.WithLabelValues(tool, status).Inc()
	toolDurationSeconds.WithLabelValues(tool).Observe(time.Since(start).Seconds())
}

// ---------------------------------------------------------------------------
// Helper: resolve server address and get a Redfish client with 401 retry
// ---------------------------------------------------------------------------

// resolveServer returns the first configured server address, or the caller-
// supplied override if non-empty.
func (s *Server) resolveServer(override string) string {
	if override != "" {
		return override
	}
	addrs := s.hostManager.GetAddresses()
	if len(addrs) == 0 {
		return ""
	}
	return addrs[0]
}

// fetchResource is a convenience wrapper: GET path with 401 retry.
// It delegates to withRetryOn401 to avoid duplicating the retry pattern.
func (s *Server) fetchResource(serverAddr, path string) (*redfish.RedfishResponse, error) {
	return s.withRetryOn401(serverAddr, func(c *redfish.Client) (*redfish.RedfishResponse, error) {
		return c.GetWithHeaders(path)
	})
}

// mapGet safely navigates a nested map[string]interface{}.
func mapGet(m map[string]interface{}, keys ...string) interface{} {
	var cur interface{} = m
	for _, k := range keys {
		mm, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func mapStr(m map[string]interface{}, keys ...string) string {
	v, _ := mapGet(m, keys...).(string)
	return v
}

func mapFloat(m map[string]interface{}, keys ...string) float64 {
	v, _ := mapGet(m, keys...).(float64)
	return v
}

// collectMembers fetches a Redfish collection and returns each member's data.
func (s *Server) collectMembers(serverAddr, collectionPath string) ([]map[string]interface{}, error) {
	resp, err := s.fetchResource(serverAddr, collectionPath)
	if err != nil {
		return nil, err
	}
	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected collection format")
	}
	members, _ := data["Members"].([]interface{})

	var results []map[string]interface{}
	for _, m := range members {
		mObj, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		link, _ := mObj["@odata.id"].(string)
		if link == "" {
			continue
		}
		memberResp, err := s.fetchResource(serverAddr, link)
		if err != nil {
			s.logger.Warn("Failed to fetch member", "path", link, "error", err)
			continue
		}
		mData, ok := memberResp.Data.(map[string]interface{})
		if !ok {
			continue
		}
		results = append(results, mData)
	}
	return results, nil
}

// ---------------------------------------------------------------------------
// Input / output types
// ---------------------------------------------------------------------------

// ServerAddressInput is embedded in tools that optionally accept a server.
type ServerAddressInput struct {
	ServerAddress string `json:"server_address,omitempty" jsonschema:"Server address (optional, uses first configured server if omitted)"`
}

// -- get_system_health -------------------------------------------------------

type GetSystemHealthInput struct {
	ServerAddressInput
}

type ComponentStatus struct {
	Name         string `json:"name"`
	Health       string `json:"health"`
	HealthRollup string `json:"health_rollup,omitempty"`
}

type GetSystemHealthOutput struct {
	Model           string            `json:"model"`
	PowerState      string            `json:"power_state"`
	Health          string            `json:"health"`
	HealthRollup    string            `json:"health_rollup"`
	CPUSummary      map[string]interface{} `json:"cpu_summary"`
	MemorySummary   map[string]interface{} `json:"memory_summary"`
	RollupStatuses  []ComponentStatus `json:"rollup_statuses,omitempty"`
}

// -- get_event_log -----------------------------------------------------------

type GetEventLogInput struct {
	ServerAddressInput
	Count int `json:"count,omitempty" jsonschema:"Number of log entries to return (default 50, max 200)"`
}

type EventLogEntry struct {
	Id        string `json:"id"`
	Created   string `json:"created"`
	Message   string `json:"message"`
	Severity  string `json:"severity"`
	MessageId string `json:"message_id"`
}

type GetEventLogOutput struct {
	Entries []EventLogEntry `json:"entries"`
	Total   int             `json:"total"`
}

// -- get_thermal_data --------------------------------------------------------

type GetThermalInput struct {
	ServerAddressInput
}

type TemperatureReading struct {
	Name            string `json:"name"`
	ReadingCelsius  float64 `json:"reading_celsius"`
	Health          string `json:"health"`
}

type FanReading struct {
	Name         string  `json:"name"`
	Reading      float64 `json:"reading"`
	ReadingUnits string  `json:"reading_units"`
	Health       string  `json:"health"`
}

type GetThermalOutput struct {
	Temperatures []TemperatureReading `json:"temperatures"`
	Fans         []FanReading         `json:"fans"`
}

// -- get_power_data ----------------------------------------------------------

type GetPowerInput struct {
	ServerAddressInput
}

type PowerSupplyInfo struct {
	Name             string  `json:"name"`
	PowerOutputWatts float64 `json:"power_output_watts"`
	Health           string  `json:"health"`
	LineInputVoltage float64 `json:"line_input_voltage"`
}

type PowerControlInfo struct {
	PowerConsumedWatts  float64 `json:"power_consumed_watts"`
	PowerCapacityWatts  float64 `json:"power_capacity_watts"`
}

type GetPowerOutput struct {
	PowerSupplies []PowerSupplyInfo  `json:"power_supplies"`
	PowerControl  []PowerControlInfo `json:"power_control"`
}

// -- get_storage_data --------------------------------------------------------

type GetStorageInput struct {
	ServerAddressInput
}

type DriveInfo struct {
	Name                         string  `json:"name"`
	CapacityBytes                float64 `json:"capacity_bytes"`
	MediaType                    string  `json:"media_type"`
	Health                       string  `json:"health"`
	PredictedMediaLifeLeftPercent float64 `json:"predicted_media_life_left_percent,omitempty"`
}

type ControllerInfo struct {
	Name               string   `json:"name"`
	Health             string   `json:"health"`
	SupportedRAIDTypes []string `json:"supported_raid_types,omitempty"`
}

type GetStorageOutput struct {
	Controllers []ControllerInfo `json:"controllers"`
	Drives      []DriveInfo      `json:"drives"`
}

// -- get_network_interfaces --------------------------------------------------

type GetNetworkInterfacesInput struct {
	ServerAddressInput
}

type NetworkInterfaceInfo struct {
	Id            string                 `json:"id"`
	Name          string                 `json:"name"`
	MACAddress    string                 `json:"mac_address"`
	SpeedMbps     float64                `json:"speed_mbps"`
	Health        string                 `json:"health"`
	LinkStatus    string                 `json:"link_status"`
	IPv4Addresses []map[string]interface{} `json:"ipv4_addresses,omitempty"`
}

type GetNetworkInterfacesOutput struct {
	Interfaces []NetworkInterfaceInfo `json:"interfaces"`
}

// -- get_memory_data ---------------------------------------------------------

type GetMemoryInput struct {
	ServerAddressInput
}

type MemoryDIMMInfo struct {
	Name               string  `json:"name"`
	CapacityMiB        float64 `json:"capacity_mib"`
	MemoryDeviceType   string  `json:"memory_device_type"`
	OperatingSpeedMhz  float64 `json:"operating_speed_mhz"`
	Health             string  `json:"health"`
	ErrorCorrection    string  `json:"error_correction"`
}

type GetMemoryOutput struct {
	DIMMs []MemoryDIMMInfo `json:"dimms"`
}

// -- get_firmware_inventory --------------------------------------------------

type GetFirmwareInventoryInput struct {
	ServerAddressInput
}

type FirmwareComponent struct {
	Id         string `json:"id"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Updateable bool   `json:"updateable"`
}

type GetFirmwareInventoryOutput struct {
	Components []FirmwareComponent `json:"components"`
}

// -- discover_resources ------------------------------------------------------

type DiscoverResourcesInput struct {
	ServerAddressInput
}

type ResourceLink struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type DiscoverResourcesOutput struct {
	Links []ResourceLink `json:"links"`
}

// ---------------------------------------------------------------------------
// Handler implementations
// ---------------------------------------------------------------------------

func (s *Server) handleGetSystemHealth(ctx context.Context, req *mcp.CallToolRequest, input GetSystemHealthInput) (*mcp.CallToolResult, GetSystemHealthOutput, error) {
	start := time.Now()
	s.logger.Info("Handling get_system_health request")

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		err := fmt.Errorf("no server configured")
		observeTool("get_system_health", start, err)
		return nil, GetSystemHealthOutput{}, err
	}

	resp, err := s.fetchResource(serverAddr, "/redfish/v1/Systems/System.Embedded.1")
	if err != nil {
		observeTool("get_system_health", start, err)
		return nil, GetSystemHealthOutput{}, fmt.Errorf("failed to get system data: %w", err)
	}

	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		err := fmt.Errorf("unexpected response format")
		observeTool("get_system_health", start, err)
		return nil, GetSystemHealthOutput{}, err
	}

	status, _ := data["Status"].(map[string]interface{})
	cpuSummary, _ := data["ProcessorSummary"].(map[string]interface{})
	memSummary, _ := data["MemorySummary"].(map[string]interface{})

	out := GetSystemHealthOutput{
		Model:         mapStr(data, "Model"),
		PowerState:    mapStr(data, "PowerState"),
		Health:        mapStr(status, "Health"),
		HealthRollup:  mapStr(status, "HealthRollup"),
		CPUSummary:    cpuSummary,
		MemorySummary: memSummary,
	}

	// Extract Dell OEM rollup statuses if present
	if oem, ok := data["Oem"].(map[string]interface{}); ok {
		if dell, ok := oem["Dell"].(map[string]interface{}); ok {
			if dellSystem, ok := dell["DellSystem"].(map[string]interface{}); ok {
				rollupFields := []string{
					"BatteryRollupStatus", "CPURollupStatus", "FanRollupStatus",
					"IntrusionRollupStatus", "LicensingRollupStatus", "MemoryRollupStatus",
					"PSURollupStatus", "StorageRollupStatus", "TempRollupStatus",
					"VoltRollupStatus",
				}
				for _, field := range rollupFields {
					if val, ok := dellSystem[field].(string); ok && val != "" {
						out.RollupStatuses = append(out.RollupStatuses, ComponentStatus{
							Name:   field,
							Health: val,
						})
					}
				}
			}
		}
	}

	observeTool("get_system_health", start, nil)
	return nil, out, nil
}

func (s *Server) handleGetEventLog(ctx context.Context, req *mcp.CallToolRequest, input GetEventLogInput) (*mcp.CallToolResult, GetEventLogOutput, error) {
	start := time.Now()
	s.logger.Info("Handling get_event_log request")

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		err := fmt.Errorf("no server configured")
		observeTool("get_event_log", start, err)
		return nil, GetEventLogOutput{}, err
	}

	count := input.Count
	if count <= 0 {
		count = 50
	}
	if count > 200 {
		count = 200
	}

	// Use $top query parameter to limit entries
	path := fmt.Sprintf("/redfish/v1/Managers/iDRAC.Embedded.1/LogServices/Sel/Entries?$top=%d", count)
	resp, err := s.fetchResource(serverAddr, path)
	if err != nil {
		observeTool("get_event_log", start, err)
		return nil, GetEventLogOutput{}, fmt.Errorf("failed to get event log: %w", err)
	}

	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		err := fmt.Errorf("unexpected response format")
		observeTool("get_event_log", start, err)
		return nil, GetEventLogOutput{}, err
	}

	members, _ := data["Members"].([]interface{})
	totalCount := int(mapFloat(data, "Members@odata.count"))

	var entries []EventLogEntry
	for _, m := range members {
		entry, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		entries = append(entries, EventLogEntry{
			Id:        mapStr(entry, "Id"),
			Created:   mapStr(entry, "Created"),
			Message:   mapStr(entry, "Message"),
			Severity:  mapStr(entry, "Severity"),
			MessageId: mapStr(entry, "MessageId"),
		})
	}

	observeTool("get_event_log", start, nil)
	return nil, GetEventLogOutput{Entries: entries, Total: totalCount}, nil
}

func (s *Server) handleGetThermalData(ctx context.Context, req *mcp.CallToolRequest, input GetThermalInput) (*mcp.CallToolResult, GetThermalOutput, error) {
	start := time.Now()
	s.logger.Info("Handling get_thermal_data request")

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		err := fmt.Errorf("no server configured")
		observeTool("get_thermal_data", start, err)
		return nil, GetThermalOutput{}, err
	}

	resp, err := s.fetchResource(serverAddr, "/redfish/v1/Chassis/System.Embedded.1/Thermal")
	if err != nil {
		observeTool("get_thermal_data", start, err)
		return nil, GetThermalOutput{}, fmt.Errorf("failed to get thermal data: %w", err)
	}

	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		err := fmt.Errorf("unexpected response format")
		observeTool("get_thermal_data", start, err)
		return nil, GetThermalOutput{}, err
	}

	var temps []TemperatureReading
	if tempArr, ok := data["Temperatures"].([]interface{}); ok {
		for _, t := range tempArr {
			tm, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			temps = append(temps, TemperatureReading{
				Name:           mapStr(tm, "Name"),
				ReadingCelsius: mapFloat(tm, "ReadingCelsius"),
				Health:         mapStr(tm, "Status", "Health"),
			})
		}
	}

	var fans []FanReading
	if fanArr, ok := data["Fans"].([]interface{}); ok {
		for _, f := range fanArr {
			fm, ok := f.(map[string]interface{})
			if !ok {
				continue
			}
			fans = append(fans, FanReading{
				Name:         mapStr(fm, "Name"),
				Reading:      mapFloat(fm, "Reading"),
				ReadingUnits: mapStr(fm, "ReadingUnits"),
				Health:       mapStr(fm, "Status", "Health"),
			})
		}
	}

	observeTool("get_thermal_data", start, nil)
	return nil, GetThermalOutput{Temperatures: temps, Fans: fans}, nil
}

func (s *Server) handleGetPowerData(ctx context.Context, req *mcp.CallToolRequest, input GetPowerInput) (*mcp.CallToolResult, GetPowerOutput, error) {
	start := time.Now()
	s.logger.Info("Handling get_power_data request")

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		err := fmt.Errorf("no server configured")
		observeTool("get_power_data", start, err)
		return nil, GetPowerOutput{}, err
	}

	resp, err := s.fetchResource(serverAddr, "/redfish/v1/Chassis/System.Embedded.1/Power")
	if err != nil {
		observeTool("get_power_data", start, err)
		return nil, GetPowerOutput{}, fmt.Errorf("failed to get power data: %w", err)
	}

	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		err := fmt.Errorf("unexpected response format")
		observeTool("get_power_data", start, err)
		return nil, GetPowerOutput{}, err
	}

	var supplies []PowerSupplyInfo
	if psArr, ok := data["PowerSupplies"].([]interface{}); ok {
		for _, p := range psArr {
			pm, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			supplies = append(supplies, PowerSupplyInfo{
				Name:             mapStr(pm, "Name"),
				PowerOutputWatts: mapFloat(pm, "PowerOutputWatts"),
				Health:           mapStr(pm, "Status", "Health"),
				LineInputVoltage: mapFloat(pm, "LineInputVoltage"),
			})
		}
	}

	var controls []PowerControlInfo
	if pcArr, ok := data["PowerControl"].([]interface{}); ok {
		for _, p := range pcArr {
			pm, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			controls = append(controls, PowerControlInfo{
				PowerConsumedWatts: mapFloat(pm, "PowerConsumedWatts"),
				PowerCapacityWatts: mapFloat(pm, "PowerCapacityWatts"),
			})
		}
	}

	observeTool("get_power_data", start, nil)
	return nil, GetPowerOutput{PowerSupplies: supplies, PowerControl: controls}, nil
}

func (s *Server) handleGetStorageData(ctx context.Context, req *mcp.CallToolRequest, input GetStorageInput) (*mcp.CallToolResult, GetStorageOutput, error) {
	start := time.Now()
	s.logger.Info("Handling get_storage_data request")

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		err := fmt.Errorf("no server configured")
		observeTool("get_storage_data", start, err)
		return nil, GetStorageOutput{}, err
	}

	// Fetch storage collection
	resp, err := s.fetchResource(serverAddr, "/redfish/v1/Systems/System.Embedded.1/Storage")
	if err != nil {
		observeTool("get_storage_data", start, err)
		return nil, GetStorageOutput{}, fmt.Errorf("failed to get storage data: %w", err)
	}

	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		err := fmt.Errorf("unexpected response format")
		observeTool("get_storage_data", start, err)
		return nil, GetStorageOutput{}, err
	}

	members, _ := data["Members"].([]interface{})

	var controllers []ControllerInfo
	var drives []DriveInfo

	for _, m := range members {
		mObj, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		link, _ := mObj["@odata.id"].(string)
		if link == "" {
			continue
		}

		ctrlResp, err := s.fetchResource(serverAddr, link)
		if err != nil {
			s.logger.Warn("Failed to fetch storage controller", "path", link, "error", err)
			continue
		}
		ctrlData, ok := ctrlResp.Data.(map[string]interface{})
		if !ok {
			continue
		}

		// Extract supported RAID types
		var raidTypes []string
		if raids, ok := ctrlData["SupportedRAIDTypes"].([]interface{}); ok {
			for _, r := range raids {
				if rs, ok := r.(string); ok {
					raidTypes = append(raidTypes, rs)
				}
			}
		}
		// Also check StorageControllers array for RAID info
		if scArr, ok := ctrlData["StorageControllers"].([]interface{}); ok {
			for _, sc := range scArr {
				scm, ok := sc.(map[string]interface{})
				if !ok {
					continue
				}
				if raids, ok := scm["SupportedRAIDTypes"].([]interface{}); ok {
					for _, r := range raids {
						if rs, ok := r.(string); ok {
							raidTypes = append(raidTypes, rs)
						}
					}
				}
			}
		}

		controllers = append(controllers, ControllerInfo{
			Name:               mapStr(ctrlData, "Name"),
			Health:             mapStr(ctrlData, "Status", "Health"),
			SupportedRAIDTypes: raidTypes,
		})

		// Fetch drives for this controller
		if drivesArr, ok := ctrlData["Drives"].([]interface{}); ok {
			for _, d := range drivesArr {
				dm, ok := d.(map[string]interface{})
				if !ok {
					continue
				}
				driveLink, _ := dm["@odata.id"].(string)
				if driveLink == "" {
					continue
				}

				driveResp, err := s.fetchResource(serverAddr, driveLink)
				if err != nil {
					s.logger.Warn("Failed to fetch drive", "path", driveLink, "error", err)
					continue
				}
				driveData, ok := driveResp.Data.(map[string]interface{})
				if !ok {
					continue
				}

				drives = append(drives, DriveInfo{
					Name:                          mapStr(driveData, "Name"),
					CapacityBytes:                 mapFloat(driveData, "CapacityBytes"),
					MediaType:                     mapStr(driveData, "MediaType"),
					Health:                        mapStr(driveData, "Status", "Health"),
					PredictedMediaLifeLeftPercent: mapFloat(driveData, "PredictedMediaLifeLeftPercent"),
				})
			}
		}
	}

	observeTool("get_storage_data", start, nil)
	return nil, GetStorageOutput{Controllers: controllers, Drives: drives}, nil
}

func (s *Server) handleGetNetworkInterfaces(ctx context.Context, req *mcp.CallToolRequest, input GetNetworkInterfacesInput) (*mcp.CallToolResult, GetNetworkInterfacesOutput, error) {
	start := time.Now()
	s.logger.Info("Handling get_network_interfaces request")

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		err := fmt.Errorf("no server configured")
		observeTool("get_network_interfaces", start, err)
		return nil, GetNetworkInterfacesOutput{}, err
	}

	members, err := s.collectMembers(serverAddr, "/redfish/v1/Systems/System.Embedded.1/EthernetInterfaces")
	if err != nil {
		observeTool("get_network_interfaces", start, err)
		return nil, GetNetworkInterfacesOutput{}, fmt.Errorf("failed to get network interfaces: %w", err)
	}

	var ifaces []NetworkInterfaceInfo
	for _, m := range members {
		var ipv4 []map[string]interface{}
		if addrs, ok := m["IPv4Addresses"].([]interface{}); ok {
			for _, a := range addrs {
				if am, ok := a.(map[string]interface{}); ok {
					ipv4 = append(ipv4, am)
				}
			}
		}

		ifaces = append(ifaces, NetworkInterfaceInfo{
			Id:            mapStr(m, "Id"),
			Name:          mapStr(m, "Name"),
			MACAddress:    mapStr(m, "MACAddress"),
			SpeedMbps:     mapFloat(m, "SpeedMbps"),
			Health:        mapStr(m, "Status", "Health"),
			LinkStatus:    mapStr(m, "LinkStatus"),
			IPv4Addresses: ipv4,
		})
	}

	observeTool("get_network_interfaces", start, nil)
	return nil, GetNetworkInterfacesOutput{Interfaces: ifaces}, nil
}

func (s *Server) handleGetMemoryData(ctx context.Context, req *mcp.CallToolRequest, input GetMemoryInput) (*mcp.CallToolResult, GetMemoryOutput, error) {
	start := time.Now()
	s.logger.Info("Handling get_memory_data request")

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		err := fmt.Errorf("no server configured")
		observeTool("get_memory_data", start, err)
		return nil, GetMemoryOutput{}, err
	}

	members, err := s.collectMembers(serverAddr, "/redfish/v1/Systems/System.Embedded.1/Memory")
	if err != nil {
		observeTool("get_memory_data", start, err)
		return nil, GetMemoryOutput{}, fmt.Errorf("failed to get memory data: %w", err)
	}

	var dimms []MemoryDIMMInfo
	for _, m := range members {
		dimms = append(dimms, MemoryDIMMInfo{
			Name:              mapStr(m, "Name"),
			CapacityMiB:       mapFloat(m, "CapacityMiB"),
			MemoryDeviceType:  mapStr(m, "MemoryDeviceType"),
			OperatingSpeedMhz: mapFloat(m, "OperatingSpeedMhz"),
			Health:            mapStr(m, "Status", "Health"),
			ErrorCorrection:   mapStr(m, "ErrorCorrection"),
		})
	}

	observeTool("get_memory_data", start, nil)
	return nil, GetMemoryOutput{DIMMs: dimms}, nil
}

func (s *Server) handleGetFirmwareInventory(ctx context.Context, req *mcp.CallToolRequest, input GetFirmwareInventoryInput) (*mcp.CallToolResult, GetFirmwareInventoryOutput, error) {
	start := time.Now()
	s.logger.Info("Handling get_firmware_inventory request")

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		err := fmt.Errorf("no server configured")
		observeTool("get_firmware_inventory", start, err)
		return nil, GetFirmwareInventoryOutput{}, err
	}

	members, err := s.collectMembers(serverAddr, "/redfish/v1/UpdateService/FirmwareInventory")
	if err != nil {
		observeTool("get_firmware_inventory", start, err)
		return nil, GetFirmwareInventoryOutput{}, fmt.Errorf("failed to get firmware inventory: %w", err)
	}

	var components []FirmwareComponent
	for _, m := range members {
		updateable, _ := m["Updateable"].(bool)
		components = append(components, FirmwareComponent{
			Id:         mapStr(m, "Id"),
			Name:       mapStr(m, "Name"),
			Version:    mapStr(m, "Version"),
			Updateable: updateable,
		})
	}

	observeTool("get_firmware_inventory", start, nil)
	return nil, GetFirmwareInventoryOutput{Components: components}, nil
}

func (s *Server) handleDiscoverResources(ctx context.Context, req *mcp.CallToolRequest, input DiscoverResourcesInput) (*mcp.CallToolResult, DiscoverResourcesOutput, error) {
	start := time.Now()
	s.logger.Info("Handling discover_resources request")

	serverAddr := s.resolveServer(input.ServerAddress)
	if serverAddr == "" {
		err := fmt.Errorf("no server configured")
		observeTool("discover_resources", start, err)
		return nil, DiscoverResourcesOutput{}, err
	}

	resp, err := s.fetchResource(serverAddr, "/redfish/v1/")
	if err != nil {
		observeTool("discover_resources", start, err)
		return nil, DiscoverResourcesOutput{}, fmt.Errorf("failed to get service root: %w", err)
	}

	data, ok := resp.Data.(map[string]interface{})
	if !ok {
		err := fmt.Errorf("unexpected response format")
		observeTool("discover_resources", start, err)
		return nil, DiscoverResourcesOutput{}, err
	}

	// Collect all keys that have an @odata.id value
	var links []ResourceLink
	for key, val := range data {
		if vm, ok := val.(map[string]interface{}); ok {
			if odataID, ok := vm["@odata.id"].(string); ok {
				links = append(links, ResourceLink{
					Name: key,
					Path: odataID,
				})
			}
		}
	}

	observeTool("discover_resources", start, nil)
	return nil, DiscoverResourcesOutput{Links: links}, nil
}
