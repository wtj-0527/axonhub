package provider_quota

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

func apimesTestClient(t *testing.T, status int, body string) *httpclient.HttpClient {
	t.Helper()
	return httpclient.NewHttpClientWithClient(&http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			require.Equal(t, "Bearer sk-test", req.Header.Get("Authorization"))
			require.Equal(t, "apimes.com", req.URL.Host)
			require.Equal(t, "/v1/usage", req.URL.Path)
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	})
}

func apimesTestChannel() *ent.Channel {
	return &ent.Channel{
		Type:        channel.TypeOpenai,
		BaseURL:     "https://apimes.com/v1",
		Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
	}
}

func TestApimes_CheckQuota_TokenPlanWindows(t *testing.T) {
	reset5h := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	reset7d := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	body := fmt.Sprintf(`{"mode":"unrestricted","token_plan":{"windows":[
		{"window":"5h","used_percent":42.5,"reset_at":%q},
		{"window":"7d","used_percent":85,"reset_at":%q}]}}`,
		reset5h.Format(time.RFC3339), reset7d.Format(time.RFC3339))
	checker := NewApimesQuotaChecker(apimesTestClient(t, http.StatusOK, body))

	quota, err := checker.CheckQuota(context.Background(), apimesTestChannel())
	require.NoError(t, err)
	require.Equal(t, "apimes", quota.ProviderType)
	require.Equal(t, "warning", quota.Status)
	require.True(t, quota.Ready)
	require.Len(t, quota.Limits, 2)
	require.Equal(t, QuotaWindow5h, quota.Limits[0].Window)
	require.InDelta(t, 0.425, quota.Limits[0].UsageRatio, 0.0001)
	require.Equal(t, "available", quota.Limits[0].Status)
	require.Equal(t, QuotaWindow7d, quota.Limits[1].Window)
	require.Equal(t, "warning", quota.Limits[1].Status)
	require.NotNil(t, quota.NextResetAt)
	require.True(t, reset5h.Equal(*quota.NextResetAt))
}

func TestApimes_CheckQuota_Exhausted(t *testing.T) {
	body := `{"token_plan":{"windows":[{"window":"5h","used_percent":100},{"window":"7d","used_percent":null}]}}`
	checker := NewApimesQuotaChecker(apimesTestClient(t, http.StatusOK, body))

	quota, err := checker.CheckQuota(context.Background(), apimesTestChannel())
	require.NoError(t, err)
	require.Equal(t, "exhausted", quota.Status)
	require.False(t, quota.Ready)
	require.Len(t, quota.Limits, 1)
}

func TestApimes_CheckQuota_NoTokenPlan(t *testing.T) {
	checker := NewApimesQuotaChecker(apimesTestClient(t, http.StatusOK, `{"mode":"quota_limited","remaining":3}`))

	_, err := checker.CheckQuota(context.Background(), apimesTestChannel())
	require.Error(t, err)
}

func TestApimes_CheckQuota_HTTPError(t *testing.T) {
	checker := NewApimesQuotaChecker(apimesTestClient(t, http.StatusUnauthorized, `{"error":"invalid"}`))

	_, err := checker.CheckQuota(context.Background(), apimesTestChannel())
	require.Error(t, err)
}

func TestApimes_SupportsChannel(t *testing.T) {
	checker := NewApimesQuotaChecker(nil)
	require.True(t, checker.SupportsChannel(apimesTestChannel()))
	require.True(t, checker.SupportsChannel(&ent.Channel{Type: channel.TypeOpenaiResponses, BaseURL: "https://api.apimes.com"}))
	require.False(t, checker.SupportsChannel(&ent.Channel{Type: channel.TypeAnthropic, BaseURL: "https://apimes.com"}))
	require.False(t, checker.SupportsChannel(&ent.Channel{Type: channel.TypeOpenai, BaseURL: "https://api.openai.com/v1"}))
}
