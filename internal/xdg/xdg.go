package xdg

import (
	"os"
	"path/filepath"
	"strings"
)

// Home and XDG base directory locations populated from standard environment variables.
var (
	Home       string
	DataHome   string
	ConfigHome string
	StateHome  string
	CacheHome  string
	RuntimeDir string

	DataDirs        []string
	ConfigDirs      []string
	FontDirs        []string
	ApplicationDirs []string

	UserDirs userDirs
)

type userDirs struct {
	Desktop     string
	Download    string
	Documents   string
	Music       string
	Pictures    string
	Videos      string
	Templates   string
	PublicShare string
}

func init() {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	Home = home

	DataHome = envOrDefault("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	ConfigHome = envOrDefault("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	StateHome = envOrDefault("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	CacheHome = envOrDefault("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	RuntimeDir = os.Getenv("XDG_RUNTIME_DIR")

	DataDirs = splitDirs("XDG_DATA_DIRS", "/usr/local/share:/usr/share")
	ConfigDirs = splitDirs("XDG_CONFIG_DIRS", "/etc/xdg")

	FontDirs = []string{
		filepath.Join(DataHome, "fonts"),
		filepath.Join(home, ".fonts"),
	}
	for _, d := range DataDirs {
		FontDirs = append(FontDirs, filepath.Join(d, "fonts"))
	}

	ApplicationDirs = []string{filepath.Join(DataHome, "applications")}
	for _, d := range DataDirs {
		ApplicationDirs = append(ApplicationDirs, filepath.Join(d, "applications"))
	}

	UserDirs = userDirs{
		Desktop:     envOrDefault("XDG_DESKTOP_DIR", filepath.Join(home, "Desktop")),
		Download:    envOrDefault("XDG_DOWNLOAD_DIR", filepath.Join(home, "Downloads")),
		Documents:   envOrDefault("XDG_DOCUMENTS_DIR", filepath.Join(home, "Documents")),
		Music:       envOrDefault("XDG_MUSIC_DIR", filepath.Join(home, "Music")),
		Pictures:    envOrDefault("XDG_PICTURES_DIR", filepath.Join(home, "Pictures")),
		Videos:      envOrDefault("XDG_VIDEOS_DIR", filepath.Join(home, "Videos")),
		Templates:   envOrDefault("XDG_TEMPLATES_DIR", filepath.Join(home, "Templates")),
		PublicShare: envOrDefault("XDG_PUBLICSHARE_DIR", filepath.Join(home, "Public")),
	}
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func splitDirs(key, fallback string) []string {
	raw := envOrDefault(key, fallback)
	parts := strings.Split(raw, string(filepath.ListSeparator))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
