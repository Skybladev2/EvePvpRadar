package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
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

	sdeEnsureOnce sync.Once
	sdeEnsureErr  error

	// sdeDownloadMu serializes index/table refreshes so startup and the
	// background stargate refresher cannot rewrite the manifest concurrently.
	sdeDownloadMu sync.Mutex
)

// ensureSDE refreshes the SDE at most once per process. Tables are version
// checked, so subsequent calls are cheap if the data is already current.
func ensureSDE() error {
	sdeEnsureOnce.Do(func() {
		sdeEnsureErr = downloadSDE()
	})
	return sdeEnsureErr
}

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
	GroupID              int               `json:"groupID"`
	Anchorable           bool              `json:"anchorable"`
	Anchored             bool              `json:"anchored"`
	CategoryID           int               `json:"categoryID"`
	FittableNonSingleton bool              `json:"fittableNonSingleton"`
	Name                 map[string]string `json:"name"`
	Published            bool              `json:"published"`
	UseBasePrice         bool              `json:"useBasePrice"`
}

// ItemType represents an EVE Online item type from the SDE
type ItemType struct {
	TypeID      int               `json:"typeID"`
	GroupID     int               `json:"groupID"`
	Name        map[string]string `json:"name"`
	Description map[string]string `json:"description"`
	Published   bool              `json:"published"`
}

const (
	// The SDE API serves one JSONL file per table. The index at sdeAPIBaseURL
	// redirects to the current release and gives release-pinned table URLs.
	sdeAPIBaseURL   = "https://sde.evetech.net/tranquility/latest/"
	sdeDataDir      = "sde"
	sdeManifestPath = "sde/.manifest.json"
)

// sdeRequiredTables are the tables the backend reads. Table versions are
// compared between runs so unchanged tables are not downloaded again.
var sdeRequiredTables = []string{
	"mapSolarSystems",
	"mapStargates",
	"npcStations",
	"groups",
	"types",
}

// sdeStargateTables are refreshed by the running application when re-checking
// the stargate layout. Only these tables affect the pathfinding graph.
var sdeStargateTables = []string{
	"mapSolarSystems",
	"mapStargates",
}

type sdeJSONLInfo struct {
	URL             string `json:"url"`
	RecordSchemaURL string `json:"recordSchemaUrl"`
}

type sdeTableIndex struct {
	Version string       `json:"version"`
	JSONL   sdeJSONLInfo `json:"jsonl"`
}

type sdeIndex struct {
	Release     string                   `json:"release"`
	BuildNumber int                      `json:"buildNumber"`
	ReleaseDate string                   `json:"releaseDate"`
	Tables      map[string]sdeTableIndex `json:"tables"`
}

type sdeManifest struct {
	Release     string            `json:"release"`
	BuildNumber int               `json:"buildNumber"`
	Tables      map[string]string `json:"tables"` // table name -> version
}

// downloadSDE fetches the SDE index and refreshes only the tables whose
// version changed since the last run. If the index cannot be reached but all
// tables are already on disk, the cached data is used.
func downloadSDE() error {
	return downloadSDETables(sdeRequiredTables)
}

// downloadSDETables refreshes only the requested SDE tables, preserving the
// recorded versions of every other table in the manifest.
func downloadSDETables(tables []string) error {
	sdeDownloadMu.Lock()
	defer sdeDownloadMu.Unlock()

	var index *sdeIndex
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<uint(attempt)) * time.Second
			log.Printf("Retrying SDE index fetch in %v (attempt %d/3)", wait, attempt+1)
			time.Sleep(wait)
		}
		index, lastErr = fetchSDEIndex()
		if lastErr == nil {
			break
		}
		log.Printf("SDE index fetch attempt %d/3 failed: %v", attempt+1, lastErr)
	}

	if lastErr != nil {
		if missing := missingSDETablesIn(tables); len(missing) == 0 {
			log.Printf("Warning: failed to fetch SDE index (%v); using cached SDE tables", lastErr)
			return nil
		} else {
			return fmt.Errorf("failed to fetch SDE index and tables missing (%s): %v", strings.Join(missing, ", "), lastErr)
		}
	}

	log.Printf("SDE release %s (build %d, %s)", index.Release, index.BuildNumber, index.ReleaseDate)

	previous := readSDEManifest()
	manifest := sdeManifest{
		Release:     index.Release,
		BuildNumber: index.BuildNumber,
		Tables:      make(map[string]string, len(previous.Tables)+len(tables)),
	}
	for table, version := range previous.Tables {
		manifest.Tables[table] = version
	}

	if err := ensureSDETables(index, tables, previous, &manifest); err != nil {
		return err
	}
	if err := writeSDEManifest(manifest); err != nil {
		log.Printf("Warning: failed to write SDE manifest: %v", err)
	}
	return nil
}

func fetchSDEIndex() (*sdeIndex, error) {
	initSDEHTTPClient()

	req, err := http.NewRequest("GET", sdeAPIBaseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create SDE index request: %v", err)
	}
	setUserAgent(req)
	req.Header.Set("Accept", "application/json")

	resp, err := sdeHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch SDE index: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch SDE index: HTTP %d", resp.StatusCode)
	}

	var index sdeIndex
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 32<<20))
	if err := decoder.Decode(&index); err != nil {
		return nil, fmt.Errorf("failed to decode SDE index: %v", err)
	}
	if index.Release == "" || len(index.Tables) == 0 {
		return nil, fmt.Errorf("SDE index does not contain a release or tables")
	}
	return &index, nil
}

func ensureSDETables(index *sdeIndex, tables []string, previous sdeManifest, manifest *sdeManifest) error {
	if err := os.MkdirAll(sdeDataDir, 0755); err != nil { // #nosec G301 -- 0755 for SDE data
		return fmt.Errorf("failed to create SDE directory: %v", err)
	}

	for _, table := range tables {
		info, ok := index.Tables[table]
		if !ok {
			return fmt.Errorf("SDE table %q not present in release %s", table, index.Release)
		}
		if info.JSONL.URL == "" {
			return fmt.Errorf("SDE table %q has no JSONL URL", table)
		}

		dest := filepath.Join(sdeDataDir, table+".jsonl")
		if info.Version != "" && previous.Tables[table] == info.Version && fileHasData(dest) {
			log.Printf("SDE table %s is up to date (version %s)", table, info.Version)
			manifest.Tables[table] = info.Version
			continue
		}

		if err := downloadSDETable(table, info.JSONL.URL, dest); err != nil {
			if fileHasData(dest) {
				log.Printf("Warning: failed to refresh SDE table %s (%v); using cached file", table, err)
				manifest.Tables[table] = previous.Tables[table]
				continue
			}
			return err
		}
		manifest.Tables[table] = info.Version
	}
	return nil
}

func downloadSDETable(table, tableURL, dest string) error {
	parsed, err := url.Parse(tableURL)
	if err != nil {
		return fmt.Errorf("invalid SDE URL for %s: %v", table, err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("refusing non-HTTPS SDE URL for %s: %s", table, tableURL)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		lastErr = downloadSDETableOnce(table, tableURL, dest)
		if lastErr == nil {
			return nil
		}
		log.Printf("SDE table %s download attempt %d/3 failed: %v", table, attempt+1, lastErr)
	}
	return fmt.Errorf("failed to download SDE table %s: %v", table, lastErr)
}

func downloadSDETableOnce(table, tableURL, dest string) error {
	initSDEHTTPClient()

	req, err := http.NewRequest("GET", tableURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	setUserAgent(req)

	resp, err := sdeHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	tmpPath := dest + ".tmp"
	out, err := os.Create(tmpPath) // #nosec G304 -- dest is built from a fixed table list
	if err != nil {
		return fmt.Errorf("failed to create temp file: %v", err)
	}
	tmpDone := false
	defer func() {
		_ = out.Close()
		if !tmpDone {
			_ = os.Remove(tmpPath)
		}
	}()

	if resp.ContentLength > 0 {
		log.Printf("Downloading SDE table %s (%.1f MB)", table, float64(resp.ContentLength)/1024/1024)
	} else {
		log.Printf("Downloading SDE table %s", table)
	}

	pw := newProgressWriter(out, table)
	go pw.reportLoop()

	written, err := io.Copy(pw, resp.Body)
	close(pw.done)

	if err != nil {
		return fmt.Errorf("failed to save table: %v", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %v", err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("failed to move table into place: %v", err)
	}
	tmpDone = true

	log.Printf("SDE table %s downloaded (%d bytes)", table, written)
	return nil
}

type progressWriter struct {
	wrapped io.Writer
	label   string
	total   int64
	ticker  *time.Ticker
	done    chan struct{}
	last    int64
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.wrapped.Write(p)
	pw.total += int64(n)
	return n, err
}

func (pw *progressWriter) reportLoop() {
	defer pw.ticker.Stop()
	for {
		select {
		case <-pw.ticker.C:
			written := pw.total
			delta := written - pw.last
			pw.last = written
			log.Printf("SDE %s download progress: %d bytes (%.1f MB/s)", pw.label, written, float64(delta)/1024/1024)
		case <-pw.done:
			return
		}
	}
}

func newProgressWriter(w io.Writer, label string) *progressWriter {
	return &progressWriter{
		wrapped: w,
		label:   label,
		ticker:  time.NewTicker(1 * time.Second),
		done:    make(chan struct{}),
	}
}

func fileHasData(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func missingSDETablesIn(tables []string) []string {
	var missing []string
	for _, table := range tables {
		if _, err := sdeTableFile(table); err != nil {
			missing = append(missing, table)
		}
	}
	return missing
}

func readSDEManifest() sdeManifest {
	manifest := sdeManifest{Tables: make(map[string]string)}
	content, err := os.ReadFile(sdeManifestPath) // #nosec G304 -- fixed path under sdeDataDir
	if err != nil {
		return manifest
	}
	if err := json.Unmarshal(content, &manifest); err != nil {
		log.Printf("Warning: failed to parse SDE manifest: %v", err)
		return sdeManifest{Tables: make(map[string]string)}
	}
	if manifest.Tables == nil {
		manifest.Tables = make(map[string]string)
	}
	return manifest
}

func writeSDEManifest(manifest sdeManifest) error {
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	tmpPath := sdeManifestPath + ".tmp"
	if err := os.WriteFile(tmpPath, content, 0600); err != nil { // #nosec G306 -- private cache metadata
		return err
	}
	return os.Rename(tmpPath, sdeManifestPath)
}

// sdeTableFile returns the local JSONL path for a table. The first name that
// exists wins; if none is present at the top level, nested legacy SDE layouts
// are searched as a fallback.
func sdeTableFile(names ...string) (string, error) {
	for _, name := range names {
		direct := filepath.Join(sdeDataDir, name+".jsonl")
		if fileHasData(direct) {
			return direct, nil
		}
	}

	var found string
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name+".jsonl"] = true
	}
	err := filepath.Walk(sdeDataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && wanted[info.Name()] {
			found = path
			return io.EOF
		}
		return nil
	})
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("error searching for SDE table %s: %v", strings.Join(names, "/"), err)
	}
	if found == "" {
		return "", fmt.Errorf("SDE table %s.jsonl not found", strings.Join(names, "/"))
	}
	return found, nil
}

// readJSONLines calls handle for every non-empty line of a JSONL file.
func readJSONLines(path string, handle func(line []byte)) error {
	f, err := os.Open(path) // #nosec G304 -- path comes from sdeTableFile
	if err != nil {
		return err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 256*1024)
	for {
		line, readErr := reader.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			handle(line)
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

type sdePosition struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// getSystemsFromSDE reads systems data from the SDE
func getSystemsFromSDE() ([]System, error) {
	systems := []System{}

	systemsFile, err := sdeTableFile("mapSolarSystems")
	if err != nil {
		return nil, err
	}
	log.Printf("Reading systems from %s", systemsFile)

	type solarSystemSDE struct {
		SolarSystemID  int               `json:"solarSystemID"`
		RegionID       int               `json:"regionID"`
		SecurityStatus *float64          `json:"securityStatus"`
		Name           map[string]string `json:"name"`
	}

	parseErr := readJSONLines(systemsFile, func(line []byte) {
		var systemData solarSystemSDE
		if err := json.Unmarshal(line, &systemData); err != nil {
			log.Printf("Warning: failed to parse solar system record: %v", err)
			return
		}
		systemName := systemData.Name["en"]
		if systemData.SolarSystemID == 0 || systemName == "" {
			log.Printf("Warning: skipping solar system %d without ID or English name", systemData.SolarSystemID)
			return
		}
		if systemData.SecurityStatus == nil {
			log.Printf("Warning: failed to extract security status for system %s", systemName)
			return
		}
		systems = append(systems, System{
			SystemID:   systemData.SolarSystemID,
			SystemName: systemName,
			Security:   *systemData.SecurityStatus,
			RegionID:   systemData.RegionID,
		})
	})
	if parseErr != nil {
		return nil, fmt.Errorf("failed to read systems file: %v", parseErr)
	}

	log.Printf("Loaded %d systems from SDE", len(systems))
	return systems, nil
}

// getGroupsFromSDE reads groups data from the SDE
func getGroupsFromSDE() (map[string]string, error) {
	groups := make(map[string]string)

	groupsFile, err := sdeTableFile("groups")
	if err != nil {
		return nil, err
	}
	log.Printf("Reading groups from %s", groupsFile)

	parseErr := readJSONLines(groupsFile, func(line []byte) {
		var groupData ItemGroup
		if err := json.Unmarshal(line, &groupData); err != nil {
			log.Printf("Warning: failed to parse group record: %v", err)
			return
		}
		if enName, ok := groupData.Name["en"]; ok {
			groups[fmt.Sprintf("%d", groupData.GroupID)] = enName
		}
	})
	if parseErr != nil {
		return nil, fmt.Errorf("failed to read groups file: %v", parseErr)
	}

	log.Printf("Loaded %d groups from SDE", len(groups))
	return groups, nil
}

// getGroupIDToCategoryIDFromSDE reads groups.jsonl and returns groupID -> categoryID.
// In the SDE, categoryID corresponds to the key in categories.jsonl.
func getGroupIDToCategoryIDFromSDE() (map[int]int, error) {
	groupToCategory := make(map[int]int)

	groupsFile, err := sdeTableFile("groups")
	if err != nil {
		return nil, err
	}
	log.Printf("Reading groups categories from %s", groupsFile)

	parseErr := readJSONLines(groupsFile, func(line []byte) {
		var groupData ItemGroup
		if err := json.Unmarshal(line, &groupData); err != nil {
			log.Printf("Warning: failed to parse group record: %v", err)
			return
		}
		groupToCategory[groupData.GroupID] = groupData.CategoryID
	})
	if parseErr != nil {
		return nil, fmt.Errorf("failed to read groups file: %v", parseErr)
	}

	log.Printf("Loaded groupID->categoryID for %d groups", len(groupToCategory))
	return groupToCategory, nil
}

// getTypesFromSDE reads types data from the SDE
func getTypesFromSDE() (map[int]string, error) {
	types := make(map[int]string)

	typesFile, err := sdeTableFile("types")
	if err != nil {
		return nil, err
	}
	log.Printf("Reading types from %s", typesFile)

	parseErr := readJSONLines(typesFile, func(line []byte) {
		var typeData ItemType
		if err := json.Unmarshal(line, &typeData); err != nil {
			log.Printf("Warning: failed to parse type record: %v", err)
			return
		}

		// Keep only types that exist in description.en.
		// This avoids selecting "non-ship" / broken labels that can otherwise appear in mock data.
		enDesc, hasDesc := typeData.Description["en"]
		if !hasDesc || strings.TrimSpace(enDesc) == "" {
			return
		}

		if enName, ok := typeData.Name["en"]; ok && strings.TrimSpace(enName) != "" {
			types[typeData.TypeID] = enName
		}
	})
	if parseErr != nil {
		return nil, fmt.Errorf("failed to read types file: %v", parseErr)
	}

	log.Printf("Loaded %d types from SDE", len(types))
	return types, nil
}

// getTypeToGroupFromSDE reads typeID->groupID from the SDE types file
func getTypeToGroupFromSDE() (map[int]int, error) {
	result := make(map[int]int)

	typesFile, err := sdeTableFile("types")
	if err != nil {
		return nil, err
	}
	log.Printf("Reading type groups from %s", typesFile)

	parseErr := readJSONLines(typesFile, func(line []byte) {
		var typeData ItemType
		if err := json.Unmarshal(line, &typeData); err != nil {
			log.Printf("Warning: failed to parse type record: %v", err)
			return
		}
		result[typeData.TypeID] = typeData.GroupID
	})
	if parseErr != nil {
		return nil, fmt.Errorf("failed to read types file: %v", parseErr)
	}
	return result, nil
}

// getStargatesFromSDE reads stargate data from the SDE
func getStargatesFromSDE() (map[int][]Stargate, error) {
	stargates := make(map[int][]Stargate)

	stargatesFile, err := sdeTableFile("mapStargates")
	if err != nil {
		return nil, err
	}
	log.Printf("Reading stargates from %s", stargatesFile)

	type stargateSDE struct {
		StargateID    int         `json:"stargateID"`
		SolarSystemID int         `json:"solarSystemID"`
		Position      sdePosition `json:"position"`
		Destination   struct {
			SolarSystemID int `json:"solarSystemID"`
			StargateID    int `json:"stargateID"`
		} `json:"destination"`
	}

	parseErr := readJSONLines(stargatesFile, func(line []byte) {
		var stargateData stargateSDE
		if err := json.Unmarshal(line, &stargateData); err != nil {
			log.Printf("Warning: failed to parse stargate record: %v", err)
			return
		}
		if stargateData.StargateID == 0 || stargateData.SolarSystemID == 0 || stargateData.Destination.SolarSystemID == 0 {
			log.Printf("Warning: skipping incomplete stargate record")
			return
		}

		// Only add one direction per record; the SDE contains both directions.
		stargates[stargateData.SolarSystemID] = append(stargates[stargateData.SolarSystemID], Stargate{
			ID:                    stargateData.StargateID,
			Position:              [3]float64{stargateData.Position.X, stargateData.Position.Y, stargateData.Position.Z},
			DestinationStargateID: stargateData.Destination.SolarSystemID,
		})
	})
	if parseErr != nil {
		return nil, fmt.Errorf("failed to read stargates file: %v", parseErr)
	}

	log.Printf("Loaded stargate connections from SDE")
	return stargates, nil
}

// StationSDE represents an NPC station from the SDE npcStations file
type StationSDE struct {
	StationID int
	Name      string
	SystemID  int
	Position  [3]float64
}

// getStationsFromSDE reads station data from npcStations.jsonl. Older SDE
// layouts exposed staStations.jsonl with station names; those are still
// accepted so previously extracted data keeps working.
func getStationsFromSDE() ([]StationSDE, error) {
	stations := []StationSDE{}

	stationsFile, err := sdeTableFile("npcStations", "staStations")
	if err != nil {
		return nil, err
	}
	log.Printf("Reading stations from %s", stationsFile)

	type stationSDE struct {
		StationID     int         `json:"stationID"`
		StationName   string      `json:"stationName"`
		SolarSystemID int         `json:"solarSystemID"`
		Position      sdePosition `json:"position"`
	}

	parseErr := readJSONLines(stationsFile, func(line []byte) {
		var stationData stationSDE
		if err := json.Unmarshal(line, &stationData); err != nil {
			log.Printf("Warning: failed to parse station record: %v", err)
			return
		}
		if stationData.StationID == 0 || stationData.SolarSystemID == 0 {
			return
		}
		name := stationData.StationName
		if name == "" {
			// The SDE API npcStations table has no station names.
			name = fmt.Sprintf("Station %d", stationData.StationID)
		}
		stations = append(stations, StationSDE{
			StationID: stationData.StationID,
			Name:      name,
			SystemID:  stationData.SolarSystemID,
			Position:  [3]float64{stationData.Position.X, stationData.Position.Y, stationData.Position.Z},
		})
	})
	if parseErr != nil {
		return nil, fmt.Errorf("failed to read stations file: %v", parseErr)
	}

	log.Printf("Loaded %d stations from SDE", len(stations))
	return stations, nil
}

// loadSystemsFromSDE downloads and loads systems and stargates from SDE.
func loadSystemsFromSDE() ([]System, error) {
	if err := ensureSDE(); err != nil {
		return nil, fmt.Errorf("failed to download SDE: %v", err)
	}
	return loadSystemsFromSDEFiles()
}

// refreshSystemsFromSDE re-checks the SDE index and reloads systems/stargates.
// Unlike ensureSDE it is safe to call repeatedly: unchanged tables are skipped
// by version, so a refresh only downloads what the SDE release changed.
func refreshSystemsFromSDE() ([]System, error) {
	if err := downloadSDETables(sdeStargateTables); err != nil {
		return nil, fmt.Errorf("failed to refresh SDE stargate tables: %v", err)
	}
	return loadSystemsFromSDEFiles()
}

// loadSystemsFromSDEFiles reads the already-downloaded systems and stargates
// tables and joins stargate connections onto their systems.
func loadSystemsFromSDEFiles() ([]System, error) {
	systems, err := getSystemsFromSDE()
	if err != nil {
		return nil, fmt.Errorf("failed to load systems from SDE: %v", err)
	}

	stargates, err := getStargatesFromSDE()
	if err != nil {
		return nil, fmt.Errorf("failed to load stargates from SDE: %v", err)
	}

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
