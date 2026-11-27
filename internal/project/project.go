// which project a folder belongs to, should be the same on every machine.
package project

import (
	"bufio"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// walking up, nearest thing wins
func Detect(dir string) string {
	if dir == "" {
		return ""
	}
	dir = filepath.Clean(dir)
	for d := dir; ; {
		for _, f := range []string{".sablier-project", ".wakatime-project"} {
			if name, ok := projectFile(filepath.Join(d, f)); ok {
				return name
			}
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			if remote := originRemote(d); remote != "" {
				return remote
			}
			return filepath.Base(d)
		}
		parent := filepath.Dir(d)
		if parent == d {
			return filepath.Base(dir)
		}
		d = parent
	}
}

// name is the repo name for a remote, else the id itself
func Name(id string) string {
	return path.Base(id)
}

var folderName = regexp.MustCompile(`^[^/\\\x00-\x1f\x7f]{1,200}$`)

func ValidName(name string) bool {
	return folderName.MatchString(name) && name != "." && name != ".."
}

func projectFile(p string) (string, bool) {
	f, err := os.Open(p)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	name := ""
	if sc.Scan() {
		name = strings.TrimSpace(sc.Text())
	}
	if sc.Err() != nil {
		return "", false
	}
	if name == "" {
		name = filepath.Base(filepath.Dir(p))
	}
	return name, ValidName(name)
}

func originRemote(root string) string {
	cfg, err := os.Open(gitConfig(root))
	if err != nil {
		return ""
	}
	defer cfg.Close()

	var section, first, origin string
	sc := bufio.NewScanner(cfg)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "url" || !strings.HasPrefix(section, "[remote ") {
			continue
		}
		u := normalize(strings.TrimSpace(v))
		if u == "" {
			continue
		}
		if section == `[remote "origin"]` {
			origin = u
		} else if first == "" {
			first = u
		}
	}
	if sc.Err() != nil {
		return ""
	}
	if origin != "" {
		return origin
	}
	return first
}

func gitConfig(root string) string {
	gitPath := filepath.Join(root, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	gitDir := gitPath
	if !info.IsDir() {
		b, err := os.ReadFile(gitPath)
		if err != nil {
			return ""
		}
		target, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
		if !ok {
			return ""
		}
		gitDir = strings.TrimSpace(target)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
	}
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		common := strings.TrimSpace(string(b))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
		gitDir = common
	}
	return filepath.Join(gitDir, "config")
}

var scpLike = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// git@github.com:a/b.git, https://user@github.com/a/b, ssh://... all become github.com/a/b.
func normalize(remote string) string {
	var host, p string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Hostname() == "" || u.Scheme == "file" {
			return ""
		}
		host, p = u.Hostname(), u.Path
	} else if m := scpLike.FindStringSubmatch(remote); m != nil && !strings.HasPrefix(remote, "/") {
		host, p = m[1], m[2]
	} else {
		return ""
	}
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	if p == "" {
		return ""
	}
	return strings.ToLower(host) + "/" + p
}
