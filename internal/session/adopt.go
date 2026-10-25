package session

import (
	"encoding/json"
	"os"
	"path/filepath"

	"gogen/internal/config"
)

// LatestSavedModel returns the model and provider profile stored on the
// newest top-level session under workingDir. The web UI and the CLI keep
// the chosen model on the session. An empty model means no saved session
// chose one.
func LatestSavedModel(workingDir string) (model, provider string) {
	if workingDir == "" {
		return "", ""
	}
	// List is the session index: newest first, including nested and
	// oneshot entries. The model itself is not in the index, so only
	// the chosen file is read, and only for its model fields.
	infos, err := (&Store{enabled: true}).List(workingDir)
	if err != nil || len(infos) == 0 {
		return "", ""
	}
	var id string
	for _, info := range infos {
		if info.MessageCount == 0 || info.Oneshot || info.ParentID != "" || validateSessionID(info.ID) != nil {
			continue
		}
		id = info.ID
		break
	}
	if id == "" {
		return "", ""
	}
	body, err := os.ReadFile(filepath.Join(workingDir, ".gogen", "sessions", id+".json"))
	if err != nil {
		return "", ""
	}
	var head struct {
		Model         string `json:"model"`
		ModelProvider string `json:"modelProvider"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return "", ""
	}
	return head.Model, head.ModelProvider
}

// ApplyLatestSavedModel copies the newest saved session's model onto cfg
// when cfg does not already name one. A named provider receives it on that
// provider. The default profile receives it as the workspace model.
// Returns whether cfg changed.
func ApplyLatestSavedModel(cfg *config.Config, workingDir string) bool {
	if cfg == nil || workingDir == "" || configNamesModel(cfg) {
		return false
	}
	model, provider := LatestSavedModel(workingDir)
	if model == "" {
		return false
	}
	if provider == "" || provider == "default" {
		cfg.OpenAIModel = model
		return true
	}
	for i := range cfg.OpenAIProviders {
		if cfg.OpenAIProviders[i].Name == provider {
			cfg.OpenAIProviders[i].Model = model
			return true
		}
	}
	return false
}

func configNamesModel(cfg *config.Config) bool {
	if cfg.OpenAIModel != "" {
		return true
	}
	for _, p := range cfg.OpenAIProviders {
		if p.Model != "" {
			return true
		}
	}
	return false
}
