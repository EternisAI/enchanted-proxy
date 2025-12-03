package proxy

// modelsWithoutToolSupport lists models that don't support native tool calling.
// Matches iOS implementation in ChatsLib/Sources/ChatsLib/Models/AIChatModels.swift
var modelsWithoutToolSupport = map[string]bool{
	"dolphin-mistral-eternis": true, // Venice Uncensored
	"deep-research":           true, // Deep Research
	"/workspace/model":        true, // THE BOX (local model)
}

// SupportsTools returns whether a model supports native tool calling.
// Default: true for all models except those explicitly listed above.
func SupportsTools(modelID string) bool {
	return !modelsWithoutToolSupport[modelID]
}
