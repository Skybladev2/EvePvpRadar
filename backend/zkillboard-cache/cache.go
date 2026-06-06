// zkillboard-cache package for zKillmail data
// Streams killmails via R2Z2; zkillboard embeds full killmail JSON (no ESI fetch).

package zkillboardcache

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"evepvpsearch/doublebuffer"
	"evepvpsearch/logging"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/net/proxy"
)

// Custom logger with millisecond precision
var logger = log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)

// createDNSTraceContext creates a context with DNS tracing for debugging
// hostname is the final destination (e.g., r2z2.zkillboard.com)
func createDNSTraceContext(ctx context.Context, hostname string) context.Context {
	// Use sync.Once to ensure GotConn is only logged once per request
	var gotConnOnce sync.Once

	// Store resolved target addresses (not proxy address)
	var targetAddrs []string
	var targetAddrsMutex sync.Mutex

	// Track which IP address is actually being connected to
	var selectedIP string
	var selectedIPMutex sync.Mutex

	// Check if using proxy
	usingProxy := os.Getenv("SOCKS5_PROXY") != ""
	proxyInfo := ""
	if usingProxy {
		proxyInfo = fmt.Sprintf(" (via proxy %s)", os.Getenv("SOCKS5_PROXY"))
	}

	clientTrace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			logging.Debugf("DNS: Starting lookup for %s -> %s%s", hostname, info.Host, proxyInfo)
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			if info.Err != nil {
				logging.Debugf("DNS: Lookup failed for %s: %v%s", hostname, info.Err, proxyInfo)
				// Even on error, try to resolve manually as fallback
				resolved, err := net.LookupHost(hostname)
				if err == nil && len(resolved) > 0 {
					targetAddrsMutex.Lock()
					targetAddrs = resolved
					targetAddrsMutex.Unlock()
					logging.Debugf("DNS: Fallback lookup succeeded for %s: %v", hostname, resolved)
				}
			} else {
				addrs := make([]string, len(info.Addrs))
				for i, addr := range info.Addrs {
					// Extract just the IP address, not the port
					ip := addr.IP.String()
					if ip != "" {
						addrs[i] = ip
					} else {
						addrs[i] = addr.String()
					}
				}
				// Store resolved addresses for later display
				targetAddrsMutex.Lock()
				targetAddrs = addrs
				targetAddrsMutex.Unlock()
				logging.Debugf("DNS: Lookup completed for %s: addresses=%v (resolved %d IPs), coalesced=%v%s",
					hostname, addrs, len(addrs), info.Coalesced, proxyInfo)
				if len(addrs) > 1 {
					logging.Debugf("DNS: Multiple IPs resolved for %s - Go will try them in order%s", hostname, proxyInfo)
				}
			}
		},
		ConnectStart: func(network, addr string) {
			// Extract IP from address (format: "IP:port" or just "IP")
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				// If no port, addr is just the IP
				host = addr
			}

			// When using proxy, this will be the proxy address, not the destination
			if usingProxy {
				logging.Debugf("DNS: Starting connection to proxy %s://%s (destination: %s)", network, addr, hostname)
				logging.Debugf("DNS: Note - proxy will select which target IP to use from resolved addresses")
			} else {
				// Track which IP is being connected to
				selectedIPMutex.Lock()
				selectedIP = host
				selectedIPMutex.Unlock()
				logging.Debugf("DNS: Starting connection to %s://%s (destination: %s) - attempting IP: %s", network, addr, hostname, host)
			}
		},
		ConnectDone: func(network, addr string, err error) {
			// Extract IP from address
			host, _, parseErr := net.SplitHostPort(addr)
			if parseErr != nil {
				host = addr
			}

			if err != nil {
				if usingProxy {
					logging.Debugf("DNS: Connection failed to proxy %s://%s (destination: %s): %v", network, addr, hostname, err)
				} else {
					logging.Debugf("DNS: Connection failed to %s://%s (destination: %s, IP: %s): %v", network, addr, hostname, host, err)
				}
			} else {
				if usingProxy {
					logging.Debugf("DNS: Connection established to proxy %s://%s (destination: %s)", network, addr, hostname)
					logging.Debugf("DNS: Proxy connected - proxy selected target IP (not visible to client)")
				} else {
					selectedIPMutex.Lock()
					selectedIP = host
					selectedIPMutex.Unlock()
					logging.Debugf("DNS: Connection established to %s://%s (destination: %s) - SUCCESS using IP: %s", network, addr, hostname, host)
				}
			}
		},
		GotConn: func(info httptrace.GotConnInfo) {
			// Only log once per request to avoid duplicates
			gotConnOnce.Do(func() {
				// Get target addresses (resolved IPs) instead of proxy address
				targetAddrsMutex.Lock()
				targetAddrsCopy := make([]string, len(targetAddrs))
				copy(targetAddrsCopy, targetAddrs)
				targetAddrsMutex.Unlock()

				targetAddrStr := "unknown"
				if len(targetAddrsCopy) > 0 {
					// Format addresses nicely
					targetAddrStr = fmt.Sprintf("%v", targetAddrsCopy)
				} else {
					// Fallback: manually resolve DNS if not captured (e.g., when using proxy)
					// This can happen when proxy handles DNS resolution or DNSDone fires after GotConn
					resolved, err := net.LookupHost(hostname)
					if err == nil && len(resolved) > 0 {
						targetAddrStr = fmt.Sprintf("%v", resolved)
						// Store for potential future use
						targetAddrsMutex.Lock()
						targetAddrs = resolved
						targetAddrsMutex.Unlock()
						logging.Debugf("DNS: Fallback manual resolution for %s: %v (DNSDone may not have fired or proxy handled DNS)", hostname, resolved)
					} else if err != nil {
						logging.Debugf("DNS: Fallback manual resolution failed for %s: %v", hostname, err)
					}
				}

				// Get selected IP if available
				selectedIPMutex.Lock()
				selectedIPCopy := selectedIP
				selectedIPMutex.Unlock()

				if usingProxy {
					if selectedIPCopy != "" {
						logging.Debugf("DNS: Got connection for %s - reused=%v, idle=%v, target_addrs=%v, selected_by_proxy=unknown (proxy chose from: %v)",
							hostname, info.Reused, info.WasIdle, targetAddrStr, targetAddrStr)
					} else {
						logging.Debugf("DNS: Got connection for %s - reused=%v, idle=%v, target_addrs=%v, selected_by_proxy=unknown (proxy selects from resolved IPs)",
							hostname, info.Reused, info.WasIdle, targetAddrStr)
					}
				} else {
					// When not using proxy, we can show the actual remote address and selected IP
					remoteAddr := "unknown"
					if info.Conn != nil {
						remoteAddr = info.Conn.RemoteAddr().String()
					}
					selectedInfo := "unknown"
					if selectedIPCopy != "" {
						selectedInfo = selectedIPCopy
					}
					logging.Debugf("DNS: Got connection for %s - reused=%v, idle=%v, target_addrs=%v, selected_ip=%v, remote_addr=%v",
						hostname, info.Reused, info.WasIdle, targetAddrStr, selectedInfo, remoteAddr)
				}
			})
		},
	}
	return httptrace.WithClientTrace(ctx, clientTrace)
}

// createHTTPClient creates an HTTP client with optional SOCKS5 proxy support
// and DNS optimizations based on https://www.dmicheltest.com/posts/2022-01-29-exploring-the-go-http-client/
func createHTTPClient(timeout time.Duration) (*http.Client, error) {
	// Configure dialer with DNS optimizations
	// FallbackDelay: wait 300ms for IPv6 before falling back to IPv4 (RFC 6555 Happy Eyeballs)
	// This prevents delays when IPv6 is misconfigured
	dialer := &net.Dialer{
		Timeout:       30 * time.Second,
		KeepAlive:     30 * time.Second,
		FallbackDelay: 300 * time.Millisecond, // RFC 6555 Fast Fallback
	}

	// Optionally disable IPv6 to avoid AAAA DNS lookups
	// Set DISABLE_IPV6=1 environment variable to enable
	disableIPv6 := os.Getenv("DISABLE_IPV6") == "1"

	var dialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	if disableIPv6 {
		// Force IPv4 only to avoid AAAA DNS lookups
		dialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		}
		logging.Debugf("DNS: IPv6 disabled, using IPv4 only")
	} else {
		dialContext = dialer.DialContext
	}

	proxyAddr := os.Getenv("SOCKS5_PROXY")
	if proxyAddr != "" {
		// Create SOCKS5 dialer
		socksDialer, err := proxy.SOCKS5("tcp", proxyAddr, nil, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("failed to create SOCKS5 dialer: %v", err)
		}
		// Use SOCKS5 dialer but wrap it with our DNS settings
		dialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if disableIPv6 && network == "tcp" {
				network = "tcp4"
			}
			return socksDialer.Dial(network, addr)
		}
	}

	// Create HTTP transport with optimized DNS settings
	transport := &http.Transport{
		DialContext:           dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		DisableKeepAlives:     false,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}

type CachedKillmail struct {
	KillmailID    int            `json:"killmail_id"`
	KillmailTime  string         `json:"killmail_time"`
	Victim        ESIVictim      `json:"victim"`
	Attackers     []ESIAttacker  `json:"attackers"`
	ZKBInfo       ZKillboardKill `json:"zkb"`
	SolarSystemID int            `json:"solar_system_id"` // Added for Near trade hubs mode compatibility
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

// Prometheus metrics
var (
	// Cache metrics
	cacheSize = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "zkillboard_cache_killmail_cache_size",
			Help: "Current number of killmails in cache",
		},
	)

	cacheKillmailsAdded = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "zkillboard_cache_killmail_cache_added_total",
			Help: "Total number of killmails added to cache",
		},
	)

	// R2Z2 API metrics (ephemeral killmail stream). source = "stream" | "backfill".
	r2z2RequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "zkillboard_cache_r2z2_requests_total",
			Help: "Total number of R2Z2 API requests by endpoint, status, and source (stream vs backfill)",
		},
		[]string{"endpoint", "status_code", "source"},
	)

	r2z2RequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "zkillboard_cache_r2z2_request_duration_seconds",
			Help:    "R2Z2 API request duration in seconds",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5},
		},
		[]string{"endpoint"},
	)

	r2z2KillmailsReceived = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "zkillboard_cache_r2z2_killmails_received_total",
			Help: "Total number of killmails received from R2Z2",
		},
	)

	r2z2Errors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "zkillboard_cache_r2z2_errors_total",
			Help: "Total number of R2Z2 API errors",
		},
	)

	r2z2RateLimits = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "zkillboard_cache_r2z2_rate_limits_total",
			Help: "Total number of R2Z2 rate limit (429) responses",
		},
	)
)

// KillmailCallback is a function type for callbacks when new killmails are added
type KillmailCallback func(killmailID int, killmail *CachedKillmail)

// KillmailCacheData wraps the killmail cache map for double-buffering
type KillmailCacheData struct {
	Killmails map[int]CachedKillmail
}

// copyKillmailCacheData creates a deep copy of KillmailCacheData
func copyKillmailCacheData(src *KillmailCacheData) *KillmailCacheData {
	dst := &KillmailCacheData{
		Killmails: make(map[int]CachedKillmail),
	}
	for k, v := range src.Killmails {
		dst.Killmails[k] = v
	}
	return dst
}

// Cache is the main cache structure
type Cache struct {
	killmailCache   *doublebuffer.DoubleBuffer[KillmailCacheData]
	onKillmailAdded KillmailCallback // Callback for new killmails
	onAfterBackfill func()           // Optional: run once after R2Z2 backfill (e.g. recalculate all routes)
	onStream404     func()           // Optional: run when stream gets 404 (natural pause; e.g. full recalc)
	userAgent       string           // User-Agent for zkillboard/R2Z2 requests (set via SetUserAgent)

	// Stream 404 debounce: only run onStream404 when at least one new killmail arrived since last recalc (no timer)
	streamHasNewKillsSinceRecalc bool

	// Backfill: buffer killmails until backfill completes, then merge into main cache
	backfillBuffer map[int]CachedKillmail

	// warmupComplete is set after the first R2Z2 backfill finishes and onAfterBackfill returns (or via SetWarmupComplete in mock mode).
	warmupMu       sync.Mutex
	warmupComplete bool
}

// NewCache creates a new cache instance
func NewCache() *Cache {
	c := &Cache{
		killmailCache: doublebuffer.NewDoubleBuffer(
			&KillmailCacheData{Killmails: make(map[int]CachedKillmail)},
			copyKillmailCacheData,
		),
	}

	// Initialize cache size metric
	readFn := c.killmailCache.Read()
	data := readFn()
	cacheSize.Set(float64(len(data.Killmails)))

	return c
}

// SetKillmailCallback sets a callback function to be called when new killmails are added
func (c *Cache) SetKillmailCallback(callback KillmailCallback) {
	c.killmailCache.Write(func(data *KillmailCacheData) { _ = data })
	c.onKillmailAdded = callback
}

// SetUserAgent sets the User-Agent header for zkillboard/R2Z2 HTTP requests.
func (c *Cache) SetUserAgent(ua string) {
	c.userAgent = ua
}

// SetAfterBackfillCallback sets a function to run once after R2Z2 backfill completes.
// Used to recalculate all routes from the filled cache before resuming new killmail stream.
func (c *Cache) SetAfterBackfillCallback(fn func()) {
	c.onAfterBackfill = fn
}

// SetOnStream404Callback sets a function to run when the stream gets 404 (no new killmail yet).
// Only invoked if at least one new killmail arrived since the last recalc (debounce: no recalc when no new data).
func (c *Cache) SetOnStream404Callback(fn func()) {
	c.onStream404 = fn
}

// SetWarmupComplete marks initial cache warmup as done (e.g. MOCK_DATA mode where R2Z2 backfill does not run).
func (c *Cache) SetWarmupComplete() {
	c.warmupMu.Lock()
	defer c.warmupMu.Unlock()
	c.warmupComplete = true
}

func (c *Cache) markWarmupComplete() {
	c.warmupMu.Lock()
	defer c.warmupMu.Unlock()
	c.warmupComplete = true
}

// WarmupComplete reports whether the first backfill (and its post-backfill hook) has finished, or SetWarmupComplete was called.
func (c *Cache) WarmupComplete() bool {
	c.warmupMu.Lock()
	defer c.warmupMu.Unlock()
	return c.warmupComplete
}

// Start starts the cache service (R2Z2 stream and cleanup)
func (c *Cache) Start() {
	logger.Println("Starting zKillboard Cache Service...")

	// Log DNS resolver info if GODEBUG is set (only at debug level)
	if godebug := os.Getenv("GODEBUG"); godebug != "" {
		logging.Debugf("GODEBUG=%s (DNS resolver info will be printed to stderr if netdns=2 is set)", godebug)
	} else {
		logging.Debugf("DNS: To see DNS resolver info, set GODEBUG=netdns=2 (shows which resolver Go uses: native Go or cgo)")
	}

	logger.Println("Starting R2Z2 ephemeral stream goroutine...")
	go c.startR2Z2Polling()

	// Start cache cleanup goroutine to remove old killmails
	logger.Println("Starting cache cleanup goroutine...")
	go c.startCacheCleanup()
}

// GetRecentKills returns all cached killmails within the last hour
func (c *Cache) GetRecentKills() []CachedKillmail {
	readFn := c.killmailCache.Read()
	data := readFn()

	// Return all cached killmails within the last hour (newest first)
	now := time.Now()
	oneHourAgo := now.Add(-1 * time.Hour)
	var kills []CachedKillmail
	for _, kill := range data.Killmails {
		killTime, err := time.Parse("2006-01-02T15:04:05Z", kill.KillmailTime)
		if err != nil {
			// Skip killmails with invalid timestamps
			continue
		}
		// Only include killmails from the last hour
		if killTime.After(oneHourAgo) {
			kills = append(kills, kill)
		}
	}

	// Sort by killmail ID descending (newest first)
	// Note: In production, you might want to sort by timestamp
	for i := 0; i < len(kills)-1; i++ {
		for j := i + 1; j < len(kills); j++ {
			if kills[i].KillmailID < kills[j].KillmailID {
				kills[i], kills[j] = kills[j], kills[i]
			}
		}
	}

	return kills
}

// AddKillmail adds a killmail to the cache (for development mode)
func (c *Cache) AddKillmail(killmailID int, killmail *CachedKillmail) {
	c.killmailCache.Write(func(data *KillmailCacheData) {
		data.Killmails[killmailID] = *killmail
		cacheSize.Set(float64(len(data.Killmails)))
	})
	cacheKillmailsAdded.Inc()
}

// R2Z2 polling defaults: 100ms between fetches (10/s), 6s sleep on 404. Rate limit 20 req/s per IP.
const (
	r2z2DefaultSleepBetweenMs   = 100
	r2z2DefaultSleepOn404Ms     = 6000
	r2z2BackfillWindow          = 1 * time.Hour // backfill killmails within this window on startup
	r2z2DefaultMaxBackfillKills = 700          // max backfill iterations (env MAX_BACKFILL_KILLMAILS)
)

// r2z2DurationFromEnv parses env key as milliseconds; returns defaultMs if unset or invalid.
func r2z2DurationFromEnv(envKey string, defaultMs int) time.Duration {
	s := strings.TrimSpace(os.Getenv(envKey))
	if s == "" {
		logging.Debugf("R2Z2: env %s unset, using default %dms", envKey, defaultMs)
		return time.Duration(defaultMs) * time.Millisecond
	}
	ms, err := strconv.Atoi(s)
	if err != nil || ms < 0 {
		logging.Debugf("R2Z2: env %s=%q invalid, using default %dms", envKey, s, defaultMs)
		return time.Duration(defaultMs) * time.Millisecond
	}
	return time.Duration(ms) * time.Millisecond
}

// r2z2IntFromEnv parses env key as int; returns defaultVal if unset or invalid.
func r2z2IntFromEnv(envKey string, defaultVal int) int {
	s := strings.TrimSpace(os.Getenv(envKey))
	if s == "" {
		logging.Debugf("R2Z2: env %s unset, using default %d", envKey, defaultVal)
		return defaultVal
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		logging.Debugf("R2Z2: env %s=%q invalid, using default %d", envKey, s, defaultVal)
		return defaultVal
	}
	return v
}

// backfillR2Z2Backward fetches killmails by iterating sequence backward from startSequence,
// buffering those within the past hour. Does not add to main cache until backfill completes.
// Iterates up to maxIterations times; skips killmails older than 1h but continues until max iterations or 404.
// Does not invoke onKillmailAdded.
// On completion, merges the buffer into the main cache so one full recalc can run afterward.
func (c *Cache) backfillR2Z2Backward(client *http.Client, startSequence int64, sleepBetween time.Duration, maxIterations int) {
	cutoff := time.Now().Add(-r2z2BackfillWindow)
	added := 0
	iterations := 0
	seq := startSequence - 1
	c.backfillBuffer = make(map[int]CachedKillmail)
	logging.Debugf("R2Z2 backfill: starting backward from sequence %d, sleep_between=%v, max_iterations=%d", startSequence-1, sleepBetween, maxIterations)

	for seq > 0 && iterations < maxIterations {
		iterations++

		km, statusCode, err := c.fetchR2Z2Killmail(client, seq, "backfill", iterations, maxIterations)
		time.Sleep(sleepBetween)

		if err != nil && statusCode != 404 {
			logger.Printf("R2Z2 backfill: fetch sequence %d error: %v", seq, err)
			seq--
			continue
		}
		if statusCode == 404 {
			logging.Debugf("R2Z2 backfill: no file for sequence %d, stopping", seq)
			logger.Printf("R2Z2 backfill: no file for sequence %d, stopping", seq)
			break
		}
		if km == nil {
			seq--
			continue
		}

		killTime, parseErr := time.Parse("2006-01-02T15:04:05Z", km.ESI.KillmailTime)
		if parseErr != nil {
			logging.Debugf("R2Z2 backfill: sequence %d invalid killmail_time %q, skip", seq, km.ESI.KillmailTime)
			seq--
			continue
		}
		if killTime.Before(cutoff) {
			logging.Debugf("R2Z2 backfill: sequence %d killmail_time %s older than 1h, skip (continue up to %d iterations)", seq, km.ESI.KillmailTime, maxIterations)
			seq--
			continue
		}

		if isJSpace(km.ESI.SolarSystemID) {
			logging.Debugf("R2Z2 backfill: sequence %d killmail_id=%d J-space system %d, skip", seq, km.KillmailID, km.ESI.SolarSystemID)
			seq--
			continue
		}

		logging.Debugf("R2Z2 backfill: buffering killmail_id=%d system=%d time=%s", km.KillmailID, km.ESI.SolarSystemID, km.ESI.KillmailTime)
		cached := r2z2ToCachedKillmail(km)
		c.backfillBuffer[km.KillmailID] = cached
		r2z2KillmailsReceived.Inc()
		added++
		seq--
	}

	if iterations >= maxIterations {
		logger.Printf("R2Z2 backfill: reached max iterations %d, stopping", maxIterations)
	}

	// Merge backfill buffer into main cache only after backfill is complete (handles bursts; one full recalc then runs).
	// This Write is serialized with stream writes by the doublebuffer, so cache data cannot be corrupted by concurrent updates.
	if len(c.backfillBuffer) > 0 {
		c.killmailCache.Write(func(data *KillmailCacheData) {
			for id, km := range c.backfillBuffer {
				data.Killmails[id] = km
				cacheKillmailsAdded.Inc()
			}
			cacheSize.Set(float64(len(data.Killmails)))
		})
		logger.Printf("R2Z2 backfill: merged %d killmail(s) into cache from past hour", len(c.backfillBuffer))
		c.backfillBuffer = nil
	}
}

func (c *Cache) startR2Z2Polling() {
	if os.Getenv("MOCK_DATA") == "1" || os.Getenv("MOCK_DATA") == "true" {
		logger.Println("R2Z2 polling disabled in development mode (zkillboard requests mocked)")
		return
	}

	client, err := createHTTPClient(15 * time.Second)
	if err != nil {
		logger.Printf("R2Z2: failed to create HTTP client: %v", err)
		return
	}

	var sequence int64
	for {
		seq, err := c.getR2Z2Sequence(client)
		if err != nil {
			logger.Printf("R2Z2: get sequence failed: %v; retrying in 30s", err)
			time.Sleep(30 * time.Second)
			continue
		}
		sequence = seq
		logger.Printf("R2Z2: starting from sequence %d", sequence)
		break
	}

	sleepBetween := r2z2DurationFromEnv("R2Z2_SLEEP_BETWEEN_MS", r2z2DefaultSleepBetweenMs)
	sleepOn404 := r2z2DurationFromEnv("R2Z2_SLEEP_ON_404_MS", r2z2DefaultSleepOn404Ms)
	maxBackfill := r2z2IntFromEnv("MAX_BACKFILL_KILLMAILS", r2z2DefaultMaxBackfillKills)
	logging.Debugf("R2Z2: delays from env: R2Z2_SLEEP_BETWEEN_MS=%v, R2Z2_SLEEP_ON_404_MS=%v, MAX_BACKFILL_KILLMAILS=%d", sleepBetween, sleepOn404, maxBackfill)
	logger.Printf("R2Z2: sleep between=%v, sleep on 404=%v, max backfill iterations=%d (MAX_BACKFILL_KILLMAILS)", sleepBetween, sleepOn404, maxBackfill)

	// Run backfill in background so stream can poll for new killmails concurrently
	go func() {
		c.backfillR2Z2Backward(client, sequence, sleepBetween, maxBackfill)
		if c.onAfterBackfill != nil {
			logger.Println("R2Z2: running post-backfill callback (recalculate all)...")
			c.onAfterBackfill()
			logger.Println("R2Z2: post-backfill done")
		}
		c.markWarmupComplete()
	}()

	for {
		km, statusCode, err := c.fetchR2Z2Killmail(client, sequence, "stream", 0, 0)
		if err != nil && statusCode != 404 {
			logger.Printf("R2Z2: fetch sequence %d error: %v", sequence, err)
			if statusCode == 429 {
				logger.Println("R2Z2: rate limited (429), sleeping 60s")
				time.Sleep(60 * time.Second)
			} else {
				time.Sleep(10 * time.Second)
			}
			continue
		}

		if statusCode == 404 {
			logging.Debugf("R2Z2: sequence %d 404, sleeping %v before next try", sequence, sleepOn404)
			if c.onStream404 != nil && c.streamHasNewKillsSinceRecalc {
				c.streamHasNewKillsSinceRecalc = false
				c.onStream404()
			}
			time.Sleep(sleepOn404)
			continue
		}

		if km == nil {
			sequence++
			time.Sleep(sleepBetween)
			continue
		}

		// Process killmail (same filters as backfill)
		if isJSpace(km.ESI.SolarSystemID) {
			logging.Debugf("R2Z2: ignoring killmail_id=%d J-space system %d", km.KillmailID, km.ESI.SolarSystemID)
			logger.Printf("R2Z2: ignoring killmail %d from J-space system %d", km.KillmailID, km.ESI.SolarSystemID)
			sequence++
			time.Sleep(sleepBetween)
			continue
		}

		logging.Debugf("R2Z2: cached killmail_id=%d system=%d time=%s", km.KillmailID, km.ESI.SolarSystemID, km.ESI.KillmailTime)
		r2z2KillmailsReceived.Inc()
		cached := r2z2ToCachedKillmail(km)
		c.streamHasNewKillsSinceRecalc = true

		c.killmailCache.Write(func(data *KillmailCacheData) {
			data.Killmails[km.KillmailID] = cached
			cacheSize.Set(float64(len(data.Killmails)))
		})
		cacheKillmailsAdded.Inc()

		if c.onKillmailAdded != nil {
			go c.onKillmailAdded(km.KillmailID, &cached)
		}

		sequence++
		time.Sleep(sleepBetween)
	}
}

// TheraSystemID is the system ID for Thera (shattered wormhole with trade hub connections)
const TheraSystemID = 31000005

// isJSpace checks if a system ID belongs to J-space (wormhole) only.
// J-space systems: 31000000-31999999
// Pochven (30000001-30000144) is not included so Pochven killmails are cached for proximity mode.
// Thera (31000005) is excluded so Thera kills can be shown for camp detection.
func isJSpace(systemID int) bool {
	if systemID == TheraSystemID {
		return false // Thera is cached for camp detection
	}
	return systemID >= 31000000 && systemID <= 31999999
}

// startCacheCleanup runs a periodic cleanup to remove killmails older than 1 hour
func (c *Cache) startCacheCleanup() {
	logger.Println("Cache cleanup service started")
	ticker := time.NewTicker(5 * time.Minute) // Run cleanup every 5 minutes
	defer ticker.Stop()

	// Run initial cleanup after 1 minute
	time.Sleep(1 * time.Minute)
	c.cleanupOldKillmails()

	for range ticker.C {
		c.cleanupOldKillmails()
	}
}

// cleanupOldKillmails removes killmails from cache that are older than 1 hour
func (c *Cache) cleanupOldKillmails() {
	now := time.Now()
	oneHourAgo := now.Add(-1 * time.Hour)
	removedCount := 0

	c.killmailCache.Write(func(data *KillmailCacheData) {
		// Parse killmail times and remove old entries
		for killmailID, killmail := range data.Killmails {
			killTime, err := time.Parse("2006-01-02T15:04:05Z", killmail.KillmailTime)
			if err != nil {
				// If we can't parse the time, log and remove it (safer to remove invalid data)
				logger.Printf("Warning: Failed to parse killmail time for ID %d: %v. Removing from cache.", killmailID, err)
				delete(data.Killmails, killmailID)
				removedCount++
				continue
			}

			// Remove if killmail is older than 1 hour
			if killTime.Before(oneHourAgo) {
				delete(data.Killmails, killmailID)
				removedCount++
			}
		}

		// Update cache size metric
		cacheSize.Set(float64(len(data.Killmails)))
	})

	if removedCount > 0 {
		readFn := c.killmailCache.Read()
		data := readFn()
		logger.Printf("Cache cleanup: Removed %d killmail(s) older than 1 hour. Cache size: %d", removedCount, len(data.Killmails))
	}
}
