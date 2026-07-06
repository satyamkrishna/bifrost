package batchaccounting

import (
	"context"
	"fmt"
	"time"

	"github.com/bytedance/sonic"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/logstore"
	"github.com/maximhq/bifrost/framework/modelcatalog"
)

const (
	defaultClaimTTL = 5 * time.Minute

	UnpriceableReasonNoResults           = "no_results"
	UnpriceableReasonNoUsage             = "no_usage"
	UnpriceableReasonMissingModel        = "missing_model"
	UnpriceableReasonMissingBatchPricing = "missing_batch_pricing"
)

type Store interface {
	CreateIfNotExists(ctx context.Context, entry *logstore.Log) error
	UpsertBatchJob(ctx context.Context, job *logstore.BatchJob) error
	ClaimBatchJobAccounting(ctx context.Context, jobID string, claimedBy string, ttl time.Duration) (string, bool, error)
	CompleteBatchJobAccounting(ctx context.Context, jobID string, claimToken string, logID string, cost float64, promptTokens int, completionTokens int, totalTokens int, modelBreakdown string) error
	MarkBatchJobUnpriceable(ctx context.Context, jobID string, claimToken string, reason string, err error) error
	FailBatchJobAccounting(ctx context.Context, jobID string, claimToken string, err error) error
}

type AggregateLogWriter interface {
	CreateBatchAggregateLog(ctx context.Context, entry *logstore.Log) error
}

type UsageReporter interface {
	ReportBatchUsage(ctx context.Context, usage BatchUsageReport) error
}

type BatchUsageReport struct {
	RequestID    string
	Provider     schemas.ModelProvider
	Model        string
	Cost         float64
	TokensUsed   int64
	BudgetIDs    []string
	RateLimitIDs []string
}

type PricingManager interface {
	CalculateCostForUsage(usage *schemas.BifrostLLMUsage, provider schemas.ModelProvider, model string, requestType schemas.RequestType, scopes *modelcatalog.PricingLookupScopes) float64
}

type Request struct {
	Provider      schemas.ModelProvider
	BatchID       string
	FallbackModel string
	Results       []schemas.BatchResultItem
	BatchJob      *logstore.BatchJob
	BaseLog       *logstore.Log
	LogWriter     AggregateLogWriter
	UsageReporter UsageReporter
	ClaimedBy     string
	Scopes        *modelcatalog.PricingLookupScopes
	Now           time.Time
}

type Summary struct {
	JobID             string                    `json:"job_id"`
	LogID             string                    `json:"log_id"`
	Provider          schemas.ModelProvider     `json:"provider"`
	BatchID           string                    `json:"batch_id"`
	Cost              float64                   `json:"cost"`
	Usage             schemas.BifrostLLMUsage   `json:"usage"`
	ModelBreakdowns   map[string]ModelBreakdown `json:"model_breakdowns"`
	Accounted         bool                      `json:"accounted"`
	Claimed           bool                      `json:"claimed"`
	UnpriceableReason string                    `json:"unpriceable_reason,omitempty"`
}

type ModelBreakdown struct {
	Model            string                  `json:"model"`
	RequestCount     int                     `json:"request_count"`
	Usage            schemas.BifrostLLMUsage `json:"usage"`
	Cost             float64                 `json:"cost"`
	ProviderCost     float64                 `json:"provider_cost,omitempty"`
	ProviderCostUsed bool                    `json:"provider_cost_used,omitempty"`
}

type extractedUsage struct {
	model        string
	usage        *schemas.BifrostLLMUsage
	hasUsage     bool
	missingModel bool
}

func AccountBatchResults(ctx context.Context, store Store, pricing PricingManager, req Request) (*Summary, error) {
	if store == nil {
		return nil, fmt.Errorf("batch accounting store is nil")
	}
	if pricing == nil {
		return nil, fmt.Errorf("batch accounting pricing manager is nil")
	}
	if req.Provider == "" || req.BatchID == "" {
		return nil, nil
	}

	now := req.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	job := req.BatchJob
	if job == nil {
		job = &logstore.BatchJob{
			Provider:         string(req.Provider),
			BatchID:          req.BatchID,
			Model:            req.FallbackModel,
			AccountingStatus: logstore.BatchJobAccountingStatusPending,
		}
	}
	if job.ID == "" {
		job.ID = logstore.BatchJobID(string(req.Provider), req.BatchID)
	}
	if err := store.UpsertBatchJob(ctx, job); err != nil {
		return nil, err
	}

	summary := &Summary{
		JobID:    job.ID,
		LogID:    AccountingLogID(req.Provider, req.BatchID),
		Provider: req.Provider,
		BatchID:  req.BatchID,
	}

	claimToken, claimed, err := store.ClaimBatchJobAccounting(ctx, job.ID, req.ClaimedBy, defaultClaimTTL)
	if err != nil {
		return nil, err
	}
	summary.Claimed = claimed
	if !claimed {
		return summary, nil
	}

	computed, err := summarizeResults(pricing, req)
	if err != nil {
		_ = store.FailBatchJobAccounting(ctx, job.ID, claimToken, err)
		return nil, err
	}
	if computed == nil || computed.Cost <= 0 {
		reason := UnpriceableReasonNoUsage
		if computed != nil && computed.UnpriceableReason != "" {
			reason = computed.UnpriceableReason
		}
		summary.UnpriceableReason = reason
		if err := store.MarkBatchJobUnpriceable(ctx, job.ID, claimToken, reason, nil); err != nil {
			return nil, err
		}
		return summary, nil
	}

	summary.Cost = computed.Cost
	summary.Usage = computed.Usage
	summary.ModelBreakdowns = computed.ModelBreakdowns

	breakdownJSON, err := sonic.MarshalString(summary.ModelBreakdowns)
	if err != nil {
		_ = store.FailBatchJobAccounting(ctx, job.ID, claimToken, err)
		return nil, err
	}

	entry := buildAggregateLog(req, summary, now)
	if err := createAggregateLog(ctx, store, req.LogWriter, entry); err != nil {
		_ = store.FailBatchJobAccounting(ctx, job.ID, claimToken, err)
		return nil, err
	}
	if req.UsageReporter != nil {
		if err := req.UsageReporter.ReportBatchUsage(ctx, batchUsageReportFromLog(req.Provider, entry)); err != nil {
			_ = store.FailBatchJobAccounting(ctx, job.ID, claimToken, err)
			return nil, err
		}
	}
	if err := store.CompleteBatchJobAccounting(ctx, job.ID, claimToken, summary.LogID, summary.Cost, summary.Usage.PromptTokens, summary.Usage.CompletionTokens, summary.Usage.TotalTokens, breakdownJSON); err != nil {
		_ = store.FailBatchJobAccounting(ctx, job.ID, claimToken, err)
		return nil, err
	}
	summary.Accounted = true
	return summary, nil
}

func createAggregateLog(ctx context.Context, store Store, writer AggregateLogWriter, entry *logstore.Log) error {
	if writer != nil {
		return writer.CreateBatchAggregateLog(ctx, entry)
	}
	return store.CreateIfNotExists(ctx, entry)
}

func batchUsageReportFromLog(provider schemas.ModelProvider, entry *logstore.Log) BatchUsageReport {
	report := BatchUsageReport{
		RequestID:    entry.ID,
		Provider:     provider,
		Model:        entry.Model,
		TokensUsed:   int64(entry.TotalTokens),
		BudgetIDs:    stringSliceFromParsedOrJSON(entry.BudgetIDsParsed, entry.BudgetIDs),
		RateLimitIDs: entry.RateLimitIDsParsed,
	}
	report.RateLimitIDs = stringSliceFromParsedOrJSON(entry.RateLimitIDsParsed, entry.RateLimitIDs)
	if entry.Cost != nil {
		report.Cost = *entry.Cost
	}
	return report
}

func stringSliceFromParsedOrJSON(parsed []string, raw *string) []string {
	if len(parsed) > 0 {
		return parsed
	}
	if raw == nil || *raw == "" {
		return nil
	}
	var values []string
	if err := sonic.Unmarshal([]byte(*raw), &values); err != nil {
		return nil
	}
	return values
}

func AccountingLogID(provider schemas.ModelProvider, batchID string) string {
	return fmt.Sprintf("batch-cost:%s:%s", provider, batchID)
}

func summarizeResults(pricing PricingManager, req Request) (*Summary, error) {
	if len(req.Results) == 0 {
		return &Summary{UnpriceableReason: UnpriceableReasonNoResults}, nil
	}

	breakdowns := make(map[string]ModelBreakdown)
	totalUsage := schemas.BifrostLLMUsage{}
	totalCost := 0.0
	usageSeen := false
	missingModelSeen := false
	missingPricingSeen := false

	for _, item := range req.Results {
		extracted, err := extractUsage(req.Provider, req.FallbackModel, item)
		if err != nil {
			return nil, err
		}
		if !extracted.hasUsage {
			continue
		}
		usageSeen = true
		if extracted.missingModel {
			missingModelSeen = true
			continue
		}

		cost := pricing.CalculateCostForUsage(extracted.usage, req.Provider, extracted.model, schemas.BatchResultsRequest, req.Scopes)
		if cost <= 0 {
			missingPricingSeen = true
			continue
		}

		breakdown := breakdowns[extracted.model]
		breakdown.Model = extracted.model
		breakdown.RequestCount++
		addUsage(&breakdown.Usage, extracted.usage)
		breakdown.Cost += cost
		if extracted.usage.Cost != nil && extracted.usage.Cost.TotalCost > 0 {
			breakdown.ProviderCost += extracted.usage.Cost.TotalCost
			breakdown.ProviderCostUsed = true
		}
		breakdowns[extracted.model] = breakdown

		addUsage(&totalUsage, extracted.usage)
		totalCost += cost
	}

	if len(breakdowns) == 0 {
		reason := UnpriceableReasonNoUsage
		switch {
		case missingModelSeen:
			reason = UnpriceableReasonMissingModel
		case usageSeen && missingPricingSeen:
			reason = UnpriceableReasonMissingBatchPricing
		}
		return &Summary{UnpriceableReason: reason}, nil
	}

	return &Summary{
		Provider:        req.Provider,
		BatchID:         req.BatchID,
		Cost:            totalCost,
		Usage:           totalUsage,
		ModelBreakdowns: breakdowns,
	}, nil
}

func extractUsage(provider schemas.ModelProvider, fallbackModel string, item schemas.BatchResultItem) (extractedUsage, error) {
	switch provider {
	case schemas.OpenAI, schemas.Bedrock:
		return extractResponseBodyUsage(fallbackModel, item)
	case schemas.Anthropic:
		return extractAnthropicUsage(fallbackModel, item)
	default:
		return extractedUsage{}, nil
	}
}

func extractResponseBodyUsage(fallbackModel string, item schemas.BatchResultItem) (extractedUsage, error) {
	if item.Response == nil || item.Response.StatusCode >= 400 || item.Response.Body == nil {
		return extractedUsage{}, nil
	}
	usageValue, ok := item.Response.Body["usage"]
	if !ok || usageValue == nil {
		return extractedUsage{}, nil
	}
	usage, err := usageFromValue(usageValue)
	if err != nil {
		return extractedUsage{}, err
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	if usage.TotalTokens == 0 {
		return extractedUsage{}, nil
	}
	model, _ := item.Response.Body["model"].(string)
	if model == "" {
		model = fallbackModel
	}
	if model == "" {
		return extractedUsage{usage: usage, hasUsage: true, missingModel: true}, nil
	}
	return extractedUsage{model: model, usage: usage, hasUsage: true}, nil
}

func extractAnthropicUsage(fallbackModel string, item schemas.BatchResultItem) (extractedUsage, error) {
	if item.Result == nil || item.Result.Type != "succeeded" || item.Result.Message == nil {
		return extractedUsage{}, nil
	}
	usageValue, ok := item.Result.Message["usage"]
	if !ok || usageValue == nil {
		return extractedUsage{}, nil
	}
	usage, err := anthropicUsageFromValue(usageValue)
	if err != nil {
		return extractedUsage{}, err
	}
	if usage.TotalTokens == 0 {
		return extractedUsage{}, nil
	}
	model, _ := item.Result.Message["model"].(string)
	if model == "" {
		model = fallbackModel
	}
	if model == "" {
		return extractedUsage{usage: usage, hasUsage: true, missingModel: true}, nil
	}
	return extractedUsage{model: model, usage: usage, hasUsage: true}, nil
}

func anthropicUsageFromValue(value interface{}) (*schemas.BifrostLLMUsage, error) {
	bytes, err := sonic.Marshal(value)
	if err != nil {
		return nil, err
	}
	var usage struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreation            struct {
			Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
			Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
		OutputTokens int `json:"output_tokens"`
	}
	if err := sonic.Unmarshal(bytes, &usage); err != nil {
		return nil, err
	}
	promptTokens := usage.InputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
	totalTokens := promptTokens + usage.OutputTokens
	if totalTokens == 0 {
		return &schemas.BifrostLLMUsage{}, nil
	}
	out := &schemas.BifrostLLMUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      totalTokens,
	}
	if usage.CacheCreationInputTokens > 0 || usage.CacheReadInputTokens > 0 {
		out.PromptTokensDetails = &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  usage.CacheReadInputTokens,
			CachedWriteTokens: usage.CacheCreationInputTokens,
		}
		if usage.CacheCreation.Ephemeral5mInputTokens > 0 || usage.CacheCreation.Ephemeral1hInputTokens > 0 {
			out.PromptTokensDetails.CachedWriteTokenDetails = &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens5m: usage.CacheCreation.Ephemeral5mInputTokens,
				CachedWriteTokens1h: usage.CacheCreation.Ephemeral1hInputTokens,
			}
		}
	}
	return out, nil
}

func usageFromValue(value interface{}) (*schemas.BifrostLLMUsage, error) {
	bytes, err := sonic.Marshal(value)
	if err != nil {
		return nil, err
	}
	var usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`

		InputTokensSnake  int `json:"input_tokens"`
		OutputTokensSnake int `json:"output_tokens"`

		InputTokensCamel  int `json:"inputTokens"`
		OutputTokensCamel int `json:"outputTokens"`
		TotalTokensCamel  int `json:"totalTokens"`

		CacheCreationInputTokens  int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens      int `json:"cache_read_input_tokens"`
		CacheReadInputTokensCamel int `json:"cacheReadInputTokens"`
		CacheWriteInputTokens     int `json:"cacheWriteInputTokens"`
		CacheCreation             struct {
			Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
			Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
		CacheDetails []struct {
			InputTokens int    `json:"inputTokens"`
			TTL         string `json:"ttl"`
		} `json:"cacheDetails"`
		Cost *schemas.BifrostCost `json:"cost,omitempty"`
	}
	if err := sonic.Unmarshal(bytes, &usage); err != nil {
		return nil, err
	}
	cacheWriteTokens := usage.CacheCreationInputTokens + usage.CacheWriteInputTokens
	cacheWrite5m := usage.CacheCreation.Ephemeral5mInputTokens
	cacheWrite1h := usage.CacheCreation.Ephemeral1hInputTokens
	cacheDetailsWriteTokens := 0
	for _, detail := range usage.CacheDetails {
		cacheDetailsWriteTokens += detail.InputTokens
		switch detail.TTL {
		case "5m":
			cacheWrite5m += detail.InputTokens
		case "1h":
			cacheWrite1h += detail.InputTokens
		}
	}
	if cacheWriteTokens == 0 {
		cacheWriteTokens = cacheDetailsWriteTokens
	}
	cacheReadTokens := usage.CacheReadInputTokens + usage.CacheReadInputTokensCamel

	promptTokens := firstNonZero(usage.PromptTokens, usage.InputTokensSnake, usage.InputTokensCamel) + cacheReadTokens + cacheWriteTokens
	completionTokens := firstNonZero(usage.CompletionTokens, usage.OutputTokensSnake, usage.OutputTokensCamel)
	totalTokens := firstNonZero(usage.TotalTokens, usage.TotalTokensCamel)
	if totalTokens == 0 {
		totalTokens = promptTokens + completionTokens
	}
	out := &schemas.BifrostLLMUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		Cost:             usage.Cost,
	}
	if cacheReadTokens > 0 || cacheWriteTokens > 0 {
		out.PromptTokensDetails = &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  cacheReadTokens,
			CachedWriteTokens: cacheWriteTokens,
		}
		if cacheWrite5m > 0 || cacheWrite1h > 0 {
			out.PromptTokensDetails.CachedWriteTokenDetails = &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens5m: cacheWrite5m,
				CachedWriteTokens1h: cacheWrite1h,
			}
		}
	}
	return out, nil
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func addUsage(dst *schemas.BifrostLLMUsage, src *schemas.BifrostLLMUsage) {
	if src == nil {
		return
	}
	dst.PromptTokens += src.PromptTokens
	dst.CompletionTokens += src.CompletionTokens
	dst.TotalTokens += src.TotalTokens
	if src.PromptTokensDetails != nil {
		if dst.PromptTokensDetails == nil {
			dst.PromptTokensDetails = &schemas.ChatPromptTokensDetails{}
		}
		dst.PromptTokensDetails.TextTokens += src.PromptTokensDetails.TextTokens
		dst.PromptTokensDetails.AudioTokens += src.PromptTokensDetails.AudioTokens
		dst.PromptTokensDetails.ImageTokens += src.PromptTokensDetails.ImageTokens
		dst.PromptTokensDetails.CachedReadTokens += src.PromptTokensDetails.CachedReadTokens
		dst.PromptTokensDetails.CachedWriteTokens += src.PromptTokensDetails.CachedWriteTokens
		if src.PromptTokensDetails.CachedWriteTokenDetails != nil {
			if dst.PromptTokensDetails.CachedWriteTokenDetails == nil {
				dst.PromptTokensDetails.CachedWriteTokenDetails = &schemas.ChatCachedWriteTokenDetails{}
			}
			dst.PromptTokensDetails.CachedWriteTokenDetails.CachedWriteTokens5m += src.PromptTokensDetails.CachedWriteTokenDetails.CachedWriteTokens5m
			dst.PromptTokensDetails.CachedWriteTokenDetails.CachedWriteTokens1h += src.PromptTokensDetails.CachedWriteTokenDetails.CachedWriteTokens1h
		}
	}
}

func buildAggregateLog(req Request, summary *Summary, now time.Time) *logstore.Log {
	model := req.FallbackModel
	if len(summary.ModelBreakdowns) == 1 {
		for key := range summary.ModelBreakdowns {
			model = key
		}
	} else if len(summary.ModelBreakdowns) > 1 {
		model = "mixed"
	}
	if model == "" {
		model = "mixed"
	}

	entry := &logstore.Log{
		ID:               summary.LogID,
		Timestamp:        now,
		Object:           string(schemas.BatchResultsRequest),
		Provider:         string(req.Provider),
		Model:            model,
		Status:           "success",
		Cost:             &summary.Cost,
		TokenUsageParsed: &summary.Usage,
		PromptTokens:     summary.Usage.PromptTokens,
		CompletionTokens: summary.Usage.CompletionTokens,
		TotalTokens:      summary.Usage.TotalTokens,
		CreatedAt:        now,
		MetadataParsed: map[string]interface{}{
			"batch_accounting": true,
			"batch_id":         req.BatchID,
			"provider":         string(req.Provider),
			"model_breakdown":  summary.ModelBreakdowns,
		},
	}
	if req.BaseLog != nil {
		applyLogAttribution(entry, req.BaseLog)
		return entry
	}
	if req.BatchJob != nil {
		applyBatchJobAttribution(entry, req.BatchJob)
	}
	return entry
}

func applyLogAttribution(entry *logstore.Log, source *logstore.Log) {
	if source.ID != "" {
		entry.ParentRequestID = &source.ID
		entry.MetadataParsed["source_request_id"] = source.ID
	}
	entry.SelectedKeyID = source.SelectedKeyID
	entry.SelectedKeyName = source.SelectedKeyName
	entry.VirtualKeyID = source.VirtualKeyID
	entry.VirtualKeyName = source.VirtualKeyName
	entry.RoutingRuleID = source.RoutingRuleID
	entry.RoutingRuleName = source.RoutingRuleName
	entry.UserID = source.UserID
	entry.UserName = source.UserName
	entry.TeamID = source.TeamID
	entry.TeamName = source.TeamName
	entry.CustomerID = source.CustomerID
	entry.CustomerName = source.CustomerName
	entry.BusinessUnitID = source.BusinessUnitID
	entry.BusinessUnitName = source.BusinessUnitName
	entry.TeamIDsParsed = source.TeamIDsParsed
	entry.TeamNamesParsed = source.TeamNamesParsed
	entry.CustomerIDsParsed = source.CustomerIDsParsed
	entry.CustomerNamesParsed = source.CustomerNamesParsed
	entry.BusinessUnitIDsParsed = source.BusinessUnitIDsParsed
	entry.BusinessUnitNamesParsed = source.BusinessUnitNamesParsed
	entry.BudgetIDsParsed = source.BudgetIDsParsed
	entry.RateLimitIDsParsed = source.RateLimitIDsParsed
	entry.ClusterNodeID = source.ClusterNodeID
	entry.Alias = source.Alias
	entry.CanonicalModelName = source.CanonicalModelName
	entry.AliasModelFamily = source.AliasModelFamily
}

func applyBatchJobAttribution(entry *logstore.Log, job *logstore.BatchJob) {
	entry.SelectedKeyID = job.SelectedKeyID
	entry.VirtualKeyID = job.VirtualKeyID
	entry.RoutingRuleID = job.RoutingRuleID
	entry.UserID = job.UserID
	entry.TeamID = job.TeamID
	entry.CustomerID = job.CustomerID
	entry.BusinessUnitID = job.BusinessUnitID
	entry.BudgetIDs = job.BudgetIDs
	entry.RateLimitIDs = job.RateLimitIDs
	entry.TeamIDs = job.TeamIDs
	entry.CustomerIDs = job.CustomerIDs
	entry.BusinessUnitIDs = job.BusinessUnitIDs
	entry.ClusterNodeID = job.ClusterNodeID
}
