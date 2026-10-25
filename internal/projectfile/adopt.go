package projectfile

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// AdoptConfEnv is the environment variable a host sets to an existing
// project config when the global config file does not exist yet. The web
// UI and the CLI write that project file. The current folder's own conf
// still wins field by field. Once the global file exists, this variable
// is ignored.
const AdoptConfEnv = "GOGEN_ADOPT_CONF"

// LoadAdoptConf loads the project config named by AdoptConfEnv. dir is
// the project that owns the file, so the caller can read that project's
// sessions. ok is false when the variable is unset, the file cannot be
// read, or it has no config.
func LoadAdoptConf() (dir string, pf *ProjectFile, ok bool) {
	path := strings.TrimSpace(os.Getenv(AdoptConfEnv))
	if path == "" {
		return "", nil, false
	}
	loaded, err := Load(path)
	if err != nil || loaded == nil || !loaded.HasConfig {
		if err != nil {
			log.Printf("adopt config: %v", err)
		}
		return "", nil, false
	}
	return projectDirOf(path), loaded, true
}

func projectDirOf(configPath string) string {
	dir := filepath.Dir(configPath)
	if strings.EqualFold(filepath.Base(dir), ".gogen") {
		return filepath.Dir(dir)
	}
	return dir
}
