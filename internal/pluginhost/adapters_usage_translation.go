package pluginhost

import (
	"bytes"
	"context"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

func (h *Host) RegisterUsagePlugins() {
	if h == nil {
		return
	}

	for _, record := range h.activeRecords() {
		plugin := record.plugin.Capabilities.UsagePlugin
		if plugin == nil || h.isPluginFused(record.id) {
			continue
		}
		coreusage.RegisterNamedPlugin("plugin:"+record.id, &usageAdapter{
			host:     h,
			pluginID: record.id,
			plugin:   plugin,
		})
	}
}

// resolvedThinkingProvider caches only identity metadata, never a native call result
// that could authorize a different loaded instance.
type resolvedThinkingProvider struct {
	path, version, provider string
}

func (h *Host) refreshThinkingProviders(records []capabilityRecord) {
	if h == nil {
		thinking.ClearPluginProviders()
		return
	}
	resolved := make(map[string]resolvedThinkingProvider)
	for _, record := range records {
		h.mu.Lock()
		_, loading := h.loading[record.id]
		cached, hasCached := h.thinkingProviders[record.id]
		h.mu.Unlock()
		if loading {
			// A fenced plugin must not receive new native identifier calls from
			// an unrelated ApplyConfig while its lifecycle call is still running.
			if hasCached && cached.path == record.path && cached.version == record.version {
				resolved[record.id] = cached
			}
			continue
		}
		provider, okProvider := h.callThinkingIdentifier(record, record.plugin.Capabilities.ThinkingApplier)
		if okProvider {
			resolved[record.id] = resolvedThinkingProvider{record.path, record.version, provider}
		}
	}
	h.mu.Lock()
	h.thinkingProviders = resolved
	h.publishThinkingProvidersLocked(records)
	h.mu.Unlock()
}

// publishThinkingProvidersLocked requires mu. All identifiers were resolved before
// taking publication locks, so restoring a canceled replacement cannot block here
// in native code or accidentally discard another plugin's provider registration.
func (h *Host) publishThinkingProvidersLocked(records []capabilityRecord) {
	thinking.ClearPluginProviders()
	for _, record := range records {
		resolved, ok := h.thinkingProviders[record.id]
		_, fused := h.fused[record.id]
		if !ok || fused || resolved.provider == "" || resolved.path != record.path || resolved.version != record.version || record.plugin.Capabilities.ThinkingApplier == nil {
			continue
		}
		thinking.RegisterPluginProvider(record.id, resolved.provider, record.priority, &thinkingAdapter{
			host: h, pluginID: record.id, path: record.path, version: record.version,
			provider: resolved.provider, applier: record.plugin.Capabilities.ThinkingApplier,
		})
	}
}

func (h *Host) callThinkingIdentifier(record capabilityRecord, applier pluginapi.ThinkingApplier) (string, bool) {
	if h == nil || !h.recordCurrent(record) {
		return "", false
	}
	return h.resolveThinkingIdentifier(record, applier)
}

func (h *Host) resolveThinkingIdentifier(record capabilityRecord, applier pluginapi.ThinkingApplier) (provider string, ok bool) {
	if h == nil || applier == nil || h.isPluginFused(record.id) {
		return "", false
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "ThinkingApplier.Identifier", recovered)
			provider = ""
			ok = false
		}
	}()
	provider = strings.ToLower(strings.TrimSpace(applier.Identifier()))
	if provider == "" {
		return "", false
	}
	return provider, true
}

func (h *Host) currentUsagePlugin(pluginID string) pluginapi.UsagePlugin {
	if h == nil || strings.TrimSpace(pluginID) == "" {
		return nil
	}
	for _, record := range h.activeRecords() {
		if record.id != pluginID {
			continue
		}
		if h.isPluginFused(record.id) {
			return nil
		}
		return record.plugin.Capabilities.UsagePlugin
	}
	return nil
}

func (h *Host) fusePlugin(id, method string, recovered any) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.fused[id] = fmt.Sprintf("%s panic: %v", method, recovered)
	h.mu.Unlock()
	thinking.UnregisterPluginProviders(id)
	log.WithField("plugin_id", id).WithField("method", method).Errorf("pluginhost: plugin panic recovered: %v\n%s", recovered, debug.Stack())
}

func (h *Host) isPluginFused(id string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	_, fused := h.fused[id]
	h.mu.Unlock()
	return fused
}

type usageAdapter struct {
	host     *Host
	pluginID string
	plugin   pluginapi.UsagePlugin
}

type thinkingAdapter struct {
	host     *Host
	pluginID string
	path     string
	version  string
	provider string
	applier  pluginapi.ThinkingApplier
}

func (a *usageAdapter) HandleUsage(ctx context.Context, record coreusage.Record) {
	if a == nil {
		return
	}
	plugin := a.host.currentUsagePlugin(a.pluginID)
	if plugin == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			a.host.fusePlugin(a.pluginID, "UsagePlugin.HandleUsage", recovered)
		}
	}()
	plugin.HandleUsage(ctx, pluginapi.UsageRecord{
		EventID:         record.EventID,
		RequestID:       record.RequestID,
		UsageKnown:      record.UsageKnown,
		Provider:        record.Provider,
		ExecutorType:    record.ExecutorType,
		Model:           record.Model,
		Alias:           record.Alias,
		APIKey:          record.APIKey,
		AuthID:          record.AuthID,
		AuthIndex:       record.AuthIndex,
		AuthType:        record.AuthType,
		Source:          record.Source,
		ReasoningEffort: record.ReasoningEffort,
		ServiceTier:     record.ServiceTier,
		Generate:        coreusage.GenerateEnabled(record.Generate),
		RequestedAt:     record.RequestedAt,
		Latency:         record.Latency,
		TTFT:            record.TTFT,
		Failed:          record.Failed,
		Failure: pluginapi.UsageFailure{
			StatusCode: record.Fail.StatusCode,
			Body:       record.Fail.Body,
		},
		Detail: pluginapi.UsageDetail{
			InputTokens:         record.Detail.InputTokens,
			OutputTokens:        record.Detail.OutputTokens,
			ReasoningTokens:     record.Detail.ReasoningTokens,
			CachedTokens:        record.Detail.CachedTokens,
			CacheReadTokens:     record.Detail.CacheReadTokens,
			CacheCreationTokens: record.Detail.CacheCreationTokens,
			TotalTokens:         record.Detail.TotalTokens,
		},
		ResponseHeaders: cloneHeader(record.ResponseHeaders),
	})
}

func (a *thinkingAdapter) Apply(body []byte, config thinking.ThinkingConfig, modelInfo *registry.ModelInfo) (out []byte, err error) {
	if a == nil || a.applier == nil || a.host == nil || a.host.isPluginFused(a.pluginID) || !a.host.pluginIdentityCurrent(a.pluginID, a.path, a.version) {
		return bytes.Clone(body), nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			a.host.fusePlugin(a.pluginID, "ThinkingApplier.ApplyThinking", recovered)
			out = bytes.Clone(body)
			err = nil
		}
	}()
	resp, errApply := a.applier.ApplyThinking(context.Background(), pluginapi.ThinkingApplyRequest{
		Provider: a.provider,
		Model:    registryModelInfoToPluginModelInfo(modelInfo),
		Config: pluginapi.ThinkingConfig{
			Mode:   config.Mode.String(),
			Budget: config.Budget,
			Level:  string(config.Level),
		},
		Body: bytes.Clone(body),
	})
	if errApply != nil || len(resp.Body) == 0 {
		return bytes.Clone(body), nil
	}
	return bytes.Clone(resp.Body), nil
}

func (h *Host) NormalizeRequest(ctx context.Context, from, to sdktranslator.Format, model string, body []byte, stream bool) []byte {
	current := bytes.Clone(body)
	for _, record := range h.activeRecords() {
		if h.isPluginFused(record.id) || record.plugin.Capabilities.RequestNormalizer == nil {
			continue
		}
		if normalized, ok := h.callRequestNormalizer(ctx, record, from, to, model, current, stream); ok {
			current = normalized
		}
	}
	return current
}

func (h *Host) TranslateRequest(ctx context.Context, from, to sdktranslator.Format, model string, body []byte, stream bool) ([]byte, bool) {
	for _, record := range h.activeRecords() {
		if h.isPluginFused(record.id) || record.plugin.Capabilities.RequestTranslator == nil {
			continue
		}
		if translated, ok := h.callRequestTranslator(ctx, record, from, to, model, body, stream); ok {
			return translated, true
		}
	}
	return bytes.Clone(body), false
}

func (h *Host) NormalizeResponseBefore(ctx context.Context, from, to sdktranslator.Format, model string, originalRequestRawJSON, requestRawJSON, body []byte, stream bool) []byte {
	current := bytes.Clone(body)
	for _, record := range h.activeRecords() {
		normalizer := record.plugin.Capabilities.ResponseBeforeTranslator
		if h.isPluginFused(record.id) || normalizer == nil {
			continue
		}
		if normalized, ok := h.callResponseNormalizer(ctx, record, "ResponseBeforeTranslator.NormalizeResponse", normalizer, from, to, model, originalRequestRawJSON, requestRawJSON, current, stream); ok {
			current = normalized
		}
	}
	return current
}

func (h *Host) TranslateResponse(ctx context.Context, from, to sdktranslator.Format, model string, originalRequestRawJSON, requestRawJSON, body []byte, stream bool) ([]byte, bool) {
	for _, record := range h.activeRecords() {
		translator := record.plugin.Capabilities.ResponseTranslator
		if h.isPluginFused(record.id) || translator == nil {
			continue
		}
		if translated, ok := h.callResponseTranslator(ctx, record, translator, from, to, model, originalRequestRawJSON, requestRawJSON, body, stream); ok {
			return translated, true
		}
	}
	return bytes.Clone(body), false
}

func (h *Host) NormalizeResponseAfter(ctx context.Context, from, to sdktranslator.Format, model string, originalRequestRawJSON, requestRawJSON, body []byte, stream bool) []byte {
	current := bytes.Clone(body)
	for _, record := range h.activeRecords() {
		normalizer := record.plugin.Capabilities.ResponseAfterTranslator
		if h.isPluginFused(record.id) || normalizer == nil {
			continue
		}
		if normalized, ok := h.callResponseNormalizer(ctx, record, "ResponseAfterTranslator.NormalizeResponse", normalizer, from, to, model, originalRequestRawJSON, requestRawJSON, current, stream); ok {
			current = normalized
		}
	}
	return current
}

func (h *Host) callRequestNormalizer(ctx context.Context, record capabilityRecord, from, to sdktranslator.Format, model string, body []byte, stream bool) (out []byte, ok bool) {
	if h == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) || record.plugin.Capabilities.RequestNormalizer == nil {
		return nil, false
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "RequestNormalizer.NormalizeRequest", recovered)
			out = nil
			ok = false
		}
	}()
	resp, errNormalizeRequest := record.plugin.Capabilities.RequestNormalizer.NormalizeRequest(ctx, pluginapi.RequestTransformRequest{
		FromFormat: from.String(),
		ToFormat:   to.String(),
		Model:      model,
		Stream:     stream,
		Body:       bytes.Clone(body),
	})
	if errNormalizeRequest != nil || len(resp.Body) == 0 {
		return nil, false
	}
	return bytes.Clone(resp.Body), true
}

func (h *Host) callRequestTranslator(ctx context.Context, record capabilityRecord, from, to sdktranslator.Format, model string, body []byte, stream bool) (out []byte, ok bool) {
	if h == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) || record.plugin.Capabilities.RequestTranslator == nil {
		return nil, false
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "RequestTranslator.TranslateRequest", recovered)
			out = nil
			ok = false
		}
	}()
	resp, errTranslateRequest := record.plugin.Capabilities.RequestTranslator.TranslateRequest(ctx, pluginapi.RequestTransformRequest{
		FromFormat: from.String(),
		ToFormat:   to.String(),
		Model:      model,
		Stream:     stream,
		Body:       bytes.Clone(body),
	})
	if errTranslateRequest != nil || len(resp.Body) == 0 {
		return nil, false
	}
	return bytes.Clone(resp.Body), true
}

func (h *Host) callResponseNormalizer(ctx context.Context, record capabilityRecord, method string, normalizer pluginapi.ResponseNormalizer, from, to sdktranslator.Format, model string, originalRequestRawJSON, requestRawJSON, body []byte, stream bool) (out []byte, ok bool) {
	if h == nil || normalizer == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
		return nil, false
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, method, recovered)
			out = nil
			ok = false
		}
	}()
	resp, errNormalizeResponse := normalizer.NormalizeResponse(ctx, pluginapi.ResponseTransformRequest{
		FromFormat:        from.String(),
		ToFormat:          to.String(),
		Model:             model,
		Stream:            stream,
		OriginalRequest:   bytes.Clone(originalRequestRawJSON),
		TranslatedRequest: bytes.Clone(requestRawJSON),
		Body:              bytes.Clone(body),
	})
	if errNormalizeResponse != nil || len(resp.Body) == 0 {
		return nil, false
	}
	return bytes.Clone(resp.Body), true
}

func (h *Host) callResponseTranslator(ctx context.Context, record capabilityRecord, translator pluginapi.ResponseTranslator, from, to sdktranslator.Format, model string, originalRequestRawJSON, requestRawJSON, body []byte, stream bool) (out []byte, ok bool) {
	if h == nil || translator == nil || h.isPluginFused(record.id) || !h.recordCurrent(record) {
		return nil, false
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			h.fusePlugin(record.id, "ResponseTranslator.TranslateResponse", recovered)
			out = nil
			ok = false
		}
	}()
	resp, errTranslateResponse := translator.TranslateResponse(ctx, pluginapi.ResponseTransformRequest{
		FromFormat:        from.String(),
		ToFormat:          to.String(),
		Model:             model,
		Stream:            stream,
		OriginalRequest:   bytes.Clone(originalRequestRawJSON),
		TranslatedRequest: bytes.Clone(requestRawJSON),
		Body:              bytes.Clone(body),
	})
	if errTranslateResponse != nil || len(resp.Body) == 0 {
		return nil, false
	}
	return bytes.Clone(resp.Body), true
}
