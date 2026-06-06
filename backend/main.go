// https://docs.esi.evetech.net/docs/esi_introduction.html
// https://developers.eveonline.com/blog/article/error-limiting-imminent
// https://github.com/zKillboard/zKillboard/wiki/API-(Killmails)
// https://github.com/zKillboard/zKillboard/wiki/API-(R2Z2) — ephemeral killmail stream (default)
// https://zkillboard.com/api/kills/systemID/30003935/
// https://zkillboard.com/api/kills/killID/120558741/
// https://esi.evetech.net/ui/
// https://esi.evetech.net/latest/killmails/120558741/124028029c064e32fadc427c4f2eea5d8e2d6428/?datasource=tranquility
// https://docs.esi.evetech.net/docs/sso/web_based_sso_flow.html

package main

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"io/fs"

	"evepvpsearch/doublebuffer"
	"evepvpsearch/logging"
	"evepvpsearch/routefinder"
	zkillboardcache "evepvpsearch/zkillboard-cache"

	"github.com/joho/godotenv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:embed static
var staticFS embed.FS

// standingHumanIconHTML is the inline SVG for the single-system pilot icon (from static/images/standing-human.svg), loaded once.
// runningHumanIconHTML is the inline SVG for the multi-system pilot icon (from static/images/running-human.svg), loaded once.
var (
	standingHumanIconHTML string
	standingHumanIconOnce sync.Once

	runningHumanIconHTML string
	runningHumanIconOnce sync.Once

	filterIconHTML string
	filterIconOnce sync.Once
)

const (
	uedamaScoutTwitchURL          = "https://www.twitch.tv/uedamascout"
	uedamaScoutStreamStatusTTL    = 60 * time.Second
	uedamaScoutStreamProbeTimeout = 5 * time.Second
)

var (
	uedamaScoutMonitoredSystems = map[string]struct{}{
		"uedama":    {},
		"sivala":    {},
		"gheth":     {},
		"ohide":     {},
		"odin":      {},
		"ahbazon":   {},
		"tama":      {},
		"josekorn":  {},
		"sasoutikh": {},
		"esescama":  {},
	}
	uedamaScoutLiveStatusMu      sync.Mutex
	uedamaScoutLiveStatusOnline  bool
	uedamaScoutLiveStatusChecked time.Time
	uedamaScoutProbeStartedAt    time.Time
)

var ErrUnauthorized = errors.New("esi unauthorized")

func getStandingHumanIconHTML() string {
	standingHumanIconOnce.Do(func() {
		data, err := staticFS.ReadFile("static/images/standing-human.svg")
		if err != nil {
			return
		}
		svg := string(data)
		svg = strings.TrimSpace(svg)
		// Inject class and size. viewBox is 9.1×24 so use height 1em and width 9.1/24 em so the box matches the figure (no letterboxing).
		svg = strings.Replace(svg, "<svg ", "<svg class='pilot-icon' width='0.38em' height='1em' aria-hidden='true' focusable='false' role='presentation' ", 1)
		standingHumanIconHTML = svg
	})
	return standingHumanIconHTML
}

func getRunningHumanIconHTML() string {
	runningHumanIconOnce.Do(func() {
		data, err := staticFS.ReadFile("static/images/running-human.svg")
		if err != nil {
			return
		}
		svg := string(data)
		svg = strings.TrimSpace(svg)
		// running-human.svg viewBox is 90×84.4 so set width to keep aspect ratio (avoid letterboxing).
		svg = strings.Replace(svg, "<svg ", "<svg class='pilot-icon' width='1.07em' height='1em' aria-hidden='true' focusable='false' role='presentation' ", 1)
		runningHumanIconHTML = svg
	})
	return runningHumanIconHTML
}

func getFilterIconHTML() string {
	filterIconOnce.Do(func() {
		data, err := staticFS.ReadFile("static/images/filter.svg")
		if err != nil {
			return
		}
		svg := string(data)
		svg = strings.TrimSpace(svg)
		svg = strings.Replace(svg, "<svg ", "<svg class='pilot-filter-icon' width='0.75em' height='0.75em' aria-hidden='true' focusable='false' role='presentation' ", 1)
		filterIconHTML = svg
	})
	return filterIconHTML
}

func getFilterIconHTMLOrFallback() string {
	f := getFilterIconHTML()
	if f != "" {
		return f
	}
	return "<svg class='pilot-filter-icon' viewBox='0 0 24 24' width='0.75em' height='0.75em' aria-hidden='true' focusable='false' role='presentation'><path fill='currentColor' d='M4 6h16v2H4V6zm3 5h10v2H7v-2zm-3 5h16v2H4v-2z'/></svg>"
}

func getTwitchIconHTMLOrFallback() string {
	return "<svg class='system-twitch-icon' viewBox='0 0 24 24' width='0.85em' height='0.85em' aria-hidden='true' focusable='false' role='presentation'><path fill='#9146FF' d='M3 2h18v13l-4 4h-3l-3 3v-3H7l-4-4V2zm2 2v10.2L7.8 17H11v1.8l1.8-1.8h3.4L19 14.2V4H5zm5 8V7h2v5h-2zm5 0V7h2v5h-2z'/></svg>"
}

var (
	loginImageLargeDataURI template.URL
	loginImageSmallDataURI template.URL
	loginImageOnce         sync.Once
)

func embedLoginImages() {
	loginImageOnce.Do(func() {
		const (
			largePath = "static/images/eve-sso-login-black-large.png"
			smallPath = "static/images/eve-sso-login-black-small.png"
			largeURL  = "https://web.ccpgamescdn.com/eveonlineassets/developers/eve-sso-login-black-large.png"
			smallURL  = "https://web.ccpgamescdn.com/eveonlineassets/developers/eve-sso-login-black-small.png"
		)
		largeData, err := staticFS.ReadFile(largePath)
		if err == nil {
			loginImageLargeDataURI = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(largeData))
		} else {
			loginImageLargeDataURI = template.URL(largeURL)
			log.Printf("Failed to read embedded login image (large): %v", err)
		}
		smallData, err := staticFS.ReadFile(smallPath)
		if err == nil {
			loginImageSmallDataURI = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(smallData))
		} else {
			loginImageSmallDataURI = template.URL(smallURL)
			log.Printf("Failed to read embedded login image (small): %v", err)
		}
	})
}

func isUedamaScoutMonitoredSystem(systemName string) bool {
	_, ok := uedamaScoutMonitoredSystems[strings.ToLower(strings.TrimSpace(systemName))]
	return ok
}

func isUedamaScoutLive() bool {
	uedamaScoutLiveStatusMu.Lock()
	now := time.Now()
	if now.Sub(uedamaScoutLiveStatusChecked) < uedamaScoutStreamStatusTTL {
		online := uedamaScoutLiveStatusOnline
		uedamaScoutLiveStatusMu.Unlock()
		return online
	}
	probeStartedAt := now
	uedamaScoutProbeStartedAt = probeStartedAt
	uedamaScoutLiveStatusMu.Unlock()

	online := probeUedamaScoutLive()
	probeCompletedAt := time.Now()

	uedamaScoutLiveStatusMu.Lock()
	// If multiple probes overlap, only the most recently started probe is allowed
	// to update the cache so slower older probes cannot overwrite newer state.
	if probeStartedAt.Equal(uedamaScoutProbeStartedAt) {
		uedamaScoutLiveStatusOnline = online
		uedamaScoutLiveStatusChecked = probeCompletedAt
	} else {
		online = uedamaScoutLiveStatusOnline
	}
	uedamaScoutLiveStatusMu.Unlock()
	return online
}

func probeUedamaScoutLive() bool {
	client := &http.Client{Timeout: uedamaScoutStreamProbeTimeout}
	req, err := http.NewRequest(http.MethodGet, uedamaScoutTwitchURL, nil)
	if err != nil {
		logging.Debugf("Could not build Twitch live request: %v", err)
		return false
	}
	req.Header.Set("User-Agent", "EvePvPRadar/1.0")

	resp, err := client.Do(req)
	if err != nil {
		logging.Debugf("Could not probe Twitch live status: %v", err)
		return false
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		logging.Debugf("Could not read Twitch live response: %v", err)
		return false
	}

	page := strings.ToLower(string(body))
	return strings.Contains(page, "\"islivebroadcast\":true") ||
		strings.Contains(page, "\"is_live\":true") ||
		strings.Contains(page, "\"islive\":true")
}

func getStandingHumanIconHTMLOrFallback() string {
	standing := getStandingHumanIconHTML()
	if standing != "" {
		return standing
	}
	// Fallback if file missing: minimal inline standing figure.
	return "<svg class='pilot-icon' viewBox='0 0 24 24' width='1em' height='1em' aria-hidden='true' focusable='false' role='presentation'><circle cx='12' cy='7' r='2.2' fill='#9a9a9a'/><rect x='10' y='9' width='4' height='8' rx='1' fill='#9a9a9a'/><rect x='9' y='17' width='2' height='5' fill='#9a9a9a'/><rect x='13' y='17' width='2' height='5' fill='#9a9a9a'/></svg>"
}

func getRunningHumanIconHTMLOrFallback() string {
	running := getRunningHumanIconHTML()
	if running != "" {
		return running
	}
	// Fallback if file missing: minimal inline "running" mark.
	return "<svg class='pilot-icon' viewBox='0 0 24 24' width='1em' height='1em' aria-hidden='true' focusable='false' role='presentation'>" +
		"<circle cx='12' cy='12' r='8.2' fill='none' stroke='#9a9a9a' stroke-width='2.2'/>" +
		"<circle cx='12' cy='12' r='5.6' fill='none' stroke='#9a9a9a' stroke-width='2.2'/>" +
		// Inner "chevron" hint.
		"<path d='M10 9.8l2 2.2-2 2.2h2.1l2-2.2-2-2.2H10z' fill='#9a9a9a'/>" +
		"</svg>"
}

// Embedded ship icons: one <style> block with data URLs so killmail tables do not trigger one HTTP request per icon.
var (
	shipIconsEmbeddedStyle     template.HTML
	shipIconsEmbeddedStyleOnce sync.Once
)

func shipIconCSSClassSuffixOK(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}

func getShipIconsEmbeddedStyle() template.HTML {
	shipIconsEmbeddedStyleOnce.Do(func() {
		entries, err := fs.ReadDir(staticFS, "static/icons")
		if err != nil {
			log.Printf("embedded ship icons: could not read static/icons: %v", err)
			return
		}
		var b strings.Builder
		b.WriteString("<style id=\"embedded-ship-icons\">\n")
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(strings.ToLower(name), ".png") {
				continue
			}
			data, err := staticFS.ReadFile("static/icons/" + name)
			if err != nil {
				log.Printf("embedded ship icons: skip %s: %v", name, err)
				continue
			}
			base := strings.TrimSuffix(name, ".png")
			if !shipIconCSSClassSuffixOK(base) {
				continue
			}
			b64 := base64.StdEncoding.EncodeToString(data)
			b.WriteString(".ship-type-icon.ship-type-icon--")
			b.WriteString(base)
			b.WriteString("{background-image:url(data:image/png;base64,")
			b.WriteString(b64)
			b.WriteString(")}\n")
		}
		b.WriteString("</style>\n")
		shipIconsEmbeddedStyle = template.HTML(b.String()) // #nosec G203 -- built only from embedded PNGs
	})
	return shipIconsEmbeddedStyle
}

// Site root unique visitor tracking (IP + User-Agent) for Prometheus/Grafana.
// Counter increments by 1 immediately on each new unique visitor (first-time only).
// Prometheus aggregates via increase(...[1m]) for per-minute bars.
var (
	siteRootVisitorsMu     sync.Mutex
	siteRootEverSeen       = map[string]struct{}{} // visitor keys ever seen (first-time = increment counter)
	siteRootNewUniqueTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "evepvpsearch_site_root_new_unique_visitors_total",
		Help: "Total new unique visitors (IP + User-Agent) to site root; use increase(...[1m]) for per-minute rate",
	})
	// HTTP request count by traffic source (frontend = via nginx proxy, backend = direct) and status code.
	// Use increase(evepvpsearch_http_requests_total[1m]) in Grafana for requests per minute by status.
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "evepvpsearch_http_requests_total",
			Help: "Total HTTP requests by source (frontend|backend) and status code; use increase(...[1m]) for req/min",
		},
		[]string{"source", "status_code"},
	)
	proximityRouteCacheEntriesGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "evepvpsearch_proximity_route_cache_entries",
		Help: "Current number of entries in proximity route cache",
	})
	proximityRouteCacheInvalidationsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "evepvpsearch_proximity_route_cache_invalidations_total",
		Help: "Total number of full proximity route cache invalidations caused by Thera signature fingerprint changes",
	})
)

func siteRootVisitorKey(ip, userAgent string) string {
	h := sha256.Sum256([]byte(ip + "\n" + userAgent))
	return hex.EncodeToString(h[:])
}

func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func recordSiteRootVisitor(ip, userAgent string) {
	key := siteRootVisitorKey(ip, userAgent)
	siteRootVisitorsMu.Lock()
	defer siteRootVisitorsMu.Unlock()
	if _, seen := siteRootEverSeen[key]; seen {
		return
	}
	siteRootEverSeen[key] = struct{}{}
	siteRootNewUniqueTotal.Add(1)
}

// statusCodeRecorder wraps ResponseWriter to capture the HTTP status code for metrics.
type statusCodeRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusCodeRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

// metricsMiddleware records evepvpsearch_http_requests_total by source (frontend=via proxy, backend=direct) and status code.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		source := "backend"
		if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" {
			source = "frontend"
		}
		rec := &statusCodeRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)
		httpRequestsTotal.WithLabelValues(source, strconv.Itoa(rec.statusCode)).Inc()
	})
}

type Stargate struct {
	ID                    int        `json:"id"`
	Position              [3]float64 `json:"position"`
	DestinationStargateID int        `json:"destination"`
}
type System struct {
	SystemID   int        `json:"system_id"`
	SystemName string     `json:"system_name"`
	Stargates  []Stargate `json:"stargates"`
	Security   float64    `json:"security"`
	RegionID   int        `json:"region_id"`
}

type StargateInfo struct {
	StargateID            int    `json:"stargate_id"`
	DestinationSystemID   int    `json:"destination_system_id"`
	DestinationSystemName string `json:"destination_system_name,omitempty"`
}

type CachedKillmail struct {
	KillmailID            int            `json:"killmail_id"`
	KillmailTime          string         `json:"killmail_time"`
	Victim                ESIVictim      `json:"victim"`
	Attackers             []ESIAttacker  `json:"attackers"`
	ZKBInfo               ZKillboardKill `json:"zkb"`
	SolarSystemID         int            `json:"solar_system_id"`                    // Added for Near trade hubs mode compatibility
	KillLocation          *[3]float64    `json:"kill_location,omitempty"`            // Kill position in 3D space (x, y, z in meters) - calculated
	MinDistanceToStargate *float64       `json:"min_distance_to_stargate,omitempty"` // Minimum distance to nearest stargate in meters
	StargateInfo          *StargateInfo  `json:"stargate_info,omitempty"`            // Information about the nearest stargate
}

type SystemInRange struct {
	SystemID               int
	Name                   string
	Dist                   int
	Security               float64
	ViaThera               bool             `json:"ViaThera,omitempty"`
	TheraDist              int              `json:"TheraDist,omitempty"`
	TheraInfo              string           `json:"TheraInfo,omitempty"`
	TheraInboundSignature  string           `json:"TheraInboundSignature,omitempty"`
	TheraOutboundSignature string           `json:"TheraOutboundSignature,omitempty"`
	MaxShipSize            string           `json:"MaxShipSize,omitempty"` // Maximum ship size for Thera route
	Route                  []EveScoutSystem `json:"Route,omitempty"`
	RecentKills            []CachedKillmail `json:"recent_kills,omitempty"`
	TradeHub               string           `json:"trade_hub,omitempty"` // Trade hub name for Near trade hubs mode
	Weight                 float64          `json:"weight,omitempty"`    // Weight for Near trade hubs mode
}

func (s System) AdjacentSystemIDs() []int {
	if s.Stargates == nil {
		return []int{}
	}
	adjacent := make([]int, 0, len(s.Stargates))
	for _, stargate := range s.Stargates {
		for _, sys := range systems {
			// Skip systems with null stargates
			if sys.Stargates == nil {
				continue
			}
			for _, sysStargate := range sys.Stargates {
				if stargate.DestinationStargateID == sysStargate.ID {
					adjacent = append(adjacent, sys.SystemID)
				}
			}
		}
	}
	return adjacent
}

var systems []System
var itemGroups = make(map[string]string)
var groupIDToCategoryID map[int]int
var types map[int]string
var typeIDToGroupName map[int]string
var typeIDToGroupID map[int]int // typeID -> groupID, for weapon/ship group checks
var logFile io.Writer
var lastAPIRequest time.Time
var globalRouteFinder *routefinder.RouteFinder
var killmailCache *zkillboardcache.Cache
var mockData bool // Use mock data when MOCK_DATA=1 or true

// SSO related structures and variables
// Authentication Duration:
// - Access tokens expire after 20 minutes (1200 seconds) by default in EVE SSO
// - Refresh tokens are long-lived and can be used indefinitely until revoked by the user
// - Tokens are automatically refreshed when:
//  1. A request is made and the token is within 5 minutes of expiration
//  2. A background worker refreshes tokens at 80% of their lifetime (16 minutes for a 20-minute token)
//
// - Users remain authenticated as long as they don't revoke access or the refresh token fails
type SSOSession struct {
	CharacterID   int
	CharacterName string
	AccessToken   string // #nosec G117 -- OAuth field name, not a hardcoded secret
	RefreshToken  string // #nosec G117 -- OAuth field name, not a hardcoded secret
	ExpiresAt     time.Time
}

var (
	ssoSessions     = make(map[string]*SSOSession) // sessionID -> SSOSession
	ssoSessionsMu   sync.RWMutex
	ssoStates       = make(map[string]time.Time) // state -> expiration time
	ssoStatesMu     sync.RWMutex
	ssoClientID     string
	ssoClientSecret string
	ssoRedirectURI  string
	ssoFrontendURL  string // Frontend URL for post-authentication redirects
	// Optional footer link (index.html): set DONATE_URL and DONATE_TEXT
	donateURL  string
	donateText string
	// httpUserAgent is set at startup: USER_AGENT (base string) + commit when set
	httpUserAgent string
	// Station ID cache: systemID -> stationID (0 means no station found or not yet fetched)
	systemStationCache   = make(map[int]int) // systemID -> stationID
	systemStationCacheMu sync.RWMutex
)

const (
	ssoAuthURL        = "https://login.eveonline.com/v2/oauth/authorize"
	ssoTokenURL       = "https://login.eveonline.com/v2/oauth/token" // #nosec G101 -- OAuth endpoint URL, not a credential
	ssoMetadataURL    = "https://login.eveonline.com/.well-known/oauth-authorization-server"
	ssoSessionName    = "eve_sso_session"
	ssoStateTimeout   = 10 * time.Minute
	ssoSessionTimeout = 24 * time.Hour
)

// corsAllowedOrigins is populated at startup from EVE_SSO_FRONTEND_URL and CORS_ALLOWED_ORIGINS.
// Credentialed CORS responses must only echo Access-Control-Allow-Origin for origins in this set.
var corsAllowedOrigins map[string]struct{}

// normalizeOriginKey returns scheme://host (lowercased) for an absolute URL, or "" if invalid.
func normalizeOriginKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

func initCORSAllowlist(mw io.Writer) {
	corsAllowedOrigins = make(map[string]struct{})
	if k := normalizeOriginKey(ssoFrontendURL); k != "" {
		corsAllowedOrigins[k] = struct{}{}
	}
	if extra := os.Getenv("CORS_ALLOWED_ORIGINS"); extra != "" {
		for _, part := range strings.Split(extra, ",") {
			if k := normalizeOriginKey(strings.TrimSpace(part)); k != "" {
				corsAllowedOrigins[k] = struct{}{}
			}
		}
	}
	fmt.Fprintf(mw, "CORS allowed origins: %d\n", len(corsAllowedOrigins))
}

// setCORSHeadersForCredentialedAPI sets CORS headers for endpoints that use session cookies cross-origin.
// Arbitrary Origin reflection with Access-Control-Allow-Credentials is not allowed (CSRF-style token theft).
func setCORSHeadersForCredentialedAPI(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin != "" {
		if _, ok := corsAllowedOrigins[origin]; ok {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Vary", "Origin")
		}
	} else {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
}

// cookieSecure returns whether the session cookie should use the Secure flag (HTTPS).
func cookieSecure(r *http.Request) bool {
	if v := strings.TrimSpace(strings.ToLower(os.Getenv("EVE_SSO_COOKIE_SECURE"))); v == "1" || v == "true" || v == "yes" {
		return true
	}
	if r != nil && r.TLS != nil {
		return true
	}
	if r != nil && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		return true
	}
	return false
}

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// commit is set at build time via -ldflags "-X main.commit=..."
var commit = ""

// containerTag returns the IMAGE_TAG from env, falling back to the ldflags commit value.
func containerTag() string {
	if tag := os.Getenv("IMAGE_TAG"); tag != "" {
		return tag
	}
	return commit
}

// Cache for full index HTML for unauthenticated requests (avoid rebuilding on every hit).
// Invalidated when precalculated data changes (new killmail, Thera signatures, or full recalc).
var (
	indexHTMLCache        []byte
	indexHTMLCacheMu      sync.RWMutex
	indexHTMLLastModified time.Time
	indexHTMLETag         string
)

// Ready-to-render HTML fragments built in the background.
// Requests serve the latest ready snapshot; when new data arrives we rebuild in background and swap atomically.
var (
	readyTablesMu          sync.RWMutex
	readyNearTradeHubsHTML string
	readyTheraCampsHTML    string
	readyTablesBuilding    bool
	readyTablesDirty       bool
)

// invalidateIndexHTMLCache now marks table HTML as dirty and schedules background rebuild.
// We intentionally keep serving the previous ready HTML until the new snapshot is ready.
func invalidateIndexHTMLCache() {
	readyTablesMu.Lock()
	readyTablesDirty = true
	shouldStart := !readyTablesBuilding
	if shouldStart {
		readyTablesBuilding = true
	}
	readyTablesMu.Unlock()

	if shouldStart {
		go rebuildReadyTablesHTML()
	}
}

func rebuildReadyTablesHTML() {
	for {
		log.Printf("Ready tables: rebuild started")
		// Build a fresh snapshot based on latest precalculated data.
		result := getNearTradeHubsResult()
		campKills := getTheraCampKills()

		// Collect attacker character IDs (only those shown in UI: up to 3 kills per system and up to 10 Thera camps)
		var characterIDs []int
		for _, system := range result {
			for i := 0; i < 3 && i < len(system.RecentKills); i++ {
				for _, a := range system.RecentKills[i].Attackers {
					if a.CharacterID != 0 {
						characterIDs = append(characterIDs, a.CharacterID)
					}
				}
			}
		}
		for i := 0; i < 10 && i < len(campKills); i++ {
			for _, a := range campKills[i].Attackers {
				if a.CharacterID != 0 {
					characterIDs = append(characterIDs, a.CharacterID)
				}
			}
		}

		characterNames, characterNameErrors := resolveCharacterNames(characterIDs) // single-attempt per character; failures render as missing

		newNearTradeHubs := renderHTMLTableWithNames(result, "near_trade_hubs", characterNames, characterNameErrors)
		newTheraCamps := renderTheraCampsHTMLWithNames(campKills, characterNames, characterNameErrors)

		// Swap atomically. Preserve whether we became dirty while building so we can rebuild again.
		readyTablesMu.Lock()
		rebuildAgain := readyTablesDirty
		readyNearTradeHubsHTML = newNearTradeHubs
		readyTheraCampsHTML = newTheraCamps
		readyTablesDirty = false
		readyTablesBuilding = rebuildAgain
		readyTablesMu.Unlock()
		log.Printf("Ready tables: rebuild completed (near_trade_hubs=%d bytes, thera_camps=%d bytes)", len(newNearTradeHubs), len(newTheraCamps))

		// Force index HTML cache rebuild on next request so it picks up the new fragments.
		indexHTMLCacheMu.Lock()
		indexHTMLCache = nil
		indexHTMLLastModified = time.Time{}
		indexHTMLETag = ""
		indexHTMLCacheMu.Unlock()

		// If we became dirty again while building, loop and rebuild again.
		if !rebuildAgain {
			return
		}
	}
}

type TradeHub struct {
	Name        string `json:"name"`
	SystemID    int    `json:"system_id"`
	Region      string `json:"region"`
	Type        string `json:"type"`
	StationName string `json:"station_name,omitempty"` // main trade station in hub (if different from jump clone station)
	StationID   int    `json:"station_id,omitempty"`   // station ID for the trade hub station (if known)
}

var tradeHubs []TradeHub

type JumpCloneStation struct {
	Name       string `json:"name"`
	StationID  int    `json:"station_id"` // Station ID from JSON (verified via ESI)
	SystemID   int    // SystemID populated from SDE based on station name
	SystemName string // SystemName populated from SDE based on station name
}

var jumpCloneStations []JumpCloneStation

const (
	TheraSystemID   = 31000005
	ZarzakhSystemID = 30100000
	// Thera station/structure IDs - kills at these are "at station", not camp-style
	TheraStationID1              = 60015148
	TheraStationID2              = 60015149
	TheraStationID3              = 60015150
	TheraStationID4              = 60015151
	maxRangeJumps                = 15 // Max search range for proximity mode
	nearTradeHubsMaxDisplayJumps = 15 // Display only routes <= 15 in near trade hubs mode
)

const ccpFooterHTML = `<footer id="ccp-disclaimer">EVE Online and the EVE logo are registered trademarks of CCP hf. All rights are reserved worldwide. All other trademarks are the property of their respective owners. EVE Online, the EVE logo, EVE and all associated logos and designs are the intellectual property of CCP hf. All artwork, screenshots, characters, vehicles, storylines, world facts or other recognizable features of the intellectual property relating to these trademarks are likewise the intellectual property of CCP hf. CCP hf. has granted permission to Eve PvP Radar to use EVE Online and all associated logos and designs for promotional and informational purposes on its website but does not endorse, and is not in any way affiliated with, Eve PvP Radar. CCP is in no way responsible for the content on or functioning of this website, nor can it be liable for any damage arising from the use of this website.</footer>`

// PrecalculatedSystemData stores precalculated data for a system with kills
type PrecalculatedSystemData struct {
	SystemID    int
	Name        string
	Security    float64
	Dist        int
	ViaThera    bool
	TheraDist   int
	TheraInfo   string
	MaxShipSize string
	Route       []EveScoutSystem
	RecentKills []CachedKillmail
	TradeHub    string
	Weight      float64
	LastUpdated time.Time
}

// PrecalculatedData stores all precalculated data, organized by mode and start system
type PrecalculatedData struct {
	// For normal mode: map[startSystemID][]PrecalculatedSystemData
	normalMode map[int][]PrecalculatedSystemData
	// For Near trade hubs mode: map[compoundKey]PrecalculatedSystemData
	// compoundKey = fmt.Sprintf("%d|%s", systemID, tradeHubName) to allow the same system to appear with different trade hubs
	nearTradeHubsMode map[string]PrecalculatedSystemData
	// Track which killmail IDs we've calculated (for Thera recalculation)
	calculatedKillmails map[int]time.Time // killmailID -> calculation time
	// Precalculated systems with kills: map[systemID][]CachedKillmail (fully processed killmails)
	systemsWithKills map[int][]CachedKillmail
}

// copyPrecalculatedData creates a deep copy of PrecalculatedData
func copyPrecalculatedData(src *PrecalculatedData) *PrecalculatedData {
	dst := &PrecalculatedData{
		normalMode:          make(map[int][]PrecalculatedSystemData),
		nearTradeHubsMode:   make(map[string]PrecalculatedSystemData),
		calculatedKillmails: make(map[int]time.Time),
		systemsWithKills:    make(map[int][]CachedKillmail),
	}

	// Deep copy normalMode map
	for k, v := range src.normalMode {
		dst.normalMode[k] = append([]PrecalculatedSystemData(nil), v...)
	}

	// Deep copy nearTradeHubsMode map
	for k, v := range src.nearTradeHubsMode {
		dst.nearTradeHubsMode[k] = v
	}

	// Deep copy calculatedKillmails map
	for k, v := range src.calculatedKillmails {
		dst.calculatedKillmails[k] = v
	}

	// Deep copy systemsWithKills map
	for k, v := range src.systemsWithKills {
		dst.systemsWithKills[k] = make([]CachedKillmail, len(v))
		copy(dst.systemsWithKills[k], v)
	}

	return dst
}

var precalculatedData = doublebuffer.NewDoubleBuffer(
	&PrecalculatedData{
		normalMode:          make(map[int][]PrecalculatedSystemData),
		nearTradeHubsMode:   make(map[string]PrecalculatedSystemData),
		calculatedKillmails: make(map[int]time.Time),
		systemsWithKills:    make(map[int][]CachedKillmail),
	},
	copyPrecalculatedData,
)

// recalcMu serializes EnsureRecalculated so only one goroutine runs the full recalculation (recalculateFromKills)
// when multiple HTTP requests are released after backfill pauses (Broadcast wakes all).
var (
	recalcMu         sync.Mutex
	recalcCond       = sync.NewCond(&recalcMu)
	recalcInProgress bool
)

// proximityRouteCache caches route results for proximity and near-trade-hubs modes.
// Entries are invalidated atomically when the Thera signatures fingerprint changes.
const proximityRouteCacheMaxSize = 10000

type proximityRouteCacheKey struct {
	from int
	to   int
}

type proximityRouteCacheEntry struct {
	viaThera         bool
	dist             int
	theraInfo        string
	theraInboundSig  string
	theraOutboundSig string
	maxShipSize      string
	route            []EveScoutSystem
}

var proximityRouteCache = struct {
	sync.Mutex
	entries map[proximityRouteCacheKey]proximityRouteCacheEntry
	order   *list.List // front = most recently used, back = least recently used
	index   map[proximityRouteCacheKey]*list.Element
	fp      string
}{
	entries: make(map[proximityRouteCacheKey]proximityRouteCacheEntry),
	order:   list.New(),
	index:   make(map[proximityRouteCacheKey]*list.Element),
}

func updateProximityRouteCacheMetricsLocked() {
	proximityRouteCacheEntriesGauge.Set(float64(len(proximityRouteCache.entries)))
}

func proximityRouteCacheRemoveKeyLocked(key proximityRouteCacheKey) {
	delete(proximityRouteCache.entries, key)
	if el, ok := proximityRouteCache.index[key]; ok {
		proximityRouteCache.order.Remove(el)
		delete(proximityRouteCache.index, key)
	}
	updateProximityRouteCacheMetricsLocked()
}

func refreshTheraAndGetFingerprint() string {
	if globalRouteFinder == nil {
		return ""
	}
	globalRouteFinder.EnsureTheraSignaturesFresh()
	return globalRouteFinder.GetTheraSignaturesFingerprint()
}

func proximityRouteCacheSyncFingerprint(currentFP string) {
	proximityRouteCache.Lock()
	defer proximityRouteCache.Unlock()

	// Empty cache (bootstrap) simply adopts current fingerprint.
	if proximityRouteCache.fp == "" {
		proximityRouteCache.fp = currentFP
		return
	}
	if proximityRouteCache.fp == currentFP {
		return
	}

	// Thera signatures changed: atomically invalidate entire proximity route cache.
	proximityRouteCache.entries = make(map[proximityRouteCacheKey]proximityRouteCacheEntry)
	proximityRouteCache.order = list.New()
	proximityRouteCache.index = make(map[proximityRouteCacheKey]*list.Element)
	proximityRouteCache.fp = currentFP
	proximityRouteCacheInvalidationsTotal.Inc()
	updateProximityRouteCacheMetricsLocked()
}

func proximityRouteCacheGet(key proximityRouteCacheKey) (proximityRouteCacheEntry, bool) {
	proximityRouteCache.Lock()
	defer proximityRouteCache.Unlock()

	ent, ok := proximityRouteCache.entries[key]
	if !ok {
		return proximityRouteCacheEntry{}, false
	}
	if el, ok := proximityRouteCache.index[key]; ok {
		proximityRouteCache.order.MoveToFront(el)
	}
	return ent, true
}

func proximityRouteCacheSet(key proximityRouteCacheKey, ent proximityRouteCacheEntry) {
	proximityRouteCache.Lock()
	defer proximityRouteCache.Unlock()

	if _, exists := proximityRouteCache.entries[key]; exists {
		proximityRouteCache.entries[key] = ent
		if el, ok := proximityRouteCache.index[key]; ok {
			proximityRouteCache.order.MoveToFront(el)
		}
	} else {
		proximityRouteCache.entries[key] = ent
		proximityRouteCache.index[key] = proximityRouteCache.order.PushFront(key)
		updateProximityRouteCacheMetricsLocked()
	}

	// Keep cache bounded: evict least-recently-used entries incrementally.
	for len(proximityRouteCache.entries) > proximityRouteCacheMaxSize {
		lruEl := proximityRouteCache.order.Back()
		if lruEl == nil {
			break
		}
		lruKey, ok := lruEl.Value.(proximityRouteCacheKey)
		if !ok {
			proximityRouteCache.order.Remove(lruEl)
			continue
		}
		proximityRouteCacheRemoveKeyLocked(lruKey)
	}
	updateProximityRouteCacheMetricsLocked()
}

// loadTradeHubs loads trade hubs from JSON file at startup
func loadTradeHubs(mw io.Writer) {
	tradeHubsFile, err := os.Open("./data/qualified_trade_hubs.json")
	if err != nil {
		fmt.Fprintln(mw, "Warning: Could not load trade hubs:", err)
		return
	}
	defer tradeHubsFile.Close()

	decoder := json.NewDecoder(tradeHubsFile)
	if err := decoder.Decode(&tradeHubs); err != nil {
		fmt.Fprintln(mw, "Warning: Could not decode trade hubs:", err)
		return
	}

	fmt.Fprintln(mw, "Loaded", len(tradeHubs), "trade hubs")
}

// getSystemIDByName finds a system ID by system name
func getSystemIDByName(systemName string) int {
	for _, sys := range systems {
		if strings.EqualFold(sys.SystemName, systemName) {
			return sys.SystemID
		}
	}
	return 0
}

// extractSystemNameFromStationName extracts the system name from a station name
// Station names follow pattern: "SystemName ..." (first word is system name)
func extractSystemNameFromStationName(stationName string) string {
	parts := strings.Fields(stationName)
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

// loadJumpCloneStations loads jump clone service stations from JSON file at startup
// Station IDs are read from JSON (no ESI calls)
func loadJumpCloneStations(mw io.Writer) {
	jumpCloneStationsFile, err := os.Open("./data/jump_clone_stations.json")
	if err != nil {
		fmt.Fprintln(mw, "Warning: Could not load jump clone stations:", err)
		return
	}
	defer jumpCloneStationsFile.Close()

	var stations []struct {
		Name       string `json:"name"`
		StationID  int    `json:"station_id"`
		SystemID   int    `json:"system_id"`
		SystemName string `json:"system_name"`
	}
	decoder := json.NewDecoder(jumpCloneStationsFile)
	if err := decoder.Decode(&stations); err != nil {
		fmt.Fprintln(mw, "Warning: Could not decode jump clone stations:", err)
		return
	}

	fmt.Fprintf(mw, "Loading %d jump clone service stations from JSON...\n", len(stations))

	jumpCloneStations = make([]JumpCloneStation, 0, len(stations))
	for i, station := range stations {
		fmt.Fprintf(mw, "[%d/%d] Loading station: %s\n", i+1, len(stations), station.Name)

		if station.StationID == 0 {
			fmt.Fprintf(mw, "  ERROR: Station %s has no station_id in JSON\n", station.Name)
			continue
		}

		systemID := station.SystemID
		systemName := station.SystemName

		if systemID == 0 || systemName == "" {
			systemNameStr := extractSystemNameFromStationName(station.Name)
			if systemNameStr == "" {
				fmt.Fprintf(mw, "  ERROR: Could not extract system name from station name: %s\n", station.Name)
				continue
			}
			systemID = getSystemIDByName(systemNameStr)
			if systemID == 0 {
				fmt.Fprintf(mw, "  ERROR: Could not find system ID for system name: %s\n", systemNameStr)
				continue
			}
			sys := getSystemById(systemID)
			if sys.SystemID == 0 {
				fmt.Fprintf(mw, "  ERROR: System ID %d not found in SDE\n", systemID)
				continue
			}
			systemName = sys.SystemName
		}

		jumpCloneStations = append(jumpCloneStations, JumpCloneStation{
			Name:       station.Name,
			StationID:  station.StationID,
			SystemID:   systemID,
			SystemName: systemName,
		})
		fmt.Fprintf(mw, "  SUCCESS: Loaded %s -> system: %s (ID: %d), station_id: %d\n",
			station.Name, systemName, systemID, station.StationID)
	}

	fmt.Fprintf(mw, "Loaded %d/%d jump clone service stations from JSON\n", len(jumpCloneStations), len(stations))
	if len(jumpCloneStations) == 0 {
		log.Printf("WARNING: No jump clone stations loaded! Check startup logs for errors.")
	} else {
		log.Printf("Successfully loaded %d jump clone stations", len(jumpCloneStations))
		for _, station := range jumpCloneStations {
			log.Printf("  - %s (System: %s, SystemID: %d, StationID: %d)", station.Name, station.SystemName, station.SystemID, station.StationID)
		}
	}
}

// calculateDistance calculates the Euclidean distance between two 3D points
func calculateDistance(pos1, pos2 [3]float64) float64 {
	dx := pos1[0] - pos2[0]
	dy := pos1[1] - pos2[1]
	dz := pos1[2] - pos2[2]
	return math.Sqrt(dx*dx + dy*dy + dz*dz)
}

// getStargateInfo gets stargate information from a locationID in a system
// Returns: stargate info, error
func getStargateInfo(systemID int, locationID int) (*StargateInfo, error) {
	// Get the system
	system := getSystemById(systemID)
	if system.SystemID == 0 {
		return nil, fmt.Errorf("system %d not found", systemID)
	}

	// Find the stargate that matches the locationID
	var targetStargate *Stargate
	for i := range system.Stargates {
		if system.Stargates[i].ID == locationID {
			targetStargate = &system.Stargates[i]
			break
		}
	}

	if targetStargate == nil {
		return nil, fmt.Errorf("locationID %d is not a stargate ID in system %d", locationID, systemID)
	}

	// DestinationStargateID is actually the destination system ID (despite the misleading name)
	destinationSystemID := targetStargate.DestinationStargateID
	destinationSystem := getSystemById(destinationSystemID)
	destinationSystemName := ""
	if destinationSystem.SystemID != 0 {
		destinationSystemName = destinationSystem.SystemName
	}

	return &StargateInfo{
		StargateID:            targetStargate.ID,
		DestinationSystemID:   destinationSystemID,
		DestinationSystemName: destinationSystemName,
	}, nil
}

// getKillPositionAndDistance gets the kill position from killmail and calculates the distance to the stargate specified by locationID
// Returns: killPosition, distance, isWithinRange, stargateInfo, error
func getKillPositionAndDistance(systemID int, locationID int, killmail *ESIKillmail) ([3]float64, float64, bool, *StargateInfo, error) {
	// Convert 1000 km to meters (EVE uses meters for positions)
	const maxDistance = 1000000.0 // 1000 km in meters

	// Get the system
	system := getSystemById(systemID)
	if system.SystemID == 0 {
		return [3]float64{}, 0, false, nil, fmt.Errorf("system %d not found", systemID)
	}

	// Get kill position from killmail victim
	var killPosition [3]float64
	if killmail.Victim.Position == nil {
		return [3]float64{}, 0, false, nil, fmt.Errorf("killmail %d does not have position data", killmail.KillmailID)
	}
	killPosition = [3]float64{killmail.Victim.Position.X, killmail.Victim.Position.Y, killmail.Victim.Position.Z}

	// Find the stargate that matches the locationID
	var targetStargate *Stargate
	for i := range system.Stargates {
		if system.Stargates[i].ID == locationID {
			targetStargate = &system.Stargates[i]
			break
		}
	}

	if targetStargate == nil {
		// locationID is not a stargate ID - skip this kill as it's not at a stargate
		return killPosition, 0, false, nil, fmt.Errorf("locationID %d is not a stargate ID in system %d, skipping kill", locationID, systemID)
	}

	// Check if stargate has valid position
	if targetStargate.Position[0] == 0 && targetStargate.Position[1] == 0 && targetStargate.Position[2] == 0 {
		return killPosition, 0, false, nil, fmt.Errorf("stargate %d in system %d has invalid position [0,0,0]", targetStargate.ID, systemID)
	}

	// Calculate distance from kill position to stargate
	distance := calculateDistance(killPosition, targetStargate.Position)
	isWithinRange := distance <= maxDistance

	// Get stargate info
	stargateInfo, err := getStargateInfo(systemID, locationID)
	if err != nil {
		// Log error but don't fail - stargate info is optional
		log.Printf("Warning: failed to get stargate info: %v", err)
	}

	return killPosition, distance, isWithinRange, stargateInfo, nil
}

// Station name to ID cache: "systemID:stationName" -> stationID
var stationNameCache = make(map[string]int) // "systemID:stationName" -> stationID
var stationNameCacheMu sync.RWMutex

// Station ID verification cache: stationID -> {systemID, stationName, verified}
var stationVerificationCache = make(map[int]struct {
	SystemID    int
	StationName string
	Verified    bool
})
var stationVerificationCacheMu sync.RWMutex

// Caches for station lookup to avoid repeated ESI requests
var systemInfoCache = make(map[int][]int) // systemID -> stationIDs
var systemInfoCacheMu sync.RWMutex

var stationDetailCache = make(map[int]struct {
	Name     string
	SystemID int
}) // stationID -> {name, systemID}
var stationDetailCacheMu sync.RWMutex

// Thera station positions cache (meters); 10000 km = 10e6 m
const theraCampMinDistanceFromStation = 10_000_000.0

var theraStationPositionsCache = make(map[int][3]float64)
var theraStationPositionsMu sync.RWMutex

// characterNameCache caches character ID -> name from ESI (GET /characters/{id}/).
// ESI rate limiting: https://developers.eveonline.com/docs/services/esi/rate-limiting/
// We use low concurrency, staggered requests, and long cache to stay within limits.
var (
	characterNameCache = make(map[int]struct {
		name   string
		errMsg string // user-facing tooltip when name lookup failed
		expiry time.Time
	})
	characterNameCacheMu     sync.RWMutex
	characterNameCacheTTL    = time.Hour
	characterResolveSem      = make(chan struct{}, 2) // low concurrency to avoid bursting (best practice: don't operate at the limit)
	characterResolveDelay    = 180 * time.Millisecond // stagger requests to spread over time
	characterNameNegativeTTL = 5 * time.Minute        // avoid hammering ESI for missing/unreachable names
)

// esiCharacterNameFailureMsg builds a user-facing tooltip when ESI did not return a pilot name.
func esiCharacterNameFailureMsg(characterID int, detail string) string {
	if detail == "" {
		return fmt.Sprintf("Could not load pilot name from ESI (character ID %d)", characterID)
	}
	return fmt.Sprintf("Could not load pilot name from ESI (character ID %d): %s", characterID, detail)
}

// resolveCharacterNames returns character ID -> name for the given IDs (player attackers only; 0 is skipped).
// The second map has ESI failure tooltips for IDs with an empty name.
// Uses in-memory cache and ESI GET /characters/{character_id}/ for missing entries.
// Respects ESI rate limits: staggered requests, limited concurrency.
// IMPORTANT: single-attempt only (no retries). Failures are cached briefly (negative cache).
func resolveCharacterNames(ids []int) (map[int]string, map[int]string) {
	seen := make(map[int]struct{})
	var need []int
	cachedHits := 0
	characterNameCacheMu.RLock()
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if e, ok := characterNameCache[id]; ok && time.Now().Before(e.expiry) {
			cachedHits++
			continue
		}
		need = append(need, id)
	}
	characterNameCacheMu.RUnlock()

	log.Printf("ESI character name lookup: ids=%d unique=%d cached=%d fetch=%d", len(ids), len(seen), cachedHits, len(need))

	if len(need) == 0 {
		characterNameCacheMu.RLock()
		out, errs := characterNamesFromCache(seen)
		characterNameCacheMu.RUnlock()
		return out, errs
	}

	// Fetch missing from ESI: staggered start, limited concurrency, respect 429 Retry-After
	type result struct {
		id     int
		name   string
		ok     bool
		errMsg string
	}
	results := make(chan result, len(need))
	client := &http.Client{Timeout: 15 * time.Second}
	for i, id := range need {
		id := id
		// Stagger request start to spread over time (ESI best practice: "Spread requests over time rather than bursting")
		if i > 0 {
			time.Sleep(characterResolveDelay)
		}
		go func() {
			characterResolveSem <- struct{}{}
			defer func() { <-characterResolveSem }()
			esiURL := fmt.Sprintf("https://esi.evetech.net/latest/characters/%d/?datasource=tranquility", id)
			req, err := http.NewRequest("GET", esiURL, nil)
			if err != nil {
				results <- result{id: id, name: "", ok: false, errMsg: esiCharacterNameFailureMsg(id, "failed to create request")}
				return
			}
			resp, err := client.Do(req)
			if err != nil || resp == nil {
				if resp != nil {
					log.Printf("ESI request: GET /characters/%d/ (character name) -> HTTP %d", id, resp.StatusCode)
					_ = resp.Body.Close()
				} else {
					log.Printf("ESI request: GET /characters/%d/ (character name) failed: %v", id, err)
				}
				detail := "request failed"
				if err != nil {
					detail = err.Error()
				}
				results <- result{id: id, name: "", ok: false, errMsg: esiCharacterNameFailureMsg(id, detail)}
				return
			}
			defer resp.Body.Close()
			log.Printf("ESI request: GET /characters/%d/ (character name) -> HTTP %d", id, resp.StatusCode)
			if resp.StatusCode != http.StatusOK {
				results <- result{id: id, name: "", ok: false, errMsg: esiCharacterNameFailureMsg(id, fmt.Sprintf("HTTP %d", resp.StatusCode))}
				return
			}
			var data struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				results <- result{id: id, name: "", ok: false, errMsg: esiCharacterNameFailureMsg(id, "invalid response")}
				return
			}
			if data.Name == "" {
				results <- result{id: id, name: "", ok: false, errMsg: esiCharacterNameFailureMsg(id, "empty name in response")}
				return
			}
			results <- result{id: id, name: data.Name, ok: true}
		}()
	}
	for i := 0; i < len(need); i++ {
		r := <-results
		characterNameCacheMu.Lock()
		ttl := characterNameCacheTTL
		if !r.ok {
			ttl = characterNameNegativeTTL
		}
		characterNameCache[r.id] = struct {
			name   string
			errMsg string
			expiry time.Time
		}{name: r.name, errMsg: r.errMsg, expiry: time.Now().Add(ttl)}
		characterNameCacheMu.Unlock()
	}

	characterNameCacheMu.RLock()
	out, errs := characterNamesFromCache(seen)
	characterNameCacheMu.RUnlock()
	return out, errs
}

func characterNamesFromCache(seen map[int]struct{}) (map[int]string, map[int]string) {
	out := make(map[int]string, len(seen))
	errs := make(map[int]string)
	for id := range seen {
		if id == 0 {
			continue
		}
		e, ok := characterNameCache[id]
		if !ok {
			continue
		}
		out[id] = e.name
		if e.name == "" {
			if e.errMsg != "" {
				errs[id] = e.errMsg
			} else {
				errs[id] = esiCharacterNameFailureMsg(id, "")
			}
		}
	}
	return out, errs
}

type pilotLinkMeta struct {
	ariaLabel     string
	pilotNameAttr string
	tooltip       string
	unresolved    bool
}

func pilotLinkMetaFor(characterID int, name string, characterNameErrors map[int]string) pilotLinkMeta {
	if name != "" {
		return pilotLinkMeta{ariaLabel: name, pilotNameAttr: name, tooltip: name, unresolved: false}
	}
	tooltip := esiCharacterNameFailureMsg(characterID, "")
	if characterNameErrors != nil {
		if msg := characterNameErrors[characterID]; msg != "" {
			tooltip = msg
		}
	}
	return pilotLinkMeta{
		ariaLabel:     tooltip,
		pilotNameAttr: "Pilot",
		tooltip:       tooltip,
		unresolved:    true,
	}
}

func writePilotLinkAttrs(html *strings.Builder, meta pilotLinkMeta, characterID int) {
	html.WriteString("' aria-label='")
	html.WriteString(template.HTMLEscapeString(meta.ariaLabel))
	html.WriteString("' data-pilot-name='")
	html.WriteString(template.HTMLEscapeString(meta.pilotNameAttr))
	html.WriteString("'")
	if meta.unresolved {
		html.WriteString(" data-pilot-unresolved='true'")
	}
	html.WriteString(" data-character-id='")
	html.WriteString(strconv.Itoa(characterID))
	html.WriteString("' data-tooltip='")
	html.WriteString(template.HTMLEscapeString(meta.tooltip))
}

// getTheraStationPositions returns positions of Thera stations (meters), fetched from ESI and cached
func getTheraStationPositions() [][3]float64 {
	stationIDs := []int{TheraStationID1, TheraStationID2, TheraStationID3, TheraStationID4}
	var positions [][3]float64
	theraStationPositionsMu.RLock()
	allCached := true
	for _, sid := range stationIDs {
		if _, ok := theraStationPositionsCache[sid]; !ok {
			allCached = false
			break
		}
	}
	if allCached {
		for _, sid := range stationIDs {
			positions = append(positions, theraStationPositionsCache[sid])
		}
		theraStationPositionsMu.RUnlock()
		return positions
	}
	theraStationPositionsMu.RUnlock()

	// Fetch missing positions from ESI
	client := &http.Client{Timeout: 10 * time.Second}
	for _, sid := range stationIDs {
		theraStationPositionsMu.RLock()
		if pos, ok := theraStationPositionsCache[sid]; ok {
			positions = append(positions, pos)
			theraStationPositionsMu.RUnlock()
			continue
		}
		theraStationPositionsMu.RUnlock()

		url := fmt.Sprintf("https://esi.evetech.net/latest/universe/stations/%d/?datasource=tranquility", sid)
		resp, err := client.Get(url)
		if err != nil || resp == nil {
			if resp != nil {
				log.Printf("ESI request: GET /universe/stations/%d/ (Thera position) -> HTTP %d", sid, resp.StatusCode)
				_ = resp.Body.Close()
			} else {
				log.Printf("ESI request: GET /universe/stations/%d/ (Thera position) failed: %v", sid, err)
			}
			continue
		}
		log.Printf("ESI request: GET /universe/stations/%d/ (Thera position) -> HTTP %d", sid, resp.StatusCode)
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			continue
		}
		var st struct {
			Position struct {
				X float64 `json:"x"`
				Y float64 `json:"y"`
				Z float64 `json:"z"`
			} `json:"position"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			_ = resp.Body.Close()
			continue
		}
		_ = resp.Body.Close()
		pos := [3]float64{st.Position.X, st.Position.Y, st.Position.Z}
		theraStationPositionsMu.Lock()
		theraStationPositionsCache[sid] = pos
		theraStationPositionsMu.Unlock()
		positions = append(positions, pos)
		time.Sleep(100 * time.Millisecond) // ESI rate limiting
	}
	return positions
}

// getStationIDByName finds a station ID by name in a system
// Checks loaded trade hubs and jump clone stations first (from JSON), then cache, then ESI
// Returns station ID if found, 0 if not found
func getStationIDByName(systemID int, stationName string) int {
	if stationName == "" {
		return 0
	}

	// Check trade hubs first (loaded from JSON with station_id)
	for _, hub := range tradeHubs {
		if hub.StationName == stationName && hub.StationID > 0 {
			return hub.StationID
		}
	}

	// Check jump clone stations (loaded from JSON with station_id)
	for _, jc := range jumpCloneStations {
		if jc.Name == stationName && jc.StationID > 0 {
			return jc.StationID
		}
	}

	// Don't use station IDs from jump_clone_stations.json as they may be incorrect
	// Always fetch from ESI based on station name

	// Check cache
	cacheKey := fmt.Sprintf("%d:%s", systemID, stationName)
	stationNameCacheMu.RLock()
	if stationID, found := stationNameCache[cacheKey]; found {
		stationNameCacheMu.RUnlock()
		return stationID
	}
	stationNameCacheMu.RUnlock()

	// Not in cache, fetch from ESI
	// Rate limiting
	elapsed := time.Since(lastAPIRequest)
	if elapsed < time.Second {
		time.Sleep(time.Second - elapsed)
	}
	lastAPIRequest = time.Now()

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	// Get station IDs from system info (use cache if available)
	systemInfoCacheMu.RLock()
	stationIDs, cached := systemInfoCache[systemID]
	systemInfoCacheMu.RUnlock()

	if !cached {
		// Fetch system info which includes station IDs
		url := fmt.Sprintf("https://esi.evetech.net/latest/universe/systems/%d/?datasource=tranquility", systemID)
		resp, err := client.Get(url)
		if err != nil {
			logging.Debugf("Failed to fetch stations for system %d: %v", systemID, err)
			// Cache 0 to avoid repeated failed requests
			stationNameCacheMu.Lock()
			stationNameCache[cacheKey] = 0
			stationNameCacheMu.Unlock()
			return 0
		}
		defer resp.Body.Close()
		log.Printf("ESI request: GET /universe/systems/%d/ -> HTTP %d", systemID, resp.StatusCode)

		if resp.StatusCode != http.StatusOK {
			logging.Debugf("Failed to fetch stations for system %d: HTTP %d", systemID, resp.StatusCode)
			// Cache 0 to avoid repeated failed requests
			stationNameCacheMu.Lock()
			stationNameCache[cacheKey] = 0
			stationNameCacheMu.Unlock()
			return 0
		}

		// Only decode Stations array - SystemID comes from SDE (we already know it's systemID)
		var systemInfo struct {
			Stations []int `json:"stations"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&systemInfo); err != nil {
			logging.Debugf("Failed to decode stations for system %d: %v", systemID, err)
			// Cache 0 to avoid repeated failed requests
			stationNameCacheMu.Lock()
			stationNameCache[cacheKey] = 0
			stationNameCacheMu.Unlock()
			return 0
		}
		stationIDs = systemInfo.Stations

		// Cache the station IDs
		systemInfoCacheMu.Lock()
		systemInfoCache[systemID] = stationIDs
		systemInfoCacheMu.Unlock()
	}

	// Fetch each station's details to match by name
	stationID := 0
	for _, sid := range stationIDs {
		// Check cache for station details
		stationDetailCacheMu.RLock()
		stationDetail, cached := stationDetailCache[sid]
		stationDetailCacheMu.RUnlock()

		var stationNameFound string
		if cached {
			stationNameFound = stationDetail.Name
		} else {
			// Rate limiting between station detail fetches
			elapsed := time.Since(lastAPIRequest)
			if elapsed < time.Second {
				time.Sleep(time.Second - elapsed)
			}
			lastAPIRequest = time.Now()

			stationURL := fmt.Sprintf("https://esi.evetech.net/latest/universe/stations/%d/?datasource=tranquility", sid)
			stationResp, err := client.Get(stationURL)
			if err != nil {
				log.Printf("ESI request: GET /universe/stations/%d/ (station name) -> ERROR: %v", sid, err)
				continue
			}
			log.Printf("ESI request: GET /universe/stations/%d/ (station name) -> HTTP %d", sid, stationResp.StatusCode)
			if stationResp.StatusCode != http.StatusOK {
				_ = stationResp.Body.Close()
				continue
			}

			// Only decode Name - SystemID comes from SDE (we already know it's systemID)
			var station struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(stationResp.Body).Decode(&station); err != nil {
				_ = stationResp.Body.Close()
				continue
			}
			_ = stationResp.Body.Close()

			stationNameFound = station.Name

			// Cache the station details (SystemID is known from context)
			stationDetailCacheMu.Lock()
			stationDetailCache[sid] = struct {
				Name     string
				SystemID int
			}{
				Name:     station.Name,
				SystemID: systemID, // Use known systemID instead of fetching from ESI
			}
			stationDetailCacheMu.Unlock()
		}

		// Match by name (case-insensitive)
		if strings.EqualFold(stationNameFound, stationName) {
			stationID = sid
			break
		}
	}

	// Cache the result
	stationNameCacheMu.Lock()
	stationNameCache[cacheKey] = stationID
	stationNameCacheMu.Unlock()

	return stationID
}

// verifyStationID verifies a station ID exists and returns its system ID and name
// Uses caching to avoid repeated ESI calls
// Returns: systemID, stationName, error
func verifyStationID(stationID int) (int, string, error) {
	// Check cache first
	stationVerificationCacheMu.RLock()
	if cached, found := stationVerificationCache[stationID]; found {
		stationVerificationCacheMu.RUnlock()
		if cached.Verified {
			return cached.SystemID, cached.StationName, nil
		}
		return 0, "", fmt.Errorf("station %d previously failed verification", stationID)
	}
	stationVerificationCacheMu.RUnlock()

	// Rate limiting
	elapsed := time.Since(lastAPIRequest)
	if elapsed < time.Second {
		time.Sleep(time.Second - elapsed)
	}
	lastAPIRequest = time.Now()

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	// Fetch station details from ESI
	url := fmt.Sprintf("https://esi.evetech.net/latest/universe/stations/%d/?datasource=tranquility", stationID)
	resp, err := client.Get(url)
	if err != nil {
		// Cache failure
		stationVerificationCacheMu.Lock()
		stationVerificationCache[stationID] = struct {
			SystemID    int
			StationName string
			Verified    bool
		}{Verified: false}
		stationVerificationCacheMu.Unlock()
		return 0, "", fmt.Errorf("failed to fetch station: %v", err)
	}
	defer resp.Body.Close()
	log.Printf("ESI request: GET /universe/stations/%d/ (verify) -> HTTP %d", stationID, resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		// Cache failure
		stationVerificationCacheMu.Lock()
		stationVerificationCache[stationID] = struct {
			SystemID    int
			StationName string
			Verified    bool
		}{Verified: false}
		stationVerificationCacheMu.Unlock()
		return 0, "", fmt.Errorf("station not found: HTTP %d", resp.StatusCode)
	}

	var station struct {
		Name     string `json:"name"`
		SystemID int    `json:"system_id"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&station); err != nil {
		// Cache failure
		stationVerificationCacheMu.Lock()
		stationVerificationCache[stationID] = struct {
			SystemID    int
			StationName string
			Verified    bool
		}{Verified: false}
		stationVerificationCacheMu.Unlock()
		return 0, "", fmt.Errorf("failed to decode station: %v", err)
	}

	// Cache success
	stationVerificationCacheMu.Lock()
	stationVerificationCache[stationID] = struct {
		SystemID    int
		StationName string
		Verified    bool
	}{
		SystemID:    station.SystemID,
		StationName: station.Name,
		Verified:    true,
	}
	stationVerificationCacheMu.Unlock()

	return station.SystemID, station.Name, nil
}

// getStationIDForSystem gets the first station ID for a system, with caching
// Returns station ID if found in cache, 0 if not cached or no station exists
// Does NOT make ESI requests - only returns cached values to avoid blocking HTML rendering
func getStationIDForSystem(systemID int) int {
	// Check cache first
	systemStationCacheMu.RLock()
	if stationID, found := systemStationCache[systemID]; found {
		systemStationCacheMu.RUnlock()
		return stationID
	}
	systemStationCacheMu.RUnlock()

	// Check if we have system info cached (station IDs list)
	systemInfoCacheMu.RLock()
	stationIDs, cached := systemInfoCache[systemID]
	systemInfoCacheMu.RUnlock()

	if cached && len(stationIDs) > 0 {
		// Use first station ID from cached system info
		stationID := stationIDs[0]
		// Cache the result
		systemStationCacheMu.Lock()
		systemStationCache[systemID] = stationID
		systemStationCacheMu.Unlock()
		return stationID
	}

	// Not in cache - return 0 without making ESI request
	// This prevents blocking HTML rendering with ESI requests
	return 0
}

// convertSystemsToRoutefinder converts main.System slice to routefinder.System slice
func convertSystemsToRoutefinder(mainSystems []System) []routefinder.System {
	result := make([]routefinder.System, len(mainSystems))
	for i, sys := range mainSystems {
		stargates := make([]routefinder.Stargate, len(sys.Stargates))
		for j, sg := range sys.Stargates {
			stargates[j] = routefinder.Stargate{
				ID:                    sg.ID,
				Position:              sg.Position,
				DestinationStargateID: sg.DestinationStargateID,
			}
		}
		result[i] = routefinder.System{
			SystemID:   sys.SystemID,
			SystemName: sys.SystemName,
			Stargates:  stargates,
			Security:   sys.Security,
		}
	}
	return result
}

func startup(mw io.Writer) {
	// Check for development mode
	mockData = os.Getenv("MOCK_DATA") == "1" || os.Getenv("MOCK_DATA") == "true"
	if mockData {
		fmt.Fprintln(mw, "=== DEVELOPMENT MODE ENABLED ===")
		fmt.Fprintln(mw, "Using mocked data for testing UI")
	}

	// User-Agent for outgoing HTTP (ESI, etc.): base string from USER_AGENT env + commit when set
	base := strings.TrimSpace(os.Getenv("USER_AGENT"))
	if base == "" {
		log.Fatal("USER_AGENT is required")
	}
	commitVal := commit
	if commitVal == "" {
		commitVal = strings.TrimSpace(os.Getenv("COMMIT"))
	}
	httpUserAgent = base
	if commitVal != "" {
		httpUserAgent += "+" + commitVal
	}
	fmt.Fprintln(mw, "User-Agent:", httpUserAgent)

	// Initialize SSO configuration
	ssoClientID = os.Getenv("EVE_SSO_CLIENT_ID")
	ssoClientSecret = os.Getenv("EVE_SSO_CLIENT_SECRET")
	ssoRedirectURI = os.Getenv("EVE_SSO_REDIRECT_URI")
	if ssoRedirectURI == "" {
		// Default to localhost for development
		ssoRedirectURI = "http://localhost:8080/api/auth/callback"
	}
	ssoFrontendURL = os.Getenv("EVE_SSO_FRONTEND_URL")
	if ssoFrontendURL == "" {
		// Default to localhost frontend for development
		ssoFrontendURL = "http://localhost:8888"
	}

	if ssoClientID != "" && ssoClientSecret != "" {
		fmt.Fprintln(mw, "SSO authentication enabled")
		fmt.Fprintln(mw, "Redirect URI:", ssoRedirectURI)
		fmt.Fprintln(mw, "Frontend URL:", ssoFrontendURL)
	} else {
		fmt.Fprintln(mw, "SSO authentication disabled (missing EVE_SSO_CLIENT_ID or EVE_SSO_CLIENT_SECRET)")
	}

	donateURL = strings.TrimSpace(os.Getenv("DONATE_URL"))
	donateText = strings.TrimSpace(os.Getenv("DONATE_TEXT"))

	if err := initCache(); err != nil {
		fmt.Fprintln(mw, "Cache initialization failed:", err)
	}
	initCORSAllowlist(mw)
	parseSystems(mw)
	parseGroups(mw)
	parseTypes(mw)
	parseTypeToGroup(mw)
	// Initialize route finder after systems are loaded
	routefinderSystems := convertSystemsToRoutefinder(systems)
	globalRouteFinder = routefinder.NewRouteFinder(routefinderSystems)

	// Load trade hubs
	loadTradeHubs(mw)

	// Load jump clone stations metadata.
	// Note: loadJumpCloneStations reads station_id from JSON (no ESI calls)
	// and must NOT alter route-finder graph edges.
	loadJumpCloneStations(mw)
}

func parseGroups(mw io.Writer) bool {
	// Try to load from groups.json first (similar to parseTypes)
	if file, err := os.Open("generated/groups.json"); err == nil {
		defer file.Close()

		decoder := json.NewDecoder(file)
		err = decoder.Decode(&itemGroups)
		if err != nil {
			fmt.Fprintln(mw, "Error decoding generated/groups.json:", err)
		}
		// Even if we loaded group names from cache, we still need category IDs
		// (used for mock ship validation).
	}

	// Load groupID -> categoryID mapping from SDE so we can filter mock ship types
	// to the correct SDE category (categories.jsonl _key=6 = ship category).
	groupIDToCategoryIDFromSDE, err := getGroupIDToCategoryIDFromSDE()
	if err != nil {
		fmt.Fprintln(mw, "Error loading groupID->categoryID mapping from SDE:", err)
		groupIDToCategoryID = nil
	} else {
		groupIDToCategoryID = groupIDToCategoryIDFromSDE
	}

	// Try to load from SDE
	if len(itemGroups) == 0 {
		fmt.Fprintln(mw, "Loading groups from SDE...")
		sdeGroups, err := getGroupsFromSDE()
		if err != nil {
			fmt.Fprintln(mw, "Error loading groups from SDE:", err)
			return true
		}

		itemGroups = sdeGroups

		// Save to groups.json for future use
		jsonData, err := json.Marshal(itemGroups)
		if err != nil {
			fmt.Fprintln(mw, "Error marshaling groups:", err)
			return true
		}

		err = os.WriteFile("generated/groups.json", jsonData, fs.FileMode(0644))
		if err != nil {
			fmt.Fprintln(mw, "Error writing groups.json:", err)
			return true
		}

		fmt.Fprintln(mw, "Loaded", len(itemGroups), "groups from SDE and saved to groups.json")
	}
	return true
}

func parseTypes(mw io.Writer) bool {
	// Try to load from types_desc.json first (SDE-filtered by description.en presence)
	if file, err := os.Open("generated/types_desc.json"); err == nil {
		defer file.Close()

		decoder := json.NewDecoder(file)
		err = decoder.Decode(&types)
		if err != nil {
			fmt.Fprintln(mw, "Error decoding generated/types.json:", err)
		}
		return true
	}

	// Try to load from SDE
	fmt.Fprintln(mw, "Loading types from SDE...")
	sdeTypes, err := getTypesFromSDE()
	if err != nil {
		fmt.Fprintln(mw, "Error loading types from SDE:", err)
		return true
	}

	types = sdeTypes

	// Save to types_desc.json for future use
	jsonData, err := json.Marshal(types)
	if err != nil {
		fmt.Fprintln(mw, "Error marshaling types:", err)
		return true
	}

	err = os.WriteFile("generated/types_desc.json", jsonData, fs.FileMode(0644))
	if err != nil {
		fmt.Fprintln(mw, "Error writing types.json:", err)
		return true
	}

	fmt.Fprintln(mw, "Loaded", len(types), "types from SDE (description.en filtered) and saved to types_desc.json")
	return true
}

func parseTypeToGroup(mw io.Writer) {
	typeIDToGroupName = make(map[int]string)
	typeIDToGroupID = make(map[int]int)
	// Try to load typeID->groupID from cached file
	if file, err := os.Open("generated/typeToGroup.json"); err == nil {
		defer file.Close()
		var typeIDToGroupIDFromFile map[string]int
		if err := json.NewDecoder(file).Decode(&typeIDToGroupIDFromFile); err == nil {
			for typeIDStr, groupID := range typeIDToGroupIDFromFile {
				typeID, _ := strconv.Atoi(typeIDStr)
				typeIDToGroupID[typeID] = groupID
				if groupName := itemGroups[strconv.Itoa(groupID)]; groupName != "" {
					typeIDToGroupName[typeID] = groupName
				}
			}
			return
		}
	}
	// Load from SDE
	typeIDToGroupIDFromSDE, err := getTypeToGroupFromSDE()
	if err != nil {
		fmt.Fprintln(mw, "Could not load typeToGroup from SDE:", err)
		return
	}
	for typeID, groupID := range typeIDToGroupIDFromSDE {
		typeIDToGroupID[typeID] = groupID
		if groupName := itemGroups[strconv.Itoa(groupID)]; groupName != "" {
			typeIDToGroupName[typeID] = groupName
		}
	}
	// Cache to generated/typeToGroup.json
	toSave := make(map[string]int)
	for typeID, groupID := range typeIDToGroupIDFromSDE {
		toSave[strconv.Itoa(typeID)] = groupID
	}
	jsonData, _ := json.Marshal(toSave)
	_ = os.MkdirAll("generated", 0755) // #nosec G301 -- 0755 intentional for generated output
	_ = os.WriteFile("generated/typeToGroup.json", jsonData, fs.FileMode(0644))
	fmt.Fprintln(mw, "Loaded typeID->group from SDE and saved to generated/typeToGroup.json")
}

func parseSystems(mw io.Writer) bool {
	// Try to load from systems.json first
	if file, err := os.Open("generated/systems.json"); err == nil {
		defer file.Close()

		decoder := json.NewDecoder(file)
		err = decoder.Decode(&systems)
		if err != nil {
			fmt.Fprintln(mw, "Error decoding generated/systems.json:", err)
		} else if len(systems) > 0 {
			fmt.Fprintln(mw, "Loaded", len(systems), "systems from generated/systems.json")
			return true
		}
	}

	// Try to load from SDE
	fmt.Fprintln(mw, "Loading systems from SDE...")
	sdeSystems, err := loadSystemsFromSDE()
	if err != nil {
		fmt.Fprintln(mw, "Error loading systems from SDE:", err)
		return true
	}

	systems = sdeSystems

	// Save to systems.json for future use
	jsonData, err := json.Marshal(systems)
	if err != nil {
		fmt.Fprintln(mw, "Error marshaling systems:", err)
		return true
	}

	err = os.WriteFile("generated/systems.json", jsonData, fs.FileMode(0644))
	if err != nil {
		fmt.Fprintln(mw, "Error writing systems.json:", err)
		return true
	}

	fmt.Fprintln(mw, "Loaded", len(systems), "systems from SDE and saved to systems.json")
	return false
}

func getSystemById(id int) System {
	for _, sys := range systems {
		if sys.SystemID == id {
			return sys
		}
	}

	return System{}
}

type EveScoutSystem struct {
	SystemID        int     `json:"system_id"`
	SystemName      string  `json:"system_name"`
	RegionID        int     `json:"region_id"`
	RegionName      string  `json:"region_name"`
	SystemClass     string  `json:"system_class"`
	SecurityStatus  float64 `json:"security_status"`
	JoveObservatory bool    `json:"jove_observatory,omitempty"`
}

type ZKillboardKill struct {
	KillmailID int                `json:"killmail_id"`
	ZKB        ZKillboardKillInfo `json:"zkb"`
}

type ZKillboardKillInfo struct {
	LocationID     int      `json:"locationID"`
	Hash           string   `json:"hash"`
	FittedValue    float64  `json:"fittedValue"`
	DroppedValue   float64  `json:"droppedValue"`
	DestroyedValue float64  `json:"destroyedValue"`
	TotalValue     float64  `json:"totalValue"`
	Points         int      `json:"points"`
	NPC            bool     `json:"npc"`
	Solo           bool     `json:"solo"`
	AWOX           bool     `json:"awox"`
	Labels         []string `json:"labels"`
}

type KillSystemInfo struct {
	SystemID               int
	Name                   string
	Security               float64
	KillCount              int
	LatestKillTime         time.Time
	ViaThera               bool             `json:"ViaThera,omitempty"`
	TheraDist              int              `json:"TheraDist,omitempty"`
	TheraInfo              string           `json:"TheraInfo,omitempty"`
	TheraInboundSignature  string           `json:"TheraInboundSignature,omitempty"`
	TheraOutboundSignature string           `json:"TheraOutboundSignature,omitempty"`
	MaxShipSize            string           `json:"MaxShipSize,omitempty"` // Maximum ship size for Thera route
	Route                  []EveScoutSystem `json:"Route,omitempty"`
	RecentKills            []CachedKillmail `json:"recent_kills,omitempty"`
}

type ESIKillmail struct {
	KillmailID    int           `json:"killmail_id"`
	KillmailTime  string        `json:"killmail_time"`
	Victim        ESIVictim     `json:"victim"`
	Attackers     []ESIAttacker `json:"attackers"`
	SolarSystemID int           `json:"solar_system_id"`
}

type ESIVictim struct {
	CharacterID   int `json:"character_id,omitempty"`
	CorporationID int `json:"corporation_id,omitempty"`
	AllianceID    int `json:"alliance_id,omitempty"`
	FactionID     int `json:"faction_id,omitempty"`
	DamageTaken   int `json:"damage_taken"`
	ShipTypeID    int `json:"ship_type_id"`
	Position      *struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		Z float64 `json:"z"`
	} `json:"position,omitempty"`
}

type ESIAttacker struct {
	CharacterID    int     `json:"character_id,omitempty"`
	CorporationID  int     `json:"corporation_id,omitempty"`
	AllianceID     int     `json:"alliance_id,omitempty"`
	FactionID      int     `json:"faction_id,omitempty"`
	SecurityStatus float64 `json:"security_status"`
	DamageDone     int     `json:"damage_done"`
	FinalBlow      bool    `json:"final_blow"`
	WeaponTypeID   int     `json:"weapon_type_id"`
	ShipTypeID     int     `json:"ship_type_id"`
}

func checkRouteViaThera(fromSystemID, toSystemID, maxJumps int) (bool, int, string, string, string, string, []EveScoutSystem) {
	// Use local route finder instead of EVE Scout API
	if globalRouteFinder == nil {
		logging.Debugf("checkRouteViaThera: RouteFinder not initialized")
		return false, -1, "", "", "", "", nil
	}

	// Find route with Thera support (maxJumps 0 = unlimited)
	route, err := globalRouteFinder.FindShortestRouteWithThera(fromSystemID, toSystemID, maxJumps)
	if err != nil {
		logging.Debugf("checkRouteViaThera: No route found from %d to %d: %v", fromSystemID, toSystemID, err)
		return false, -1, "", "", "", "", nil
	}

	// Convert Route to EveScoutSystem format
	eveScoutRoute := make([]EveScoutSystem, 0, len(route.Path))
	for _, systemID := range route.Path {
		system := getSystemById(systemID)
		if system.SystemID == 0 {
			continue
		}
		eveScoutRoute = append(eveScoutRoute, EveScoutSystem{
			SystemID:       system.SystemID,
			SystemName:     system.SystemName,
			SecurityStatus: system.Security,
		})
	}

	// Check if route goes through Thera
	// Check both the ViaThera flag AND if Thera is actually in the route path
	viaThera := route.ViaThera
	routeContainsThera := false
	// Also check if Thera system ID is in the route path (more reliable)
	for _, systemID := range route.Path {
		if systemID == TheraSystemID {
			viaThera = true
			routeContainsThera = true
			break
		}
	}
	theraInfo := ""
	theraInboundSig := ""
	theraOutboundSig := ""
	maxShipSize := ""
	if viaThera {
		theraInfo = "Thera"
		maxShipSize = route.MaxShipSize

		// If route contains Thera but MaxShipSize is empty, we need to find it
		// This can happen if a direct route was returned but Thera is still in the path
		if routeContainsThera && globalRouteFinder != nil {
			logging.Debugf("checkRouteViaThera: Route contains Thera, looking up signature info from routefinder")
			lookupMaxShipSize, isEOL := globalRouteFinder.GetTheraSignatureInfoForRoute(route.Path)
			if maxShipSize == "" && lookupMaxShipSize != "" {
				maxShipSize = lookupMaxShipSize
				logging.Debugf("checkRouteViaThera: Found MaxShipSize=%s by looking up route path", maxShipSize)
			}
			inSig, outSig, _ := globalRouteFinder.GetTheraSignatureIDsForRoute(route.Path)
			theraInboundSig, theraOutboundSig = inSig, outSig
			if isEOL {
				// Mark Thera info as EOL so UI can display it
				if !strings.Contains(theraInfo, "EOL") {
					theraInfo = theraInfo + " (EOL)"
				}
				logging.Debugf("checkRouteViaThera: Thera signature for this route is EOL")
			}
		}

		logging.Debugf("checkRouteViaThera: Route via Thera found, MaxShipSize=%s, TheraInfo=%s", maxShipSize, theraInfo)
	}

	return viaThera, route.Jumps, theraInfo, theraInboundSig, theraOutboundSig, maxShipSize, eveScoutRoute
}

func buildEveScoutRouteFromPath(path []int) []EveScoutSystem {
	eveScoutRoute := make([]EveScoutSystem, 0, len(path))
	for _, systemID := range path {
		system := getSystemById(systemID)
		if system.SystemID == 0 {
			continue
		}
		eveScoutRoute = append(eveScoutRoute, EveScoutSystem{
			SystemID:       system.SystemID,
			SystemName:     system.SystemName,
			SecurityStatus: system.Security,
		})
	}
	return eveScoutRoute
}

type proximityRouteResult struct {
	viaThera         bool
	dist             int
	theraInfo        string
	theraInboundSig  string
	theraOutboundSig string
	maxShipSize      string
	route            []EveScoutSystem
}

// getProximityRoutesBatch computes best routes for many targets using two single-source BFS traversals:
// from source and from Thera. This avoids running BFS once per target on cold requests.
func getProximityRoutesBatch(fromSystemID int, targetSystemIDs []int, maxJumps int) map[int]proximityRouteResult {
	results := make(map[int]proximityRouteResult, len(targetSystemIDs))
	if globalRouteFinder == nil {
		for _, targetSystemID := range targetSystemIDs {
			viaThera, dist, theraInfo, theraInboundSig, theraOutboundSig, maxShipSize, route := getProximityRoute(fromSystemID, targetSystemID)
			results[targetSystemID] = proximityRouteResult{
				viaThera:         viaThera,
				dist:             dist,
				theraInfo:        theraInfo,
				theraInboundSig:  theraInboundSig,
				theraOutboundSig: theraOutboundSig,
				maxShipSize:      maxShipSize,
				route:            route,
			}
		}
		return results
	}

	// Keep route cache coherent with current Thera signature set.
	currentTheraFP := refreshTheraAndGetFingerprint()
	proximityRouteCacheSyncFingerprint(currentTheraFP)

	remaining := make([]int, 0, len(targetSystemIDs))
	for _, targetSystemID := range targetSystemIDs {
		key := proximityRouteCacheKey{from: fromSystemID, to: targetSystemID}
		if ent, ok := proximityRouteCacheGet(key); ok {
			results[targetSystemID] = proximityRouteResult(ent)
			continue
		}
		remaining = append(remaining, targetSystemID)
	}

	if len(remaining) == 0 {
		return results
	}

	fromPaths := globalRouteFinder.FindShortestPathsFrom(fromSystemID, maxJumps)
	theraPaths := globalRouteFinder.FindShortestPathsFrom(TheraSystemID, maxJumps)
	fromToTheraDist, fromCanReachThera := fromPaths.Distances[TheraSystemID]
	var pathToThera []int
	if fromCanReachThera {
		pathToThera = globalRouteFinder.BuildPath(fromPaths, TheraSystemID)
	}

	for _, targetSystemID := range remaining {
		bestViaThera := false
		bestDist := -1
		var bestPath []int

		if directDist, ok := fromPaths.Distances[targetSystemID]; ok {
			bestDist = directDist
			bestPath = globalRouteFinder.BuildPath(fromPaths, targetSystemID)
		}

		if fromCanReachThera {
			if theraToTargetDist, ok := theraPaths.Distances[targetSystemID]; ok {
				viaTheraDist := fromToTheraDist + theraToTargetDist
				if bestDist < 0 || viaTheraDist < bestDist {
					pathFromThera := globalRouteFinder.BuildPath(theraPaths, targetSystemID)
					if len(pathToThera) > 0 && len(pathFromThera) > 0 {
						combined := make([]int, 0, len(pathToThera)+len(pathFromThera)-1)
						combined = append(combined, pathToThera...)
						combined = append(combined, pathFromThera[1:]...)
						bestPath = combined
						bestDist = viaTheraDist
						bestViaThera = true
					}
				}
			}
		}

		if bestDist < 0 || len(bestPath) == 0 {
			results[targetSystemID] = proximityRouteResult{dist: -1}
			continue
		}

		result := proximityRouteResult{
			viaThera: bestViaThera,
			dist:     bestDist,
			route:    buildEveScoutRouteFromPath(bestPath),
		}

		if bestViaThera {
			result.theraInfo = "Thera"
			lookupMaxShipSize, isEOL := globalRouteFinder.GetTheraSignatureInfoForRoute(bestPath)
			if lookupMaxShipSize != "" {
				result.maxShipSize = lookupMaxShipSize
			}
			inSig, outSig, _ := globalRouteFinder.GetTheraSignatureIDsForRoute(bestPath)
			result.theraInboundSig = inSig
			result.theraOutboundSig = outSig
			if isEOL {
				result.theraInfo += " (EOL)"
			}
		}

		results[targetSystemID] = result

		key := proximityRouteCacheKey{from: fromSystemID, to: targetSystemID}
		proximityRouteCacheSet(key, proximityRouteCacheEntry(result))
	}

	return results
}

// getProximityRoute returns route info for proximity mode with caching and early BFS termination
func getProximityRoute(fromSystemID, toSystemID int) (bool, int, string, string, string, string, []EveScoutSystem) {
	key := proximityRouteCacheKey{from: fromSystemID, to: toSystemID}
	currentTheraFP := refreshTheraAndGetFingerprint()
	proximityRouteCacheSyncFingerprint(currentTheraFP)

	if ent, ok := proximityRouteCacheGet(key); ok {
		return ent.viaThera, ent.dist, ent.theraInfo, ent.theraInboundSig, ent.theraOutboundSig, ent.maxShipSize, ent.route
	}

	viaThera, dist, theraInfo, theraInboundSig, theraOutboundSig, maxShipSize, route := checkRouteViaThera(fromSystemID, toSystemID, maxRangeJumps)

	proximityRouteCacheSet(key, proximityRouteCacheEntry{
		viaThera:         viaThera,
		dist:             dist,
		theraInfo:        theraInfo,
		theraInboundSig:  theraInboundSig,
		theraOutboundSig: theraOutboundSig,
		maxShipSize:      maxShipSize,
		route:            route,
	})

	return viaThera, dist, theraInfo, theraInboundSig, theraOutboundSig, maxShipSize, route
}

const (
	PochvenRegionID = 10000070
)

func isPochvenSystem(systemID int) bool {
	system := getSystemById(systemID)
	if system.SystemID == 0 {
		return false
	}
	return system.RegionID == PochvenRegionID
}

func isLowsecOrNullsecSystem(systemID int) bool {
	system := getSystemById(systemID)
	if system.SystemID == 0 {
		return false
	}
	// Lowsec: 0.1 to 0.4, Nullsec: <= 0.0, Highsec: >= 0.45 (rounded in game)
	return system.Security < 0.45
}

// initCache creates logs and generated directories
func initCache() error {
	dirs := []string{"logs", "generated"}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil { // #nosec G301 -- 0755 intentional for app dirs
			return fmt.Errorf("failed to create directory %s: %v", dir, err)
		}
	}

	log.Println("Logs and generated directories initialized")
	return nil
}

// isValidKillmail checks if a killmail is valid for precalculation
// Valid means: lowsec/nullsec system (or Pochven, or Thera), within 1000km of a stargate
// Thera has no stargates, so Thera kills are valid if they have position data.
// Includes all kills (ships, pods, structures, etc.) as they indicate enemy presence
// Pochven is included because it makes sense in proximity mode.
func isValidKillmail(killmail *zkillboardcache.CachedKillmail) bool {
	// Check if system is lowsec, nullsec, Pochven, or Thera
	isThera := killmail.SolarSystemID == TheraSystemID
	if !isLowsecOrNullsecSystem(killmail.SolarSystemID) && !isPochvenSystem(killmail.SolarSystemID) && !isThera {
		system := getSystemById(killmail.SolarSystemID)
		systemName := "unknown"
		security := 0.0
		if system.SystemID != 0 {
			systemName = system.SystemName
			security = system.Security
		}
		logging.Debugf("Killmail %d: System %s (%d) is not lowsec/nullsec/Pochven/Thera (security: %.2f), skipping", killmail.KillmailID, systemName, killmail.SolarSystemID, security)
		return false
	}

	// Thera has no stargates - valid if kill has position data (all space kills do)
	if isThera {
		if killmail.Victim.Position == nil {
			logging.Debugf("Killmail %d: Thera kill has no position data, skipping", killmail.KillmailID)
			return false
		}
		return true
	}

	// Check if kill is within 1000km of stargate
	// Convert CachedKillmail to ESIKillmail format for getKillPositionAndDistance
	attackers := make([]ESIAttacker, len(killmail.Attackers))
	for i, attacker := range killmail.Attackers {
		attackers[i] = ESIAttacker{
			CharacterID:    attacker.CharacterID,
			CorporationID:  attacker.CorporationID,
			AllianceID:     attacker.AllianceID,
			FactionID:      attacker.FactionID,
			SecurityStatus: attacker.SecurityStatus,
			DamageDone:     attacker.DamageDone,
			FinalBlow:      attacker.FinalBlow,
			WeaponTypeID:   attacker.WeaponTypeID,
			ShipTypeID:     attacker.ShipTypeID,
		}
	}

	var victimPosition *struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		Z float64 `json:"z"`
	}
	if killmail.Victim.Position != nil {
		victimPosition = &struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
			Z float64 `json:"z"`
		}{
			X: killmail.Victim.Position.X,
			Y: killmail.Victim.Position.Y,
			Z: killmail.Victim.Position.Z,
		}
	}

	esiKillmail := &ESIKillmail{
		KillmailID:   killmail.KillmailID,
		KillmailTime: killmail.KillmailTime,
		Victim: ESIVictim{
			CharacterID:   killmail.Victim.CharacterID,
			CorporationID: killmail.Victim.CorporationID,
			AllianceID:    killmail.Victim.AllianceID,
			FactionID:     killmail.Victim.FactionID,
			DamageTaken:   killmail.Victim.DamageTaken,
			ShipTypeID:    killmail.Victim.ShipTypeID,
			Position:      victimPosition,
		},
		Attackers:     attackers,
		SolarSystemID: killmail.SolarSystemID,
	}

	_, _, isWithinRange, _, err := getKillPositionAndDistance(
		killmail.SolarSystemID,
		killmail.ZKBInfo.ZKB.LocationID,
		esiKillmail,
	)
	if err != nil {
		system := getSystemById(killmail.SolarSystemID)
		systemName := "unknown"
		if system.SystemID != 0 {
			systemName = system.SystemName
		}
		logging.Debugf("Killmail %d: Error getting position/distance in system %s (%d): %v, skipping", killmail.KillmailID, systemName, killmail.SolarSystemID, err)
		return false
	}
	if !isWithinRange {
		system := getSystemById(killmail.SolarSystemID)
		systemName := "unknown"
		if system.SystemID != 0 {
			systemName = system.SystemName
		}
		logging.Debugf("Killmail %d: Kill in system %s (%d) is not within 1000km of stargate, skipping", killmail.KillmailID, systemName, killmail.SolarSystemID)
		return false
	}

	return true
}

// calculateDataForKillmail calculates and stores precalculated data for a valid killmail
func calculateDataForKillmail(killmailID int, killmail *zkillboardcache.CachedKillmail) {
	if !isValidKillmail(killmail) {
		system := getSystemById(killmail.SolarSystemID)
		systemName := "unknown"
		if system.SystemID != 0 {
			systemName = system.SystemName
		}
		logging.Debugf("Killmail %d in system %s (%d) is invalid for precalculation, skipping", killmailID, systemName, killmail.SolarSystemID)
		return
	}

	// Get system info
	system := getSystemById(killmail.SolarSystemID)
	if system.SystemID == 0 {
		logging.Debugf("Warning: System %d not found for killmail %d", killmail.SolarSystemID, killmailID)
		return
	}

	logging.Debugf("Precalculating data for valid killmail %d in system %s (%d)", killmailID, system.SystemName, killmail.SolarSystemID)

	// Get kill position and distance
	attackers := make([]ESIAttacker, len(killmail.Attackers))
	for i, attacker := range killmail.Attackers {
		attackers[i] = ESIAttacker{
			CharacterID:    attacker.CharacterID,
			CorporationID:  attacker.CorporationID,
			AllianceID:     attacker.AllianceID,
			FactionID:      attacker.FactionID,
			SecurityStatus: attacker.SecurityStatus,
			DamageDone:     attacker.DamageDone,
			FinalBlow:      attacker.FinalBlow,
			WeaponTypeID:   attacker.WeaponTypeID,
			ShipTypeID:     attacker.ShipTypeID,
		}
	}

	var victimPosition *struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
		Z float64 `json:"z"`
	}
	if killmail.Victim.Position != nil {
		victimPosition = &struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
			Z float64 `json:"z"`
		}{
			X: killmail.Victim.Position.X,
			Y: killmail.Victim.Position.Y,
			Z: killmail.Victim.Position.Z,
		}
	}

	esiKillmail := &ESIKillmail{
		KillmailID:   killmail.KillmailID,
		KillmailTime: killmail.KillmailTime,
		Victim: ESIVictim{
			CharacterID:   killmail.Victim.CharacterID,
			CorporationID: killmail.Victim.CorporationID,
			AllianceID:    killmail.Victim.AllianceID,
			FactionID:     killmail.Victim.FactionID,
			DamageTaken:   killmail.Victim.DamageTaken,
			ShipTypeID:    killmail.Victim.ShipTypeID,
			Position:      victimPosition,
		},
		Attackers:     attackers,
		SolarSystemID: killmail.SolarSystemID,
	}

	// Calculate kill position and distance (Thera has no stargates, use victim position only)
	var killPos [3]float64
	var minDistance *float64
	var stargateInfo *StargateInfo
	if killmail.SolarSystemID == TheraSystemID {
		if killmail.Victim.Position == nil {
			log.Printf("Error: Thera killmail %d has no position data", killmailID)
			return
		}
		killPos = [3]float64{killmail.Victim.Position.X, killmail.Victim.Position.Y, killmail.Victim.Position.Z}
		// No stargate info for Thera
	} else {
		pos, dist, _, sgInfo, err := getKillPositionAndDistance(
			killmail.SolarSystemID,
			killmail.ZKBInfo.ZKB.LocationID,
			esiKillmail,
		)
		if err != nil {
			log.Printf("Error calculating position for killmail %d: %v", killmailID, err)
			return
		}
		killPos = pos
		minDistance = &dist
		stargateInfo = sgInfo
	}

	// Convert zkillboardcache types to main package types (attackers already converted above)
	var victim ESIVictim
	victim.CharacterID = killmail.Victim.CharacterID
	victim.CorporationID = killmail.Victim.CorporationID
	victim.AllianceID = killmail.Victim.AllianceID
	victim.FactionID = killmail.Victim.FactionID
	victim.DamageTaken = killmail.Victim.DamageTaken
	victim.ShipTypeID = killmail.Victim.ShipTypeID
	if killmail.Victim.Position != nil {
		victim.Position = &struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
			Z float64 `json:"z"`
		}{
			X: killmail.Victim.Position.X,
			Y: killmail.Victim.Position.Y,
			Z: killmail.Victim.Position.Z,
		}
	}

	zkbKill := ZKillboardKill{
		KillmailID: killmail.ZKBInfo.KillmailID,
		ZKB: ZKillboardKillInfo{
			LocationID:     killmail.ZKBInfo.ZKB.LocationID,
			Hash:           killmail.ZKBInfo.ZKB.Hash,
			FittedValue:    killmail.ZKBInfo.ZKB.FittedValue,
			DroppedValue:   killmail.ZKBInfo.ZKB.DroppedValue,
			DestroyedValue: killmail.ZKBInfo.ZKB.DestroyedValue,
			TotalValue:     killmail.ZKBInfo.ZKB.TotalValue,
			Points:         killmail.ZKBInfo.ZKB.Points,
			NPC:            killmail.ZKBInfo.ZKB.NPC,
			Solo:           killmail.ZKBInfo.ZKB.Solo,
			AWOX:           killmail.ZKBInfo.ZKB.AWOX,
			Labels:         killmail.ZKBInfo.ZKB.Labels,
		},
	}

	cachedKillmail := CachedKillmail{
		KillmailID:            killmail.KillmailID,
		KillmailTime:          killmail.KillmailTime,
		Victim:                victim,
		Attackers:             attackers,
		ZKBInfo:               zkbKill,
		SolarSystemID:         killmail.SolarSystemID,
		KillLocation:          &killPos,
		MinDistanceToStargate: minDistance,
		StargateInfo:          stargateInfo,
	}

	// Store in precalculated data - store both the timestamp and the fully processed killmail
	precalculatedData.Write(func(data *PrecalculatedData) {
		data.calculatedKillmails[killmailID] = time.Now()
		if data.systemsWithKills == nil {
			data.systemsWithKills = make(map[int][]CachedKillmail)
		}
		existingKills := len(data.systemsWithKills[killmail.SolarSystemID])
		data.systemsWithKills[killmail.SolarSystemID] = append(data.systemsWithKills[killmail.SolarSystemID], cachedKillmail)
		logging.Debugf("Writing killmail %d to precalculatedData: system %s (%d) now has %d kill(s) (was %d)", killmailID, system.SystemName, killmail.SolarSystemID, len(data.systemsWithKills[killmail.SolarSystemID]), existingKills)
		logging.Debugf("Total systems in precalculatedData.systemsWithKills: %d", len(data.systemsWithKills))
	})

	// Verify the write by reading back immediately
	readFn := precalculatedData.Read()
	verifyData := readFn()
	if kills, exists := verifyData.systemsWithKills[killmail.SolarSystemID]; exists {
		logging.Debugf("Verified: System %s (%d) has %d kill(s) in precalculatedData after write", system.SystemName, killmail.SolarSystemID, len(kills))
	} else {
		logging.Debugf("ERROR: System %s (%d) NOT FOUND in precalculatedData after write! Total systems: %d", system.SystemName, killmail.SolarSystemID, len(verifyData.systemsWithKills))
	}

	// Precalculate Near trade hubs mode data for this system (trade hub, distance, route)
	precalculateNearTradeHubsModeForSystem(killmail.SolarSystemID)
	// Invalidate after both systemsWithKills and nearTradeHubsMode are updated.
	invalidateIndexHTMLCache()

	logging.Debugf("Precalculated data for killmail %d in system %s (%d)", killmailID, system.SystemName, killmail.SolarSystemID)
}

// isTheraStationLocation returns true if locationID is one of Thera's station/structure IDs
func isTheraStationLocation(locationID int) bool {
	return locationID == TheraStationID1 || locationID == TheraStationID2 ||
		locationID == TheraStationID3 || locationID == TheraStationID4
}

// isDictorShip returns true if the ship type is Interdictor (group 541) or Heavy Interdictor (group 894)
func isDictorShip(shipTypeID int) bool {
	if typeIDToGroupID == nil {
		return false
	}
	groupID := typeIDToGroupID[shipTypeID]
	return groupID == 541 || groupID == 894 // Interdictor, Heavy Interdiction Cruiser
}

// isArtilleryWeapon returns true if the weapon type is artillery (projectile turret)
func isArtilleryWeapon(weaponTypeID int) bool {
	if weaponTypeID == 0 || typeIDToGroupName == nil {
		return false
	}
	groupName := typeIDToGroupName[weaponTypeID]
	return strings.Contains(groupName, "Artillery")
}

// shipTypeIconFilenameFromGroup maps the existing ship group name (from typeIDToGroupName)
// to the closest icon filename under backend/static/icons/.
//
// Icon bitmaps are inlined in the page via getShipIconsEmbeddedStyle (no per-row /icons/ requests).
func shipTypeIconFilenameFromGroup(groupName string, isNPC bool) string {
	g := strings.ToLower(strings.TrimSpace(groupName))
	if g == "" {
		return ""
	}

	// Normalize so "Battle Cruiser" and "BattleCruisers" both match.
	compact := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, g)

	// Citizen Ships should intentionally render no icon.
	if strings.Contains(compact, "citizenships") || strings.Contains(compact, "citizenship") {
		return ""
	}

	// Determine base filename (non-NPC).
	// Note: group names come from SDE and may be plural, e.g. "Mining Frigates".
	base := ""
	switch {
	// Advanced hull variants that may not include base hull names directly.
	case strings.Contains(compact, "blackops"):
		base = "battleship_16.png"
	case strings.Contains(compact, "blockaderunner"):
		base = "industrial_16.png"
	case strings.Contains(compact, "interceptor"):
		base = "frigate_16.png"
	case strings.Contains(compact, "interdictor"):
		base = "destroyer_16.png"
	case strings.Contains(compact, "covertops"):
		base = "frigate_16.png"
	// Force Recon Ship, Combat Recon Ship (compact e.g. forcereconship, combatreconship) — cruisers, not frigates.
	case strings.Contains(compact, "reconship"):
		base = "cruiser_16.png"
	case strings.Contains(compact, "commandship"):
		base = "battleCruiser_16.png"
	case strings.Contains(compact, "corvette"):
		base = "rookie_16.png"
	case strings.Contains(compact, "deepspacetransport"):
		base = "industrial_16.png"
	case strings.Contains(compact, "electronicattackship"):
		base = "frigate_16.png"
	case strings.Contains(compact, "exhumer"):
		base = "miningBarge_16.png"
	case strings.Contains(compact, "expeditioncommandship"):
		base = "battleCruiser_16.png"
	case strings.Contains(compact, "hauler"):
		base = "industrial_16.png"
	case strings.Contains(compact, "logistics"):
		base = "cruiser_16.png"
	case strings.Contains(compact, "marauder"):
		base = "battleship_16.png"
	case strings.Contains(compact, "prototypeexplorationship"):
		base = "frigate_16.png"
	case strings.Contains(compact, "stealthbomber"):
		base = "frigate_16.png"
	case strings.Contains(compact, "protectivesentry"):
		base = "protectiveSentry.png"
	case strings.Contains(compact, "sentrygun"):
		base = "sentry.png"
	case strings.Contains(compact, "sentry"):
		base = "sentry.png"

	case strings.Contains(compact, "miningfrigate"):
		base = "miningFrigate_16.png"
	case strings.Contains(compact, "miningbarge"):
		base = "miningBarge_16.png"
	case strings.Contains(compact, "forceauxiliary"):
		base = "forceAuxiliary_16.png"
	case strings.Contains(compact, "industrialcommand"):
		base = "industrialCommand_16.png"
	case strings.Contains(compact, "battlecruis"):
		base = "battleCruiser_16.png"
	case strings.Contains(compact, "battleship"):
		base = "battleship_16.png"
	case strings.Contains(compact, "dreadnought"):
		base = "dreadnought_16.png"
	case strings.Contains(compact, "supercarrier"):
		base = "supercarrier_16.png"
	case strings.Contains(compact, "carrier"):
		base = "carrier_16.png"
	case strings.Contains(compact, "titan"):
		base = "titan_16.png"
	case strings.Contains(compact, "freighter"):
		base = "freighter_16.png"
	case strings.Contains(compact, "destroyer"):
		base = "destroyer_16.png"
	case strings.Contains(compact, "cruiser") && !strings.Contains(compact, "battlecruis"):
		base = "cruiser_16.png"
	case strings.Contains(compact, "frigate"):
		base = "frigate_16.png"
	case strings.Contains(compact, "industrial"):
		base = "industrial_16.png"
	case strings.Contains(compact, "capsule"):
		base = "capsule_16.png"
	case strings.Contains(compact, "shuttle"):
		base = "shuttle_16.png"
	case strings.Contains(compact, "rookie"):
		base = "rookie_16.png"
	}

	if base == "" {
		return ""
	}
	if isNPC {
		// Some NPC structure/entity icons have only a non-NPC filename.
		switch base {
		case "sentry.png", "protectiveSentry.png":
			return base
		case "supercarrier_16.png":
			// Asset name is npcsuperCarrier_16.png (camel "Carrier"), unlike lowercase supercarrier_16.png.
			return "npcsuperCarrier_16.png"
		}
		return "npc" + base
	}
	return base
}

func shipTypeIconHTMLFromGroup(groupName string, isNPC bool) string {
	filename := shipTypeIconFilenameFromGroup(groupName, isNPC)
	if filename == "" {
		return ""
	}
	base := strings.TrimSuffix(filename, ".png")
	if !shipIconCSSClassSuffixOK(base) {
		return ""
	}
	tooltip := strings.TrimSpace(groupName)
	if tooltip == "" {
		tooltip = "Ship"
	}
	escaped := template.HTMLEscapeString(tooltip)
	return "<span class='ship-type-icon ship-type-icon--" + base + "' role='img' aria-label='" + escaped + "' data-tooltip='" + escaped + "'></span>"
}

func writeShipTypeIconWithWikiHTML(html *strings.Builder, iconHTML, shipTypeName string, isNPC bool) {
	if iconHTML == "" {
		html.WriteString("• ")
		return
	}
	if isNPC {
		// NPC attackers are never wiki-linked.
		html.WriteString(iconHTML)
		return
	}
	wikiURL := euniWikiShipPageURL(shipTypeName)
	if wikiURL == "" {
		html.WriteString(iconHTML)
		return
	}
	html.WriteString("<a target='_blank' rel='noopener noreferrer' href='")
	html.WriteString(template.HTMLEscapeString(wikiURL))
	html.WriteString("'>")
	html.WriteString(iconHTML)
	html.WriteString("</a>")
}

func writeShipTypeTextHTML(html *strings.Builder, shipTypeName string) {
	html.WriteString("<span><span class='ship-type-text'>")
	html.WriteString(template.HTMLEscapeString(shipTypeName))
	html.WriteString("</span></span>")
}

// euniWikiShipPageURL returns a wiki.eveuniversity.org article URL for a ship type display name, or "" when no page should be assumed.
func euniWikiShipPageURL(shipTypeName string) string {
	if shipTypeName == "" || shipTypeName == "Unknown ship" {
		return ""
	}
	title := strings.ReplaceAll(strings.TrimSpace(shipTypeName), " ", "_")
	if title == "" {
		return ""
	}
	return "https://wiki.eveuniversity.org/" + url.PathEscape(title)
}

// writeAttackerShipTypeHTML writes the ship icon (or bullet), optionally wrapped in an EVE University wiki link for the ship type.
// NPC attackers (CharacterID 0) are never linked — wiki pages are for capsuleer ships.
func writeAttackerShipTypeHTML(html *strings.Builder, iconHTML, attackerShip string, isNPC bool) {
	var wikiURL string
	if !isNPC {
		wikiURL = euniWikiShipPageURL(attackerShip)
	}
	if wikiURL != "" {
		html.WriteString("<a target='_blank' rel='noopener noreferrer' href='")
		html.WriteString(template.HTMLEscapeString(wikiURL))
		html.WriteString("'>")
	}
	if iconHTML != "" {
		html.WriteString(iconHTML)
	} else {
		html.WriteString("• ")
	}
	html.WriteString("<span><span class='ship-type-text'>")
	html.WriteString(template.HTMLEscapeString(attackerShip))
	html.WriteString("</span></span>")
	if wikiURL != "" {
		html.WriteString("</a>")
	}
}

// isTheraCampKill returns true if the kill indicates a possible camp in Thera:
// - Kill in Thera, not at station
// - Kill not within 10,000 km of any Thera station
// - Either Interdictor/Heavy Interdictor involved, or artillery-only ships (artillery weapons used)
func isTheraCampKill(kill *CachedKillmail) bool {
	if kill.SolarSystemID != TheraSystemID {
		return false
	}
	// Not at station: locationID not one of Thera's structures
	if isTheraStationLocation(kill.ZKBInfo.ZKB.LocationID) {
		return false
	}
	// Not within 10,000 km of any Thera station
	var killPos [3]float64
	if kill.KillLocation != nil {
		killPos = *kill.KillLocation
	} else if kill.Victim.Position != nil {
		killPos = [3]float64{kill.Victim.Position.X, kill.Victim.Position.Y, kill.Victim.Position.Z}
	} else {
		return false // No position data, can't verify distance from stations
	}
	stationPositions := getTheraStationPositions()
	for _, stationPos := range stationPositions {
		if calculateDistance(killPos, stationPos) < theraCampMinDistanceFromStation {
			return false
		}
	}
	// Check victim for dictor
	if isDictorShip(kill.Victim.ShipTypeID) {
		return true
	}
	// Check attackers for dictor or artillery
	for _, a := range kill.Attackers {
		if isDictorShip(a.ShipTypeID) {
			return true
		}
		if isArtilleryWeapon(a.WeaponTypeID) {
			return true
		}
	}
	return false
}

// getTheraCampKills returns kills that indicate possible camps in Thera (not at station, dictor or artillery involved)
func getTheraCampKills() []CachedKillmail {
	readFn := precalculatedData.Read()
	data := readFn()
	if data.systemsWithKills == nil {
		return nil
	}
	kills := data.systemsWithKills[TheraSystemID]
	if len(kills) == 0 {
		return nil
	}
	var campKills []CachedKillmail
	for _, k := range kills {
		if isTheraCampKill(&k) {
			campKills = append(campKills, k)
		}
	}
	// Sort by time descending (newest first)
	sort.Slice(campKills, func(i, j int) bool {
		timeI, _ := time.Parse("2006-01-02T15:04:05Z", campKills[i].KillmailTime)
		timeJ, _ := time.Parse("2006-01-02T15:04:05Z", campKills[j].KillmailTime)
		return timeI.After(timeJ)
	})
	return campKills
}

// precalculateNearTradeHubsModeForSystem calculates and stores near trade hubs mode data (trade hub, distance, route) for a system
func precalculateNearTradeHubsModeForSystem(targetSystemID int) {
	targetSystem := getSystemById(targetSystemID)
	if targetSystem.SystemID == 0 {
		return
	}

	// Get kills for this system from precalculated data
	var kills []CachedKillmail
	readFn := precalculatedData.Read()
	data := readFn()
	if data.systemsWithKills != nil {
		kills = data.systemsWithKills[targetSystemID]
	}

	if len(kills) == 0 {
		return // No kills, don't precalculate
	}

	type hubCandidate struct {
		hub struct {
			Name     string
			SystemID int
			Region   string
			Type     string
		}
		distance    int
		viaThera    bool
		theraInfo   string
		maxShipSize string
		route       []EveScoutSystem
	}
	var closestPrimary *hubCandidate
	var specialCandidates []hubCandidate

	// Check all trade hubs: primary hubs find the closest one, special hubs get their own entries
	for _, hub := range tradeHubs {
		if hub.Type != "primary" && hub.Type != "special" {
			continue
		}

		// For primary hubs, skip if the system itself is a trade hub
		if hub.Type == "primary" && targetSystemID == hub.SystemID {
			continue
		}

		hubSystem := getSystemById(hub.SystemID)
		if hubSystem.SystemID == 0 {
			continue
		}

		// Use shared route cache (same as proximity mode)
		viaThera, distance, theraInfo, _, _, maxShipSize, route := getProximityRoute(hub.SystemID, targetSystemID)
		log.Printf("DEBUG precalculateNearTradeHubs: hub=%s(%d) → target=%s(%d): distance=%d", hub.Name, hub.SystemID, targetSystem.SystemName, targetSystemID, distance)
		if distance < 0 {
			continue
		}
		maxDisplayJumps := nearTradeHubsMaxDisplayJumps

		if hub.Type == "primary" {
			if distance > 0 && distance <= maxDisplayJumps {
				candidate := hubCandidate{
					hub: struct {
						Name     string
						SystemID int
						Region   string
						Type     string
					}{hub.Name, hub.SystemID, hub.Region, hub.Type},
					distance:    distance,
					viaThera:    viaThera,
					theraInfo:   theraInfo,
					maxShipSize: maxShipSize,
					route:       route,
				}

				if closestPrimary == nil || candidate.distance < closestPrimary.distance {
					closestPrimary = &candidate
				}
			}
		} else if hub.Type == "special" {
			if distance <= maxDisplayJumps {
				specialCandidates = append(specialCandidates, hubCandidate{
					hub: struct {
						Name     string
						SystemID int
						Region   string
						Type     string
					}{hub.Name, hub.SystemID, hub.Region, hub.Type},
					distance:    distance,
					viaThera:    viaThera,
					theraInfo:   theraInfo,
					maxShipSize: maxShipSize,
					route:       route,
				})
			}
		}
	}

	if len(kills) > 0 && (closestPrimary != nil || len(specialCandidates) > 0) {
		precalculatedData.Write(func(data *PrecalculatedData) {
			if data.nearTradeHubsMode == nil {
				data.nearTradeHubsMode = make(map[string]PrecalculatedSystemData)
			}

			sortedKills := make([]CachedKillmail, len(kills))
			copy(sortedKills, kills)
			sort.Slice(sortedKills, func(i, j int) bool {
				timeI, _ := time.Parse("2006-01-02T15:04:05Z", sortedKills[i].KillmailTime)
				timeJ, _ := time.Parse("2006-01-02T15:04:05Z", sortedKills[j].KillmailTime)
				return timeI.After(timeJ)
			})

			now := time.Now()

			if closestPrimary != nil {
				key := fmt.Sprintf("%d|%s", targetSystemID, closestPrimary.hub.Name)
				data.nearTradeHubsMode[key] = PrecalculatedSystemData{
					SystemID:    targetSystem.SystemID,
					Name:        targetSystem.SystemName,
					Security:    targetSystem.Security,
					Dist:        closestPrimary.distance,
					ViaThera:    closestPrimary.viaThera,
					TheraDist:   closestPrimary.distance,
					TheraInfo:   closestPrimary.theraInfo,
					MaxShipSize: closestPrimary.maxShipSize,
					Route:       closestPrimary.route,
					RecentKills: sortedKills,
					TradeHub:    closestPrimary.hub.Name,
					Weight:      0,
					LastUpdated: now,
				}
			}

			for _, sc := range specialCandidates {
				key := fmt.Sprintf("%d|%s", targetSystemID, sc.hub.Name)
				data.nearTradeHubsMode[key] = PrecalculatedSystemData{
					SystemID:    targetSystem.SystemID,
					Name:        targetSystem.SystemName,
					Security:    targetSystem.Security,
					Dist:        sc.distance,
					ViaThera:    sc.viaThera,
					TheraDist:   sc.distance,
					TheraInfo:   sc.theraInfo,
					MaxShipSize: sc.maxShipSize,
					Route:       sc.route,
					RecentKills: sortedKills,
					TradeHub:    sc.hub.Name,
					Weight:      0,
					LastUpdated: now,
				}
			}
		})
		invalidateIndexHTMLCache()
	}
}

// recalculateFromKills clears precalculated data and recalculates routes for the given killmails.
// Used by EnsureRecalculated (and after-backfill callback) with a snapshot so backfill can resume while recalc runs.
func recalculateFromKills(kills []zkillboardcache.CachedKillmail) {
	log.Printf("Recalculating all routes from cache: %d killmails", len(kills))

	precalculatedData.Write(func(data *PrecalculatedData) {
		data.systemsWithKills = make(map[int][]CachedKillmail)
		data.normalMode = make(map[int][]PrecalculatedSystemData)
		data.nearTradeHubsMode = make(map[string]PrecalculatedSystemData)
		data.calculatedKillmails = make(map[int]time.Time)
	})

	recalculatedCount := 0
	for i := range kills {
		kill := &kills[i]
		calculateDataForKillmail(kill.KillmailID, kill)
		recalculatedCount++
	}

	readFn := precalculatedData.Read()
	data := readFn()
	log.Printf("Recalculate from cache done: %d killmails, %d systems with kills", recalculatedCount, len(data.systemsWithKills))
	invalidateIndexHTMLCache()
}

// EnsureRecalculated recalculates routes from current cache so precalculated data is up to date.
// Invoked only after backfill completes or when the stream hits 404 (natural pause). Stream killmails are calculated incrementally via callback.
// Snapshot is taken via killmailCache.GetRecentKills(); only one goroutine runs the full recalculation at a time.
func EnsureRecalculated() {
	kills := killmailCache.GetRecentKills()

	recalcMu.Lock()
	for recalcInProgress {
		recalcCond.Wait()
	}
	recalcInProgress = true
	recalcMu.Unlock()

	recalculateFromKills(kills)

	recalcMu.Lock()
	recalcInProgress = false
	recalcCond.Broadcast()
	recalcMu.Unlock()
}

// appReadyForBalancer is true when initial R2Z2 backfill (and post-backfill hook) has finished and no full precalculation rebuild is running.
func appReadyForBalancer() bool {
	if killmailCache == nil || !killmailCache.WarmupComplete() {
		return false
	}
	recalcMu.Lock()
	defer recalcMu.Unlock()
	return !recalcInProgress
}

// recalculateForTheraUpdate recalculates data for all killmails in cache from the last hour when Thera signatures update.
// Uses the same set as EnsureRecalculated (GetRecentKills). Serializes with EnsureRecalculated via recalcMu so full table rebuilds do not overlap.
func recalculateForTheraUpdate() {
	log.Printf("Thera signatures updated, recalculating data for all kills within an hour")

	kills := killmailCache.GetRecentKills()
	log.Printf("Found %d killmails to recalculate", len(kills))

	recalcMu.Lock()
	for recalcInProgress {
		recalcCond.Wait()
	}
	recalcInProgress = true
	recalcMu.Unlock()

	// Clear precalculated data before recalculating (same as recalculateFromKills) to avoid duplicates and stale data
	precalculatedData.Write(func(data *PrecalculatedData) {
		beforeClear := len(data.systemsWithKills)
		data.systemsWithKills = make(map[int][]CachedKillmail)
		data.normalMode = make(map[int][]PrecalculatedSystemData)
		data.nearTradeHubsMode = make(map[string]PrecalculatedSystemData)
		data.calculatedKillmails = make(map[int]time.Time)
		logging.Debugf("recalculateForTheraUpdate: Clearing precalculated data (had %d systems with kills)", beforeClear)
		logging.Debugf("recalculateForTheraUpdate: Cleared precalculated data, will recalculate %d killmails", len(kills))
	})

	// Recalculate each killmail (this will rebuild systemsWithKills and nearTradeHubsMode with updated Thera routes)
	for i := range kills {
		kill := &kills[i]
		calculateDataForKillmail(kill.KillmailID, kill)
	}

	verifyReadFn := precalculatedData.Read()
	verifyData := verifyReadFn()
	log.Printf("Recalculate from Thera update done: %d killmails, %d systems with kills", len(kills), len(verifyData.systemsWithKills))
	logging.Debugf("recalculateForTheraUpdate: Completed - %d systems with kills after recalculation", len(verifyData.systemsWithKills))
	invalidateIndexHTMLCache()

	recalcMu.Lock()
	recalcInProgress = false
	recalcCond.Broadcast()
	recalcMu.Unlock()
}

// setupTheraUpdateListener sets up a listener for Thera signature updates
func setupTheraUpdateListener() {
	// In dev mode, skip Thera update listener (signatures are mocked)
	if mockData {
		log.Println("Thera update listener disabled in development mode")
		return
	}

	// Poll Thera signatures every minute and check if they've changed
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		lastTheraFingerprint := ""

		for range ticker.C {
			if globalRouteFinder == nil {
				continue
			}

			// Force fetch Thera signatures
			globalRouteFinder.ForceFetchTheraSignatures()
			fp := globalRouteFinder.GetTheraSignaturesFingerprint()

			if fp != lastTheraFingerprint {
				log.Printf("Thera signatures changed (fingerprint updated, count=%d)", globalRouteFinder.GetTheraSignaturesCount())
				lastTheraFingerprint = fp
				recalculateForTheraUpdate()
			}
		}
	}()
}

// loadMockData generates and loads mock data for development mode
// This covers all UI cases:
// - Lowsec and nullsec target systems
// - Less than 10 and more than 10 attackers in kill details
// - Routes through stargates only, through Thera, through Zarzakh, and both
// - Systems with 1, 2, and 3 recent kills
// - Routes which lead through systems where kills occurred
func loadMockData(mw io.Writer) {
	startTime := time.Now()
	// Generate killmail times within 15 minutes from startup
	// All times must be in the past to avoid negative Recency values
	// Start 14 minutes ago, so even the latest kill (14 minutes offset) is still in the past
	baseTime := startTime.Add(-14 * time.Minute)

	// SDE type IDs for mock NPC attackers (CharacterID 0): pirate NPC ship + station sentry gun.
	const (
		mockNPCAttackerShipTypeID   = 23441 // Serpentis Captain (Asteroid Serpentis BattleCruiser)
		mockNPCAttackerSentryTypeID = 1194  // Amarr Sentry Gun
	)

	// Mock system IDs (using real system IDs from nullsec_systems.md)
	// Lowsec systems - using Tama and Oto (actual lowsec systems)
	lowsecSystem1 := 30002813 // Tama (lowsec, 0.3)
	lowsecSystem2 := 30002808 // Oto (lowsec, 0.4)

	// Nullsec systems
	nullsecSystem1 := 30004046 // F7C-H0 (nullsec, -0.0119)
	nullsecSystem2 := 30002440 // BWF-ZZ (nullsec, -0.5754)
	nullsecSystem3 := 30000250 // P3EN-E (nullsec, -0.2749)
	nullsecSystem4 := 30002355 // LXQ2-T (nullsec, -0.0765)
	nullsecSystem5 := 30004019 // 3-FKCZ (nullsec, -0.2835)

	// Systems for routes with kills on the way
	routeSystemWithKill := 30000999 // N-RAEL (nullsec, -0.0265)

	// Get system info to ensure they exist
	sys1 := getSystemById(nullsecSystem1)
	sys2 := getSystemById(nullsecSystem2)
	sys3 := getSystemById(nullsecSystem3)
	sys4 := getSystemById(nullsecSystem4)
	sys5 := getSystemById(nullsecSystem5)
	lowsec1 := getSystemById(lowsecSystem1)
	lowsec2 := getSystemById(lowsecSystem2)
	routeSys := getSystemById(routeSystemWithKill)

	// Use actual system data if available, otherwise create mock systems with stargates
	// This ensures all systems exist and have proper connections
	// We need to connect mock systems to existing systems so routes can be found
	ensureSystemExists := func(sys *System, systemID int, name string, security float64) {
		// Find a nearby existing system to connect to (for route finding)
		// Try to find a lowsec/nullsec system that exists, or fall back to Jita
		var connectToSystemID int = 30000142 // Default to Jita
		for _, existingSys := range systems {
			if existingSys.SystemID != 0 && existingSys.SystemID != systemID {
				// Prefer connecting to a lowsec/nullsec system if available
				if existingSys.Security < 0.45 {
					connectToSystemID = existingSys.SystemID
					break
				}
			}
		}

		// Check if system is already in the global list (not just the passed-in struct)
		existingInList := getSystemById(systemID).SystemID != 0
		if !existingInList {
			// Create a mock system with at least one stargate for positioning
			*sys = System{
				SystemID:   systemID,
				SystemName: name,
				Security:   security,
				Stargates: []Stargate{
					{
						ID:                    50000000 + systemID,
						Position:              [3]float64{1.0, 1.0, 1.0},
						DestinationStargateID: connectToSystemID, // Connect to an existing system
					},
				},
			}
			// Add to systems list so it's available
			systems = append(systems, *sys)

			// Also add reverse connection from the connected system to this one
			for i := range systems {
				if systems[i].SystemID == connectToSystemID {
					// Add a stargate from the connected system back to this one
					reverseStargateID := 50000001 + systemID
					systems[i].Stargates = append(systems[i].Stargates, Stargate{
						ID:                    reverseStargateID,
						Position:              [3]float64{0, 0, 0},
						DestinationStargateID: systemID,
					})
					break
				}
			}
		} else {
			// System exists, but ensure it has at least one stargate for validation
			if len(sys.Stargates) == 0 {
				sys.Stargates = []Stargate{
					{
						ID:                    50000000 + systemID,
						Position:              [3]float64{1.0, 1.0, 1.0},
						DestinationStargateID: connectToSystemID,
					},
				}
				// Update in systems list
				for i := range systems {
					if systems[i].SystemID == systemID {
						systems[i] = *sys
						break
					}
				}

				// Add reverse connection
				for i := range systems {
					if systems[i].SystemID == connectToSystemID {
						reverseStargateID := 50000001 + systemID
						systems[i].Stargates = append(systems[i].Stargates, Stargate{
							ID:                    reverseStargateID,
							Position:              [3]float64{0, 0, 0},
							DestinationStargateID: systemID,
						})
						break
					}
				}
			}
		}
	}

	// Helper function to update system connection
	updateSystemConnection := func(systemID int, connectToID int) {
		for i := range systems {
			if systems[i].SystemID == systemID {
				// Update or create stargate connection
				if len(systems[i].Stargates) == 0 {
					systems[i].Stargates = []Stargate{
						{
							ID:                    50000000 + systemID,
							Position:              [3]float64{0, 0, 0},
							DestinationStargateID: connectToID,
						},
					}
				} else {
					systems[i].Stargates[0].DestinationStargateID = connectToID
				}

				// Add reverse connection
				for j := range systems {
					if systems[j].SystemID == connectToID {
						// Check if reverse connection already exists
						hasReverse := false
						for _, sg := range systems[j].Stargates {
							if sg.DestinationStargateID == systemID {
								hasReverse = true
								break
							}
						}
						if !hasReverse {
							reverseStargateID := 50000001 + systemID
							systems[j].Stargates = append(systems[j].Stargates, Stargate{
								ID:                    reverseStargateID,
								Position:              [3]float64{0, 0, 0},
								DestinationStargateID: systemID,
							})
						}
						break
					}
				}
				break
			}
		}
	}

	// Connect mock systems strategically:
	// - Systems that should use stargates only: connect directly to a trade hub or nearby system
	// - Systems that should use Thera: don't connect directly, only through Thera
	// - Systems that should use Zarzakh: don't connect directly, only through Zarzakh
	// - Systems that should use both: don't connect directly, require both
	// All systems need to be within max range (maxRangeJumps) of a trade hub for "Near trade hubs" mode

	// Find a trade hub to connect to (prefer one that exists)
	tradeHubID := 30000142 // Jita (default)
	for _, hub := range tradeHubs {
		if hub.Type == "primary" {
			if getSystemById(hub.SystemID).SystemID != 0 {
				tradeHubID = hub.SystemID
				break
			}
		}
	}

	// Connect all systems to trade hub to ensure they're within max range
	// System 1: Stargates only - connect directly to trade hub (1 jump)
	ensureSystemExists(&sys1, nullsecSystem1, "F7C-H0", -0.0119)
	updateSystemConnection(nullsecSystem1, tradeHubID)

	// System 2: Thera only - connect through a long chain so Thera route is shorter
	// Create a chain of 10 intermediate systems to make direct route very long
	ensureSystemExists(&sys2, nullsecSystem2, "BWF-ZZ", -0.5754)
	prevSystemID := tradeHubID
	for i := 0; i < 10; i++ {
		intermediateID := 40000001 + i
		intermediateSys := System{SystemID: intermediateID, SystemName: fmt.Sprintf("Intermediate-%d", i), Security: 0.0}
		ensureSystemExists(&intermediateSys, intermediateID, fmt.Sprintf("Intermediate-%d", i), 0.0)
		updateSystemConnection(intermediateID, prevSystemID)
		prevSystemID = intermediateID
	}
	updateSystemConnection(nullsecSystem2, prevSystemID)
	// Now Thera route (trade hub -> Thera -> System 2, 2 jumps) will be much shorter than direct route (11 jumps)

	// System 3: Zarzakh only - connect through a long chain so Zarzakh route is shorter
	ensureSystemExists(&sys3, nullsecSystem3, "P3EN-E", -0.2749)
	prevSystemID = tradeHubID
	for i := 0; i < 10; i++ {
		intermediateID := 40000011 + i
		intermediateSys := System{SystemID: intermediateID, SystemName: fmt.Sprintf("Intermediate-Z-%d", i), Security: 0.0}
		ensureSystemExists(&intermediateSys, intermediateID, fmt.Sprintf("Intermediate-Z-%d", i), 0.0)
		updateSystemConnection(intermediateID, prevSystemID)
		prevSystemID = intermediateID
	}
	updateSystemConnection(nullsecSystem3, prevSystemID)
	// Now Zarzakh route will be shorter

	// System 4: Both Thera and Zarzakh - connect through a long chain
	ensureSystemExists(&sys4, nullsecSystem4, "LXQ2-T", -0.0765)
	prevSystemID = tradeHubID
	for i := 0; i < 10; i++ {
		intermediateID := 40000021 + i
		intermediateSys := System{SystemID: intermediateID, SystemName: fmt.Sprintf("Intermediate-B-%d", i), Security: 0.0}
		ensureSystemExists(&intermediateSys, intermediateID, fmt.Sprintf("Intermediate-B-%d", i), 0.0)
		updateSystemConnection(intermediateID, prevSystemID)
		prevSystemID = intermediateID
	}
	updateSystemConnection(nullsecSystem4, prevSystemID)
	// Route will go through both Thera and Zarzakh (shorter than 11-jump direct route)

	// System 5: Stargates only - connect to System 1 (2 jumps from trade hub)
	ensureSystemExists(&sys5, nullsecSystem5, "3-FKCZ", -0.2835)
	updateSystemConnection(nullsecSystem5, nullsecSystem1)

	// Lowsec 1: Stargates only - connect directly to trade hub (1 jump) to ensure it appears
	ensureSystemExists(&lowsec1, lowsecSystem1, "Tama", 0.3)
	updateSystemConnection(lowsecSystem1, tradeHubID)

	// Lowsec 2: Stargates only - connect to Lowsec 1 (2 jumps from trade hub)
	ensureSystemExists(&lowsec2, lowsecSystem2, "Oto", 0.4)
	updateSystemConnection(lowsecSystem2, lowsecSystem1)

	// Route system: Stargates only - connect to Lowsec 2 (5 jumps from trade hub)
	ensureSystemExists(&routeSys, routeSystemWithKill, "N-RAEL", -0.0265)
	updateSystemConnection(routeSystemWithKill, lowsecSystem2)

	// Ensure all primary trade hubs exist in the systems list so precalculateNearTradeHubsModeForSystem
	// can find routes to them. Only create hubs that don't already exist (avoids overwriting SDE stargates).
	jitaHubID := 30000142 // Jita
	amarrHubID := 30002187 // Amarr
	rensHubID := 30002510  // Rens
	dodixieHubID := 30002659 // Dodixie
	hekHubID := 30002053  // Hek

	ensureTradeHubExists := func(hubID int, name string, sec float64) {
		if getSystemById(hubID).SystemID == 0 {
			ensureSystemExists(&System{}, hubID, name, sec)
		}
	}
	ensureTradeHubExists(jitaHubID, "Jita", 1.0)
	ensureTradeHubExists(amarrHubID, "Amarr", 1.0)
	ensureTradeHubExists(rensHubID, "Rens", 0.9)
	ensureTradeHubExists(dodixieHubID, "Dodixie", 0.8)
	ensureTradeHubExists(hekHubID, "Hek", 0.7)

	// Mock systems near other primary trade hubs (for testing multi-hub filtering)
	amarrSystem := 40001001
	rensSystem := 40001002
	dodixieSystem := 40001003
	hekSystem := 40001004

	sysAmarr := getSystemById(amarrSystem)
	sysRens := getSystemById(rensSystem)
	sysDodixie := getSystemById(dodixieSystem)
	sysHek := getSystemById(hekSystem)

	ensureSystemExists(&sysAmarr, amarrSystem, "Mock-Amarr-Near", -0.1)
	updateSystemConnection(amarrSystem, amarrHubID)

	ensureSystemExists(&sysRens, rensSystem, "Mock-Rens-Near", -0.2)
	updateSystemConnection(rensSystem, rensHubID)

	ensureSystemExists(&sysDodixie, dodixieSystem, "Mock-Dodixie-Near", -0.3)
	updateSystemConnection(dodixieSystem, dodixieHubID)

	ensureSystemExists(&sysHek, hekSystem, "Mock-Hek-Near", -0.4)
	updateSystemConnection(hekSystem, hekHubID)

	// Ensure Thera and Zarzakh systems exist in the systems list
	theraSystem := getSystemById(TheraSystemID)
	if theraSystem.SystemID == 0 {
		theraSystem = System{
			SystemID:   TheraSystemID,
			SystemName: "Thera",
			Security:   0.0,
			Stargates:  []Stargate{},
		}
		systems = append(systems, theraSystem)
		log.Printf("Added Thera system to systems list")
	}

	zarzakhSystem := getSystemById(ZarzakhSystemID)
	if zarzakhSystem.SystemID == 0 {
		zarzakhSystem = System{
			SystemID:   ZarzakhSystemID,
			SystemName: "Zarzakh",
			Security:   0.0,
			Stargates:  []Stargate{},
		}
		systems = append(systems, zarzakhSystem)
		log.Printf("Added Zarzakh system to systems list")
	}

	// Connect Thera and Zarzakh to trade hub so they're reachable
	// Thera connects to trade hub
	updateSystemConnection(TheraSystemID, tradeHubID)
	// Zarzakh connects to Thera (so routes can go through both)
	updateSystemConnection(ZarzakhSystemID, TheraSystemID)
	// This allows routes like: trade hub -> Thera -> Zarzakh -> System 4

	// Rebuild routefinder graph once after all systems are ensured and connected
	if globalRouteFinder != nil {
		routefinderSystems := convertSystemsToRoutefinder(systems)
		globalRouteFinder.RebuildGraph(routefinderSystems)
		log.Printf("Rebuilt routefinder graph with %d systems for mock data", len(systems))
	}

	// Get stargate info for positioning kills near stargates
	// Position will be calculated per system based on actual stargate positions

	// Generate mock killmails
	// We need to ensure all cases are covered:
	// - Lowsec and nullsec systems
	// - 1, 2, and 3 kills per system
	// - < 10 and >= 10 attackers
	// - Routes: stargates only, Thera only, Zarzakh only, both Thera and Zarzakh
	// - Route through system with kills
	//
	// Pilot name lookup:
	// The UI renders attacker "pilot names" by calling resolveCharacterNames(),
	// which prefers in-memory `characterNameCache` and otherwise hits ESI.
	// In mock mode we pre-seed the cache so UI shows stable, non-empty names.
	mockPilotNames := []string{
		"Ashes of Dawn",
		"Kraken Widow",
		"Neon Marauder",
		"Vanta Hex",
		"Redshift Rook",
		"Obsidian Warden",
		"Solar Lancer",
		"Void Sable",
		"Chrome Vanguard",
		"Silver Tempest",
		"Starlit Reaver",
		"Night Tide",
		"Ember Protocol",
		"Rogue Comet",
		"Grim Albatross",
		"Arc Reactor",
		"Pulse Corsair",
		"Frostbit Sentinel",
		"Dusk Cartographer",
		"Helios Drift",
		"Blackwater Sparrow",
		"Atlas Whisper",
		"Orchid Striker",
		"Fenrir Knave",
		"Zircon Fox",
		"Echo Division",
		"Monolith Runner",
		"Opal Saboteur",
		"Rift Engineer",
		"Nova Cartel",
	}
	mockPilotNameForCharacter := func(characterID int) string {
		if characterID == 0 || len(mockPilotNames) == 0 {
			return ""
		}
		return mockPilotNames[characterID%len(mockPilotNames)]
	}
	setMockCharacterName := func(characterID int) {
		if characterID == 0 {
			return
		}
		name := mockPilotNameForCharacter(characterID)
		if name == "" {
			return
		}
		characterNameCacheMu.Lock()
		characterNameCache[characterID] = struct {
			name   string
			errMsg string
			expiry time.Time
		}{name: name, expiry: time.Now().Add(365 * 24 * time.Hour)}
		characterNameCacheMu.Unlock()
	}
	setMockCharacterNameFailed := func(characterID int) {
		if characterID == 0 {
			return
		}
		characterNameCacheMu.Lock()
		characterNameCache[characterID] = struct {
			name   string
			errMsg string
			expiry time.Time
		}{name: "", errMsg: esiCharacterNameFailureMsg(characterID, ""), expiry: time.Now().Add(365 * 24 * time.Hour)}
		characterNameCacheMu.Unlock()
	}

	// Ensure every mock pilot (attacker + victim) has a unique ship type.
	// We build a pool of SDE-backed ship type IDs that are rendered by the UI (icon mapping exists),
	// then pick from multiple hull categories in round-robin fashion to avoid "mostly frigates/rookies".

	// Pin one Interdictor and one Heavy Interdictor so Thera "camp kill" detection keeps working.
	// We choose whatever is valid according to SDE's `description.en` presence (via `types` map)
	// and has a UI icon mapping.
	dictorTypeIDs := make([]int, 0)
	heavyDictorTypeIDs := make([]int, 0)
	for typeID, groupID := range typeIDToGroupID {
		// `types` already ensures `description.en` exists and non-empty.
		if types == nil || types[typeID] == "" {
			continue
		}
		// Only keep ship types in SDE category `_key=6` (ships).
		if groupIDToCategoryID != nil && groupIDToCategoryID[groupID] != 6 {
			continue
		}
		// No icon check here: Thera-camp logic only depends on ship group (dictor/heavy dictor).
		if groupID == 541 { // Interdictor
			dictorTypeIDs = append(dictorTypeIDs, typeID)
		} else if groupID == 894 { // Heavy Interdictor
			heavyDictorTypeIDs = append(heavyDictorTypeIDs, typeID)
		}
	}
	sort.Ints(dictorTypeIDs)
	sort.Ints(heavyDictorTypeIDs)
	pinnedDictorShipTypeID := 0
	pinnedHeavyDictorShipTypeID := 0
	if len(dictorTypeIDs) > 0 {
		pinnedDictorShipTypeID = dictorTypeIDs[0]
	}
	if len(heavyDictorTypeIDs) > 0 {
		pinnedHeavyDictorShipTypeID = heavyDictorTypeIDs[0]
	}

	// Exclude all dictors from the general pool so non-camp pilots don't become dictors.
	reservedShipTypeIDs := make(map[int]bool)
	for _, typeID := range dictorTypeIDs {
		reservedShipTypeIDs[typeID] = true
	}
	for _, typeID := range heavyDictorTypeIDs {
		reservedShipTypeIDs[typeID] = true
	}

	// iconBase -> list of ship type IDs that render with that icon.
	baseIconToTypeIDs := make(map[string][]int)
	for typeID, groupName := range typeIDToGroupName {
		if reservedShipTypeIDs[typeID] {
			continue
		}
		// Verify against SDE-backed type names (prevents picking broken/unknown ShipTypeIDs).
		if types == nil || types[typeID] == "" {
			continue
		}
		// Only keep ship types in SDE category `_key=6` (ships).
		if groupIDToCategoryID != nil {
			if groupID, ok := typeIDToGroupID[typeID]; !ok || groupIDToCategoryID[groupID] != 6 {
				continue
			}
		}
		icon := shipTypeIconFilenameFromGroup(groupName, false)
		if icon == "" {
			continue
		}
		baseIconToTypeIDs[icon] = append(baseIconToTypeIDs[icon], typeID)
	}
	for _, list := range baseIconToTypeIDs {
		sort.Ints(list)
	}

	// Preferred hull/icon categories (order matters for round-robin diversity).
	// These filenames are the ones produced by shipTypeIconFilenameFromGroup().
	preferredIcons := []string{
		"battleship_16.png",
		"battleCruiser_16.png",
		"cruiser_16.png",
		"destroyer_16.png",
		"frigate_16.png",
		"industrial_16.png",
		"miningFrigate_16.png",
		"miningBarge_16.png",
		"carrier_16.png",
		"dreadnought_16.png",
		"forceAuxiliary_16.png",
		"supercarrier_16.png",
		"titan_16.png",
		"rookie_16.png",
		"protectiveSentry.png",
		"sentry.png",
	}

	// Flatten into a selection list by taking the i-th element from each preferred icon list.
	// This gives "different types where possible" while still ensuring every ShipTypeID is valid.
	iconIdx := make(map[string]int, len(preferredIcons))
	shipTypePool := make([]int, 0, 500)
	for {
		added := false
		for _, icon := range preferredIcons {
			idx := iconIdx[icon]
			list := baseIconToTypeIDs[icon]
			if idx < len(list) {
				shipTypePool = append(shipTypePool, list[idx])
				iconIdx[icon] = idx + 1
				added = true
			}
		}
		if !added {
			break
		}
	}
	// Fallback safety: if for some reason the preferred category set is empty, include any icon-backed ship types.
	if len(shipTypePool) == 0 {
		for typeID, groupName := range typeIDToGroupName {
			if reservedShipTypeIDs[typeID] {
				continue
			}
			if shipTypeIconFilenameFromGroup(groupName, false) != "" {
				shipTypePool = append(shipTypePool, typeID)
			}
		}
		sort.Ints(shipTypePool)
	}

	characterIDToShipTypeID := make(map[int]int, 200)
	nextShipPoolIdx := 0

	assignShipType := func(characterID int, preferred *int) {
		if characterID == 0 {
			return
		}
		if _, exists := characterIDToShipTypeID[characterID]; exists {
			return
		}
		if preferred != nil {
			// Preferred IDs must exist in the SDE with `description.en` (via `types`).
			if types != nil && types[*preferred] != "" {
				characterIDToShipTypeID[characterID] = *preferred
			}
			// If preferred is missing from `types`, do not assign it here.
			// The caller should ensure preferred IDs come from SDE-verified candidates.
			return
		}
		if nextShipPoolIdx < len(shipTypePool) {
			characterIDToShipTypeID[characterID] = shipTypePool[nextShipPoolIdx]
			nextShipPoolIdx++
			return
		}
		// If we run out (should be rare), keep it valid by reusing a known-good existing ship type.
		if len(shipTypePool) > 0 {
			characterIDToShipTypeID[characterID] = shipTypePool[0]
			return
		}
		// Nothing to assign: leave zero and let UI display "Unknown ship".
		characterIDToShipTypeID[characterID] = 0
	}

	// Pre-assign the interdictors that must remain for the Thera camp logic.
	if pinnedDictorShipTypeID != 0 {
		assignShipType(1000100, &pinnedDictorShipTypeID)
	}
	if pinnedHeavyDictorShipTypeID != 0 {
		assignShipType(1000102, &pinnedHeavyDictorShipTypeID)
	}

	mockKillmails := []struct {
		systemID      int
		systemName    string
		security      float64
		killCount     int // 1, 2, or 3 kills
		attackerCount int // < 10 or >= 10
		killmailID    int
		timeOffset    time.Duration
		routeType     string // "stargates", "thera", "zarzakh", "both"
	}{
		// System 1: Nullsec, 1 kill, < 10 attackers, route through stargates only
		{nullsecSystem1, sys1.SystemName, sys1.Security, 1, 5, 100001, 0 * time.Minute, "stargates"},

		// System 2: Nullsec, 2 kills, >= 10 attackers, route through Thera only
		{nullsecSystem2, sys2.SystemName, sys2.Security, 2, 12, 100002, 1 * time.Minute, "thera"},
		{nullsecSystem2, sys2.SystemName, sys2.Security, 2, 12, 100003, 2 * time.Minute, "thera"},

		// System 3: Nullsec, 3 kills, < 10 attackers, route through Zarzakh only
		{nullsecSystem3, sys3.SystemName, sys3.Security, 3, 7, 100004, 3 * time.Minute, "zarzakh"},
		{nullsecSystem3, sys3.SystemName, sys3.Security, 3, 7, 100005, 4 * time.Minute, "zarzakh"},
		{nullsecSystem3, sys3.SystemName, sys3.Security, 3, 7, 100006, 5 * time.Minute, "zarzakh"},

		// System 4: Nullsec, 2 kills, >= 10 attackers, route through both Thera and Zarzakh
		{nullsecSystem4, sys4.SystemName, sys4.Security, 2, 15, 100007, 6 * time.Minute, "both"},
		{nullsecSystem4, sys4.SystemName, sys4.Security, 2, 15, 100008, 7 * time.Minute, "both"},

		// System 5: Nullsec, 3 kills, >= 10 attackers, route through stargates only (to have 3 kills with >= 10 attackers)
		{nullsecSystem5, sys5.SystemName, sys5.Security, 3, 12, 100013, 8 * time.Minute, "stargates"},
		{nullsecSystem5, sys5.SystemName, sys5.Security, 3, 12, 100014, 9 * time.Minute, "stargates"},
		{nullsecSystem5, sys5.SystemName, sys5.Security, 3, 12, 100015, 10 * time.Minute, "stargates"},

		// System 6: Lowsec, 1 kill, < 10 attackers, route through stargates only
		{lowsecSystem1, lowsec1.SystemName, lowsec1.Security, 1, 3, 100009, 11 * time.Minute, "stargates"},

		// System 7: Lowsec, 2 kills, >= 10 attackers, route through stargates only
		{lowsecSystem2, lowsec2.SystemName, lowsec2.Security, 2, 11, 100010, 12 * time.Minute, "stargates"},
		{lowsecSystem2, lowsec2.SystemName, lowsec2.Security, 2, 11, 100011, 13 * time.Minute, "stargates"},

		// System 8: Nullsec, 1 kill, < 10 attackers, route through system with kill on the way
		{routeSystemWithKill, routeSys.SystemName, routeSys.Security, 1, 6, 100012, 14 * time.Minute, "stargates"},

		// Near Amarr: Lowsec, 1 kill, < 10 attackers
		{amarrSystem, sysAmarr.SystemName, sysAmarr.Security, 1, 3, 100020, 0 * time.Minute, "stargates"},

		// Near Rens: Lowsec, 1 kill, < 10 attackers
		{rensSystem, sysRens.SystemName, sysRens.Security, 1, 4, 100021, 1 * time.Minute, "stargates"},

		// Near Dodixie: Lowsec, 2 kills, < 10 attackers
		{dodixieSystem, sysDodixie.SystemName, sysDodixie.Security, 2, 5, 100022, 2 * time.Minute, "stargates"},
		{dodixieSystem, sysDodixie.SystemName, sysDodixie.Security, 2, 5, 100023, 3 * time.Minute, "stargates"},

		// Near Hek: Lowsec, 1 kill, < 10 attackers
		{hekSystem, sysHek.SystemName, sysHek.Security, 1, 6, 100024, 4 * time.Minute, "stargates"},
	}

	// Create mock Thera signatures with WhType information
	// System 2: route through Thera only (use Capital class wormhole)
	//   Route: trade hub -> Thera -> System 2
	//   Signature stored for System 2 (system immediately after Thera)
	// System 4: route through both Thera and Zarzakh (use Battleship class wormhole)
	//   Route: trade hub -> Thera -> Zarzakh -> System 4
	//   Signature stored for Zarzakh (system immediately after Thera), not System 4
	mockTheraSignatures := map[int]string{
		nullsecSystem2:  "THR-001", // System 2 (destination after Thera)
		ZarzakhSystemID: "THR-002", // Zarzakh (system immediately after Thera for System 4 route)
	}
	mockTheraWhTypes := map[int]string{
		nullsecSystem2:  "N944", // Capital class
		ZarzakhSystemID: "D845", // Battleship class (for route through Thera -> Zarzakh -> System 4)
	}
	// Mark one of the mock Thera signatures as End-of-Life so EOL indication is visible in UI
	mockTheraEOL := map[int]bool{
		nullsecSystem2: true, // System 2's Thera connection is EOL in mock data
	}
	// Note: MaxShipSize will be calculated from WhType in SetMockTheraSignaturesWithWhType

	// Create mock Zarzakh connections
	// System 3: route through Zarzakh only (trade hub -> Zarzakh -> System 3)
	// System 4: route through both Thera and Zarzakh (trade hub -> Thera -> Zarzakh -> System 4)
	// Note: Since Zarzakh is connected to Thera, routes to System 4 will go through both
	mockZarzakhConnections := []int{
		nullsecSystem3, // System 3: route through Zarzakh only
		nullsecSystem4, // System 4: route through both Thera and Zarzakh (via Thera->Zarzakh connection)
	}

	// Set up mock Thera signatures and Zarzakh connections in routefinder
	// This must be done after systems are ensured to exist
	if globalRouteFinder != nil {
		globalRouteFinder.SetMockTheraSignaturesWithWhType(mockTheraSignatures, mockTheraWhTypes, mockTheraEOL)
		log.Printf("Set up %d mock Thera signatures with WhTypes (including EOL flags)", len(mockTheraSignatures))
		globalRouteFinder.SetMockZarzakhConnections(mockZarzakhConnections)
		log.Printf("Set up %d mock Zarzakh connections", len(mockZarzakhConnections))
	}

	// Generate and cache killmails
	// Group by system first
	systemKills := make(map[int][]struct {
		killmailID    int
		attackerCount int
		timeOffset    time.Duration
	})

	for _, mock := range mockKillmails {
		if systemKills[mock.systemID] == nil {
			systemKills[mock.systemID] = make([]struct {
				killmailID    int
				attackerCount int
				timeOffset    time.Duration
			}, 0)
		}
		systemKills[mock.systemID] = append(systemKills[mock.systemID], struct {
			killmailID    int
			attackerCount int
			timeOffset    time.Duration
		}{mock.killmailID, mock.attackerCount, mock.timeOffset})
	}

	// Note: routeSystemWithKill is already included in mockKillmails above,
	// so we don't need to add it again here

	// Generate killmails for each system
	for systemID, kills := range systemKills {
		system := getSystemById(systemID)

		// Ensure system has at least one stargate for positioning
		var locationID int
		var stargatePos [3]float64
		if len(system.Stargates) > 0 {
			locationID = system.Stargates[0].ID
			stargatePos = system.Stargates[0].Position
		} else {
			// Create a mock stargate if system doesn't have one
			locationID = 50000000 + systemID
			stargatePos = [3]float64{0, 0, 0}

			// Add the stargate to the system's Stargates array
			system.Stargates = append(system.Stargates, Stargate{
				ID:                    locationID,
				Position:              stargatePos,
				DestinationStargateID: 30000142, // Connect to Jita
			})

			// Update the system in the systems list
			for i := range systems {
				if systems[i].SystemID == systemID {
					systems[i] = system
					break
				}
			}

			// Rebuild routefinder graph
			if globalRouteFinder != nil {
				routefinderSystems := convertSystemsToRoutefinder(systems)
				globalRouteFinder.RebuildGraph(routefinderSystems)
			}
		}

		// Position kill 500km from stargate
		killPosNearGate := [3]float64{
			stargatePos[0] + 500000,
			stargatePos[1],
			stargatePos[2],
		}

		for i, kill := range kills {
			killmailID := kill.killmailID
			// Ensure killmail time is in the past (within 15 minutes from now)
			// baseTime is 14 minutes ago, add offset (0-14 minutes) + i*30 seconds
			// This ensures all times are between 14 minutes ago and now
			offsetSeconds := int(kill.timeOffset.Seconds()) + i*30
			killTime := baseTime.Add(time.Duration(offsetSeconds) * time.Second)

			// Safety check: ensure killTime is not in the future
			if killTime.After(startTime) {
				killTime = startTime.Add(-1 * time.Minute) // Set to 1 minute ago if somehow in future
			}

			// Generate attackers
			attackers := make([]zkillboardcache.ESIAttacker, 0, kill.attackerCount+2)
			for j := 0; j < kill.attackerCount; j++ {
				characterID := 1000000 + j + killmailID
				assignShipType(characterID, nil)
				attackers = append(attackers, zkillboardcache.ESIAttacker{
					CharacterID:    characterID,
					CorporationID:  2000000 + j + killmailID,
					AllianceID:     3000000 + j + killmailID,
					SecurityStatus: -2.0 + float64(j)*0.1,
					DamageDone:     1000 + j*100,
					FinalBlow:      j == 0,
					WeaponTypeID:   2456, // Standard weapon
					ShipTypeID:     characterIDToShipTypeID[characterID],
				})
				if killmailID%2 == 0 && j == 0 {
					setMockCharacterNameFailed(characterID)
				} else {
					setMockCharacterName(characterID)
				}
			}
			attackers = append(attackers,
				zkillboardcache.ESIAttacker{
					CharacterID:    0,
					CorporationID:  0,
					AllianceID:     0,
					SecurityStatus: 0,
					DamageDone:     400,
					FinalBlow:      false,
					WeaponTypeID:   2456,
					ShipTypeID:     mockNPCAttackerShipTypeID,
				},
				zkillboardcache.ESIAttacker{
					CharacterID:    0,
					CorporationID:  0,
					AllianceID:     0,
					SecurityStatus: 0,
					DamageDone:     250,
					FinalBlow:      false,
					WeaponTypeID:   2456,
					ShipTypeID:     mockNPCAttackerSentryTypeID,
				},
			)

			// Create victim
			victimCharacterID := 5000000 + killmailID
			assignShipType(victimCharacterID, nil)
			victim := zkillboardcache.ESIVictim{
				CharacterID:   victimCharacterID,
				CorporationID: 6000000 + killmailID,
				AllianceID:    7000000 + killmailID,
				DamageTaken:   10000,
				ShipTypeID:    characterIDToShipTypeID[victimCharacterID],
				Position: &struct {
					X float64 `json:"x"`
					Y float64 `json:"y"`
					Z float64 `json:"z"`
				}{
					X: killPosNearGate[0],
					Y: killPosNearGate[1],
					Z: killPosNearGate[2],
				},
			}
			setMockCharacterName(victimCharacterID)

			// Create ZKB info
			zkbInfo := zkillboardcache.ZKillboardKill{
				KillmailID: killmailID,
				ZKB: zkillboardcache.ZKillboardKillInfo{
					LocationID:     locationID,
					Hash:           fmt.Sprintf("mockhash%d", killmailID),
					FittedValue:    1000000.0,
					DroppedValue:   500000.0,
					DestroyedValue: 500000.0,
					TotalValue:     2000000.0,
					Points:         10,
					NPC:            false,
					Solo:           false,
					AWOX:           false,
					Labels:         []string{},
				},
			}

			// Create cached killmail
			cachedKillmail := zkillboardcache.CachedKillmail{
				KillmailID:    killmailID,
				KillmailTime:  killTime.Format("2006-01-02T15:04:05Z"),
				Victim:        victim,
				Attackers:     attackers,
				ZKBInfo:       zkbInfo,
				SolarSystemID: systemID,
			}

			// Add to cache
			killmailCache.AddKillmail(killmailID, &cachedKillmail)

			// Trigger callback to precalculate data
			calculateDataForKillmail(killmailID, &cachedKillmail)
		}
	}

	// Add Thera camp kill: in Thera, not at station, >10k km from stations, Interdictor involved
	// Pre-populate Thera station positions for distance check (mock positions at origin)
	theraStationPositionsMu.Lock()
	theraStationPositionsCache[TheraStationID1] = [3]float64{0, 0, 0}
	theraStationPositionsCache[TheraStationID2] = [3]float64{0, 0, 0}
	theraStationPositionsCache[TheraStationID3] = [3]float64{0, 0, 0}
	theraStationPositionsCache[TheraStationID4] = [3]float64{0, 0, 0}
	theraStationPositionsMu.Unlock()
	theraCampKillmailID := 100100
	theraCampKillTime := baseTime.Add(12 * time.Minute)
	// Position 15 billion m from origin (stations at 0,0,0) = 15M km, well over 10k km
	theraCampKillPos := [3]float64{15e9, 0, 0}

	// Assign unique ship types for the remaining Thera-camp pilots.
	assignShipType(1000101, nil)
	assignShipType(1000104, nil)
	assignShipType(1000105, nil)
	assignShipType(1000106, nil)
	assignShipType(1000107, nil)
	assignShipType(5000100, nil)

	theraCampAttackers := []zkillboardcache.ESIAttacker{
		{
			CharacterID:    1000100,
			CorporationID:  2000100,
			AllianceID:     3000100,
			SecurityStatus: -1.5,
			DamageDone:     8000,
			FinalBlow:      true,
			WeaponTypeID:   2456,
			ShipTypeID:     characterIDToShipTypeID[1000100], // Sabre (Interdictor)
		},
		{
			CharacterID:    1000101,
			CorporationID:  2000100,
			AllianceID:     3000100,
			SecurityStatus: -2.0,
			DamageDone:     2000,
			FinalBlow:      false,
			WeaponTypeID:   2456,
			ShipTypeID:     characterIDToShipTypeID[1000101],
		},
		{
			CharacterID:    1000104,
			CorporationID:  2000100,
			AllianceID:     3000100,
			SecurityStatus: -1.8,
			DamageDone:     1500,
			FinalBlow:      false,
			WeaponTypeID:   2456,
			ShipTypeID:     characterIDToShipTypeID[1000104],
		},
		{
			CharacterID:    1000105,
			CorporationID:  2000101,
			AllianceID:     3000100,
			SecurityStatus: -1.2,
			DamageDone:     1200,
			FinalBlow:      false,
			WeaponTypeID:   2456,
			ShipTypeID:     characterIDToShipTypeID[1000105],
		},
		{
			CharacterID:    1000106,
			CorporationID:  2000100,
			AllianceID:     3000101,
			SecurityStatus: -0.5,
			DamageDone:     900,
			FinalBlow:      false,
			WeaponTypeID:   2456,
			ShipTypeID:     characterIDToShipTypeID[1000106],
		},
		{
			CharacterID:    1000107,
			CorporationID:  2000102,
			AllianceID:     3000101,
			SecurityStatus: -2.5,
			DamageDone:     600,
			FinalBlow:      false,
			WeaponTypeID:   2456,
			ShipTypeID:     characterIDToShipTypeID[1000107],
		},
		{
			CharacterID: 0, CorporationID: 0, AllianceID: 0, SecurityStatus: 0,
			DamageDone: 500, FinalBlow: false, WeaponTypeID: 2456,
			ShipTypeID: mockNPCAttackerShipTypeID,
		},
		{
			CharacterID: 0, CorporationID: 0, AllianceID: 0, SecurityStatus: 0,
			DamageDone: 200, FinalBlow: false, WeaponTypeID: 2456,
			ShipTypeID: mockNPCAttackerSentryTypeID,
		},
	}
	setMockCharacterName(1000100)
	setMockCharacterName(1000101)
	setMockCharacterName(1000104)
	setMockCharacterName(1000105)
	setMockCharacterNameFailed(1000106)
	setMockCharacterNameFailed(1000107)
	theraCampVictim := zkillboardcache.ESIVictim{
		CharacterID:   5000100,
		CorporationID: 6000100,
		AllianceID:    7000100,
		DamageTaken:   10000,
		ShipTypeID:    characterIDToShipTypeID[5000100],
		Position: &struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
			Z float64 `json:"z"`
		}{
			X: theraCampKillPos[0],
			Y: theraCampKillPos[1],
			Z: theraCampKillPos[2],
		},
	}
	setMockCharacterName(5000100)
	theraCampZKB := zkillboardcache.ZKillboardKill{
		KillmailID: theraCampKillmailID,
		ZKB: zkillboardcache.ZKillboardKillInfo{
			LocationID:     0, // In space, not at structure
			Hash:           "mockhash100100",
			FittedValue:    5000000.0,
			DroppedValue:   1000000.0,
			DestroyedValue: 4000000.0,
			TotalValue:     10000000.0,
			Points:         15,
			NPC:            false,
			Solo:           false,
			AWOX:           false,
			Labels:         []string{},
		},
	}
	theraCampCached := zkillboardcache.CachedKillmail{
		KillmailID:    theraCampKillmailID,
		KillmailTime:  theraCampKillTime.Format("2006-01-02T15:04:05Z"),
		Victim:        theraCampVictim,
		Attackers:     theraCampAttackers,
		ZKBInfo:       theraCampZKB,
		SolarSystemID: TheraSystemID,
	}
	killmailCache.AddKillmail(theraCampKillmailID, &theraCampCached)
	calculateDataForKillmail(theraCampKillmailID, &theraCampCached)

	// Second Thera camp kill (different position, Eris/Interdictor)
	theraCamp2KillmailID := 100101
	theraCamp2KillTime := baseTime.Add(14 * time.Minute)
	theraCamp2KillPos := [3]float64{20e9, 0, 0}

	// Assign unique ship types for the remaining Thera-camp pilots.
	assignShipType(1000103, nil)
	assignShipType(5000101, nil)

	theraCamp2Attackers := []zkillboardcache.ESIAttacker{
		{
			CharacterID:    1000102,
			CorporationID:  2000100,
			AllianceID:     3000100,
			SecurityStatus: -2.0,
			DamageDone:     7500,
			FinalBlow:      true,
			WeaponTypeID:   2456,
			ShipTypeID:     characterIDToShipTypeID[1000102], // Eris (Interdictor)
		},
		{
			CharacterID:    1000103,
			CorporationID:  2000100,
			AllianceID:     3000100,
			SecurityStatus: -1.0,
			DamageDone:     2500,
			FinalBlow:      false,
			WeaponTypeID:   2486,
			ShipTypeID:     characterIDToShipTypeID[1000103],
		},
		{
			CharacterID: 0, CorporationID: 0, AllianceID: 0, SecurityStatus: 0,
			DamageDone: 450, FinalBlow: false, WeaponTypeID: 2456,
			ShipTypeID: mockNPCAttackerShipTypeID,
		},
		{
			CharacterID: 0, CorporationID: 0, AllianceID: 0, SecurityStatus: 0,
			DamageDone: 180, FinalBlow: false, WeaponTypeID: 2456,
			ShipTypeID: mockNPCAttackerSentryTypeID,
		},
	}
	setMockCharacterName(1000102)
	setMockCharacterName(1000103)
	theraCamp2Victim := zkillboardcache.ESIVictim{
		CharacterID:   5000101,
		CorporationID: 6000100,
		AllianceID:    7000100,
		DamageTaken:   10000,
		ShipTypeID:    characterIDToShipTypeID[5000101],
		Position: &struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
			Z float64 `json:"z"`
		}{
			X: theraCamp2KillPos[0],
			Y: theraCamp2KillPos[1],
			Z: theraCamp2KillPos[2],
		},
	}
	setMockCharacterName(5000101)
	theraCamp2ZKB := zkillboardcache.ZKillboardKill{
		KillmailID: theraCamp2KillmailID,
		ZKB: zkillboardcache.ZKillboardKillInfo{
			LocationID:     0,
			Hash:           "mockhash100101",
			FittedValue:    4500000.0,
			DroppedValue:   900000.0,
			DestroyedValue: 3600000.0,
			TotalValue:     9000000.0,
			Points:         14,
			NPC:            false,
			Solo:           false,
			AWOX:           false,
			Labels:         []string{},
		},
	}
	theraCamp2Cached := zkillboardcache.CachedKillmail{
		KillmailID:    theraCamp2KillmailID,
		KillmailTime:  theraCamp2KillTime.Format("2006-01-02T15:04:05Z"),
		Victim:        theraCamp2Victim,
		Attackers:     theraCamp2Attackers,
		ZKBInfo:       theraCamp2ZKB,
		SolarSystemID: TheraSystemID,
	}
	killmailCache.AddKillmail(theraCamp2KillmailID, &theraCamp2Cached)
	calculateDataForKillmail(theraCamp2KillmailID, &theraCamp2Cached)

	systemKills[TheraSystemID] = []struct {
		killmailID    int
		attackerCount int
		timeOffset    time.Duration
	}{
		{theraCampKillmailID, 8, 12 * time.Minute},
		{theraCamp2KillmailID, 4, 14 * time.Minute},
	}
	log.Printf("Added 2 mock Thera camp kills (dictors + NPC Serpentis Captain + sentry)")

	totalKills := 0
	systemCount := 0
	lowsecCount := 0
	nullsecCount := 0
	systemDetails := make(map[int]string)
	for systemID, kills := range systemKills {
		totalKills += len(kills)
		systemCount++
		system := getSystemById(systemID)
		if system.SystemID != 0 {
			if system.Security < 0.45 {
				if system.Security < 0 {
					nullsecCount++
					systemDetails[systemID] = fmt.Sprintf("nullsec: %s (%.2f)", system.SystemName, system.Security)
				} else {
					lowsecCount++
					systemDetails[systemID] = fmt.Sprintf("lowsec: %s (%.2f)", system.SystemName, system.Security)
				}
			}
		}
	}
	fmt.Fprintf(mw, "Loaded %d mock killmails across %d systems (%d nullsec, %d lowsec)\n",
		totalKills, systemCount, nullsecCount, lowsecCount)

	// Log system details
	fmt.Fprintf(mw, "System details:\n")
	for systemID, details := range systemDetails {
		fmt.Fprintf(mw, "  - System %d: %s\n", systemID, details)
	}

	// Log route types
	fmt.Fprintf(mw, "Route types configured:\n")
	fmt.Fprintf(mw, "  - Stargates only: Systems 1, 5, 6 (lowsec), 7 (lowsec), 8\n")
	fmt.Fprintf(mw, "  - Thera only: System 2\n")
	fmt.Fprintf(mw, "  - Zarzakh only: System 3\n")
	fmt.Fprintf(mw, "  - Both Thera and Zarzakh: System 4\n")
	fmt.Fprintf(mw, "  - Thera camp: 2 kills in Thera with Interdictors (Sabre, Eris), >10k km from stations\n")
}

// displayEveSecurityForUI maps SDE true security to the one-decimal value used for EVE-style UI.
// Any system with 0 < truesec < 0.1 is treated as the 0.1 lowsec band (CCP stores e.g. 0.035519; naive
// %.1f prints 0.0). Truesec exactly 0 remains nullsec 0.0.
func displayEveSecurityForUI(s float64) float64 {
	if s < 0 {
		return 0
	}
	if s == 0 {
		return 0
	}
	if s < 0.1 {
		return 0.1
	}
	return math.Round(s*10) / 10
}

// getSecurityColor returns the color hex code for a given security value
func getSecurityColor(securityValue float64) string {
	// Security color mapping (same as frontend)
	securityColorMap := map[float64]string{
		1.0: "#2E74DF",
		0.9: "#3B9CEC",
		0.8: "#49D0F1",
		0.7: "#5CDCA6",
		0.6: "#72E352",
		0.5: "#EEFF83",
		0.4: "#E06A0B",
		0.3: "#CE4610",
		0.2: "#BC1211",
		0.1: "#6C2222",
		0.0: "#8D3263",
	}

	displayed := displayEveSecurityForUI(securityValue)

	// Return matching color or default to 0.0 color if not found
	if color, ok := securityColorMap[displayed]; ok {
		return color
	}

	return securityColorMap[0.0]
}

// isPrimaryTradeHub checks if a system ID is a primary trade hub
func isPrimaryTradeHub(systemID int) bool {
	for _, hub := range tradeHubs {
		if hub.SystemID == systemID && hub.Type == "primary" {
			return true
		}
	}
	return false
}

// getTradeHubSystemID returns the system ID for a trade hub by name, or 0 if not found
func getTradeHubSystemID(tradeHubName string) int {
	for _, hub := range tradeHubs {
		if hub.Name == tradeHubName {
			return hub.SystemID
		}
	}
	return 0
}

// HubClosestStationRow is used for both JSON API and server-rendered jump clone table
type HubClosestStationRow struct {
	HubName     string
	HubSystemID int
	HubStation  *struct {
		Name, SystemName string
		StationID        int
	} // nil or same system as closest
	ClosestStation struct {
		Name, SystemName    string
		SystemID, StationID int
	}
}

// getHubsClosestJumpClonesData returns hub-to-closest-jump-clone rows for server-side rendering and JSON API
func getHubsClosestJumpClonesData() []HubClosestStationRow {
	result := make([]HubClosestStationRow, 0, len(tradeHubs))
	for _, hub := range tradeHubs {
		if hub.Type != "primary" && hub.Type != "special" {
			continue
		}
		hubSystem := getSystemById(hub.SystemID)
		if hubSystem.SystemID == 0 {
			continue
		}
		if hub.Type == "special" {
			hubStationID := hub.StationID
			if hubStationID == 0 && hub.StationName != "" {
				hubStationID = getStationIDByName(hub.SystemID, hub.StationName)
				if hubStationID == 0 {
					hubStationID = getStationIDForSystem(hub.SystemID)
				}
			}
			row := HubClosestStationRow{
				HubName:     hub.Name,
				HubSystemID: hub.SystemID,
			}
			if hub.StationName != "" {
				row.HubStation = &struct {
					Name, SystemName string
					StationID        int
				}{
					hub.StationName, hubSystem.SystemName, hubStationID,
				}
			}
			result = append(result, row)
			continue
		}

		var bestStation *JumpCloneStation
		bestJumps := -1
		for i := range jumpCloneStations {
			jc := &jumpCloneStations[i]
			if jc.SystemID == 0 {
				continue
			}
			if globalRouteFinder == nil {
				if jc.SystemID == hub.SystemID {
					bestStation = jc
					bestJumps = 0
					break
				}
				continue
			}
			route, err := globalRouteFinder.FindShortestRouteWithThera(hub.SystemID, jc.SystemID, 0)
			if err != nil {
				continue
			}
			if bestJumps < 0 || route.Jumps < bestJumps {
				bestJumps = route.Jumps
				bestStation = jc
			}
		}
		if bestStation == nil {
			continue
		}
		closestStationID := bestStation.StationID
		if closestStationID == 0 {
			closestStationID = getStationIDByName(bestStation.SystemID, bestStation.Name)
			if closestStationID == 0 {
				closestStationID = getStationIDForSystem(bestStation.SystemID)
			}
		}
		row := HubClosestStationRow{
			HubName:     hub.Name,
			HubSystemID: hub.SystemID,
			ClosestStation: struct {
				Name       string
				SystemName string
				SystemID   int
				StationID  int
			}{
				Name:       bestStation.Name,
				SystemName: bestStation.SystemName,
				SystemID:   bestStation.SystemID,
				StationID:  closestStationID,
			},
		}
		if hub.StationName != "" {
			hubStationID := getStationIDByName(hub.SystemID, hub.StationName)
			if hubStationID == 0 {
				hubStationID = getStationIDForSystem(hub.SystemID)
			}
			row.HubStation = &struct {
				Name, SystemName string
				StationID        int
			}{
				hub.StationName, hubSystem.SystemName, hubStationID,
			}
		} else if bestStation.SystemID == hub.SystemID {
			hubStationID := bestStation.StationID
			if hubStationID == 0 {
				hubStationID = getStationIDByName(bestStation.SystemID, bestStation.Name)
				if hubStationID == 0 {
					hubStationID = getStationIDForSystem(bestStation.SystemID)
				}
			}
			row.HubStation = &struct {
				Name, SystemName string
				StationID        int
			}{
				bestStation.Name, hubSystem.SystemName, hubStationID,
			}
		} else {
			row.HubStation = &struct {
				Name, SystemName string
				StationID        int
			}{
				"", hubSystem.SystemName, getStationIDForSystem(hub.SystemID),
			}
		}
		result = append(result, row)
	}
	return result
}

// renderJumpCloneTableHTML returns the tbody rows HTML for the jump clone table (server-rendered)
func renderJumpCloneTableHTML() string {
	rows := getHubsClosestJumpClonesData()
	var b strings.Builder
	for _, row := range rows {
		hubText := row.HubName
		if row.HubStation != nil && row.HubStation.Name != "" {
			hubText = row.HubStation.Name
		}
		hubSystemName := row.HubName
		if row.HubStation != nil {
			hubSystemName = row.HubStation.SystemName
		}
		hubStationID := 0
		if row.HubStation != nil {
			hubStationID = row.HubStation.StationID
		}
		// Hub cell + button
		b.WriteString("<tr><td class='hub-checkbox'><input type='checkbox' class='trade-hub-checkbox' data-trade-hub='")
		b.WriteString(template.HTMLEscapeString(strings.ToLower(row.HubName)))
		b.WriteString("' checked></td><td class='hub-name'><div class='station-with-btn'><span class='station-text'>")
		b.WriteString(template.HTMLEscapeString(hubText))
		b.WriteString("</span><button class='jump-clone-destination-btn' data-system-id='")
		b.WriteString(strconv.Itoa(row.HubSystemID))
		b.WriteString("' data-system-name='")
		b.WriteString(template.HTMLEscapeString(hubSystemName))
		if hubStationID > 0 {
			b.WriteString("' data-station-id='")
			b.WriteString(strconv.Itoa(hubStationID))
		}
		b.WriteString("' style='display: none;'>Set destination</button></div></td>")
		stationsMatch := row.HubStation != nil && row.HubStation.Name != "" && row.ClosestStation.Name != "" &&
			row.HubStation.Name == row.ClosestStation.Name
		if stationsMatch {
			b.WriteString("<td style='display: none;'></td>")
		} else if row.ClosestStation.Name == "" {
			b.WriteString("<td></td>")
		} else {
			b.WriteString("<td><div class='station-with-btn'><span class='station-text'>")
			b.WriteString(template.HTMLEscapeString(row.ClosestStation.Name))
			b.WriteString("</span><button class='jump-clone-destination-btn' data-system-id='")
			b.WriteString(strconv.Itoa(row.ClosestStation.SystemID))
			b.WriteString("' data-system-name='")
			b.WriteString(template.HTMLEscapeString(row.ClosestStation.SystemName))
			if row.ClosestStation.StationID > 0 {
				b.WriteString("' data-station-id='")
				b.WriteString(strconv.Itoa(row.ClosestStation.StationID))
			}
			b.WriteString("' style='display: none;'>Set destination</button></div></td>")
		}
		b.WriteString("</tr>")
	}
	return b.String()
}

// getNearTradeHubsResult returns the same data as /api/near_trade_hubs.html for server-side initial render
func getNearTradeHubsResult() []SystemInRange {
	readFn := precalculatedData.Read()
	data := readFn()
	oneHourAgo := time.Now().Add(-1 * time.Hour)
	var result []SystemInRange
	now := time.Now()
	for _, precalc := range data.nearTradeHubsMode {
		// Exclude W-space (J-space) from main table - not reachable via stargates
		if precalc.SystemID >= 31000000 && precalc.SystemID <= 31999999 {
			continue
		}
		filteredKills := make([]CachedKillmail, 0, len(precalc.RecentKills))
		for _, kill := range precalc.RecentKills {
			if hasOnlyNPCs(&kill) {
				continue
			}
			killTime, err := time.Parse("2006-01-02T15:04:05Z", kill.KillmailTime)
			if err != nil {
				continue
			}
			if killTime.After(oneHourAgo) {
				filteredKills = append(filteredKills, kill)
			}
		}
		if len(filteredKills) == 0 || precalc.Dist > nearTradeHubsMaxDisplayJumps {
			continue
		}
		var latestKillTime time.Time
		for _, kill := range filteredKills {
			killTime, _ := time.Parse("2006-01-02T15:04:05Z", kill.KillmailTime)
			if latestKillTime.IsZero() || killTime.After(latestKillTime) {
				latestKillTime = killTime
			}
		}
		weight := float64(precalc.Dist)
		if !latestKillTime.IsZero() {
			weight += now.Sub(latestKillTime).Minutes()
		}

		theraInboundSig := ""
		theraOutboundSig := ""
		if globalRouteFinder != nil && len(precalc.Route) > 0 {
			routePath := make([]int, 0, len(precalc.Route))
			for _, rs := range precalc.Route {
				routePath = append(routePath, rs.SystemID)
			}
			inSig, outSig, _ := globalRouteFinder.GetTheraSignatureIDsForRoute(routePath)
			theraInboundSig, theraOutboundSig = inSig, outSig
		}
		result = append(result, SystemInRange{
			SystemID:               precalc.SystemID,
			Name:                   precalc.Name,
			Dist:                   precalc.Dist,
			Security:               precalc.Security,
			RecentKills:            filteredKills,
			ViaThera:               precalc.ViaThera,
			TheraDist:              precalc.TheraDist,
			TheraInfo:              precalc.TheraInfo,
			TheraInboundSignature:  theraInboundSig,
			TheraOutboundSignature: theraOutboundSig,
			MaxShipSize:            precalc.MaxShipSize,
			Route:                  precalc.Route,
			TradeHub:               precalc.TradeHub,
			Weight:                 weight,
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Weight < result[j].Weight })
	return result
}

// renderHTMLTable renders the systems table as HTML without resolving character names.
// (Used for contexts where we don't want to block on name lookups.)
func renderHTMLTable(systems []SystemInRange, mode string) string {
	return renderHTMLTableWithNames(systems, mode, nil, nil)
}

// renderHTMLTableWithNames renders the systems table as HTML, using pre-resolved character names.
func renderHTMLTableWithNames(systems []SystemInRange, mode string, characterNames map[int]string, characterNameErrors map[int]string) string {
	var html strings.Builder
	html.WriteString("<table id='result'><thead><tr>")
	showUedamaScoutIcon := isUedamaScoutLive()

	tradeHubFilterIconHTML := ""
	if mode == "near_trade_hubs" {
		tradeHubFilterIconHTML = getFilterIconHTMLOrFallback()
	}

	if mode == "near_trade_hubs" {
		html.WriteString("<th>System</th><th class='trade-hub-header'>Trade hub</th><th class='recency-header'><span class='tooltip-icon' data-tooltip='An estimated value indicates how old will be information about the recent kill when you arrive to the system. Calculated as a distance from trade hub + age of most recent kill in minutes.'>Recency\u00A0ⓘ</span></th><th class='range-header'><span class='tooltip-icon' data-tooltip='Shortest route (jump count) from the trade hub.'>Range\u00A0ⓘ</span></th><th>Notes</th>")
	} else if mode == "proximity" {
		html.WriteString("<th>System</th><th class='recency-header'><span class='tooltip-icon' data-tooltip='An estimated value indicating how old the information about the recent kill will be when you arrive. Calculated as distance from your location + age of most recent kill in minutes. Lower values indicate fresher kills and/or closer systems.'>Recency\u00A0ⓘ</span></th><th class='range-header'><span class='tooltip-icon' data-tooltip='Shortest route (jump count) from your location.'>Range\u00A0ⓘ</span></th><th>Notes</th>")
	} else {
		html.WriteString("<th>System</th><th class='range-header'><span class='tooltip-icon' data-tooltip='Shortest route (jump count).'>Range\u00A0ⓘ</span></th><th>Notes</th>")
	}

	html.WriteString("</tr></thead><tbody>")

	// Get all systems with recent kills to check route systems
	systemsWithKills := getSystemsWithPrecalculatedKills()

	// Compute pilots that appear in multiple different systems (current result set).
	// Used to highlight "repeat offenders" across systems.
	pilotMultiSystem := make(map[int]bool) // characterID -> true
	{
		pilotSystems := make(map[int]map[int]struct{}) // characterID -> set(systemID)
		for _, sys := range systems {
			for _, kill := range sys.RecentKills {
				// Count pilots regardless of attacker count so repeat pilots get the correct icon.
				killSysID := kill.SolarSystemID
				if killSysID == 0 {
					killSysID = sys.SystemID
				}
				for _, attacker := range kill.Attackers {
					if attacker.CharacterID == 0 {
						continue
					}
					sysSet := pilotSystems[attacker.CharacterID]
					if sysSet == nil {
						sysSet = make(map[int]struct{})
						pilotSystems[attacker.CharacterID] = sysSet
					}
					sysSet[killSysID] = struct{}{}
				}
			}
		}
		for characterID, sysSet := range pilotSystems {
			if len(sysSet) > 1 {
				pilotMultiSystem[characterID] = true
			}
		}
	}

	// Create a set of system IDs that are in the current result set (for link validation)
	systemsInResult := make(map[int]bool)
	for _, system := range systems {
		systemsInResult[system.SystemID] = true
	}

	for _, system := range systems {
		html.WriteString("<tr id='system-")
		html.WriteString(strconv.Itoa(system.SystemID))
		html.WriteString("' data-system='")
		html.WriteString(strconv.Itoa(system.SystemID))
		if mode == "near_trade_hubs" {
			html.WriteString("' data-trade-hub-row='")
			html.WriteString(template.HTMLEscapeString(system.TradeHub))
		}
		html.WriteString("'>")

		securityValue := system.Security
		displayValue := displayEveSecurityForUI(securityValue)

		// System cell — data-security uses the same one-decimal display as the UI (see displayEveSecurityForUI).
		displayFormatted := fmt.Sprintf("%.1f", displayValue)
		if displayFormatted == "-0.0" {
			displayFormatted = "0.0"
		}

		html.WriteString("<td data-label='System' data-security='")
		html.WriteString(displayFormatted)
		html.WriteString("'><div class='system-cell-name'><a href='https://zkillboard.com/system/")
		html.WriteString(strconv.Itoa(system.SystemID))
		html.WriteString("/' target='_blank'")

		// Check if this system is a primary trade hub and colorize accordingly
		// Use displayValue (rounded) for colorization to match displayed security
		if isPrimaryTradeHub(system.SystemID) {
			html.WriteString(" class='trade-hub' style='color: ")
			html.WriteString(getSecurityColor(displayValue))
			html.WriteString("; font-weight: bold;'")
		}

		html.WriteString(">")
		html.WriteString(template.HTMLEscapeString(system.Name))
		html.WriteString("</a>")
		html.WriteString(" (")
		// Add security status in braces with color
		html.WriteString("<span style='color: ")
		html.WriteString(getSecurityColor(displayValue))
		html.WriteString(";'>")
		html.WriteString(displayFormatted)
		html.WriteString("</span>)")
		if showUedamaScoutIcon && isUedamaScoutMonitoredSystem(system.Name) {
			html.WriteString(" <a href='")
			html.WriteString(uedamaScoutTwitchURL)
			html.WriteString("' target='_blank' rel='noopener noreferrer' class='system-twitch-link' title='Watch live on UedamaScout Twitch'>")
			html.WriteString(getTwitchIconHTMLOrFallback())
			html.WriteString("</a>")
		}
		html.WriteString("</div><div class='system-cell-destination'>")
		// Add "Set destination" button (will be shown/hidden by frontend based on auth status)
		// Get station ID for this system to set destination exactly to station
		stationID := getStationIDForSystem(system.SystemID)
		html.WriteString("<button class='set-destination-btn' data-system-id='")
		html.WriteString(strconv.Itoa(system.SystemID))
		html.WriteString("' data-system-name='")
		html.WriteString(template.HTMLEscapeString(system.Name))
		if stationID > 0 {
			html.WriteString("' data-station-id='")
			html.WriteString(strconv.Itoa(stationID))
		}
		// Add route information for Thera handling
		// Check if Thera is in the route path (more reliable than just checking ViaThera flag)
		routeContainsThera := system.ViaThera
		if !routeContainsThera && len(system.Route) > 0 {
			// Check if Thera system ID is actually in the route path
			for _, routeSystem := range system.Route {
				if routeSystem.SystemID == TheraSystemID {
					routeContainsThera = true
					break
				}
			}
		}

		if routeContainsThera && len(system.Route) > 0 {
			html.WriteString("' data-via-thera='true' data-route-path='")
			// Encode route path as JSON array of system IDs
			routePath := make([]int, len(system.Route))
			for i, routeSystem := range system.Route {
				routePath[i] = routeSystem.SystemID
			}
			routePathJSON, _ := json.Marshal(routePath)
			html.WriteString(template.HTMLEscapeString(string(routePathJSON)))
		}
		html.WriteString("' style='display: none; padding: 2px 8px; font-size: 0.85em; cursor: pointer;'>Set destination</button></div></td>")

		if mode == "near_trade_hubs" {
			// Trade Hub cell - colorize based on trade hub's own security status
			html.WriteString("<td data-label='Trade hub' data-trade-hub='")
			html.WriteString(template.HTMLEscapeString(system.TradeHub))
			html.WriteString("'")

			// Get trade hub's system ID and look up its security status
			tradeHubSystemID := getTradeHubSystemID(system.TradeHub)
			if tradeHubSystemID != 0 {
				tradeHubSystem := getSystemById(tradeHubSystemID)
				if tradeHubSystem.SystemID != 0 {
					tradeHubDisplayValue := displayEveSecurityForUI(tradeHubSystem.Security)
					html.WriteString(" style='color: ")
					html.WriteString(getSecurityColor(tradeHubDisplayValue))
					html.WriteString(";'")
				}
			}

			html.WriteString(">")
			html.WriteString("<button type='button' class='trade-hub-filter-btn' data-trade-hub-filter='")
			html.WriteString(template.HTMLEscapeString(system.TradeHub))
			html.WriteString("' aria-label='Filter by trade hub: ")
			html.WriteString(template.HTMLEscapeString(system.TradeHub))
			html.WriteString("' title='Filter by this trade hub'>")
			html.WriteString(tradeHubFilterIconHTML)
			html.WriteString("</button>")

			html.WriteString(template.HTMLEscapeString(system.TradeHub))
			html.WriteString("</td>")

			// Recency cell
			html.WriteString("<td data-label='Recency' data-recency='")
			html.WriteString(fmt.Sprintf("%d", int(system.Weight)))
			html.WriteString("'>")
			html.WriteString(fmt.Sprintf("%d", int(system.Weight)))
			html.WriteString("</td>")
		} else if mode == "proximity" {
			// Recency cell (distance from your location + kill age)
			html.WriteString("<td data-label='Recency' data-recency='")
			html.WriteString(fmt.Sprintf("%d", int(system.Weight)))
			html.WriteString("'>")
			html.WriteString(fmt.Sprintf("%d", int(system.Weight)))
			html.WriteString("</td>")
		}

		// Distance cell
		html.WriteString("<td data-label='Range'>")

		// Check if route contains systems with kills (excluding the destination itself)
		routeHasKills := false
		for _, routeSystem := range system.Route {
			// Skip the destination system itself
			if routeSystem.SystemID == system.SystemID {
				continue
			}
			if kills, exists := systemsWithKills[routeSystem.SystemID]; exists && len(kills) > 0 {
				routeHasKills = true
				break
			}
		}

		// In live mode, EOL comes from Thera signatures via routefinder and is encoded
		// into system.TheraInfo. In mock mode, ensure at least one example system
		// (BWF-ZZ/nullsecSystem2) always shows EOL so the UI state is visible.
		theraEOL := strings.Contains(system.TheraInfo, "EOL") || (mockData && system.SystemID == 30002440)

		distanceSpanClass := "distance-value"
		if theraEOL {
			distanceSpanClass += " thera-eol"
		}
		html.WriteString("<span class='")
		html.WriteString(template.HTMLEscapeString(distanceSpanClass))
		html.WriteString("'>")

		// Add warning sign if route has kills
		if routeHasKills {
			html.WriteString("<span class='route-warning-sign'>⚠</span> ")
		}

		html.WriteString(strconv.Itoa(system.Dist))
		// Check if route contains Thera and/or Zarzakh
		// Note: routeContainsThera was already set above in button generation section
		// Re-check here for display purposes (in case it wasn't set)
		if !routeContainsThera {
			for _, routeSystem := range system.Route {
				if routeSystem.SystemID == TheraSystemID {
					routeContainsThera = true
					break
				}
			}
			// Also check if system has Thera info (for routes that go via Thera)
			if system.ViaThera || system.TheraInfo != "" {
				routeContainsThera = true
			}
		}
		routeContainsZarzakh := false
		for _, routeSystem := range system.Route {
			if routeSystem.SystemID == ZarzakhSystemID {
				routeContainsZarzakh = true
				break
			}
		}
		// Don't add redundant route suffix when trade hub already identifies the origin
		hasRedundantRouteSuffix := (system.TradeHub == "Thera" && routeContainsThera) ||
			(system.TradeHub == "Zarzakh" && routeContainsZarzakh)
		if !hasRedundantRouteSuffix {
			if routeContainsThera && routeContainsZarzakh {
			theraSuffix := " (Thera"
			// Add EOL indicator if Thera route is End-of-Life
			if strings.Contains(system.TheraInfo, "EOL") || (mockData && system.SystemID == 30002440) {
				theraSuffix += ", EOL"
			}
			if system.MaxShipSize != "" {
				theraSuffix += ", max " + template.HTMLEscapeString(system.MaxShipSize)
				logging.Debugf("HTML render: Adding MaxShipSize=%s for system %s (Thera+Zarzakh)", system.MaxShipSize, system.Name)
			} else {
				logging.Debugf("HTML render: MaxShipSize is empty for system %s (Thera+Zarzakh)", system.Name)
			}
			theraSuffix += ", Zarzakh)"
			html.WriteString(theraSuffix)
		} else if routeContainsThera {
			theraSuffix := " (Thera"
			// Add EOL indicator if Thera route is End-of-Life
			if strings.Contains(system.TheraInfo, "EOL") || (mockData && system.SystemID == 30002440) {
				theraSuffix += ", EOL"
			}
			if system.MaxShipSize != "" {
				theraSuffix += ", max " + template.HTMLEscapeString(system.MaxShipSize)
				logging.Debugf("HTML render: Adding MaxShipSize=%s for system %s (Thera only)", system.MaxShipSize, system.Name)
			} else {
				logging.Debugf("HTML render: MaxShipSize is empty for system %s (Thera only), system.ViaThera=%v, system.TheraInfo=%s", system.Name, system.ViaThera, system.TheraInfo)
			}
			theraSuffix += ")"
			html.WriteString(theraSuffix)
		} else if routeContainsZarzakh {
				html.WriteString(" (Zarzakh)")
			}
		}
		html.WriteString("</span>")

		// Route container (initially hidden)
		if len(system.Route) > 0 {
			html.WriteString("<div class='route-container hidden'>")
			routeHeaderText := "Route"
			html.WriteString("<div class='route-header'>")
			html.WriteString(template.HTMLEscapeString(routeHeaderText))
			html.WriteString("</div><div class='route-list'>")

			// For Thera routes, the signature IDs live in the systems adjacent to Thera:
			// - inbound: system right before Thera
			// - outbound: system right after Thera
			theraInboundSystemID := 0
			for i := 0; i < len(system.Route); i++ {
				if system.Route[i].SystemID == TheraSystemID {
					if i-1 >= 0 {
						theraInboundSystemID = system.Route[i-1].SystemID
					}
					break
				}
			}

			for i, routeSystem := range system.Route {
				// Skip highlighting if this is the destination system itself
				isDestinationSystem := routeSystem.SystemID == system.SystemID

				// Check if this route system has recent kills (but not if it's the destination)
				hasKills := false
				if !isDestinationSystem {
					if kills, exists := systemsWithKills[routeSystem.SystemID]; exists && len(kills) > 0 {
						hasKills = true
					}
				}

				// Check if this system is in the current result set (for link validation)
				systemInResult := systemsInResult[routeSystem.SystemID]

				html.WriteString("<span class='route-system")
				if routeSystem.SystemID == TheraSystemID {
					html.WriteString(" thera-route")
				}
				if routeSystem.SystemID == ZarzakhSystemID {
					html.WriteString(" zarzakh-route")
				}
				if hasKills {
					html.WriteString(" route-system-with-kills")
				}
				html.WriteString("'>")

				// Thera in route: link system name to EVE Scout
				isTheraInRoute := routeSystem.SystemID == TheraSystemID
				if isTheraInRoute {
					html.WriteString("<a href='https://www.eve-scout.com' target='_blank' rel='noopener noreferrer' class='route-system-link' title='EVE Scout – Thera connections'>")
					html.WriteString(template.HTMLEscapeString(routeSystem.SystemName))
					html.WriteString("</a>")
					if hasKills {
						html.WriteString(" ⚠")
					}
				} else if hasKills && systemInResult {
					// If system has kills and is in the result set, make it a clickable link to the table row
					html.WriteString("<a href='#system-")
					html.WriteString(strconv.Itoa(routeSystem.SystemID))
					html.WriteString("' class='route-system-link' title='This system has recent kills - click to jump to it in the table'>")
					html.WriteString(template.HTMLEscapeString(routeSystem.SystemName))
					html.WriteString(" ⚠")
					html.WriteString("</a>")
				} else if hasKills {
					// System has kills but is not in result set - just mark it visually
					html.WriteString("<span class='route-system-link' title='This system has recent kills (not shown in current results)'>")
					html.WriteString(template.HTMLEscapeString(routeSystem.SystemName))
					html.WriteString(" ⚠")
					html.WriteString("</span>")
				} else {
					html.WriteString(template.HTMLEscapeString(routeSystem.SystemName))
				}

				// Security status (before signature text)
				if routeSystem.SecurityStatus != 0 {
					routeDisplayValue := displayEveSecurityForUI(routeSystem.SecurityStatus)
					routeSecurityFormatted := fmt.Sprintf("%.1f", routeDisplayValue)
					if routeSecurityFormatted == "-0.0" {
						routeSecurityFormatted = "0.0"
					}
					html.WriteString(" (<span class='route-security-number' data-security='")
					html.WriteString(routeSecurityFormatted)
					html.WriteString("'>")
					html.WriteString(routeSecurityFormatted)
					html.WriteString("</span>)")
				}

				// Thera signature IDs:
				// - inbound scan ID belongs to the k-space system immediately BEFORE Thera hop
				// - outbound scan ID belongs to the Thera hop itself
				if routeSystem.SystemID == theraInboundSystemID && system.TheraInboundSignature != "" {
					html.WriteString(" <span class='thera-sig'>(in: ")
					html.WriteString(template.HTMLEscapeString(system.TheraInboundSignature))
					html.WriteString(")</span>")
				}
				if routeSystem.SystemID == TheraSystemID && system.TheraOutboundSignature != "" {
					html.WriteString(" <span class='thera-sig'>(out: ")
					html.WriteString(template.HTMLEscapeString(system.TheraOutboundSignature))
					html.WriteString(")</span>")
				}
				html.WriteString("</span>")
				if i < len(system.Route)-1 {
					html.WriteString("<span class='route-arrow'> → </span>")
				}
			}
			html.WriteString("</div></div>")
		}
		html.WriteString("</td>")

		// Notes/Killmail cell
		html.WriteString("<td data-label='Notes'><div class='killmail-container'>")
		if len(system.RecentKills) > 0 {
			displayCount := len(system.RecentKills)
			if displayCount > 3 {
				displayCount = 3
			}
			for i := 0; i < displayCount; i++ {
				html.WriteString("<div class='killmail-row' data-order='")
				html.WriteString(strconv.Itoa(i))
				html.WriteString("'")
				kill := system.RecentKills[i]
				html.WriteString(" data-attacker-count='")
				html.WriteString(strconv.Itoa(len(kill.Attackers)))
				html.WriteString("'>")
				renderKillmailHTML(&html, &kill, types, characterNames, characterNameErrors, pilotMultiSystem, systemsInResult)
				html.WriteString("</div>")
			}
			if len(system.RecentKills) > 3 {
				remainingKills := len(system.RecentKills) - 3
				html.WriteString("<details class='killmail-overflow'>")
				html.WriteString("<summary>")
				html.WriteString("<span class='killmail-overflow-show'>Show ")
				html.WriteString(strconv.Itoa(remainingKills))
				html.WriteString(" more kill")
				if remainingKills != 1 {
					html.WriteString("s")
				}
				html.WriteString("</span>")
				html.WriteString("<span class='killmail-overflow-hide'>Hide ")
				html.WriteString(strconv.Itoa(remainingKills))
				html.WriteString(" more kill")
				if remainingKills != 1 {
					html.WriteString("s")
				}
				html.WriteString("</span>")
				html.WriteString("</summary>")
				for i := 3; i < len(system.RecentKills); i++ {
					kill := system.RecentKills[i]
					html.WriteString("<div class='killmail-row' data-order='")
					html.WriteString(strconv.Itoa(i))
					html.WriteString("' data-attacker-count='")
					html.WriteString(strconv.Itoa(len(kill.Attackers)))
					html.WriteString("'>")
					renderKillmailHTML(&html, &kill, types, characterNames, characterNameErrors, pilotMultiSystem, systemsInResult)
					html.WriteString("</div>")
				}
				html.WriteString("</details>")
			}
		} else {
			for i := 0; i < 3; i++ {
				html.WriteString("<div class='killmail-row' data-order='")
				html.WriteString(strconv.Itoa(i))
				html.WriteString("'>No recent player kills</div>")
			}
		}
		html.WriteString("</div></td>")

		html.WriteString("</tr>")
	}

	html.WriteString("</tbody></table>")
	return html.String()
}

// renderTheraCampsHTML renders the "Possible camps in Thera" section
func renderTheraCampsHTMLWithNames(campKills []CachedKillmail, characterNames map[int]string, characterNameErrors map[int]string) string {
	if types == nil {
		return ""
	}
	if len(campKills) == 0 {
		return ""
	}

	var html strings.Builder
	html.WriteString("<div id='thera-camps-container' class='thera-camps-collapsible thera-camps-collapsed'>")
	html.WriteString("<h3 class='thera-camps-header'>")
	html.WriteString("<span class='toggle-icon'>▼</span>")
	html.WriteString("<span class='thera-camps-warning-icon'>⚠</span>")
	html.WriteString("<span>Possible camps in Thera</span>")
	html.WriteString("</h3>")
	html.WriteString("<div class='thera-camps-content'>")
	html.WriteString("<div class='killmail-container'>")
	for i, kill := range campKills {
		if i >= 10 {
			break // Limit to 10 most recent
		}
		html.WriteString("<div class='killmail-row' data-order='")
		html.WriteString(strconv.Itoa(i))
		html.WriteString("'>")
		// Thera camps are rendered independently; we don't compute multi-system highlights here.
		renderKillmailHTML(&html, &kill, types, characterNames, characterNameErrors, nil, nil)
		html.WriteString("</div>")
	}
	html.WriteString("</div></div></div>")
	return html.String()
}

// hasOnlyNPCs checks if all attackers in a kill are NPCs (non-player characters)
// NPCs are identified by having CharacterID == 0 or missing
func hasOnlyNPCs(kill *CachedKillmail) bool {
	if len(kill.Attackers) == 0 {
		return false // No attackers, can't determine
	}

	// Check if all attackers are NPCs
	for _, attacker := range kill.Attackers {
		if attacker.CharacterID != 0 {
			// Found at least one player attacker
			return false
		}
	}

	// All attackers are NPCs
	return true
}

// zkillAsearchPilotLossesInShipURL is zKill advanced search: losses where this pilot died in this ship type.
// Hash format matches zkillboard.com/asearch (JSON in fragment with quotes as %22).
func zkillAsearchPilotLossesInShipURL(characterID, shipTypeID int) string {
	type victimRow struct {
		Type string `json:"type"`
		ID   int    `json:"id"`
	}
	payload := struct {
		Buttons           []string    `json:"buttons"`
		Victims           []victimRow `json:"victims"`
		IncludeAssociates bool        `json:"includeAssociates"`
	}{
		Buttons: []string{
			"alltime", "attackers-and", "either-aand", "victims-and",
			"sort-date", "sort-desc", "page1", "victimsonly",
		},
		Victims: []victimRow{
			{Type: "shipID", ID: shipTypeID},
			{Type: "characterID", ID: characterID},
		},
		IncludeAssociates: false,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "https://zkillboard.com/character/" + strconv.Itoa(characterID) + "/losses/"
	}
	frag := strings.ReplaceAll(string(b), `"`, "%22")
	return "https://zkillboard.com/asearch/#" + frag
}

// renderKillmailHTML renders a single killmail as HTML
func renderKillmailHTML(
	html *strings.Builder,
	kill *CachedKillmail,
	types map[int]string,
	characterNames map[int]string,
	characterNameErrors map[int]string,
	pilotMultiSystem map[int]bool,
	systemsInResult map[int]bool,
) {
	// Grey monochrome pilot icons (SVG keeps them consistent across OS/font rendering).
	// Running human icon from static/running-human.svg (repeat pilots across multiple systems).
	pilotIconRunningHTML := getRunningHumanIconHTMLOrFallback()

	killTime, err := time.Parse("2006-01-02T15:04:05Z", kill.KillmailTime)
	if err != nil {
		html.WriteString("Error: Invalid killmail time")
		return
	}

	elapsed := time.Since(killTime)
	days := int(elapsed.Hours() / 24)
	hours := int(elapsed.Hours()) % 24
	minutes := int(elapsed.Minutes()) % 60

	timeText := ""
	if days > 0 {
		timeText += fmt.Sprintf("%dd ", days)
	}
	if hours > 0 {
		timeText += fmt.Sprintf("%dh ", hours)
	}
	if minutes > 0 {
		timeText += fmt.Sprintf("%dm", minutes)
	}
	if timeText == "" {
		timeText = "Just now"
	} else {
		timeText += " ago"
	}

	// Handle both ship kills and non-ship kills (structures, etc.)
	var victimType string
	var victimGroup string
	victimIsNPC := kill.Victim.CharacterID == 0
	if kill.Victim.ShipTypeID > 0 {
		victimType = types[kill.Victim.ShipTypeID]
		if victimType == "" {
			victimType = "Unknown type"
		}
		victimGroup = typeIDToGroupName[kill.Victim.ShipTypeID]
	} else {
		// Non-ship kill (structure, etc.) - still indicates enemy presence
		victimType = "Structure/Other"
	}

	html.WriteString("<span class='kill-recency-text'>")
	html.WriteString(template.HTMLEscapeString(timeText))
	html.WriteString("</span>")
	if victimGroup != "" {
		if iconHTML := shipTypeIconHTMLFromGroup(victimGroup, victimIsNPC); iconHTML != "" {
			// Victim ship icon should always point to EVE Uni ship page.
			writeShipTypeIconWithWikiHTML(html, iconHTML, victimType, victimIsNPC)
		}
	}
	html.WriteString("<a target='_blank' rel='noopener noreferrer' href='https://zkillboard.com/kill/")
	html.WriteString(strconv.Itoa(kill.KillmailID))
	html.WriteString("/")
	html.WriteString("'>")
	html.WriteString("<span class='ship-type-text'>")
	html.WriteString(template.HTMLEscapeString(victimType))
	html.WriteString("</span>")
	html.WriteString("</a>")

	// Display stargate info if available (inline with ship name)
	if kill.StargateInfo != nil && kill.MinDistanceToStargate != nil {
		html.WriteString(" (")
		// Get destination system name (resolve from systems list if missing, e.g. mock data)
		gateSystemName := kill.StargateInfo.DestinationSystemName
		if gateSystemName == "" {
			if destSys := getSystemById(kill.StargateInfo.DestinationSystemID); destSys.SystemID != 0 {
				gateSystemName = destSys.SystemName
			}
		}
		if gateSystemName != "" {
			if systemsInResult != nil && systemsInResult[kill.StargateInfo.DestinationSystemID] {
				html.WriteString("<a href='#system-")
				html.WriteString(strconv.Itoa(kill.StargateInfo.DestinationSystemID))
				html.WriteString("' class='stargate-system-link'>")
				html.WriteString("<span class=\"stargate-system-name\">")
				html.WriteString(template.HTMLEscapeString(gateSystemName))
				html.WriteString("</span>")
				html.WriteString("</a>")
			} else {
				html.WriteString("<span class=\"stargate-system-name\">")
				html.WriteString(template.HTMLEscapeString(gateSystemName))
				html.WriteString("</span>")
			}
		} else {
			html.WriteString("<span class=\"stargate-system-name\">")
			html.WriteString("System ")
			html.WriteString(strconv.Itoa(kill.StargateInfo.DestinationSystemID))
			html.WriteString("</span>")
		}
		html.WriteString(" gate at ")
		// Convert distance from meters to km and format
		distanceKm := *kill.MinDistanceToStargate / 1000.0
		if distanceKm < 1.0 {
			html.WriteString(fmt.Sprintf("%.0f m", *kill.MinDistanceToStargate))
		} else {
			html.WriteString(fmt.Sprintf("%.1f km", distanceKm))
		}
		html.WriteString(")")
	}

	attackerCount := len(kill.Attackers)
	html.WriteString("<span class='attackers-section'>Attackers: ")

	// Collapse attackers list if there are more than 5
	if attackerCount > 5 {
		html.WriteString(strconv.Itoa(attackerCount))
		html.WriteString(" ")
		// Generate unique ID for this attackers list
		uniqueID := fmt.Sprintf("attackers-%d-%d", kill.KillmailID, killTime.Unix())
		html.WriteString("<span class='attackers-toggle' data-target='")
		html.WriteString(uniqueID)
		html.WriteString("' data-count='")
		html.WriteString(strconv.Itoa(attackerCount))
		html.WriteString("'>")
		html.WriteString("(show)</span>")
		html.WriteString("<div class='attackers-list' id='")
		html.WriteString(uniqueID)
		html.WriteString("'>")

		for i, attacker := range kill.Attackers {
			if i > 0 {
				html.WriteString("<br/>")
			}
			attackerShip := types[attacker.ShipTypeID]
			if attackerShip == "" {
				attackerShip = "Unknown ship"
			}
			attackerGroup := typeIDToGroupName[attacker.ShipTypeID]
			weapon := types[attacker.WeaponTypeID]
			if weapon == "" {
				weapon = "Unknown weapon"
			}
			attackerIsNPC := attacker.CharacterID == 0
			// Attackers list: show icon instead of item dots (when we can map it).
			var iconHTML string
			if s := shipTypeIconHTMLFromGroup(attackerGroup, attackerIsNPC); s != "" {
				iconHTML = s
			}

			if characterNames != nil && attacker.CharacterID != 0 {
				name := characterNames[attacker.CharacterID]
				pilotID := attacker.CharacterID
				meta := pilotLinkMetaFor(pilotID, name, characterNameErrors)
				var zkbLossesURL string
				if attacker.ShipTypeID != 0 {
					zkbLossesURL = zkillAsearchPilotLossesInShipURL(pilotID, attacker.ShipTypeID)
				} else {
					zkbLossesURL = "https://zkillboard.com/character/" + strconv.Itoa(pilotID) + "/losses/"
				}

				// Ship type icon stays wiki-linked (with ship tooltip).
				writeShipTypeIconWithWikiHTML(html, iconHTML, attackerShip, attackerIsNPC)

				if pilotMultiSystem != nil && pilotMultiSystem[pilotID] {
					html.WriteString("<span class='pilot-multi-system'>")
					// Repeat offenders across multiple systems: ship type text links to pilot losses,
					// with a "running" human icon and filter button next to it.
					html.WriteString("<a target='_blank' rel='noopener noreferrer' class='pilot-link' href='")
					html.WriteString(zkbLossesURL)
					writePilotLinkAttrs(html, meta, pilotID)
					html.WriteString("'>")
					writeShipTypeTextHTML(html, attackerShip)
					html.WriteString("</a>")

					// Running-man icon is also the pilot-only filter toggle.
					html.WriteString("<span class='pilot-icon-wrap pilot-hide-systems-btn' data-character-id='")
					html.WriteString(strconv.Itoa(pilotID))
					html.WriteString("' aria-pressed='false' title='Show only systems where this pilot appears' aria-label='Show only systems where this pilot appears as an attacker' role='button' tabindex='0'>")
					html.WriteString(pilotIconRunningHTML)
					html.WriteString("</span>")
					html.WriteString("</span>")
				} else {
					// Normal case: ship type text links to pilot losses.
					html.WriteString("<a target='_blank' rel='noopener noreferrer' class='pilot-link' href='")
					html.WriteString(zkbLossesURL)
					writePilotLinkAttrs(html, meta, pilotID)
					html.WriteString("'>")
					writeShipTypeTextHTML(html, attackerShip)
					html.WriteString("</a>")
				}
			} else {
				writeAttackerShipTypeHTML(html, iconHTML, attackerShip, attackerIsNPC)
			}
			html.WriteString(" with ")
			html.WriteString(template.HTMLEscapeString(weapon))
		}

		html.WriteString("</div>")
	} else {
		// Show all attackers normally
		html.WriteString(strconv.Itoa(attackerCount))
		html.WriteString("<br/>")

		for i, attacker := range kill.Attackers {
			if i > 0 {
				html.WriteString("<br/>")
			}
			attackerShip := types[attacker.ShipTypeID]
			if attackerShip == "" {
				attackerShip = "Unknown ship"
			}
			attackerGroup := typeIDToGroupName[attacker.ShipTypeID]
			weapon := types[attacker.WeaponTypeID]
			if weapon == "" {
				weapon = "Unknown weapon"
			}
			attackerIsNPC := attacker.CharacterID == 0
			// Attackers list: show icon instead of item dots (when we can map it).
			var iconHTML string
			if s := shipTypeIconHTMLFromGroup(attackerGroup, attackerIsNPC); s != "" {
				iconHTML = s
			}

			if characterNames != nil && attacker.CharacterID != 0 {
				name := characterNames[attacker.CharacterID]
				pilotID := attacker.CharacterID
				meta := pilotLinkMetaFor(pilotID, name, characterNameErrors)
				var zkbLossesURL string
				if attacker.ShipTypeID != 0 {
					zkbLossesURL = zkillAsearchPilotLossesInShipURL(pilotID, attacker.ShipTypeID)
				} else {
					zkbLossesURL = "https://zkillboard.com/character/" + strconv.Itoa(pilotID) + "/losses/"
				}

				// Ship type icon stays wiki-linked (with ship tooltip).
				writeShipTypeIconWithWikiHTML(html, iconHTML, attackerShip, attackerIsNPC)

				if pilotMultiSystem != nil && pilotMultiSystem[pilotID] {
					html.WriteString("<span class='pilot-multi-system'>")
					// Repeat offenders across multiple systems: ship type text links to pilot losses,
					// with a "running" human icon and filter button next to it.
					html.WriteString("<a target='_blank' rel='noopener noreferrer' class='pilot-link' href='")
					html.WriteString(zkbLossesURL)
					writePilotLinkAttrs(html, meta, pilotID)
					html.WriteString("'>")
					writeShipTypeTextHTML(html, attackerShip)
					html.WriteString("</a>")
					// Running-man icon is also the pilot-only filter toggle.
					html.WriteString("<span class='pilot-icon-wrap pilot-hide-systems-btn' data-character-id='")
					html.WriteString(strconv.Itoa(pilotID))
					html.WriteString("' aria-pressed='false' title='Show only systems where this pilot appears' aria-label='Show only systems where this pilot appears as an attacker' role='button' tabindex='0'>")
					html.WriteString(pilotIconRunningHTML)
					html.WriteString("</span>")
					html.WriteString("</span>")
				} else {
					// Normal case: ship type text links to pilot losses.
					html.WriteString("<a target='_blank' rel='noopener noreferrer' class='pilot-link' href='")
					html.WriteString(zkbLossesURL)
					writePilotLinkAttrs(html, meta, pilotID)
					html.WriteString("'>")
					writeShipTypeTextHTML(html, attackerShip)
					html.WriteString("</a>")
				}
			} else {
				writeAttackerShipTypeHTML(html, iconHTML, attackerShip, attackerIsNPC)
			}
			html.WriteString(" with ")
			html.WriteString(template.HTMLEscapeString(weapon))
		}
	}
	html.WriteString("</span>")
}

// getSystemsWithPrecalculatedKills gets all systems that have precalculated valid kills
// Returns a map of systemID -> []CachedKillmail
// This function only retrieves precalculated data without any processing
func getSystemsWithPrecalculatedKills() map[int][]CachedKillmail {
	logging.Debugf("getSystemsWithPrecalculatedKills: Starting")
	result := make(map[int][]CachedKillmail)

	// Get precalculated data
	logging.Debugf("getSystemsWithPrecalculatedKills: About to acquire read access")
	readFn := precalculatedData.Read()
	data := readFn()
	logging.Debugf("getSystemsWithPrecalculatedKills: Read access acquired successfully")
	if data.systemsWithKills == nil {
		logging.Debugf("getSystemsWithPrecalculatedKills: WARNING - data.systemsWithKills is nil!")
		return result
	}
	logging.Debugf("getSystemsWithPrecalculatedKills: data.systemsWithKills has %d systems", len(data.systemsWithKills))

	// Filter by time - only include killmails from the last hour
	now := time.Now()
	oneHourAgo := now.Add(-1 * time.Hour)
	logging.Debugf("getSystemsWithPrecalculatedKills: Time filter - now: %v, oneHourAgo: %v", now, oneHourAgo)

	// Copy precalculated killmails, filtering by time
	// Exclude W-space (J-space) systems from the main table - they are not reachable via stargates.
	// Pochven is not in this range and remains included. Thera camp kills are still shown via getTheraCampKills().
	logging.Debugf("getSystemsWithPrecalculatedKills: Found %d systems with precalculated kills before filtering", len(data.systemsWithKills))
	for systemID, kills := range data.systemsWithKills {
		if kills == nil {
			continue
		}
		if systemID >= 31000000 && systemID <= 31999999 {
			// J-space / W-space: skip for main table (not stargate-reachable)
			continue
		}

		system := getSystemById(systemID)
		systemName := "unknown"
		if system.SystemID != 0 {
			systemName = system.SystemName
		}
		logging.Debugf("getSystemsWithPrecalculatedKills: System %s (%d) has %d precalculated kills before filtering", systemName, systemID, len(kills))

		// Filter kills that are within the last hour based on their actual killmail time
		// Also filter out NPC-only kills
		filteredKills := make([]CachedKillmail, 0, len(kills))
		npcFiltered := 0
		timeFiltered := 0
		parseError := 0
		for _, kill := range kills {
			// Skip NPC-only kills
			if hasOnlyNPCs(&kill) {
				npcFiltered++
				logging.Debugf("getSystemsWithPrecalculatedKills: Killmail %d in system %s (%d) filtered out (NPC-only)", kill.KillmailID, systemName, systemID)
				continue
			}

			// Check if this killmail occurred within the last hour
			killTime, err := time.Parse("2006-01-02T15:04:05Z", kill.KillmailTime)
			if err != nil {
				parseError++
				logging.Debugf("getSystemsWithPrecalculatedKills: Killmail %d in system %s (%d) filtered out (parse error: %v)", kill.KillmailID, systemName, systemID, err)
				continue
			}
			if killTime.After(oneHourAgo) {
				filteredKills = append(filteredKills, kill)
			} else {
				timeFiltered++
				logging.Debugf("getSystemsWithPrecalculatedKills: Killmail %d in system %s (%d) filtered out (too old: killTime: %v, oneHourAgo: %v, age: %v)", kill.KillmailID, systemName, systemID, killTime, oneHourAgo, time.Since(killTime))
			}
		}

		if len(filteredKills) > 0 {
			result[systemID] = filteredKills
			logging.Debugf("getSystemsWithPrecalculatedKills: System %s (%d) has %d valid kills after filtering (NPC filtered: %d, time filtered: %d, parse errors: %d)", systemName, systemID, len(filteredKills), npcFiltered, timeFiltered, parseError)
		} else {
			logging.Debugf("getSystemsWithPrecalculatedKills: System %s (%d) has no valid kills after filtering (NPC filtered: %d, time filtered: %d, parse errors: %d)", systemName, systemID, npcFiltered, timeFiltered, parseError)
		}
	}

	logging.Debugf("getSystemsWithPrecalculatedKills: Retrieved %d systems with precalculated kills", len(result))

	// Sort kills by time (newest first) for each system
	for systemID := range result {
		sort.Slice(result[systemID], func(i, j int) bool {
			timeI, _ := time.Parse("2006-01-02T15:04:05Z", result[systemID][i].KillmailTime)
			timeJ, _ := time.Parse("2006-01-02T15:04:05Z", result[systemID][j].KillmailTime)
			return timeI.After(timeJ)
		})
	}

	logging.Debugf("getSystemsWithPrecalculatedKills: Completed, returning %d systems", len(result))
	return result
}

// gzipResponseWriter wraps http.ResponseWriter to add gzip compression
type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

// gzipHandler wraps an http.HandlerFunc to add gzip compression for HTML responses
func gzipHandler(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check if client accepts gzip encoding
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			fn(w, r)
			return
		}

		// Create gzip writer
		gz := gzip.NewWriter(w)
		defer gz.Close()

		// Set headers
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")

		// Wrap response writer
		gzw := &gzipResponseWriter{Writer: gz, ResponseWriter: w}
		fn(gzw, r)
	}
}

// generateRandomState generates a random state string for CSRF protection
func generateRandomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// generateSessionID generates a random session ID
func generateSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// cleanupExpiredStates removes expired state values
func cleanupExpiredStates() {
	ssoStatesMu.Lock()
	defer ssoStatesMu.Unlock()
	now := time.Now()
	for state, expires := range ssoStates {
		if now.After(expires) {
			delete(ssoStates, state)
		}
	}
}

// cleanupExpiredSessions removes expired sessions
func cleanupExpiredSessions() {
	ssoSessionsMu.Lock()
	defer ssoSessionsMu.Unlock()
	now := time.Now()
	for sessionID, session := range ssoSessions {
		if now.After(session.ExpiresAt) {
			delete(ssoSessions, sessionID)
		}
	}
}

// refreshAccessToken refreshes an access token using the refresh token
func refreshAccessToken(session *SSOSession) error {
	if session.RefreshToken == "" {
		return fmt.Errorf("no refresh token available")
	}

	tokenData := url.Values{}
	tokenData.Set("grant_type", "refresh_token")
	tokenData.Set("refresh_token", session.RefreshToken)

	req, err := http.NewRequest("POST", ssoTokenURL, strings.NewReader(tokenData.Encode()))
	if err != nil {
		return fmt.Errorf("failed to create token request: %v", err)
	}

	// Set basic auth header
	auth := base64.StdEncoding.EncodeToString([]byte(ssoClientID + ":" + ssoClientSecret))
	req.Header.Set("Authorization", "Basic "+auth)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req) // #nosec G704 -- req uses constant ssoTokenURL
	if err != nil {
		return fmt.Errorf("failed to refresh token: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		log.Printf("Token refresh failed: %d - %s", resp.StatusCode, string(bodyBytes)) // #nosec G706 -- SSO response body in log
		return fmt.Errorf("token refresh failed: %d", resp.StatusCode)
	}

	var tokenResponse struct {
		AccessToken  string `json:"access_token"`  // #nosec G117 -- OAuth response field
		RefreshToken string `json:"refresh_token"` // #nosec G117 -- OAuth response field; may be same or new
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tokenResponse); err != nil {
		return fmt.Errorf("failed to decode token response: %v", err)
	}

	// Update session with new tokens
	expiresAt := time.Now().Add(time.Duration(tokenResponse.ExpiresIn) * time.Second)
	session.AccessToken = tokenResponse.AccessToken
	if tokenResponse.RefreshToken != "" {
		session.RefreshToken = tokenResponse.RefreshToken
	}
	session.ExpiresAt = expiresAt

	log.Printf("Successfully refreshed token for character %d, expires at %v", session.CharacterID, expiresAt)
	return nil
}

// getSession retrieves a session by session ID from cookie
// Automatically refreshes the token if it's close to expiring (within 5 minutes)
func getSession(r *http.Request) *SSOSession {
	cookie, err := r.Cookie(ssoSessionName)
	if err != nil {
		return nil
	}
	sessionID := cookie.Value

	ssoSessionsMu.Lock()
	defer ssoSessionsMu.Unlock()
	session, exists := ssoSessions[sessionID]
	if !exists {
		return nil
	}

	// Check if token is expired
	now := time.Now()
	if now.After(session.ExpiresAt) {
		// Token expired, try to refresh
		if err := refreshAccessToken(session); err != nil {
			log.Printf("Failed to refresh expired token: %v", err)
			// Delete session if refresh fails
			delete(ssoSessions, sessionID)
			return nil
		}
		return session
	}

	// Refresh token if it's close to expiring (within 5 minutes)
	timeUntilExpiry := session.ExpiresAt.Sub(now)
	if timeUntilExpiry < 5*time.Minute {
		// Refresh proactively
		if err := refreshAccessToken(session); err != nil {
			log.Printf("Failed to refresh token proactively: %v", err)
			// Don't delete session on proactive refresh failure, only on actual expiry
		}
	}

	return session
}

// setSessionCookie sets a session cookie (HttpOnly; Secure when HTTPS or EVE_SSO_COOKIE_SECURE).
func setSessionCookie(w http.ResponseWriter, r *http.Request, sessionID string) {
	secure := cookieSecure(r)
	http.SetCookie(w, &http.Cookie{
		Name:     ssoSessionName,
		Value:    sessionID,
		Path:     "/",
		MaxAge:   int(ssoSessionTimeout.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		// Don't set Domain - let browser use default (allows localhost:8080 and localhost:8888 to share)
	})
}

// clearSessionCookie clears the session cookie (must match flags used when setting).
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	secure := cookieSecure(r)
	http.SetCookie(w, &http.Cookie{
		Name:     ssoSessionName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
	})
}

// ssoLoginHandler initiates the SSO login flow
func ssoLoginHandler(w http.ResponseWriter, r *http.Request) {
	if ssoClientID == "" || ssoClientSecret == "" {
		http.Error(w, "SSO not configured", http.StatusInternalServerError)
		return
	}

	// Generate state for CSRF protection
	state, err := generateRandomState()
	if err != nil {
		http.Error(w, "Failed to generate state", http.StatusInternalServerError)
		return
	}

	// Store state with expiration
	ssoStatesMu.Lock()
	ssoStates[state] = time.Now().Add(ssoStateTimeout)
	ssoStatesMu.Unlock()

	// Clean up expired states periodically
	if len(ssoStates) > 1000 {
		go cleanupExpiredStates()
	}

	// Build authorization URL
	authURL, err := url.Parse(ssoAuthURL)
	if err != nil {
		http.Error(w, "Invalid auth URL", http.StatusInternalServerError)
		return
	}

	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", ssoClientID)
	params.Set("redirect_uri", ssoRedirectURI)
	// Note: esi-ui.write_waypoint.v1 does NOT set autopilot waypoint (just adds a waypoint) and requires character to be online.
	// See https://github.com/esi/esi-issues/issues/1472
	params.Set("scope", "esi-location.read_location.v1 esi-location.read_online.v1 esi-ui.write_waypoint.v1")
	params.Set("state", state)

	authURL.RawQuery = params.Encode()

	// Redirect to EVE SSO
	http.Redirect(w, r, authURL.String(), http.StatusFound)
}

// ssoCallbackHandler handles the callback from EVE SSO
func ssoCallbackHandler(w http.ResponseWriter, r *http.Request) {
	// Get authorization code and state
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	errorParam := r.URL.Query().Get("error")

	if errorParam != "" {
		http.Error(w, fmt.Sprintf("SSO error: %s", errorParam), http.StatusBadRequest)
		return
	}

	if code == "" || state == "" {
		http.Error(w, "Missing code or state", http.StatusBadRequest)
		return
	}

	// Verify state
	ssoStatesMu.Lock()
	expires, exists := ssoStates[state]
	if !exists || time.Now().After(expires) {
		ssoStatesMu.Unlock()
		http.Error(w, "Invalid or expired state", http.StatusBadRequest)
		return
	}
	delete(ssoStates, state)
	ssoStatesMu.Unlock()

	// Exchange authorization code for tokens
	tokenData := url.Values{}
	tokenData.Set("grant_type", "authorization_code")
	tokenData.Set("code", code)

	req, err := http.NewRequest("POST", ssoTokenURL, strings.NewReader(tokenData.Encode()))
	if err != nil {
		http.Error(w, "Failed to create token request", http.StatusInternalServerError)
		return
	}

	// Set basic auth header
	auth := base64.StdEncoding.EncodeToString([]byte(ssoClientID + ":" + ssoClientSecret))
	req.Header.Set("Authorization", "Basic "+auth)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req) // #nosec G704 -- req uses constant ssoTokenURL
	if err != nil {
		http.Error(w, "Failed to exchange code for token", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		log.Printf("Token exchange failed: %s", string(bodyBytes))
		http.Error(w, "Failed to exchange code for token", http.StatusInternalServerError)
		return
	}

	var tokenResponse struct {
		AccessToken  string `json:"access_token"`  // #nosec G117 -- OAuth response field
		RefreshToken string `json:"refresh_token"` // #nosec G117 -- OAuth response field
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tokenResponse); err != nil {
		http.Error(w, "Failed to decode token response", http.StatusInternalServerError)
		return
	}

	// Parse JWT token to get character info
	// JWT format: header.payload.signature
	parts := strings.Split(tokenResponse.AccessToken, ".")
	if len(parts) != 3 {
		http.Error(w, "Invalid token format", http.StatusInternalServerError)
		return
	}

	// Decode payload (base64url)
	// Try RawURLEncoding first (no padding), then fall back to URLEncoding if needed
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Try with padding
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			log.Printf("Failed to decode token payload with both RawURLEncoding and URLEncoding: %v", err)
			log.Printf("Token part[1] (payload): %s", parts[1])
			http.Error(w, "Failed to decode token payload", http.StatusInternalServerError)
			return
		}
	}

	// Log payload for debugging (first 200 chars to avoid logging sensitive data)
	payloadStr := string(payload)
	if len(payloadStr) > 200 {
		log.Printf("Token payload (first 200 chars): %s...", payloadStr[:200])
	} else {
		log.Printf("Token payload: %s", payloadStr)
	}

	var claims map[string]interface{}

	if err := json.Unmarshal(payload, &claims); err != nil {
		log.Printf("Failed to parse token claims: %v", err)
		log.Printf("Payload that failed to parse: %s", payloadStr)
		http.Error(w, fmt.Sprintf("Failed to parse token claims: %v", err), http.StatusInternalServerError)
		return
	}

	// Extract required fields
	sub, ok := claims["sub"].(string)
	if !ok {
		log.Printf("Missing or invalid 'sub' claim in token")
		http.Error(w, "Invalid token: missing 'sub' claim", http.StatusInternalServerError)
		return
	}

	// Character name is optional, use empty string if not present
	characterName := ""
	if name, ok := claims["name"].(string); ok {
		characterName = name
	}

	// Extract character ID from sub (format: CHARACTER:EVE:characterID)
	subParts := strings.Split(sub, ":")
	if len(subParts) != 3 {
		log.Printf("Invalid sub claim format: %s", sub)
		http.Error(w, "Invalid token sub claim", http.StatusInternalServerError)
		return
	}
	characterID, err := strconv.Atoi(subParts[2])
	if err != nil {
		log.Printf("Failed to parse character ID from sub: %s, error: %v", sub, err)
		http.Error(w, "Invalid character ID in token", http.StatusInternalServerError)
		return
	}

	// Create session
	sessionID, err := generateSessionID()
	if err != nil {
		http.Error(w, "Failed to generate session ID", http.StatusInternalServerError)
		return
	}

	expiresAt := time.Now().Add(time.Duration(tokenResponse.ExpiresIn) * time.Second)
	session := &SSOSession{
		CharacterID:   characterID,
		CharacterName: characterName,
		AccessToken:   tokenResponse.AccessToken,
		RefreshToken:  tokenResponse.RefreshToken,
		ExpiresAt:     expiresAt,
	}

	ssoSessionsMu.Lock()
	ssoSessions[sessionID] = session
	ssoSessionsMu.Unlock()

	// Clean up expired sessions periodically
	if len(ssoSessions) > 1000 {
		go cleanupExpiredSessions()
	}

	// Set session cookie
	setSessionCookie(w, r, sessionID)

	// Start background token refresh for this session
	go startTokenRefreshWorker(sessionID, session)

	// Redirect to frontend with success indicator
	// The frontend will check auth status on load
	redirectURL := ssoFrontendURL + "?auth=success"
	http.Redirect(w, r, redirectURL, http.StatusFound)
}

// startTokenRefreshWorker starts a background goroutine to refresh tokens proactively
func startTokenRefreshWorker(sessionID string, session *SSOSession) {
	go func() {
		// Refresh token when it's 80% through its lifetime (e.g., at 16 minutes for a 20-minute token)
		// This gives us a 4-minute buffer before expiration
		now := time.Now()
		tokenLifetime := session.ExpiresAt.Sub(now)
		if tokenLifetime <= 0 {
			return // Already expired
		}

		// Calculate when to refresh (80% of lifetime, but at least 1 minute before expiry)
		refreshDelay := tokenLifetime * 80 / 100
		if refreshDelay < 1*time.Minute {
			refreshDelay = tokenLifetime - 1*time.Minute
		}
		if refreshDelay <= 0 {
			refreshDelay = 1 * time.Minute // Minimum delay
		}

		// Wait until refresh time
		time.Sleep(refreshDelay)

		// Check if session still exists and refresh
		ssoSessionsMu.Lock()
		session, exists := ssoSessions[sessionID]
		if !exists {
			ssoSessionsMu.Unlock()
			return
		}

		// Double-check we still need to refresh (might have been refreshed already by another request)
		timeUntilExpiry := time.Until(session.ExpiresAt)
		if timeUntilExpiry > 5*time.Minute {
			// Token was already refreshed, reschedule
			ssoSessionsMu.Unlock()
			startTokenRefreshWorker(sessionID, session)
			return
		}

		err := refreshAccessToken(session)
		if err != nil {
			log.Printf("Background token refresh failed for session %s: %v", sessionID, err)
			// Don't delete session here - let getSession handle it on next access
		} else {
			// Schedule next refresh
			startTokenRefreshWorker(sessionID, session)
		}
		ssoSessionsMu.Unlock()
	}()
}

// ssoLogoutHandler handles logout
func ssoLogoutHandler(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	if session != nil {
		cookie, err := r.Cookie(ssoSessionName)
		if err == nil {
			sessionID := cookie.Value
			ssoSessionsMu.Lock()
			delete(ssoSessions, sessionID)
			ssoSessionsMu.Unlock()
		}
	}

	clearSessionCookie(w, r)
	http.Redirect(w, r, ssoFrontendURL, http.StatusFound)
}

// ssoLocationHandler fetches the character's current location from ESI
func ssoLocationHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	session := getSession(r)
	if session == nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}

	// getSession already handles token refresh automatically

	// Fetch location from ESI
	esiURL := fmt.Sprintf("https://esi.evetech.net/latest/characters/%d/location/?datasource=tranquility", session.CharacterID)
	req, err := http.NewRequest("GET", esiURL, nil) // #nosec G704 -- host fixed to esi.evetech.net
	if err != nil {
		http.Error(w, "Failed to create ESI request", http.StatusInternalServerError)
		return
	}

	req.Header.Set("Authorization", "Bearer "+session.AccessToken)
	req.Header.Set("User-Agent", httpUserAgent)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req) // #nosec G704 -- req uses esiURL (host esi.evetech.net)
	if err != nil {
		http.Error(w, "Failed to fetch location from ESI", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	log.Printf("ESI request: GET /characters/%d/location/ -> HTTP %d", session.CharacterID, resp.StatusCode)

	if resp.StatusCode == http.StatusUnauthorized {
		http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
		return
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		log.Printf("ESI location request failed: %d - %s", resp.StatusCode, string(bodyBytes)) // #nosec G706 -- ESI response body in log
		http.Error(w, fmt.Sprintf("ESI request failed: %d", resp.StatusCode), http.StatusInternalServerError)
		return
	}

	var locationData struct {
		SolarSystemID int `json:"solar_system_id"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&locationData); err != nil {
		http.Error(w, "Failed to decode location response", http.StatusInternalServerError)
		return
	}

	// Get system name
	system := getSystemById(locationData.SolarSystemID)
	systemName := ""
	if system.SystemID != 0 {
		systemName = system.SystemName
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"systemID":   locationData.SolarSystemID,
		"systemName": systemName,
	})
}

// proximityHandler returns proximity mode HTML table for the character's current location.
// Requires authentication; fetches character location from ESI (no system_id in URL).
func proximityHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	session := getSession(r)
	if session == nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}

	// Fetch character location from ESI
	esiURL := fmt.Sprintf("https://esi.evetech.net/latest/characters/%d/location/?datasource=tranquility", session.CharacterID)
	req, err := http.NewRequest("GET", esiURL, nil) // #nosec G704 -- host fixed to esi.evetech.net
	if err != nil {
		http.Error(w, "Failed to create ESI request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+session.AccessToken)
	req.Header.Set("User-Agent", httpUserAgent)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req) // #nosec G704 -- req uses esiURL
	if err != nil {
		http.Error(w, "Failed to fetch location from ESI", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	log.Printf("ESI request: GET /characters/%d/location/ (proximity) -> HTTP %d", session.CharacterID, resp.StatusCode)

	if resp.StatusCode == http.StatusUnauthorized {
		http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
		return
	}
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		log.Printf("ESI location request failed for proximity: %d - %s", resp.StatusCode, string(bodyBytes)) // #nosec G706
		http.Error(w, fmt.Sprintf("ESI request failed: %d", resp.StatusCode), http.StatusInternalServerError)
		return
	}

	var locationData struct {
		SolarSystemID int `json:"solar_system_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&locationData); err != nil {
		http.Error(w, "Failed to decode location response", http.StatusInternalServerError)
		return
	}

	startSystem := getSystemById(locationData.SolarSystemID)
	if startSystem.SystemID == 0 {
		http.Error(w, "Unknown system", http.StatusNotFound)
		return
	}

	systemID := startSystem.SystemID
	systemName := startSystem.SystemName

	// Get systems with precalculated kills
	logging.Debugf("Getting precalculated kills for startSystemID: %d (character location)", systemID)
	systemsWithKills := getSystemsWithPrecalculatedKills()
	logging.Debugf("Found %d systems with precalculated kills", len(systemsWithKills))

	targetSystemIDs := make([]int, 0, len(systemsWithKills))
	for targetSystemID := range systemsWithKills {
		targetSystemIDs = append(targetSystemIDs, targetSystemID)
	}
	batchRoutes := getProximityRoutesBatch(systemID, targetSystemIDs, maxRangeJumps)

	var result []SystemInRange
	unreachableCount := 0
	notFoundCount := 0
	for targetSystemID, kills := range systemsWithKills {
		system := getSystemById(targetSystemID)
		if system.SystemID == 0 {
			notFoundCount++
			if mockData {
				logging.Debugf("Dev mode: System %d not found in systems list, skipping", targetSystemID)
			}
			continue
		}

		routeResult, ok := batchRoutes[targetSystemID]
		if !ok {
			routeResult = proximityRouteResult{dist: -1}
		}
		viaThera, dist, theraInfo, theraInboundSig, theraOutboundSig, maxShipSize, route :=
			routeResult.viaThera, routeResult.dist, routeResult.theraInfo, routeResult.theraInboundSig, routeResult.theraOutboundSig, routeResult.maxShipSize, routeResult.route
		if dist < 0 {
			unreachableCount++
			if mockData {
				logging.Debugf("Dev mode: System %s (%d) is unreachable from %s (%d), skipping",
					system.SystemName, targetSystemID, systemName, systemID)
			}
			continue
		}

		systemInRange := SystemInRange{
			SystemID:    targetSystemID,
			Name:        system.SystemName,
			Dist:        dist,
			Security:    system.Security,
			RecentKills: kills,
			Route:       route,
		}
		if viaThera {
			systemInRange.ViaThera = true
			systemInRange.TheraDist = dist
			systemInRange.TheraInfo = fmt.Sprintf("Route via %s (%d jumps)", theraInfo, dist)
			systemInRange.TheraInboundSignature = theraInboundSig
			systemInRange.TheraOutboundSignature = theraOutboundSig
			systemInRange.MaxShipSize = maxShipSize
			systemInRange.Route = route
		}

		weight := float64(systemInRange.Dist)
		var latestKillTime time.Time
		for _, kill := range systemInRange.RecentKills {
			killTime, err := time.Parse("2006-01-02T15:04:05Z", kill.KillmailTime)
			if err != nil {
				continue
			}
			if latestKillTime.IsZero() || killTime.After(latestKillTime) {
				latestKillTime = killTime
			}
		}
		if !latestKillTime.IsZero() {
			weight += time.Since(latestKillTime).Minutes()
		}
		systemInRange.Weight = weight

		if systemInRange.Dist > maxRangeJumps {
			continue
		}
		result = append(result, systemInRange)
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Weight < result[j].Weight
	})

	if mockData {
		logging.Debugf("Dev mode: Returning %d systems (unreachable: %d, not found: %d)",
			len(result), unreachableCount, notFoundCount)
	}

	// Resolve attacker names for the same rows rendered in proximity mode so pilot icons/links are shown.
	var characterIDs []int
	for _, system := range result {
		for i := 0; i < 3 && i < len(system.RecentKills); i++ {
			for _, a := range system.RecentKills[i].Attackers {
				if a.CharacterID != 0 {
					characterIDs = append(characterIDs, a.CharacterID)
				}
			}
		}
	}
	characterNames, characterNameErrors := resolveCharacterNames(characterIDs)

	// Prepend meta div with start system for frontend to display location; escape system name for HTML safety
	meta := fmt.Sprintf(`<div id="proximity-meta" data-system-id="%d" data-system-name="%s" style="display:none"></div>`,
		systemID, html.EscapeString(systemName))
	html := meta + renderHTMLTableWithNames(result, "proximity", characterNames, characterNameErrors)
	_, _ = w.Write([]byte(html))
}

// checkCharacterOnline verifies if the authenticated character is currently online in EVE.
func checkCharacterOnline(session *SSOSession) (bool, error) {
	esiURL := fmt.Sprintf("https://esi.evetech.net/latest/characters/%d/online/?datasource=tranquility", session.CharacterID)
	req, err := http.NewRequest("GET", esiURL, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create online status request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+session.AccessToken)
	req.Header.Set("User-Agent", httpUserAgent)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req) // #nosec G704 -- esiURL host fixed to esi.evetech.net
	if err != nil {
		return false, fmt.Errorf("failed to fetch online status from ESI: %w", err)
	}
	defer resp.Body.Close()

	log.Printf("ESI request: GET /characters/%d/online/ -> HTTP %d", session.CharacterID, resp.StatusCode)
	if resp.StatusCode == http.StatusUnauthorized {
		return false, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("online status request failed: %d - %s", resp.StatusCode, string(bodyBytes))
	}

	var onlineData struct {
		Online bool `json:"online"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&onlineData); err != nil {
		return false, fmt.Errorf("failed to decode online status response: %w", err)
	}

	return onlineData.Online, nil
}

// ssoWaypointHandler sets a waypoint destination in EVE Online via ESI API.
// Note: The ESI endpoint only adds a waypoint to the route but does NOT activate autopilot.
// The character must be online for the request to succeed.
// See https://github.com/esi/esi-issues/issues/1472
func ssoWaypointHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Only allow POST requests
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	session := getSession(r)
	if session == nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}

	// Parse system ID and station ID from request body
	var requestData struct {
		SystemID  int   `json:"system_id"`
		StationID int   `json:"station_id,omitempty"` // Optional: if provided, use station ID instead of system ID
		IsFirst   *bool `json:"is_first,omitempty"`   // If true or omitted: clear existing route. If false: add waypoint (e.g. second leg of Thera route).
	}
	if err := json.NewDecoder(r.Body).Decode(&requestData); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if requestData.SystemID == 0 {
		http.Error(w, "system_id is required", http.StatusBadRequest)
		return
	}

	isOnline, err := checkCharacterOnline(session)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
			return
		}
		log.Printf("Failed to verify online status for character %d: %v", session.CharacterID, err)
		http.Error(w, "Failed to verify character online status", http.StatusInternalServerError)
		return
	}
	if !isOnline {
		http.Error(w, "Character is offline. Log into EVE Online and try again.", http.StatusConflict)
		return
	}

	// Get system name for response
	system := getSystemById(requestData.SystemID)
	systemName := ""
	if system.SystemID != 0 {
		systemName = system.SystemName
	} else {
		systemName = fmt.Sprintf("System %d", requestData.SystemID)
	}

	// Use station_id if provided, otherwise fall back to system_id
	destinationID := requestData.SystemID
	if requestData.StationID > 0 {
		// Always verify station ID via ESI to ensure it's correct
		// Don't trust hardcoded station IDs from jump_clone_stations.json as they may be incorrect
		stationSystemID, stationName, err := verifyStationID(requestData.StationID)
		if err != nil {
			log.Printf("Warning: Failed to verify station ID %d: %v. Falling back to system ID %d", requestData.StationID, err, requestData.SystemID)
			destinationID = requestData.SystemID
		} else if stationSystemID != requestData.SystemID {
			log.Printf("Warning: Station ID %d (%s) belongs to system %d, but request specifies system %d. Using system ID instead.",
				requestData.StationID, stationName, stationSystemID, requestData.SystemID)
			destinationID = requestData.SystemID
		} else {
			// Station ID is valid and belongs to the correct system
			destinationID = requestData.StationID
			log.Printf("Verified station ID %d (%s) in system %d - using as destination", requestData.StationID, stationName, requestData.SystemID)
		}
	}

	// Set waypoint via ESI API
	// Note: destination_id must be a query parameter, not in the request body
	// Default to clearing existing waypoints when is_first is omitted (single "Set destination" click).
	// Only skip clearing when client explicitly sends is_first: false (e.g. second waypoint of Thera route).
	// Important: This endpoint adds a waypoint to the route but does NOT activate autopilot.
	// The character must be online for the request to succeed (already verified above).
	clearOtherWaypoints := requestData.IsFirst == nil || *requestData.IsFirst
	addToBeginning := false // Always add to end to maintain route order
	esiURL := fmt.Sprintf("https://esi.evetech.net/latest/ui/autopilot/waypoint/?destination_id=%d&add_to_beginning=%v&clear_other_waypoints=%v&datasource=tranquility",
		destinationID, addToBeginning, clearOtherWaypoints)
	if requestData.StationID > 0 && destinationID == requestData.SystemID {
		log.Printf("INFO: Station ID %d was provided but not used (fallback to system ID %d)", requestData.StationID, requestData.SystemID)
	} else if requestData.StationID > 0 {
		log.Printf("INFO: Using station ID %d as destination (system ID %d)", requestData.StationID, requestData.SystemID)
	}
	req, err := http.NewRequest("POST", esiURL, nil)
	if err != nil {
		http.Error(w, "Failed to create ESI request", http.StatusInternalServerError)
		return
	}

	req.Header.Set("Authorization", "Bearer "+session.AccessToken)
	req.Header.Set("User-Agent", httpUserAgent)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req) // #nosec G704 -- esiURL host fixed to esi.evetech.net
	if err != nil {
		http.Error(w, "Failed to set waypoint via ESI", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	log.Printf("ESI request: POST /ui/autopilot/waypoint/ destination_id=%d (systemID=%d, stationID=%d) clear_other_waypoints=%v -> HTTP %d",
		destinationID, requestData.SystemID, requestData.StationID, clearOtherWaypoints, resp.StatusCode)

	if resp.StatusCode == http.StatusUnauthorized {
		http.Error(w, "Invalid or expired token", http.StatusUnauthorized)
		return
	}

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		log.Printf("ESI waypoint request failed: %d - %s (destinationID=%d, systemID=%d, stationID=%d)", // #nosec G706 -- ESI response body in log, not user input
			resp.StatusCode, string(bodyBytes), destinationID, requestData.SystemID, requestData.StationID)
		http.Error(w, fmt.Sprintf("Failed to set waypoint: %d - %s", resp.StatusCode, string(bodyBytes)), http.StatusInternalServerError)
		return
	}

	log.Printf("ESI waypoint set successfully: destinationID=%d (systemID=%d, requestedStationID=%d)",
		destinationID, requestData.SystemID, requestData.StationID)

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"systemID":   requestData.SystemID,
		"systemName": systemName,
	})
}

func main() {
	// Load .env from cwd or repo root (Go does not read .env by default; Docker Compose passes env explicitly).
	for _, p := range []string{".env", filepath.Join("..", ".env")} {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := godotenv.Load(p); err != nil {
			log.Printf("warning: could not load %s: %v", p, err)
		}
		break
	}

	// Enable Go net package DNS debug output only when LOG_LEVEL=DEBUG
	if strings.ToUpper(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) == "DEBUG" {
		_ = os.Setenv("GODEBUG", "netdns=2")
	}
	startTime := time.Now()
	logFile, _ = os.Create(fmt.Sprintf("logs/log_%v.log", time.Now().Format("2006-01-02-15-04-05")))
	mw := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(mw)

	fmt.Fprintln(mw, "Starting up in: ", filepath.Dir(os.Args[0])) // #nosec G705 -- log output, not HTML
	startup(mw)

	// Fetch Thera signatures immediately on startup (without delay)
	// In dev mode, mock Thera signatures are set up in loadMockData
	if globalRouteFinder != nil && !mockData {
		globalRouteFinder.ForceFetchTheraSignatures()
		signatureCount := globalRouteFinder.GetTheraSignaturesCount()
		log.Printf("Fetched Thera signatures on startup: %d active connections", signatureCount)
	}

	// Initialize and start zkillboard cache
	killmailCache = zkillboardcache.NewCache()
	killmailCache.SetUserAgent(httpUserAgent)

	// Set up callback for new killmails (stream). Serialize with full recalc so backfill's
	// EnsureRecalculated and stream's incremental updates never run concurrently and cannot corrupt precalculated data.
	killmailCache.SetKillmailCallback(func(killmailID int, killmail *zkillboardcache.CachedKillmail) {
		logging.Debugf("Callback triggered for killmail %d in system %d", killmailID, killmail.SolarSystemID)
		recalcMu.Lock()
		for recalcInProgress {
			recalcCond.Wait()
		}
		recalcMu.Unlock()
		calculateDataForKillmail(killmailID, killmail)
	})
	// Full recalc only when backfill completes or stream hits 404 (natural pause). Stream killmails are calculated via callback.
	killmailCache.SetAfterBackfillCallback(EnsureRecalculated)
	killmailCache.SetOnStream404Callback(EnsureRecalculated)

	// Load mock data in development mode
	if mockData {
		log.Println("Loading mock data for development mode...")
		loadMockData(mw)
		// AddKillmail() does not invoke onKillmailAdded; R2Z2 is disabled in mock mode so
		// EnsureRecalculated is never triggered by backfill/stream. Run it once so precalculated
		// data (routes, system kills) is built from the mock cache and kill tables are populated.
		EnsureRecalculated()
		killmailCache.SetWarmupComplete()
		log.Println("Mock data loaded successfully")
	}

	// Start metrics server before R2Z2 so Prometheus can scrape counter=0; then a short delay
	// so the first scrape sees 0 and increase(...[1m]) includes backfill requests when they run.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	metricsPort := os.Getenv("METRICS_PORT")
	if metricsPort == "" {
		log.Fatal("METRICS_PORT is required")
	}
	if len(os.Args) > 1 && os.Args[1] != "" {
		port = os.Args[1]
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	go func() {
		if err := http.ListenAndServe(":"+metricsPort, metricsMux); err != nil { // #nosec G114 -- metrics-only server
			log.Printf("Metrics server error: %v", err)
		}
	}()
	if !mockData {
		log.Println("Waiting 12s for initial Prometheus scrape so R2Z2 backfill is visible in rate panels...")
		time.Sleep(12 * time.Second)
	}

	killmailCache.Start()
	log.Println("zKillboard cache service started")

	// Register cache HTTP handlers; /metrics is served on METRICS_PORT only (see below)
	http.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !appReadyForBalancer() {
			http.Error(w, "starting up", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	// Verify unique-visitors metric is exposed so Grafana panel has data in non-mock mode too
	if mfs, err := prometheus.DefaultGatherer.Gather(); err == nil {
		for _, mf := range mfs {
			if mf.GetName() == "evepvpsearch_site_root_new_unique_visitors_total" {
				log.Println("Unique visitors metric registered at /metrics")
				break
			}
		}
	}

	// Set up Thera update listener
	setupTheraUpdateListener()
	log.Println("Thera update listener started")

	// CORS middleware for credentialed auth API endpoints (allowlist only; see setCORSHeadersForCredentialedAPI).
	corsHandler := func(handler http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			setCORSHeadersForCredentialedAPI(w, r)

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}

			handler(w, r)
		}
	}

	// SSO authentication endpoints
	http.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		setCORSHeadersForCredentialedAPI(w, r)
		_ = json.NewEncoder(w).Encode(map[string]string{"logLevel": logging.Level()})
	})
	http.HandleFunc("/api/auth/login", ssoLoginHandler)
	http.HandleFunc("/api/auth/callback", ssoCallbackHandler)
	http.HandleFunc("/api/auth/logout", corsHandler(ssoLogoutHandler))
	http.HandleFunc("/api/auth/location", corsHandler(ssoLocationHandler))
	http.HandleFunc("/api/auth/waypoint", corsHandler(ssoWaypointHandler))

	// API endpoints (must be registered before static file server to avoid conflicts)
	http.HandleFunc("/api/systems/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "application/json")

		term := r.URL.Query().Get("term")
		if term == "" {
			_ = json.NewEncoder(w).Encode([]System{})
			return
		}

		// Search systems by name (case-insensitive, partial match)
		termLower := strings.ToLower(term)
		var results []System
		for _, sys := range systems {
			if strings.Contains(strings.ToLower(sys.SystemName), termLower) {
				results = append(results, sys)
			}
			// Limit results to 20 for performance
			if len(results) >= 20 {
				break
			}
		}

		// Sort results by name for better autocomplete experience
		sort.Slice(results, func(i, j int) bool {
			return results[i].SystemName < results[j].SystemName
		})

		_ = json.NewEncoder(w).Encode(results)
	})
	http.HandleFunc("/api/types.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.ServeFile(w, r, "./generated/types.json")
	})
	http.HandleFunc("/api/systems.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.ServeFile(w, r, "./generated/systems.json")
	})
	http.HandleFunc("/api/qualified_trade_hubs.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.ServeFile(w, r, "./data/qualified_trade_hubs.json")
	})

	http.HandleFunc("/api/proximity.html", corsHandler(gzipHandler(proximityHandler)))

	http.HandleFunc("/api/near_trade_hubs.html", gzipHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "text/html")
		readyTablesMu.RLock()
		html := readyNearTradeHubsHTML
		readyTablesMu.RUnlock()
		if html == "" {
			// Not ready yet; trigger background rebuild and return minimal HTML.
			invalidateIndexHTMLCache()
			html = "<div>Loading…</div>"
		}
		if _, err := w.Write([]byte(html)); err != nil {
			log.Printf("Error writing HTML response: %v", err)
		}
	}))

	fmt.Fprintln(mw, "Startup time: ", time.Since(startTime))
	fmt.Fprintf(mw, "Server is listening on port %s (metrics on %s)...\n", port, metricsPort)      // #nosec G705 -- log output, not HTML
	fmt.Fprintf(logFile, "Server is listening on port %s (metrics on %s)...\n", port, metricsPort) // #nosec G705 -- log file, not HTML

	// Serve legal page from embedded static
	http.HandleFunc("/legal.html", func(w http.ResponseWriter, r *http.Request) {
		data, err := staticFS.ReadFile("static/legal.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		// Compute a weak ETag based on content hash for legal page.
		sum := sha256.Sum256(data)
		etag := fmt.Sprintf(`W/"%x"`, sum[:])

		// Use a fixed Last-Modified based on build commit if available, otherwise server start time.
		lastModified := startTime
		if commit != "" {
			// When commit is set, pretend the page was last modified at startup for that commit.
			lastModified = startTime
		}

		// Conditional GET handling.
		if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
			w.Header().Set("ETag", etag)
			w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if ims := r.Header.Get("If-Modified-Since"); ims != "" {
			if t, err := time.Parse(http.TimeFormat, ims); err == nil && !lastModified.After(t) {
				w.Header().Set("ETag", etag)
				w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))
		_, _ = w.Write(data)
	})
	// robots.txt (crawl policy for health and API/cache endpoints on this host)
	http.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		data, err := staticFS.ReadFile("static/robots.txt")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	})
	// Serve favicon from embedded static
	http.HandleFunc("/favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		data, err := staticFS.ReadFile("static/images/favicon.svg")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write(data)
	})

	// Optional direct access to PNGs (main UI inlines icons in the index response).
	iconsSubFS, iconsSubErr := fs.Sub(staticFS, "static")
	if iconsSubErr != nil {
		log.Printf("Could not initialize embedded icons FS: %v", iconsSubErr)
	} else {
		// File layout inside embedded FS is static/icons/<file>.
		// With this handler mounted at /icons/, requesting /icons/<file> resolves to icons/<file> within iconsSubFS.
		http.Handle("/icons/", http.FileServer(http.FS(iconsSubFS)))
	}
	// Serve index.html from embedded static (full server-side rendering: jump clone table + default mode table)
	indexTmpl := template.Must(template.New("index").Delims("[[", "]]").ParseFS(staticFS, "static/index.html"))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		recordSiteRootVisitor(getClientIP(r), r.UserAgent())
		session := getSession(r)
		if session != nil {
			// Authenticated: always build HTML with per-user auth data (no cache).
			jumpCloneBody := renderJumpCloneTableHTML()
			initialResult := getNearTradeHubsResult()
			readyTablesMu.RLock()
			initialTable := readyTheraCampsHTML
			nearHTML := readyNearTradeHubsHTML
			readyTablesMu.RUnlock()
			if initialTable == "" || nearHTML == "" {
				// If not ready yet, kick off background rebuild; page can load without the table.
				invalidateIndexHTMLCache()
			}
			if len(initialResult) > 0 {
				if nearHTML != "" {
					initialTable += "<div id=\"result-container\">" + nearHTML + ccpFooterHTML + "</div>"
				} else {
					initialTable += "<div id=\"result-container\">" + ccpFooterHTML + "</div>"
				}
			} else {
				initialTable += "<div id=\"result-container\">" + ccpFooterHTML + "</div>"
			}
			authData := map[string]interface{}{
				"authenticated": true,
				"characterID":   session.CharacterID,
				"characterName": session.CharacterName,
			}
			authJSON, _ := json.Marshal(authData)
			authBase64 := base64.StdEncoding.EncodeToString(authJSON)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")

			pilotIconRunningHTML := getRunningHumanIconHTMLOrFallback()
			pilotIconStandingHTML := getStandingHumanIconHTMLOrFallback()
			filterIconHTML := getFilterIconHTMLOrFallback()
			embedLoginImages()
			data := map[string]interface{}{
				"JumpCloneTableBody":      template.HTML(jumpCloneBody), // #nosec G203 -- server-rendered only
				"InitialTableHTML":        template.HTML(initialTable),  // #nosec G203 -- server-rendered only
				"AuthBase64":              template.JS(authBase64),      // #nosec G203 -- server-controlled auth JSON
				"PilotRunningIconHTML":    template.HTML(pilotIconRunningHTML),
				"PilotStandingIconHTML":   template.HTML(pilotIconStandingHTML),
				"FilterIconHTML":          template.HTML(filterIconHTML),
				"ShipIconsStyle":          getShipIconsEmbeddedStyle(),
			"LoginImageLargeDataURI":  loginImageLargeDataURI,
			"LoginImageSmallDataURI":  loginImageSmallDataURI,
				"DonateURL":               donateURL,
				"DonateText":              donateText,
				"ContainerTag":            containerTag(),
			}
			if err := indexTmpl.ExecuteTemplate(w, "index.html", data); err != nil {
				log.Printf("Error executing index template: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
			return
		}
		// Non-authenticated: serve cached HTML when present (invalidated when data is recalculated),
		// and support conditional GET via ETag/Last-Modified.
		indexHTMLCacheMu.RLock()
		cached := indexHTMLCache
		lastModified := indexHTMLLastModified
		etag := indexHTMLETag
		useCache := len(cached) > 0
		var cachedCopy []byte
		if useCache {
			cachedCopy = make([]byte, len(cached))
			copy(cachedCopy, cached)
		}
		indexHTMLCacheMu.RUnlock()

		// When we have cache metadata, honor conditional headers.
		if useCache && etag != "" {
			if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
				w.Header().Set("ETag", etag)
				if !lastModified.IsZero() {
					w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))
				}
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		if useCache && !lastModified.IsZero() {
			if ims := r.Header.Get("If-Modified-Since"); ims != "" {
				if t, err := time.Parse(http.TimeFormat, ims); err == nil && !lastModified.After(t) {
					if etag != "" {
						w.Header().Set("ETag", etag)
					}
					w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
		}

		if useCache {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if etag != "" {
				w.Header().Set("ETag", etag)
			}
			if !lastModified.IsZero() {
				w.Header().Set("Last-Modified", lastModified.UTC().Format(http.TimeFormat))
			}
			_, _ = w.Write(cachedCopy)
			return
		}

		// Cache miss or expired: build HTML and populate cache.
		jumpCloneBody := renderJumpCloneTableHTML()
		initialResult := getNearTradeHubsResult()
		readyTablesMu.RLock()
		initialTable := readyTheraCampsHTML
		nearHTML := readyNearTradeHubsHTML
		readyTablesMu.RUnlock()
		if initialTable == "" || nearHTML == "" {
			invalidateIndexHTMLCache()
		}
		if len(initialResult) > 0 {
			if nearHTML != "" {
				initialTable += "<div id=\"result-container\">" + nearHTML + ccpFooterHTML + "</div>"
			} else {
				initialTable += "<div id=\"result-container\">" + ccpFooterHTML + "</div>"
			}
		} else {
			initialTable += "<div id=\"result-container\">" + ccpFooterHTML + "</div>"
		}
		authData := map[string]interface{}{"authenticated": false}
		authJSON, _ := json.Marshal(authData)
		authBase64 := base64.StdEncoding.EncodeToString(authJSON)

		pilotIconRunningHTML := getRunningHumanIconHTMLOrFallback()
		pilotIconStandingHTML := getStandingHumanIconHTMLOrFallback()
		filterIconHTML := getFilterIconHTMLOrFallback()
		embedLoginImages()
		data := map[string]interface{}{
			"JumpCloneTableBody":      template.HTML(jumpCloneBody), // #nosec G203 -- server-rendered only
			"InitialTableHTML":        template.HTML(initialTable),  // #nosec G203 -- server-rendered only
			"AuthBase64":              template.JS(authBase64),      // #nosec G203 -- server-controlled auth JSON
			"PilotRunningIconHTML":    template.HTML(pilotIconRunningHTML),
			"PilotStandingIconHTML":   template.HTML(pilotIconStandingHTML),
			"FilterIconHTML":          template.HTML(filterIconHTML),
			"ShipIconsStyle":          getShipIconsEmbeddedStyle(),
			"LoginImageLargeDataURI":  loginImageLargeDataURI,
			"LoginImageSmallDataURI":  loginImageSmallDataURI,
			"DonateURL":               donateURL,
			"DonateText":              donateText,
			"ContainerTag":            containerTag(),
		}
		var buf bytes.Buffer
		if err := indexTmpl.ExecuteTemplate(&buf, "index.html", data); err != nil {
			log.Printf("Error executing index template: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		htmlBytes := buf.Bytes()
		now := time.Now()
		sum := sha256.Sum256(htmlBytes)
		etag = fmt.Sprintf(`W/"%x"`, sum[:])

		indexHTMLCacheMu.Lock()
		indexHTMLCache = htmlBytes
		indexHTMLLastModified = now
		indexHTMLETag = etag
		indexHTMLCacheMu.Unlock()

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", now.UTC().Format(http.TimeFormat))
		_, _ = w.Write(htmlBytes)
	})

	mainHandler := securityHeadersMiddleware(metricsMiddleware(http.DefaultServeMux))
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mainHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(mw, "Error starting server:", err)
		fmt.Fprintln(logFile, "Error starting server:", err)
	}

	// filter out deployables?
	// https://zkillboard.com/kill/121004544/
	// https://zkillboard.com/kill/121097098/
	// https://zkillboard.com/kill/121172883/ (npc kill)
}
