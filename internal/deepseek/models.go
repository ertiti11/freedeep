package deepseek

import "strings"

// Capabilities describes what a model alias can do.
type Capabilities struct {
	Reasoning bool `json:"reasoning"`
	WebSearch bool `json:"web_search"`
	Files     bool `json:"files"`
	Vision    bool `json:"vision,omitempty"`
}

// Model maps a public OpenAI-style alias onto DeepSeek Web settings.
type Model struct {
	ID                string       `json:"id"`
	RealModel         string       `json:"real_model"`
	ModelType         string       `json:"model_type"`
	ThinkingEnabled   bool         `json:"thinking_enabled"`
	SearchEnabled     bool         `json:"search_enabled"`
	Capabilities      Capabilities `json:"capabilities"`
	Supported         bool         `json:"supported"`
	UnavailableReason *string      `json:"unavailable_reason"`
}

func ptr(s string) *string { return &s }

const (
	flashFast  = "DeepSeek-V4-Flash non-thinking (DeepSeek Web “Instant” / default)"
	flashThink = "DeepSeek-V4-Flash thinking mode (DeepSeek Web “Instant” + thinking_enabled)"
)

var (
	fast       = Capabilities{Files: true}
	thinking   = Capabilities{Reasoning: true, Files: true}
	fastSearch = Capabilities{WebSearch: true, Files: true}
	thinkSrch  = Capabilities{Reasoning: true, WebSearch: true, Files: true}
)

// Models lists every alias in display order. Unsupported entries were
// observed in the Web UI but are rejected by the current Web API.
var Models = []Model{
	{ID: "deepseek-chat", ModelType: "default", RealModel: flashFast, Capabilities: fast, Supported: true},
	{ID: "deepseek-v3", ModelType: "default", RealModel: flashFast, Capabilities: fast, Supported: true},
	{ID: "deepseek-default", ModelType: "default", RealModel: flashFast, Capabilities: fast, Supported: true},
	{ID: "deepseek-reasoner", ModelType: "default", ThinkingEnabled: true, RealModel: flashThink, Capabilities: thinking, Supported: true},
	{ID: "deepseek-r1", ModelType: "default", ThinkingEnabled: true, RealModel: "DeepSeek-V4-Flash thinking mode; R1-compatible alias, not a separate R1 model_type in current Web API", Capabilities: thinking, Supported: true},
	{ID: "deepseek-chat-search", ModelType: "default", SearchEnabled: true, RealModel: flashFast + " + web search", Capabilities: fastSearch, Supported: true},
	{ID: "deepseek-default-search", ModelType: "default", SearchEnabled: true, RealModel: flashFast + " + web search", Capabilities: fastSearch, Supported: true},
	{ID: "deepseek-reasoner-search", ModelType: "default", ThinkingEnabled: true, SearchEnabled: true, RealModel: "DeepSeek-V4-Flash thinking mode + web search", Capabilities: thinkSrch, Supported: true},
	{ID: "deepseek-r1-search", ModelType: "default", ThinkingEnabled: true, SearchEnabled: true, RealModel: "DeepSeek-V4-Flash thinking mode + web search; R1-compatible alias", Capabilities: thinkSrch, Supported: true},
	{ID: "deepseek-expert", ModelType: "expert", RealModel: "DeepSeek Web “Expert” (limited resources)", Capabilities: Capabilities{}, Supported: true},
	{ID: "deepseek-v4-pro", ModelType: "expert", ThinkingEnabled: true, RealModel: "DeepSeek Web “Expert” + thinking mode (exposed as deepseek-v4-pro alias)", Capabilities: Capabilities{Reasoning: true}, Supported: true},
	{ID: "deepseek-expert-search", ModelType: "expert", SearchEnabled: true, RealModel: "DeepSeek Web “Expert” + search requested, but Expert has search_feature=null in remote config", Capabilities: Capabilities{},
		UnavailableReason: ptr("Expert mode is rejected; remote config says search is not available for Expert.")},
	{ID: "deepseek-vision", ModelType: "vision", RealModel: "DeepSeek Web “Vision” / image understanding beta", Capabilities: Capabilities{Files: true, Vision: true},
		UnavailableReason: ptr("Current Web API returns: Vision is temporarily unavailable (backend_err_by_model).")},
}

var modelIndex = func() map[string]*Model {
	m := make(map[string]*Model, len(Models))
	for i := range Models {
		m[Models[i].ID] = &Models[i]
	}
	return m
}()

// DefaultModel is used when a request names no model.
const DefaultModel = "deepseek-chat"

// LookupModel finds an alias (case-insensitive).
func LookupModel(id string) (*Model, bool) {
	m, ok := modelIndex[strings.ToLower(strings.TrimSpace(id))]
	return m, ok
}

// ResolveModel returns the alias config, falling back to deepseek-chat.
func ResolveModel(id string) *Model {
	if m, ok := LookupModel(id); ok {
		return m
	}
	return modelIndex[DefaultModel]
}

// SupportedModelIDs lists aliases that work through the proxy.
func SupportedModelIDs() []string {
	var ids []string
	for _, m := range Models {
		if m.Supported {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// UnsupportedModelIDs lists aliases that are known but rejected upstream.
func UnsupportedModelIDs() []string {
	var ids []string
	for _, m := range Models {
		if !m.Supported {
			ids = append(ids, m.ID)
		}
	}
	return ids
}
