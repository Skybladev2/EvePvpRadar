// R2Z2 API client for zKillboard ephemeral killmail stream.
// See https://github.com/zKillboard/zKillboard/wiki/API-(R2Z2)

package zkillboardcache

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"evepvpsearch/logging"
)

const (
	r2z2BaseURL     = "https://r2z2.zkillboard.com/ephemeral"
	r2z2SequenceURL = r2z2BaseURL + "/sequence.json"
)

// R2Z2SequenceResponse is the response from .../ephemeral/sequence.json
type R2Z2SequenceResponse struct {
	Sequence int64 `json:"sequence"`
}

// R2Z2ESI is the nested "esi" block in R2Z2 killmail JSON (raw ESI killmail shape).
type R2Z2ESI struct {
	Attackers     []ESIAttacker `json:"attackers"`
	KillmailID    int           `json:"killmail_id"`
	KillmailTime  string        `json:"killmail_time"`
	SolarSystemID int           `json:"solar_system_id"`
	Victim        ESIVictim     `json:"victim"`
}

// R2Z2Killmail is the response from .../ephemeral/{sequence}.json
// Top-level: killmail_id, hash, zkb, uploaded_at, sequence_id; ESI fields are under "esi".
// sequence_updated may be bool or number (sequence id) per API.
type R2Z2Killmail struct {
	SequenceID      int64                `json:"sequence_id"`
	UploadedAt      int64                `json:"uploaded_at"`
	KillmailID      int                  `json:"killmail_id"`
	Hash            string               `json:"hash"`
	SequenceUpdated interface{}         `json:"sequence_updated,omitempty"` // bool or number
	ZKB             ZKillboardKillInfo   `json:"zkb"`
	ESI             R2Z2ESI              `json:"esi"`
}

// getR2Z2Sequence fetches the current sequence from sequence.json
func (c *Cache) getR2Z2Sequence(client *http.Client) (int64, error) {
	logging.Debugf("R2Z2: requesting sequence URL: %s", r2z2SequenceURL)
	ctx := createDNSTraceContext(context.Background(), "r2z2.zkillboard.com")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r2z2SequenceURL, nil)
	if err != nil {
		return 0, fmt.Errorf("r2z2 sequence request: %w", err)
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	start := time.Now()
	resp, err := client.Do(req) // #nosec G704 -- URL host fixed to r2z2.zkillboard.com
	duration := time.Since(start)

	statusCodeLabel := "error"
	if err == nil {
		statusCodeLabel = strconv.Itoa(resp.StatusCode)
		defer resp.Body.Close()
	}

	r2z2RequestsTotal.WithLabelValues("sequence", statusCodeLabel, "stream").Inc()
	r2z2RequestDuration.WithLabelValues("sequence").Observe(duration.Seconds())

	log.Printf("R2Z2 request (stream sequence): %s -> %s", r2z2SequenceURL, statusCodeLabel) // #nosec G706 -- URL and status are build-time constant and response code, not user input
	logging.Debugf("R2Z2: sequence request result: url=%s status=%s duration_ms=%d err=%v", r2z2SequenceURL, statusCodeLabel, duration.Milliseconds(), err)

	if err != nil {
		r2z2Errors.Inc()
		return 0, fmt.Errorf("r2z2 sequence request: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		r2z2RateLimits.Inc()
		return 0, fmt.Errorf("r2z2 rate limited (429)")
	}

	if resp.StatusCode != http.StatusOK {
		r2z2Errors.Inc()
		return 0, fmt.Errorf("r2z2 sequence returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("r2z2 sequence read: %w", err)
	}

	var seqResp R2Z2SequenceResponse
	if err := json.Unmarshal(body, &seqResp); err != nil {
		return 0, fmt.Errorf("r2z2 sequence parse: %w", err)
	}
	return seqResp.Sequence, nil
}

// fetchR2Z2Killmail fetches a single killmail by sequence ID.
// source is "stream" or "backfill" for metrics. backfillIteration/maxBackfill are used
// only for backfill console progress logs. Returns (nil, 404) when no killmail exists yet for that sequence.
func (c *Cache) fetchR2Z2Killmail(client *http.Client, sequence int64, source string, backfillIteration int, maxBackfill int) (*R2Z2Killmail, int, error) {
	url := fmt.Sprintf("%s/%d.json", r2z2BaseURL, sequence)
	logging.Debugf("R2Z2: requesting killmail URL: %s", url)
	ctx := createDNSTraceContext(context.Background(), "r2z2.zkillboard.com")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("r2z2 killmail request: %w", err)
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	start := time.Now()
	resp, err := client.Do(req) // #nosec G704 -- URL host fixed to r2z2.zkillboard.com; path from sequence int
	duration := time.Since(start)

	statusCode := 0
	if err == nil {
		statusCode = resp.StatusCode
		defer resp.Body.Close()
		r2z2RequestsTotal.WithLabelValues("killmail", strconv.Itoa(resp.StatusCode), source).Inc()
		r2z2RequestDuration.WithLabelValues("killmail").Observe(duration.Seconds())
	} else {
		r2z2RequestsTotal.WithLabelValues("killmail", "error", source).Inc()
		r2z2RequestDuration.WithLabelValues("killmail").Observe(duration.Seconds())
		r2z2Errors.Inc()
		logging.Debugf("R2Z2: killmail request failed: url=%s err=%v", url, err)
		return nil, 0, err
	}

	if source == "backfill" && backfillIteration > 0 && maxBackfill > 0 {
		log.Printf("R2Z2 request (%s %d of %d): %s -> %d", source, backfillIteration, maxBackfill, url, statusCode) // #nosec G706 -- URL is base + sequence int, statusCode is response; source is stream|backfill
	} else {
		log.Printf("R2Z2 request (%s): %s -> %d", source, url, statusCode) // #nosec G706 -- URL is base + sequence int, statusCode is response; source is stream|backfill
	}
	logging.Debugf("R2Z2: killmail request result: url=%s status=%d duration_ms=%d source=%s", url, statusCode, duration.Milliseconds(), source)

	if statusCode == http.StatusNotFound {
		logging.Debugf("R2Z2: killmail 404 (no data yet): sequence=%d", sequence)
		return nil, 404, nil
	}

	if statusCode == http.StatusTooManyRequests {
		r2z2RateLimits.Inc()
		return nil, 429, fmt.Errorf("r2z2 rate limited (429)")
	}

	if statusCode != http.StatusOK {
		r2z2Errors.Inc()
		return nil, statusCode, fmt.Errorf("r2z2 killmail returned %d", statusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, statusCode, fmt.Errorf("r2z2 killmail read: %w", err)
	}

	var km R2Z2Killmail
	if err := json.Unmarshal(body, &km); err != nil {
		return nil, statusCode, fmt.Errorf("r2z2 killmail parse: %w", err)
	}

	logging.Debugf("R2Z2: killmail data: sequence=%d killmail_id=%d solar_system_id=%d killmail_time=%s", sequence, km.KillmailID, km.ESI.SolarSystemID, km.ESI.KillmailTime)
	return &km, statusCode, nil
}

// r2z2ToCachedKillmail converts an R2Z2 killmail to CachedKillmail (no ESI fetch needed).
// Uses the nested "esi" block for killmail_time, victim, attackers, solar_system_id.
func r2z2ToCachedKillmail(km *R2Z2Killmail) CachedKillmail {
	killmailID := km.KillmailID
	if killmailID == 0 {
		killmailID = km.ESI.KillmailID
	}
	zkbKill := ZKillboardKill{
		KillmailID: killmailID,
		ZKB:        km.ZKB,
	}
	return CachedKillmail{
		KillmailID:    killmailID,
		KillmailTime:  km.ESI.KillmailTime,
		Victim:        km.ESI.Victim,
		Attackers:     km.ESI.Attackers,
		ZKBInfo:       zkbKill,
		SolarSystemID: km.ESI.SolarSystemID,
	}
}
