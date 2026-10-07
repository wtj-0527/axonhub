package provider_quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/llm/httpclient"
)

const apimesProviderType = "apimes"

// apimesUsageResponse matches the subset of GET /v1/usage used for Token Plan
// keys. token_plan is absent for regular (wallet) keys.
type apimesUsageResponse struct {
	TokenPlan *struct {
		Windows []apimesUsageWindow `json:"windows"`
	} `json:"token_plan"`
}

type apimesUsageWindow struct {
	Window      string     `json:"window"`
	UsedPercent *float64   `json:"used_percent"`
	ResetAt     *time.Time `json:"reset_at"`
}

// ApimesQuotaChecker reads Token Plan 5h/7d usage from an apimes.com gateway
// via its /v1/usage endpoint using the channel's API key.
type ApimesQuotaChecker struct {
	httpClient *httpclient.HttpClient
}

func NewApimesQuotaChecker(httpClient *httpclient.HttpClient) *ApimesQuotaChecker {
	return &ApimesQuotaChecker{httpClient: httpClient}
}

func (c *ApimesQuotaChecker) CheckQuota(ctx context.Context, ch *ent.Channel) (QuotaData, error) {
	apiKey := opencodeGoAPIKey(ch)
	if apiKey == "" {
		return QuotaData{}, fmt.Errorf("missing API key for apimes channel")
	}

	request := httpclient.NewRequestBuilder().
		WithMethod(http.MethodGet).
		WithURL(buildApimesUsageURL(ch.BaseURL)).
		WithBearerToken(apiKey).
		WithHeader("Accept", "application/json").
		Build()

	hc := c.httpClient
	if ch.Settings != nil && ch.Settings.Proxy != nil {
		hc = c.httpClient.WithProxy(ch.Settings.Proxy)
	}

	resp, err := hc.Do(ctx, request)
	if err != nil {
		return QuotaData{}, fmt.Errorf("executing apimes usage request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return QuotaData{}, fmt.Errorf("apimes usage API returned status %d", resp.StatusCode)
	}

	return c.parseResponse(resp.Body)
}

func (c *ApimesQuotaChecker) parseResponse(body []byte) (QuotaData, error) {
	var parsed apimesUsageResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return QuotaData{}, fmt.Errorf("parsing apimes usage response: %w", err)
	}
	if parsed.TokenPlan == nil {
		return QuotaData{}, fmt.Errorf("apimes usage response has no token_plan (not a Token Plan key)")
	}

	limits := make([]QuotaLimitStatus, 0, len(parsed.TokenPlan.Windows))
	rawWindows := make(map[string]any, len(parsed.TokenPlan.Windows))
	normalizedStatus := "available"
	var nextResetAt *time.Time

	for _, w := range parsed.TokenPlan.Windows {
		var length time.Duration
		switch w.Window {
		case QuotaWindow5h:
			length = 5 * time.Hour
		case QuotaWindow7d:
			length = 7 * 24 * time.Hour
		default:
			continue
		}
		// No data yet for this window (seat not active / source unknown).
		if w.UsedPercent == nil {
			continue
		}

		usageRatio := *w.UsedPercent / 100.0
		status := normalizeOpenCodeGoWindowStatus(usageRatio)
		if quotaStatusRank(status) > quotaStatusRank(normalizedStatus) {
			normalizedStatus = status
		}
		if w.ResetAt != nil && (nextResetAt == nil || w.ResetAt.Before(*nextResetAt)) {
			nextResetAt = w.ResetAt
		}

		raw := map[string]any{"used_percent": *w.UsedPercent}
		if w.ResetAt != nil {
			raw["reset_at"] = w.ResetAt.Format(time.RFC3339)
		}
		rawWindows[w.Window] = raw

		limits = append(limits, NewTokenLimitStatus(status, usageRatio, w.ResetAt).WithWindow(w.Window, length))
	}

	if len(limits) == 0 {
		return QuotaData{}, fmt.Errorf("apimes token_plan has no usable windows")
	}

	return NormalizeQuotaData(QuotaData{
		Status:       normalizedStatus,
		ProviderType: apimesProviderType,
		RawData:      map[string]any{"windows": rawWindows},
		NextResetAt:  nextResetAt,
		Ready:        IsReadyStatus(normalizedStatus),
		Limits:       limits,
	}), nil
}

func (c *ApimesQuotaChecker) SupportsChannel(ch *ent.Channel) bool {
	if ch.Type != channel.TypeOpenai && ch.Type != channel.TypeOpenaiResponses {
		return false
	}
	return DetectProviderFromURL(ch.BaseURL) == apimesProviderType
}

func buildApimesUsageURL(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" {
		return "https://apimes.com/v1/usage"
	}
	parsed.Path = "/v1/usage"
	parsed.RawQuery = ""
	parsed.User = nil
	parsed.Fragment = ""
	return parsed.String()
}
