package mmxc

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	defaultThemeName = "default"
	defaultThemeMode = "system"
	defaultThemeDir  = "data/themes"
	themeAssetDir    = "assets/themes"
)

type themeOption struct {
	Name  string
	Label string
}

func ensureThemeFiles(dir string) error {
	if strings.TrimSpace(dir) == "" {
		dir = defaultThemeDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create theme directory: %w", err)
	}
	entries, err := themeAssets.ReadDir(themeAssetDir)
	if err != nil {
		return fmt.Errorf("read embedded themes: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".css") {
			continue
		}
		target := filepath.Join(dir, entry.Name())
		if _, err := os.Stat(target); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat theme %s: %w", target, err)
		}
		data, err := themeAssets.ReadFile(path.Join(themeAssetDir, entry.Name()))
		if err != nil {
			return fmt.Errorf("read embedded theme %s: %w", entry.Name(), err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return fmt.Errorf("write theme %s: %w", target, err)
		}
	}
	return nil
}

func themeOptions(dir string) []themeOption {
	if strings.TrimSpace(dir) == "" {
		dir = defaultThemeDir
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []themeOption{{Name: defaultThemeName, Label: themeLabel(defaultThemeName)}}
	}
	var themes []themeOption
	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".css") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".css")
		if !validThemeName(name) || seen[name] {
			continue
		}
		seen[name] = true
		themes = append(themes, themeOption{Name: name, Label: themeLabel(name)})
	}
	if !seen[defaultThemeName] {
		themes = append(themes, themeOption{Name: defaultThemeName, Label: themeLabel(defaultThemeName)})
	}
	sort.Slice(themes, func(i, j int) bool {
		if themes[i].Name == defaultThemeName {
			return true
		}
		if themes[j].Name == defaultThemeName {
			return false
		}
		return themes[i].Label < themes[j].Label
	})
	return themes
}

func themeAvailable(name string, themes []themeOption) bool {
	for _, theme := range themes {
		if theme.Name == name {
			return true
		}
	}
	return false
}

func normalizeThemeName(value string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(value))
	if name == "" {
		name = defaultThemeName
	}
	if !validThemeName(name) {
		return "", fmt.Errorf("theme must be a CSS filename stem from %s", defaultThemeDir)
	}
	return name, nil
}

func normalizeThemeMode(value string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(value))
	if mode == "" {
		mode = defaultThemeMode
	}
	switch mode {
	case "system", "light", "dark":
		return mode, nil
	default:
		return "", fmt.Errorf("theme_mode must be system, light, or dark")
	}
}

func validThemeName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func themeLabel(name string) string {
	switch name {
	case "default":
		return "Default"
	case "serious":
		return "Serious"
	case "greenback":
		return "Greenback"
	default:
		parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
		for i := range parts {
			if parts[i] == "" {
				continue
			}
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
		return strings.Join(parts, " ")
	}
}

func (a *app) themeCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	name := a.config.Theme
	if name == "" {
		name = defaultThemeName
	}
	data, err := os.ReadFile(filepath.Join(defaultThemeDir, name+".css"))
	if err != nil && name != defaultThemeName {
		data, err = os.ReadFile(filepath.Join(defaultThemeDir, defaultThemeName+".css"))
	}
	if err != nil {
		data, err = fs.ReadFile(themeAssets, path.Join(themeAssetDir, defaultThemeName+".css"))
	}
	if err != nil {
		http.Error(w, "theme unavailable", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(applyThemeMode(data, a.config.ThemeMode))
}

func applyThemeMode(data []byte, mode string) []byte {
	css := string(data)
	normalized, err := normalizeThemeMode(mode)
	if err != nil {
		normalized = defaultThemeMode
	}
	if normalized == "system" {
		return data
	}
	base, dark := splitDarkMedia(css)
	if normalized == "light" {
		return []byte(base + "\n:root { color-scheme: light; }\n")
	}
	if strings.TrimSpace(dark) == "" {
		return []byte(base + "\n:root { color-scheme: dark; }\n")
	}
	return []byte(base + "\n:root { color-scheme: dark; }\n" + dark + "\n")
}

func splitDarkMedia(css string) (string, string) {
	const marker = "@media (prefers-color-scheme: dark)"
	start := strings.Index(css, marker)
	if start < 0 {
		return css, ""
	}
	open := strings.Index(css[start:], "{")
	if open < 0 {
		return css, ""
	}
	open += start
	depth := 0
	for i := open; i < len(css); i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				base := strings.TrimSpace(css[:start]) + "\n" + strings.TrimSpace(css[i+1:])
				dark := strings.TrimSpace(css[open+1 : i])
				return strings.TrimSpace(base), dark
			}
		}
	}
	return css, ""
}
