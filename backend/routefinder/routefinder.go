package routefinder

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"evepvpsearch/doublebuffer"
	"evepvpsearch/logging"
)

// System represents an EVE Online solar system
type System struct {
	SystemID   int        `json:"system_id"`
	SystemName string     `json:"system_name"`
	Stargates  []Stargate `json:"stargates"`
	Security   float64    `json:"security"`
}

// Stargate represents a stargate connection
type Stargate struct {
	ID                    int        `json:"id"`
	Position              [3]float64 `json:"position"`
	DestinationStargateID int        `json:"destination"` // Actually destination system ID
}

const (
	TheraSystemID   = 31000005
	ZarzakhSystemID = 30100000 // Excluded from shortest routes: game mechanics disallow fly-through
)

// ConvertMaxShipSizeEnum converts the API enum value to display text
// API values: small, medium, large, xlarge, capital, unknown (case-insensitive)
func ConvertMaxShipSizeEnum(enumValue string) string {
	enumValue = strings.TrimSpace(strings.ToLower(enumValue))
	switch enumValue {
	case "small":
		return "Destroyer"
	case "medium":
		return "Battlecruiser"
	case "large":
		return "Battleship"
	case "xlarge":
		return "Freighter"
	case "capital":
		return "Capital"
	case "unknown":
		return "Capital" // Default to Capital for unknown to ensure consistency
	default:
		// If empty or invalid, default to Capital
		return "Capital"
	}
}

// maxShipSizeRestrictivenessRank returns a lower number for more restrictive ship sizes.
// Used to pick the most restrictive (minimum) of two wormholes on a route.
func maxShipSizeRestrictivenessRank(displaySize string) int {
	switch displaySize {
	case "Destroyer":
		return 1
	case "Battlecruiser":
		return 2
	case "Battleship":
		return 3
	case "Freighter":
		return 4
	case "Capital":
		return 5
	default:
		return 5 // treat unknown as least restrictive
	}
}

// moreRestrictiveMaxShipSize returns the more restrictive (smaller max ship) of the two.
// Empty string is treated as least restrictive (Capital).
func moreRestrictiveMaxShipSize(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	ra, rb := maxShipSizeRestrictivenessRank(a), maxShipSizeRestrictivenessRank(b)
	if ra <= rb {
		return a
	}
	return b
}

// TheraSignatureInfo stores information about a Thera signature
type TheraSignatureInfo struct {
	SignatureID string
	WhType      string // Kept for backward compatibility, but MaxShipSize is preferred
	MaxShipSize string // Direct from API: small, medium, large, xlarge, capital, unknown
	IsEOL       bool   // Whether the signature is End-of-Life (EOL)
}

// GraphData contains the graph data structures for double-buffering
type GraphData struct {
	// Adjacency list: systemID -> []destinationSystemID
	Adjacency map[int][]int
	// System name to ID mapping for quick lookup
	SystemNameToID map[string]int
	// System ID to name mapping
	SystemIDToName map[int]string
	// Thera signatures: systemID -> TheraSignatureInfo
	TheraSignatures map[int]TheraSignatureInfo
}

// copyGraphData creates a deep copy of GraphData
func copyGraphData(src *GraphData) *GraphData {
	dst := &GraphData{
		Adjacency:       make(map[int][]int),
		SystemNameToID:  make(map[string]int),
		SystemIDToName:  make(map[int]string),
		TheraSignatures: make(map[int]TheraSignatureInfo),
	}

	// Deep copy adjacency map
	for k, v := range src.Adjacency {
		dst.Adjacency[k] = append([]int(nil), v...)
	}

	// Deep copy systemNameToID map
	for k, v := range src.SystemNameToID {
		dst.SystemNameToID[k] = v
	}

	// Deep copy systemIDToName map
	for k, v := range src.SystemIDToName {
		dst.SystemIDToName[k] = v
	}

	// Deep copy theraSignatures map
	for k, v := range src.TheraSignatures {
		dst.TheraSignatures[k] = TheraSignatureInfo{
			SignatureID: v.SignatureID,
			WhType:      v.WhType,
			MaxShipSize: v.MaxShipSize,
			IsEOL:       v.IsEOL,
		}
	}

	return dst
}

// RouteFinder handles local route finding between systems
type RouteFinder struct {
	// Graph data wrapped in double-buffer for lock-free reads
	graphData *doublebuffer.DoubleBuffer[GraphData]

	// Last time Thera signatures were fetched (atomic for rate limiting checks)
	lastTheraFetch atomic.Int64 // Stores unix timestamp in nanoseconds
	// Last time EVE Scout API was called (atomic for rate limiting)
	lastEveScoutRequest atomic.Int64 // Stores unix timestamp in nanoseconds

	// HTTP client for API calls (for testing) - rarely changed, no protection needed
	httpClient *http.Client
	// Base URL for Thera API (for testing) - rarely changed, no protection needed
	theraAPIBaseURL string
	// User-Agent sent with HTTP requests
	userAgent string
}

// Route represents a path between two systems
type Route struct {
	FromSystemID   int    `json:"from_system_id"`
	ToSystemID     int    `json:"to_system_id"`
	FromSystemName string `json:"from_system_name"`
	ToSystemName   string `json:"to_system_name"`
	Jumps          int    `json:"jumps"`
	Path           []int  `json:"path"` // System IDs in order
	ViaThera       bool   `json:"via_thera,omitempty"`
	TheraInfo      string `json:"thera_info,omitempty"`
	MaxShipSize    string `json:"max_ship_size,omitempty"` // Maximum ship size for Thera route
}

// ShortestPaths stores BFS traversal results from a single source system.
// Distances are measured in jumps; Parents allows path reconstruction.
type ShortestPaths struct {
	Source    int
	Distances map[int]int
	Parents   map[int]int
}

// TheraSignature represents a Thera wormhole signature from EVE Scout API
// Based on actual API response structure from https://api.eve-scout.com/v2/public/signatures
type TheraSignature struct {
	ID             interface{} `json:"id"`                         // Can be string or int
	InSystemID     interface{} `json:"in_system_id,omitempty"`     // System where wormhole is located (source)
	OutSystemID    interface{} `json:"out_system_id,omitempty"`    // System where wormhole leads to (destination)
	InSystemName   string      `json:"in_system_name,omitempty"`   // Name of source system
	OutSystemName  string      `json:"out_system_name,omitempty"`  // Name of destination system
	InSignature    string      `json:"in_signature,omitempty"`     // Signature ID in source system
	OutSignature   string      `json:"out_signature,omitempty"`    // Signature ID in destination system
	SignatureType  string      `json:"signature_type,omitempty"`   // Type of signature (e.g., "wormhole")
	WhType         string      `json:"wh_type,omitempty"`          // Wormhole type (e.g., "N944", "J377")
	MaxShipSize    string      `json:"max_ship_size,omitempty"`    // Max ship size enum: small, medium, large, xlarge, capital, unknown
	MaxShipSizeAlt string      `json:"maxShipSize,omitempty"`      // Alternate camelCase key from API/proxies
	WhExitsOutward bool        `json:"wh_exits_outward,omitempty"` // Direction: true = exits outward from Thera
	Completed      bool        `json:"completed,omitempty"`        // Whether signature is completed
	ExpiresAt      string      `json:"expires_at,omitempty"`       // When signature expires
	EOL            bool        `json:"eol,omitempty"`              // Whether the wormhole is End-of-Life (near collapse)
	EOLAlt         bool        `json:"wh_eol,omitempty"`           // Alternate API field for EOL

	// Legacy fields for backward compatibility
	SystemID      interface{} `json:"system_id,omitempty"`          // Legacy - use InSystemID
	SystemName    string      `json:"system_name,omitempty"`        // Legacy - use InSystemName
	SignatureID   string      `json:"signature_id,omitempty"`       // Legacy - use InSignature
	LeadsTo       string      `json:"leads_to,omitempty"`           // Legacy - use OutSystemName
	LeadsToSystem interface{} `json:"leads_to_system_id,omitempty"` // Legacy - use OutSystemID
}

// EveScoutTheraResponse represents the response from EVE Scout Thera API
type EveScoutTheraResponse struct {
	Signatures []TheraSignature `json:"signatures"`
}

// NewRouteFinder creates a new RouteFinder and initializes it with system data
func NewRouteFinder(systems []System) *RouteFinder {
	initialGraphData := &GraphData{
		Adjacency:       make(map[int][]int),
		SystemNameToID:  make(map[string]int),
		SystemIDToName:  make(map[int]string),
		TheraSignatures: make(map[int]TheraSignatureInfo),
	}

	rf := &RouteFinder{
		graphData:       doublebuffer.NewDoubleBuffer(initialGraphData, copyGraphData),
		httpClient:      &http.Client{Timeout: 10 * time.Second},
		theraAPIBaseURL: "https://api.eve-scout.com",
	}

	if len(systems) > 0 {
		rf.buildGraph(systems)
	}
	return rf
}

// SetTheraAPIBaseURL sets the base URL for Thera API (for testing)
func (rf *RouteFinder) SetTheraAPIBaseURL(baseURL string) {
	rf.theraAPIBaseURL = baseURL
}

// SetUserAgent sets the User-Agent header sent with HTTP requests
func (rf *RouteFinder) SetUserAgent(ua string) {
	rf.userAgent = ua
}

// SetHTTPClient sets the HTTP client (for testing)
func (rf *RouteFinder) SetHTTPClient(client *http.Client) {
	// No mutex needed - rarely changed and not part of graph data
	rf.httpClient = client
}

// RebuildGraph rebuilds the graph with new system data
func (rf *RouteFinder) RebuildGraph(systems []System) {
	rf.buildGraph(systems)
}

// buildGraph builds the adjacency list from provided systems
func (rf *RouteFinder) buildGraph(systems []System) {
	rf.graphData.Write(func(data *GraphData) {
		// Clear existing data
		data.Adjacency = make(map[int][]int)
		data.SystemNameToID = make(map[string]int)
		data.SystemIDToName = make(map[int]string)
		data.TheraSignatures = make(map[int]TheraSignatureInfo)

		// Build adjacency list and name mappings
		for _, sys := range systems {
			data.SystemIDToName[sys.SystemID] = sys.SystemName
			data.SystemNameToID[sys.SystemName] = sys.SystemID

			// Initialize adjacency list for this system
			data.Adjacency[sys.SystemID] = make([]int, 0)

			// Add connections from stargates
			for _, stargate := range sys.Stargates {
				// DestinationStargateID is actually the destination system ID
				destSystemID := stargate.DestinationStargateID
				data.Adjacency[sys.SystemID] = append(data.Adjacency[sys.SystemID], destSystemID)
			}
		}
	})

	// Read to get count for logging
	readFn := rf.graphData.Read()
	data := readFn()
	log.Printf("RouteFinder: Built graph with %d systems", len(data.Adjacency))
}

// removeNeighbor returns a copy of neighbors with one occurrence of remove omitted.
func removeNeighbor(neighbors []int, remove int) []int {
	if len(neighbors) == 0 {
		return neighbors
	}
	out := neighbors[:0]
	for _, n := range neighbors {
		if n != remove {
			out = append(out, n)
		}
	}
	return out
}

// stripTheraWormholeEdges removes every Thera↔k-space edge from the adjacency graph.
// Thera has no stargates, so any neighbor of Thera was added from EVE Scout data.
// Without this, old wormhole links persist when connections roll and disappear from the API.
func stripTheraWormholeEdges(data *GraphData) {
	old := data.Adjacency[TheraSystemID]
	if len(old) == 0 {
		return
	}
	old = append([]int(nil), old...)
	for _, sysID := range old {
		data.Adjacency[sysID] = removeNeighbor(data.Adjacency[sysID], TheraSystemID)
	}
	data.Adjacency[TheraSystemID] = nil
}

// FindShortestRoute finds the shortest route between two systems using BFS.
// maxJumps limits the search: when > 0, stops exploring paths exceeding this length (returns error if no shorter route).
func (rf *RouteFinder) FindShortestRoute(fromSystemID, toSystemID, maxJumps int) (*Route, error) {
	readFn := rf.graphData.Read()
	data := readFn()

	if fromSystemID == toSystemID {
		return &Route{
			FromSystemID:   fromSystemID,
			ToSystemID:     toSystemID,
			FromSystemName: data.SystemIDToName[fromSystemID],
			ToSystemName:   data.SystemIDToName[toSystemID],
			Jumps:          0,
			Path:           []int{fromSystemID},
		}, nil
	}

	// BFS to find shortest path
	queue := [][]int{{fromSystemID}}
	visited := make(map[int]bool)
	visited[fromSystemID] = true

	for len(queue) > 0 {
		path := queue[0]
		queue = queue[1:]

		// Early termination: if current path already exceeds maxJumps, no shorter route exists
		jumps := len(path) - 1
		if maxJumps > 0 && jumps > maxJumps {
			return nil, fmt.Errorf("no route within %d jumps from system %d to system %d", maxJumps, fromSystemID, toSystemID)
		}

		currentSystemID := path[len(path)-1]
		neighbors := data.Adjacency[currentSystemID]

		for _, neighborID := range neighbors {
			// Skip Zarzakh: game mechanics disallow flying through this system
			if neighborID == ZarzakhSystemID {
				continue
			}
			if neighborID == toSystemID {
				// Found the destination
				finalPath := append(path, neighborID)
				return &Route{
					FromSystemID:   fromSystemID,
					ToSystemID:     toSystemID,
					FromSystemName: data.SystemIDToName[fromSystemID],
					ToSystemName:   data.SystemIDToName[toSystemID],
					Jumps:          len(finalPath) - 1,
					Path:           finalPath,
				}, nil
			}

			if !visited[neighborID] {
				visited[neighborID] = true
				newPath := make([]int, len(path)+1)
				copy(newPath, path)
				newPath[len(path)] = neighborID
				queue = append(queue, newPath)
			}
		}
	}

	// No route found
	return nil, fmt.Errorf("no route found from system %d to system %d", fromSystemID, toSystemID)
}

// FindShortestPathsFrom computes shortest-path distances from a source system to all reachable systems.
// maxJumps limits traversal depth when > 0.
func (rf *RouteFinder) FindShortestPathsFrom(fromSystemID, maxJumps int) *ShortestPaths {
	readFn := rf.graphData.Read()
	data := readFn()

	distances := make(map[int]int, len(data.Adjacency))
	parents := make(map[int]int, len(data.Adjacency))
	queue := make([]int, 0, 256)

	distances[fromSystemID] = 0
	parents[fromSystemID] = fromSystemID
	queue = append(queue, fromSystemID)

	for len(queue) > 0 {
		currentSystemID := queue[0]
		queue = queue[1:]
		currentDist := distances[currentSystemID]

		if maxJumps > 0 && currentDist >= maxJumps {
			continue
		}

		for _, neighborID := range data.Adjacency[currentSystemID] {
			// Skip Zarzakh: game mechanics disallow flying through this system.
			if neighborID == ZarzakhSystemID {
				continue
			}
			if _, seen := distances[neighborID]; seen {
				continue
			}

			distances[neighborID] = currentDist + 1
			parents[neighborID] = currentSystemID
			queue = append(queue, neighborID)
		}
	}

	return &ShortestPaths{
		Source:    fromSystemID,
		Distances: distances,
		Parents:   parents,
	}
}

// BuildPath reconstructs a route path from the source of ShortestPaths to destination.
// Returns nil if destination is unreachable.
func (rf *RouteFinder) BuildPath(paths *ShortestPaths, toSystemID int) []int {
	if paths == nil {
		return nil
	}
	if _, ok := paths.Distances[toSystemID]; !ok {
		return nil
	}

	rev := make([]int, 0, paths.Distances[toSystemID]+1)
	current := toSystemID
	for {
		rev = append(rev, current)
		parent, ok := paths.Parents[current]
		if !ok {
			return nil
		}
		if current == paths.Source {
			break
		}
		if parent == current {
			break
		}
		current = parent
	}

	path := make([]int, len(rev))
	for i := range rev {
		path[len(rev)-1-i] = rev[i]
	}
	return path
}

// EnsureTheraSignaturesFresh refreshes Thera signatures when the 1-minute cache is stale.
func (rf *RouteFinder) EnsureTheraSignaturesFresh() {
	lastFetchNanos := rf.lastTheraFetch.Load()
	lastFetch := time.Unix(0, lastFetchNanos)
	if time.Since(lastFetch) > 1*time.Minute {
		rf.fetchTheraSignatures()
	}
}

// FindShortestRouteWithThera finds the shortest route, considering Thera wormholes.
// maxJumps limits the search (0 = unlimited).
func (rf *RouteFinder) FindShortestRouteWithThera(fromSystemID, toSystemID, maxJumps int) (*Route, error) {
	// Refresh Thera signatures if needed (cache for 1 minute to reduce load on EVE Scout API)
	lastFetchNanos := rf.lastTheraFetch.Load()
	lastFetch := time.Unix(0, lastFetchNanos)
	needsRefresh := time.Since(lastFetch) > 1*time.Minute

	if needsRefresh {
		rf.fetchTheraSignatures()
	}

	// Try direct route first
	directRoute, directErr := rf.FindShortestRoute(fromSystemID, toSystemID, maxJumps)

	// Try routes through Thera
	theraSystemID := TheraSystemID
	theraRoutes := make([]*Route, 0)

	// Check if we can reach Thera from source
	routeToThera, errToThera := rf.FindShortestRoute(fromSystemID, theraSystemID, maxJumps)
	if errToThera == nil {
		// Check if we can reach destination from Thera
		routeFromThera, errFromThera := rf.FindShortestRoute(theraSystemID, toSystemID, maxJumps)
		if errFromThera == nil {
			// Combine routes (avoid duplicate Thera in path)
			combinedPath := make([]int, 0, len(routeToThera.Path)+len(routeFromThera.Path)-1)
			combinedPath = append(combinedPath, routeToThera.Path...)
			combinedPath = append(combinedPath, routeFromThera.Path[1:]...) // Skip first element (Thera) to avoid duplicate

			readFn := rf.graphData.Read()
			data := readFn()

			// Determine max ship size from BOTH wormholes on the route: inbound (system before Thera)
			// and outbound (system after Thera). Use the more restrictive of the two, since you must
			// fit through both to complete the route. E.g. route LanseZ -> Thera -> O-ZXUV: inbound
			// is LanseZ (Medium), outbound is O-ZXUV (maybe Freighter) -> show Medium.
			var afterTheraSize, beforeTheraSize string

			// System AFTER Thera (outbound wormhole from Thera)
			for i := 1; i < len(combinedPath); i++ {
				if combinedPath[i-1] == theraSystemID {
					systemID := combinedPath[i]
					if sigInfo, exists := data.TheraSignatures[systemID]; exists {
						if sigInfo.MaxShipSize != "" {
							afterTheraSize = sigInfo.MaxShipSize
						} else {
							afterTheraSize = "Capital"
						}
					}
					break
				}
			}
			// System BEFORE Thera (inbound wormhole into Thera)
			for i := 0; i < len(combinedPath)-1; i++ {
				if combinedPath[i+1] == theraSystemID {
					systemID := combinedPath[i]
					if sigInfo, exists := data.TheraSignatures[systemID]; exists {
						if sigInfo.MaxShipSize != "" {
							beforeTheraSize = sigInfo.MaxShipSize
						} else {
							beforeTheraSize = "Capital"
						}
					}
					break
				}
			}
			maxShipSize := moreRestrictiveMaxShipSize(beforeTheraSize, afterTheraSize)
			if maxShipSize == "" {
				maxShipSize = "Capital"
			}

			combinedJumps := len(combinedPath) - 1
			// When maxJumps is set, only add Thera route if it's within limit
			if maxJumps <= 0 || combinedJumps <= maxJumps {
				theraRoute := &Route{
					FromSystemID:   fromSystemID,
					ToSystemID:     toSystemID,
					FromSystemName: data.SystemIDToName[fromSystemID],
					ToSystemName:   data.SystemIDToName[toSystemID],
					Jumps:          combinedJumps,
					Path:           combinedPath,
					ViaThera:       true,
					TheraInfo:      "Route via Thera",
					MaxShipSize:    maxShipSize,
				}
				theraRoutes = append(theraRoutes, theraRoute)
			}
		}
	}

	// Return shortest route
	if directErr != nil && len(theraRoutes) == 0 {
		return nil, directErr
	}

	if directErr != nil {
		return theraRoutes[0], nil
	}

	if len(theraRoutes) == 0 {
		return directRoute, nil
	}

	// Compare direct route with Thera route
	if directRoute.Jumps <= theraRoutes[0].Jumps {
		return directRoute, nil
	}

	return theraRoutes[0], nil
}

// fetchTheraSignatures fetches Thera signatures from EVE Scout API
// Cached for 1 minute to reduce load on EVE Scout API
func (rf *RouteFinder) fetchTheraSignatures() {
	// Double-check cache to prevent concurrent fetches (cache for 1 minute)
	lastFetchNanos := rf.lastTheraFetch.Load()
	lastFetch := time.Unix(0, lastFetchNanos)
	if time.Since(lastFetch) <= 1*time.Minute {
		return // Already fetched recently, skip
	}

	// Rate limiting (don't fetch more than once per 500ms)
	lastRequestNanos := rf.lastEveScoutRequest.Load()
	lastRequest := time.Unix(0, lastRequestNanos)
	elapsed := time.Since(lastRequest)
	needsWait := elapsed < 500*time.Millisecond

	// Update request time atomically
	nowNanos := time.Now().UnixNano()
	rf.lastEveScoutRequest.Store(nowNanos)

	// Read values needed for HTTP request (not protected by mutex - rarely changed)
	url := rf.theraAPIBaseURL + "/v2/public/signatures"
	client := rf.httpClient

	if needsWait {
		time.Sleep(500*time.Millisecond - elapsed)
		// Re-check cache after sleep in case another goroutine fetched while we waited
		lastFetchNanos = rf.lastTheraFetch.Load()
		lastFetch = time.Unix(0, lastFetchNanos)
		if time.Since(lastFetch) <= 1*time.Minute {
			return
		}
	}

	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		log.Printf("Error creating request for Thera signatures: %v", err)
		return
	}
	if rf.userAgent != "" {
		req.Header.Set("User-Agent", rf.userAgent)
		req.Header.Set("X-User-Agent", rf.userAgent)
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error fetching Thera signatures: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("EVE Scout API returned status code: %d for Thera signatures", resp.StatusCode)
		return
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("Error reading Thera signatures response: %v", err)
		return
	}

	// First, parse as raw JSON to inspect the actual structure and field names (debug only)
	var rawArray []map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &rawArray); err == nil && len(rawArray) > 0 {
		firstSig := rawArray[0]
		logging.Debugf("RouteFinder: API response structure - available fields: %v", getMapKeys(firstSig))
		if systemIDVal, ok := firstSig["system_id"]; ok {
			logging.Debugf("RouteFinder: system_id example: %v (type: %T)", systemIDVal, systemIDVal)
		} else {
			logging.Debugf("RouteFinder: WARNING - system_id field not found in API response!")
		}
		if leadsToVal, ok := firstSig["leads_to_system_id"]; ok {
			logging.Debugf("RouteFinder: leads_to_system_id example: %v (type: %T)", leadsToVal, leadsToVal)
		} else {
			logging.Debugf("RouteFinder: leads_to_system_id field not found - checking alternatives...")
			for _, key := range []string{"leadsToSystemId", "leads_to", "destination_system_id", "destinationSystemId"} {
				if val, ok := firstSig[key]; ok {
					logging.Debugf("RouteFinder: Found alternative field '%s': %v (type: %T)", key, val, val)
				}
			}
		}
		if whTypeVal, ok := firstSig["wh_type"]; ok {
			logging.Debugf("RouteFinder: wh_type example: %v", whTypeVal)
		}
	}

	// Now try to parse into our struct
	var allSignatures []TheraSignature
	var theraResponse EveScoutTheraResponse

	// First, try parsing as an array directly
	if err := json.Unmarshal(bodyBytes, &allSignatures); err != nil {
		// If that fails, try parsing as an object with a signatures field
		if err2 := json.Unmarshal(bodyBytes, &theraResponse); err2 != nil {
			log.Printf("Error parsing Thera signatures (tried array and object formats): %v, %v", err, err2)
			maxLen := 500
			if len(bodyBytes) < maxLen {
				maxLen = len(bodyBytes)
			}
			log.Printf("Response body (first %d chars): %s", maxLen, string(bodyBytes[:maxLen]))
			return
		}
		allSignatures = theraResponse.Signatures
	}

	logging.Debugf("RouteFinder: Parsed %d total signatures from API", len(allSignatures))

	// Filter for Thera signatures only
	// The API returns all signatures. We need to filter for ones that connect to Thera (31000005)
	// API uses: in_system_id (where wormhole is) and out_system_id (where it leads to)
	theraSignatures := make([]TheraSignature, 0)
	for i, sig := range allSignatures {
		// Get system IDs - API uses in_system_id and out_system_id
		inSystemID := getIntFromInterface(sig.InSystemID)
		if inSystemID == 0 {
			// Fallback to legacy field
			inSystemID = getIntFromInterface(sig.SystemID)
		}
		outSystemID := getIntFromInterface(sig.OutSystemID)
		if outSystemID == 0 {
			// Fallback to legacy field
			outSystemID = getIntFromInterface(sig.LeadsToSystem)
		}

		if i < 3 {
			logging.Debugf("RouteFinder: Signature %d - in_system_id=%v, out_system_id=%v, in_system_name=%s, out_system_name=%s, wh_type=%s, wh_exits_outward=%v, completed=%v",
				i, inSystemID, outSystemID, sig.InSystemName, sig.OutSystemName, sig.WhType, sig.WhExitsOutward, sig.Completed)
		}

		// Check if this signature connects to Thera:
		// 1. out_system_id = 31000005 (wormhole leads TO Thera)
		// 2. in_system_id = 31000005 (wormhole is IN Thera, leads outward)
		// 3. out_system_name = "Thera" (text match)
		// 4. in_system_name = "Thera" (signature is in Thera)
		isTheraConnection := outSystemID == TheraSystemID ||
			inSystemID == TheraSystemID ||
			sig.OutSystemName == "Thera" ||
			sig.InSystemName == "Thera"

		if isTheraConnection && (inSystemID > 0 || outSystemID > 0) {
			theraSignatures = append(theraSignatures, sig)
		}
	}

	logging.Debugf("RouteFinder: Filtered to %d Thera signatures from %d total", len(theraSignatures), len(allSignatures))

	// Use the filtered list
	theraResponse = EveScoutTheraResponse{Signatures: theraSignatures}

	// Final check - another goroutine might have fetched while we were making the request
	lastFetchNanos = rf.lastTheraFetch.Load()
	lastFetch = time.Unix(0, lastFetchNanos)
	if time.Since(lastFetch) <= 1*time.Minute {
		return
	}

	// Update Thera signatures and adjacency maps using double-buffer Write
	rf.graphData.Write(func(data *GraphData) {
		stripTheraWormholeEdges(data)
		data.TheraSignatures = make(map[int]TheraSignatureInfo)
		for _, sig := range theraResponse.Signatures {
			// Get system IDs from API response
			inSystemID := getIntFromInterface(sig.InSystemID)
			if inSystemID == 0 {
				inSystemID = getIntFromInterface(sig.SystemID) // Fallback to legacy
			}
			outSystemID := getIntFromInterface(sig.OutSystemID)
			if outSystemID == 0 {
				outSystemID = getIntFromInterface(sig.LeadsToSystem) // Fallback to legacy
			}

			// Signature ID for the non-Thera system's bookmark:
			// - WH in k-space leading to Thera: scan ID is in_signature (source system).
			// - WH in Thera leading to k-space: scan ID is out_signature (destination system).
			// Preferring in_signature for all rows stored the Thera-side name and hid the k-space sig.
			var sigID string
			if outSystemID == TheraSystemID {
				sigID = strings.TrimSpace(sig.InSignature)
				if sigID == "" {
					sigID = strings.TrimSpace(sig.OutSignature)
				}
			} else if inSystemID == TheraSystemID {
				sigID = strings.TrimSpace(sig.OutSignature)
				if sigID == "" {
					sigID = strings.TrimSpace(sig.InSignature)
				}
			} else {
				sigID = strings.TrimSpace(sig.InSignature)
				if sigID == "" {
					sigID = strings.TrimSpace(sig.OutSignature)
				}
			}
			if sigID == "" {
				sigID = strings.TrimSpace(sig.SignatureID) // Legacy fallback
			}
			if sigID == "" {
				sigID = getStringFromInterface(sig.ID)
			}

			// Determine which system connects to Thera
			// If out_system_id == Thera, then in_system_id connects to Thera
			// If in_system_id == Thera, then out_system_id connects to Thera
			var systemConnectingToThera int
			if outSystemID == TheraSystemID {
				systemConnectingToThera = inSystemID
			} else if inSystemID == TheraSystemID {
				systemConnectingToThera = outSystemID
			} else {
				// Should not happen if filtering worked correctly
				log.Printf("Warning: Thera signature has neither in_system_id nor out_system_id equal to Thera: in=%v, out=%v", inSystemID, outSystemID)
				continue
			}

			if systemConnectingToThera == 0 {
				log.Printf("Warning: Invalid system ID in Thera signature: in_system_id=%v, out_system_id=%v", inSystemID, outSystemID)
				continue
			}

			// Store signature mapping with WhType and MaxShipSize
			// Prefer MaxShipSize from API (snake_case or camelCase), fallback to Capital if not available
			rawSize := strings.TrimSpace(sig.MaxShipSize)
			if rawSize == "" {
				rawSize = strings.TrimSpace(sig.MaxShipSizeAlt)
			}
			var maxShipSize string
			if rawSize == "" {
				maxShipSize = "Capital"
			} else {
				maxShipSize = ConvertMaxShipSizeEnum(rawSize)
			}

			// EOL: use API eol/wh_eol if present, or derive from expires_at (< 4h left)
			isEOL := sig.EOL || sig.EOLAlt || isEOLFromExpiresAt(sig.ExpiresAt)
			data.TheraSignatures[systemConnectingToThera] = TheraSignatureInfo{
				SignatureID: sigID,
				WhType:      sig.WhType,
				MaxShipSize: maxShipSize,
				IsEOL:       isEOL,
			}

			// Add bidirectional Thera connections to adjacency list
			// Thera (31000005) <-> systemConnectingToThera
			if _, exists := data.Adjacency[TheraSystemID]; !exists {
				data.Adjacency[TheraSystemID] = make([]int, 0)
			}
			// Check if connection already exists
			found := false
			for _, existing := range data.Adjacency[TheraSystemID] {
				if existing == systemConnectingToThera {
					found = true
					break
				}
			}
			if !found {
				data.Adjacency[TheraSystemID] = append(data.Adjacency[TheraSystemID], systemConnectingToThera)
			}

			// Add reverse connection (from systemConnectingToThera to Thera)
			if _, exists := data.Adjacency[systemConnectingToThera]; !exists {
				data.Adjacency[systemConnectingToThera] = make([]int, 0)
			}
			found = false
			for _, existing := range data.Adjacency[systemConnectingToThera] {
				if existing == TheraSystemID {
					found = true
					break
				}
			}
			if !found {
				data.Adjacency[systemConnectingToThera] = append(data.Adjacency[systemConnectingToThera], TheraSystemID)
			}
		}
	})

	// Update timestamp atomically after successful update
	rf.lastTheraFetch.Store(time.Now().UnixNano())

	readFn := rf.graphData.Read()
	data := readFn()
	logging.Debugf("RouteFinder: Updated Thera signatures, %d active connections", len(data.TheraSignatures))
}

// eolExpiryThreshold is the time left below which a Thera signature is considered EOL
const eolExpiryThreshold = 4 * time.Hour

// isEOLFromExpiresAt returns true if expires_at is set and the signature expires in less than 4 hours.
// EVE Scout API: expires_at* string - "The time when the signature will expire."
func isEOLFromExpiresAt(expiresAt string) bool {
	expiresAt = strings.TrimSpace(expiresAt)
	if expiresAt == "" {
		return false
	}
	// Try common ISO8601/RFC3339 formats
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05.999Z",
		"2006-01-02 15:04:05",
	}
	var t time.Time
	var err error
	for _, layout := range layouts {
		t, err = time.Parse(layout, expiresAt)
		if err == nil {
			break
		}
	}
	if err != nil {
		log.Printf("RouteFinder: could not parse expires_at %q: %v", expiresAt, err)
		return false
	}
	remaining := time.Until(t)
	return remaining > 0 && remaining < eolExpiryThreshold
}

// Helper functions to handle API response fields that might be strings or ints
func getIntFromInterface(val interface{}) int {
	if val == nil {
		return 0
	}
	switch v := val.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
		return 0
	default:
		return 0
	}
}

func getStringFromInterface(val interface{}) string {
	if val == nil {
		return ""
	}
	switch v := val.(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// getMapKeys returns keys of a map for debugging
func getMapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// GetTheraSignaturesCount returns the number of active Thera signatures
func (rf *RouteFinder) GetTheraSignaturesCount() int {
	readFn := rf.graphData.Read()
	data := readFn()
	return len(data.TheraSignatures)
}

// GetTheraSignaturesFingerprint returns a stable string that changes when any Thera
// connection or signature id changes (including swaps that keep the same count).
func (rf *RouteFinder) GetTheraSignaturesFingerprint() string {
	readFn := rf.graphData.Read()
	data := readFn()
	if len(data.TheraSignatures) == 0 {
		return ""
	}
	ids := make([]int, 0, len(data.TheraSignatures))
	for id := range data.TheraSignatures {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte('|')
		}
		info := data.TheraSignatures[id]
		fmt.Fprintf(&b, "%d:%s", id, info.SignatureID)
	}
	return b.String()
}

// GetTheraSignatureInfoForRoute returns MaxShipSize and EOL flag for a route that goes through Thera.
// Uses the more restrictive of inbound (system before Thera) and outbound (system after Thera) wormholes.
func (rf *RouteFinder) GetTheraSignatureInfoForRoute(path []int) (string, bool) {
	readFn := rf.graphData.Read()
	data := readFn()

	var afterTheraSize, beforeTheraSize string
	var eol bool

	for i := 1; i < len(path); i++ {
		if path[i-1] == TheraSystemID {
			systemID := path[i]
			if sigInfo, exists := data.TheraSignatures[systemID]; exists {
				if sigInfo.MaxShipSize != "" {
					afterTheraSize = sigInfo.MaxShipSize
				} else {
					afterTheraSize = "Capital"
				}
				eol = eol || sigInfo.IsEOL
			}
			break
		}
	}
	for i := 0; i < len(path)-1; i++ {
		if path[i+1] == TheraSystemID {
			systemID := path[i]
			if sigInfo, exists := data.TheraSignatures[systemID]; exists {
				if sigInfo.MaxShipSize != "" {
					beforeTheraSize = sigInfo.MaxShipSize
				} else {
					beforeTheraSize = "Capital"
				}
				eol = eol || sigInfo.IsEOL
			}
			break
		}
	}
	maxShipSize := moreRestrictiveMaxShipSize(beforeTheraSize, afterTheraSize)
	if maxShipSize == "" {
		return "", false
	}
	return maxShipSize, eol
}

// GetTheraSignatureIDsForRoute returns signature IDs for inbound and outbound Thera wormholes
// used by the provided route path.
//
// - inboundSig: signature ID in the system right BEFORE Thera in the path
// - outboundSig: signature ID in the system right AFTER Thera in the path
//
// eol is true if either signature is marked as End-of-Life.
func (rf *RouteFinder) GetTheraSignatureIDsForRoute(path []int) (inboundSig, outboundSig string, eol bool) {
	readFn := rf.graphData.Read()
	data := readFn()

	// Outbound: Thera -> X (signature is in X)
	for i := 1; i < len(path); i++ {
		if path[i-1] == TheraSystemID {
			systemID := path[i]
			if sigInfo, exists := data.TheraSignatures[systemID]; exists {
				outboundSig = sigInfo.SignatureID
				eol = eol || sigInfo.IsEOL
			}
			break
		}
	}

	// Inbound: X -> Thera (signature is in X)
	for i := 0; i < len(path)-1; i++ {
		if path[i+1] == TheraSystemID {
			systemID := path[i]
			if sigInfo, exists := data.TheraSignatures[systemID]; exists {
				inboundSig = sigInfo.SignatureID
				eol = eol || sigInfo.IsEOL
			}
			break
		}
	}

	return inboundSig, outboundSig, eol
}

// ForceFetchTheraSignatures forces a fetch of Thera signatures (bypasses cache)
func (rf *RouteFinder) ForceFetchTheraSignatures() {
	// Reset timestamp to 0 (epoch) to force fetch
	rf.lastTheraFetch.Store(0)
	rf.fetchTheraSignatures()
}

// SetMockTheraSignaturesWithWhType sets mock Thera signatures with WhType information
// signatures is a map of systemID -> signature name
// whTypes is an optional map of systemID -> WhType (if nil, defaults will be used)
// eolSystems is an optional map of systemID -> true for signatures that should be End-of-Life in mock data
func (rf *RouteFinder) SetMockTheraSignaturesWithWhType(signatures map[int]string, whTypes map[int]string, eolSystems map[int]bool) {
	rf.graphData.Write(func(data *GraphData) {
		stripTheraWormholeEdges(data)
		// Clear existing Thera signatures
		data.TheraSignatures = make(map[int]TheraSignatureInfo)

		// Add mock signatures with WhType
		for systemID, sigName := range signatures {
			whType := ""
			if whTypes != nil {
				if wt, exists := whTypes[systemID]; exists {
					whType = wt
				}
			}
			// If no WhType provided, use a default for testing (Capital class wormhole)
			if whType == "" {
				whType = "N944" // Default to Capital class for mock data
			}
			// Convert WhType to MaxShipSize for mock data (default to Capital)
			maxShipSize := "Capital"

			// Mark selected mock signatures as End-of-Life so EOL is visible in UI
			isEOL := false
			if eolSystems != nil {
				if flag, exists := eolSystems[systemID]; exists && flag {
					isEOL = true
				}
			}
			data.TheraSignatures[systemID] = TheraSignatureInfo{
				SignatureID: sigName,
				WhType:      whType,
				MaxShipSize: maxShipSize,
				IsEOL:       isEOL,
			}

			// Add bidirectional Thera connections to adjacency list
			theraSystemID := TheraSystemID

			// Add connection from Thera to system
			if _, exists := data.Adjacency[theraSystemID]; !exists {
				data.Adjacency[theraSystemID] = make([]int, 0)
			}
			found := false
			for _, existing := range data.Adjacency[theraSystemID] {
				if existing == systemID {
					found = true
					break
				}
			}
			if !found {
				data.Adjacency[theraSystemID] = append(data.Adjacency[theraSystemID], systemID)
			}

			// Add reverse connection (from system to Thera)
			if _, exists := data.Adjacency[systemID]; !exists {
				data.Adjacency[systemID] = make([]int, 0)
			}
			found = false
			for _, existing := range data.Adjacency[systemID] {
				if existing == theraSystemID {
					found = true
					break
				}
			}
			if !found {
				data.Adjacency[systemID] = append(data.Adjacency[systemID], theraSystemID)
			}
		}
	})

	// Update timestamp to indicate signatures are loaded
	rf.lastTheraFetch.Store(time.Now().UnixNano())

	// Read to get count for logging
	readFn := rf.graphData.Read()
	data := readFn()
	log.Printf("RouteFinder: Set %d mock Thera signatures", len(data.TheraSignatures))
}

// SetMockZarzakhConnections sets mock Zarzakh connections for development mode
// systemIDs is a list of system IDs that should connect to Zarzakh (30100000)
func (rf *RouteFinder) SetMockZarzakhConnections(systemIDs []int) {
	rf.graphData.Write(func(data *GraphData) {
		// Ensure Zarzakh system exists in the graph
		if _, exists := data.Adjacency[ZarzakhSystemID]; !exists {
			data.Adjacency[ZarzakhSystemID] = make([]int, 0)
		}
		if _, exists := data.SystemIDToName[ZarzakhSystemID]; !exists {
			data.SystemIDToName[ZarzakhSystemID] = "Zarzakh"
			data.SystemNameToID["Zarzakh"] = ZarzakhSystemID
		}

		// Add bidirectional Zarzakh connections
		for _, systemID := range systemIDs {
			// Add connection from Zarzakh to system
			found := false
			for _, existing := range data.Adjacency[ZarzakhSystemID] {
				if existing == systemID {
					found = true
					break
				}
			}
			if !found {
				data.Adjacency[ZarzakhSystemID] = append(data.Adjacency[ZarzakhSystemID], systemID)
			}

			// Add reverse connection (from system to Zarzakh)
			if _, exists := data.Adjacency[systemID]; !exists {
				data.Adjacency[systemID] = make([]int, 0)
			}
			found = false
			for _, existing := range data.Adjacency[systemID] {
				if existing == ZarzakhSystemID {
					found = true
					break
				}
			}
			if !found {
				data.Adjacency[systemID] = append(data.Adjacency[systemID], ZarzakhSystemID)
			}
		}
	})

	log.Printf("RouteFinder: Set %d mock Zarzakh connections", len(systemIDs))
}
