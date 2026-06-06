package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

var (
	sdeHTTPClientOnce sync.Once
	sdeHTTPClient     *http.Client
)

func initSDEHTTPClient() {
	sdeHTTPClientOnce.Do(func() {
		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		var dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
		dialContext = dialer.DialContext

		if proxyAddr := os.Getenv("SOCKS5_PROXY"); proxyAddr != "" {
			socksDialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
			if err != nil {
				log.Printf("Warning: failed to create SOCKS5 dialer for SDE download: %v", err)
			} else {
				dialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					return socksDialer.Dial(network, addr)
				}
				log.Printf("SDE download using SOCKS5 proxy %s", proxyAddr)
			}
		}

		transport := &http.Transport{
			DialContext:           dialContext,
			ForceAttemptHTTP2:     false,
			ResponseHeaderTimeout: 120 * time.Second,
			TLSHandshakeTimeout:   30 * time.Second,
		}
		sdeHTTPClient = &http.Client{
			Transport: transport,
			Timeout:   10 * time.Minute,
		}
	})
}

// ItemGroup represents an EVE Online item group from the SDE
type ItemGroup struct {
	Key                  int               `json:"_key"`
	Anchorable           bool              `json:"anchorable"`
	Anchored             bool              `json:"anchored"`
	CategoryID           int               `json:"categoryID"`
	FittableNonSingleton bool              `json:"fittableNonSingleton"`
	Name                 map[string]string `json:"name"`
	Published            bool              `json:"published"`
	UseBasePrice         bool              `json:"useBasePrice"`
}

const (
	sdeURL         = "https://developers.eveonline.com/static-data/eve-online-static-data-latest-jsonl.zip"
	sdeZipFile     = "sde.zip"
	sdeExtractDir  = "sde"
	sdeMaxAgeHours = 24
)

// downloadSDE downloads the EVE Online SDE if it doesn't exist or is older than 24 hours
func downloadSDE() error {
	// Check if we need to download the SDE
	needDownload := true

	if info, err := os.Stat(sdeZipFile); err == nil {
		// File exists, check if it's newer than 24 hours
		if time.Since(info.ModTime()).Hours() < sdeMaxAgeHours {
			needDownload = false
			log.Printf("SDE file is less than %d hours old, skipping download", sdeMaxAgeHours)
		} else {
			log.Printf("SDE file is older than %d hours, downloading new version", sdeMaxAgeHours)
		}
	}

	if needDownload {
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				wait := time.Duration(1<<uint(attempt)) * time.Second
				log.Printf("Retrying SDE download in %v (attempt %d/3)", wait, attempt+1)
				time.Sleep(wait)
			}
			lastErr = downloadSDEFile()
			if lastErr == nil {
				return nil
			}
			log.Printf("SDE download attempt %d/3 failed: %v", attempt+1, lastErr)
		}
		return fmt.Errorf("failed to download SDE after 3 attempts: %v", lastErr)
	}

	return nil
}

func downloadSDEFile() error {
	initSDEHTTPClient()
	log.Println("Downloading SDE from", sdeURL)

	out, err := os.Create(sdeZipFile)
	if err != nil {
		return fmt.Errorf("failed to create SDE file: %v", err)
	}
	defer out.Close()

	req, err := http.NewRequest("GET", sdeURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("User-Agent", httpUserAgent)

	resp, err := sdeHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download SDE: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download SDE: HTTP %d", resp.StatusCode)
	}

	written, err := io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to save SDE: %v", err)
	}

	log.Printf("SDE downloaded successfully (%d bytes)", written)
	return nil
}

// extractSDE extracts the SDE zip file
func extractSDE() error {
	// Check if extraction is needed
	if _, err := os.Stat(sdeExtractDir); err == nil {
		log.Println("SDE already extracted, skipping extraction")
		return nil
	}

	log.Println("Extracting SDE...")

	r, err := zip.OpenReader(sdeZipFile)
	if err != nil {
		return fmt.Errorf("failed to open SDE zip: %v", err)
	}
	defer r.Close()

	// Create extraction directory
	if err := os.MkdirAll(sdeExtractDir, 0755); err != nil { // #nosec G301 -- 0755 for SDE extract
		return fmt.Errorf("failed to create extraction directory: %v", err)
	}

	// Extract files
	for _, f := range r.File {
		// Skip directories
		if f.FileInfo().IsDir() {
			continue
		}

		// Create file path and prevent zip-slip (G305); validated with Rel check below
		filePath := filepath.Clean(filepath.Join(sdeExtractDir, f.Name)) // #nosec G305 -- path validated below
		rel, err := filepath.Rel(sdeExtractDir, filePath)
		if err != nil || strings.HasPrefix(rel, "..") {
			log.Printf("Skipping zip entry with path traversal: %s", f.Name)
			continue
		}

		// Create directory structure
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil { // #nosec G301 -- 0755 for SDE extract
			return fmt.Errorf("failed to create directory for %s: %v", filePath, err)
		}

		// Open file in zip
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("failed to open file in zip: %v", err)
		}

		// Create output file
		outFile, err := os.Create(filePath) // #nosec G304 -- path validated above (zip-slip safe)
		if err != nil {
			_ = rc.Close()
			return fmt.Errorf("failed to create output file %s: %v", filePath, err)
		}

		// Copy data
		_, err = io.Copy(outFile, rc) // #nosec G110 -- SDE from trusted EVE source
		_ = rc.Close()
		_ = outFile.Close()

		if err != nil {
			return fmt.Errorf("failed to extract file %s: %v", filePath, err)
		}
	}

	log.Println("SDE extracted successfully")
	return nil
}

// getSystemsFromSDE reads systems data from the SDE
func getSystemsFromSDE() ([]System, error) {
	systems := []System{}

	// Look for systems data in the extracted SDE
	systemsFile := filepath.Join(sdeExtractDir, "sde", "bsd", "mapSolarSystems.jsonl")
	if _, err := os.Stat(systemsFile); os.IsNotExist(err) {
		// Try alternative path
		systemsFile = filepath.Join(sdeExtractDir, "mapSolarSystems.jsonl")
	}

	// Find the correct file in the extracted SDE
	err := filepath.Walk(sdeExtractDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && strings.HasSuffix(info.Name(), "mapSolarSystems.jsonl") {
			systemsFile = path
			return io.EOF // Stop walking
		}

		return nil
	})

	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("error searching for systems file: %v", err)
	}

	if systemsFile == "" {
		return nil, fmt.Errorf("mapSolarSystems.jsonl not found in SDE")
	}

	log.Printf("Reading systems from %s", systemsFile)

	// Read the file line by line
	content, err := os.ReadFile(systemsFile) // #nosec G304 -- path from filepath.Walk under sdeExtractDir
	if err != nil {
		return nil, fmt.Errorf("failed to read systems file: %v", err)
	}

	// Split by lines
	lines := strings.Split(string(content), "\n")

	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var systemData map[string]interface{}
		if err := json.Unmarshal([]byte(line), &systemData); err != nil {
			log.Printf("Warning: failed to parse system line %d: %v", i+1, err)
			continue
		}

		// Extract system name from the name object
		nameObj, ok := systemData["name"].(map[string]interface{})
		if !ok {
			log.Printf("Warning: failed to extract name for system line %d", i+1)
			continue
		}
		systemName, ok := nameObj["en"].(string)
		if !ok {
			log.Printf("Warning: failed to extract English name for system line %d", i+1)
			continue
		}

		// Extract system ID (_key) and security status
		systemID, ok := systemData["_key"].(float64)
		if !ok {
			log.Printf("Warning: failed to extract system ID for system %s", systemName)
			continue
		}

		securityStatus, ok := systemData["securityStatus"].(float64)
		if !ok {
			log.Printf("Warning: failed to extract security status for system %s", systemName)
			continue
		}

		// Extract regionID (optional, defaults to 0 if not present)
		regionID := 0
		if regionIDVal, ok := systemData["regionID"].(float64); ok {
			regionID = int(regionIDVal)
		}

		system := System{
			SystemID:   int(systemID),
			SystemName: systemName,
			Security:   securityStatus,
			RegionID:   regionID,
		}

		systems = append(systems, system)
	}

	log.Printf("Loaded %d systems from SDE", len(systems))
	return systems, nil
}

// ItemType represents an EVE Online item type from the SDE
type ItemType struct {
	Key       int               `json:"_key"`
	GroupID   int               `json:"groupID"`
	Name      map[string]string `json:"name"`
	Description map[string]string `json:"description"`
	Published bool              `json:"published"`
}

// getGroupsFromSDE reads groups data from the SDE
func getGroupsFromSDE() (map[string]string, error) {
	groups := make(map[string]string)

	// Look for groups data in the backend/sde directory
	groupsFile := filepath.Join("sde", "groups.jsonl")

	if _, err := os.Stat(groupsFile); os.IsNotExist(err) {
		return nil, fmt.Errorf("groups.jsonl not found in sde directory")
	}

	log.Printf("Reading groups from %s", groupsFile)

	// Read the file line by line
	content, err := os.ReadFile(groupsFile) // #nosec G304 -- path is filepath.Join("sde","groups.jsonl")
	if err != nil {
		return nil, fmt.Errorf("failed to read groups file: %v", err)
	}

	// Split by lines
	lines := strings.Split(string(content), "\n")

	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var groupData ItemGroup
		if err := json.Unmarshal([]byte(line), &groupData); err != nil {
			log.Printf("Warning: failed to parse group line %d: %v", i+1, err)
			continue
		}

		// Extract English name
		if enName, ok := groupData.Name["en"]; ok {
			groups[fmt.Sprintf("%d", groupData.Key)] = enName
		}
	}

	log.Printf("Loaded %d groups from SDE", len(groups))
	return groups, nil
}

// getGroupIDToCategoryIDFromSDE reads groups.jsonl and returns groupID -> categoryID.
// In EVE SDE, categoryID corresponds to the `_key` in categories.jsonl.
func getGroupIDToCategoryIDFromSDE() (map[int]int, error) {
	groupToCategory := make(map[int]int)

	// Look for groups data in the backend/sde directory
	groupsFile := filepath.Join("sde", "groups.jsonl")

	if _, err := os.Stat(groupsFile); os.IsNotExist(err) {
		return nil, fmt.Errorf("groups.jsonl not found in sde directory")
	}

	log.Printf("Reading groups categories from %s", groupsFile)

	content, err := os.ReadFile(groupsFile) // #nosec G304 -- path is filepath.Join("sde","groups.jsonl")
	if err != nil {
		return nil, fmt.Errorf("failed to read groups file: %v", err)
	}

	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var groupData ItemGroup
		if err := json.Unmarshal([]byte(line), &groupData); err != nil {
			log.Printf("Warning: failed to parse group line %d: %v", i+1, err)
			continue
		}

		groupToCategory[groupData.Key] = groupData.CategoryID
	}

	log.Printf("Loaded groupID->categoryID for %d groups", len(groupToCategory))
	return groupToCategory, nil
}

// getTypesFromSDE reads types data from the SDE
func getTypesFromSDE() (map[int]string, error) {
	types := make(map[int]string)

	// Look for types data in the backend/sde directory
	typesFile := filepath.Join("sde", "types.jsonl")

	if _, err := os.Stat(typesFile); os.IsNotExist(err) {
		return nil, fmt.Errorf("types.jsonl not found in sde directory")
	}

	log.Printf("Reading types from %s", typesFile)

	// Read the file line by line
	content, err := os.ReadFile(typesFile) // #nosec G304 -- path is filepath.Join("sde","types.jsonl")
	if err != nil {
		return nil, fmt.Errorf("failed to read types file: %v", err)
	}

	// Split by lines
	lines := strings.Split(string(content), "\n")

	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var typeData ItemType
		if err := json.Unmarshal([]byte(line), &typeData); err != nil {
			log.Printf("Warning: failed to parse type line %d: %v", i+1, err)
			continue
		}

		// Keep only types that exist in description.en.
		// This avoids selecting "non-ship" / broken labels that can otherwise appear in mock data.
		enDesc, hasDesc := typeData.Description["en"]
		if !hasDesc || strings.TrimSpace(enDesc) == "" {
			continue
		}

		// Extract English name
		if enName, ok := typeData.Name["en"]; ok && strings.TrimSpace(enName) != "" {
			types[typeData.Key] = enName
		}
	}

	log.Printf("Loaded %d types from SDE", len(types))
	return types, nil
}

// getTypeToGroupFromSDE reads typeID->groupID from the SDE types file
func getTypeToGroupFromSDE() (map[int]int, error) {
	result := make(map[int]int)
	typesFile := filepath.Join("sde", "types.jsonl")
	if _, err := os.Stat(typesFile); os.IsNotExist(err) {
		return nil, fmt.Errorf("types.jsonl not found (sde/types.jsonl)")
	}
	content, err := os.ReadFile(typesFile) // #nosec G304 -- path is filepath.Join("sde","types.jsonl")
	if err != nil {
		return nil, fmt.Errorf("failed to read types file: %v", err)
	}
	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var typeData ItemType
		if err := json.Unmarshal([]byte(line), &typeData); err != nil {
			log.Printf("Warning: failed to parse type line %d: %v", i+1, err)
			continue
		}
		result[typeData.Key] = typeData.GroupID
	}
	return result, nil
}

// getStargatesFromSDE reads stargate data from the SDE
func getStargatesFromSDE() (map[int][]Stargate, error) {
	stargates := make(map[int][]Stargate)

	// Look for stargates data in the extracted SDE
	stargatesFile := ""

	// Find the correct file in the extracted SDE
	err := filepath.Walk(sdeExtractDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && strings.HasSuffix(info.Name(), "mapStargates.jsonl") {
			stargatesFile = path
			return io.EOF // Stop walking
		}

		return nil
	})

	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("error searching for stargates file: %v", err)
	}

	if stargatesFile == "" {
		return nil, fmt.Errorf("mapStargates.jsonl not found in SDE")
	}

	log.Printf("Reading stargates from %s", stargatesFile)

	// Read the file line by line
	content, err := os.ReadFile(stargatesFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read stargates file: %v", err)
	}

	// Split by lines
	lines := strings.Split(string(content), "\n")

	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}

		var stargateData map[string]interface{}
		if err := json.Unmarshal([]byte(line), &stargateData); err != nil {
			log.Printf("Warning: failed to parse stargate line %d: %v", i+1, err)
			continue
		}

		// Extract stargate ID (_key)
		stargateID, ok := stargateData["_key"].(float64)
		if !ok {
			log.Printf("Warning: failed to extract stargate ID for line %d", i+1)
			continue
		}

		// Extract source system ID
		fromSystem, ok := stargateData["solarSystemID"].(float64)
		if !ok {
			log.Printf("Warning: failed to extract source system ID for stargate %d", int(stargateID))
			continue
		}

		// Extract destination system ID
		destination, ok := stargateData["destination"].(map[string]interface{})
		if !ok {
			log.Printf("Warning: failed to extract destination for stargate %d", int(stargateID))
			continue
		}

		toSystem, ok := destination["solarSystemID"].(float64)
		if !ok {
			log.Printf("Warning: failed to extract destination system ID for stargate %d", int(stargateID))
			continue
		}

		// Extract position
		var position [3]float64
		if posObj, ok := stargateData["position"].(map[string]interface{}); ok {
			// Position is stored as {"x": ..., "y": ..., "z": ...} object
			var xOk, yOk, zOk bool
			if x, ok := posObj["x"].(float64); ok {
				position[0] = x
				xOk = true
			}
			if y, ok := posObj["y"].(float64); ok {
				position[1] = y
				yOk = true
			}
			if z, ok := posObj["z"].(float64); ok {
				position[2] = z
				zOk = true
			}
			// Warn if any coordinate extraction failed
			if !xOk || !yOk || !zOk {
				log.Printf("Warning: failed to extract position coordinates for stargate %d (x:%v y:%v z:%v)", int(stargateID), xOk, yOk, zOk)
			}
		} else {
			log.Printf("Warning: failed to extract position for stargate %d (expected object with x, y, z keys)", int(stargateID))
			position = [3]float64{0, 0, 0}
		}

		// Add stargate connection (only in one direction since the SDE contains both directions)
		stargates[int(fromSystem)] = append(stargates[int(fromSystem)], Stargate{
			ID:                    int(stargateID),
			Position:              position,
			DestinationStargateID: int(toSystem),
		})
	}

	log.Printf("Loaded stargate connections from SDE")
	return stargates, nil
}

// loadSystemsFromSDE downloads, extracts, and loads systems and stargates from SDE
func loadSystemsFromSDE() ([]System, error) {
	// Download SDE if needed
	if err := downloadSDE(); err != nil {
		return nil, fmt.Errorf("failed to download SDE: %v", err)
	}

	// Extract SDE if needed
	if err := extractSDE(); err != nil {
		return nil, fmt.Errorf("failed to extract SDE: %v", err)
	}

	// Load systems
	systems, err := getSystemsFromSDE()
	if err != nil {
		return nil, fmt.Errorf("failed to load systems from SDE: %v", err)
	}

	// Load stargates
	stargates, err := getStargatesFromSDE()
	if err != nil {
		return nil, fmt.Errorf("failed to load stargates from SDE: %v", err)
	}

	// Combine systems and stargates
	for i := range systems {
		if sg, exists := stargates[systems[i].SystemID]; exists {
			systems[i].Stargates = sg
		} else {
			// Initialize empty slice for systems without stargates to avoid JSON null
			systems[i].Stargates = []Stargate{}
		}
	}

	return systems, nil
}
