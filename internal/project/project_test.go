package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, p, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repo(t *testing.T, root, config string) {
	t.Helper()
	write(t, filepath.Join(root, ".git", "config"), config)
}

func TestDetect(t *testing.T) {
	tmp := t.TempDir()

	// repo with a remote, from a subfolder
	sablier := filepath.Join(tmp, "sablier")
	repo(t, sablier, "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = git@github.com:espcaa/sablier.git\n")
	os.MkdirAll(filepath.Join(sablier, "internal", "cli"), 0o755)

	// repo without a remote
	local := filepath.Join(tmp, "local-repo")
	repo(t, local, "[core]\n")

	// monorepo with a project file in one app
	mono := filepath.Join(tmp, "mono")
	repo(t, mono, "[remote \"origin\"]\n\turl = https://github.com/acme/mono\n")
	write(t, filepath.Join(mono, "apps", "web", ".sablier-project"), "acme-web\n")
	write(t, filepath.Join(mono, "apps", "cli", ".wakatime-project"), "acme-cli\nsome-branch\n")
	os.MkdirAll(filepath.Join(mono, "apps", "web", "src"), 0o755)

	// both files in one folder: sablier's wins
	both := filepath.Join(tmp, "both")
	write(t, filepath.Join(both, ".sablier-project"), "from-sablier")
	write(t, filepath.Join(both, ".wakatime-project"), "from-wakatime")

	// empty file: the folder's name. invalid name: skipped
	write(t, filepath.Join(tmp, "notes", ".sablier-project"), "")
	write(t, filepath.Join(tmp, "plain", "bad", ".sablier-project"), "../../etc")

	cases := map[string]string{
		filepath.Join(sablier, "internal", "cli"): "github.com/espcaa/sablier",
		local: "local-repo",
		filepath.Join(mono, "apps", "web", "src"): "acme-web",
		filepath.Join(mono, "apps", "cli"):        "acme-cli",
		filepath.Join(mono, "apps"):               "github.com/acme/mono",
		both:                                      "from-sablier",
		filepath.Join(tmp, "notes"):               "notes",
		filepath.Join(tmp, "plain", "bad"):        "bad",
		"":                                        "",
	}
	for dir, want := range cases {
		if got := Detect(dir); got != want {
			t.Errorf("Detect(%s) = %q, want %q", dir, got, want)
		}
	}
}

func TestWorktree(t *testing.T) {
	tmp := t.TempDir()
	main := filepath.Join(tmp, "main")
	repo(t, main, "[remote \"origin\"]\n\turl = https://github.com/espcaa/sablier.git\n")
	write(t, filepath.Join(main, ".git", "worktrees", "feat", "commondir"), "../..\n")

	wt := filepath.Join(tmp, "feat")
	write(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "feat")+"\n")

	if got := Detect(wt); got != "github.com/espcaa/sablier" {
		t.Fatalf("got %q", got)
	}
}

func TestFirstRemoteWithoutOrigin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	repo(t, dir, "[remote \"upstream\"]\n\turl = https://gitlab.com/a/b\n[remote \"fork\"]\n\turl = https://github.com/c/d\n")
	if got := Detect(dir); got != "gitlab.com/a/b" {
		t.Fatalf("got %q", got)
	}
}

func TestScannerErrorsAreNotIgnored(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	repo(t, dir, "[remote \"origin\"]\n\turl = https://github.com/acme/sablier.git\n"+strings.Repeat("x", 70*1024)+"\n")
	if got := originRemote(dir); got != "" {
		t.Fatalf("originRemote read past a scanner error: %q", got)
	}

	write(t, filepath.Join(dir, ".sablier-project"), strings.Repeat("x", 70*1024)+"\n")
	if name, ok := projectFile(filepath.Join(dir, ".sablier-project")); ok {
		t.Fatalf("projectFile accepted a line it could not scan: %q", name)
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:espcaa/sablier.git":             "github.com/espcaa/sablier",
		"https://github.com/espcaa/sablier":             "github.com/espcaa/sablier",
		"https://user:token@GitHub.com/espcaa/sablier/": "github.com/espcaa/sablier",
		"ssh://git@github.com:22/espcaa/sablier.git":    "github.com/espcaa/sablier",
		"git@gitlab.example.org:group/sub/project.git":  "gitlab.example.org/group/sub/project",
		"/Users/alice/repos/sablier.git":                "",
		"file:///Users/alice/repos/sablier":             "",
		"../other":                                      "",
		"":                                              "",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestName(t *testing.T) {
	for id, want := range map[string]string{"github.com/espcaa/sablier": "sablier", "notes": "notes", "my project": "my project"} {
		if got := Name(id); got != want {
			t.Errorf("Name(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"sablier": true, "my project": true, "web-app_2": true, "café": true, ".dotfiles": true,
		"/a/b/sablier": false, "a/b": false, `C:\code\sablier`: false, "": false, ".": false, "..": false,
		"tab\there": false, "new\nline": false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}
