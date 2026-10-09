package main

import (
	"bytes"
	"encoding/json"
	"html"
	"os"
	"path/filepath"
	"strings"
)

func deskName() string {
	if name := strings.TrimSpace(os.Getenv("RESEARCH_DESK_NAME")); name != "" && len(name) <= 120 {
		return name
	}
	return "Research Desk"
}
func brandedPage() []byte {
	alt := os.Getenv("RESEARCH_DESK_ICON_ALT")
	if alt == "" {
		alt = deskName() + " mark"
	}
	description := os.Getenv("RESEARCH_DESK_DESCRIPTION")
	if description == "" {
		description = "A personal library for questions, evidence, and connections."
	}
	return []byte(strings.NewReplacer("{{DESK_NAME}}", html.EscapeString(deskName()), "{{ICON_ALT}}", html.EscapeString(alt), "{{DESK_DESCRIPTION}}", html.EscapeString(description)).Replace(string(page)))
}
func brandedManifest() []byte {
	var data map[string]any
	json.Unmarshal(manifest, &data)
	data["name"] = deskName()
	data["short_name"] = deskName()
	out, _ := json.Marshal(data)
	return out
}
func brandedIcon() []byte {
	if dir := os.Getenv("RESEARCH_PRIVATE_CONFIG_DIR"); dir != "" {
		icon, err := os.ReadFile(filepath.Join(dir, "icon.svg"))
		if err == nil && len(icon) < 128<<10 && bytes.Contains(icon, []byte("<svg")) {
			return icon
		}
	}
	return appIcon
}
