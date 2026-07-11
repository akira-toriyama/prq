// prq answers "why is this PR blocked?" in one call: it fetches a pull
// request's full actionable state over GraphQL (reusing gh CLI auth) and
// synthesizes it into a compact JSON verdict for AI coding agents.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/repository"
	"github.com/cli/safeexec"

	"github.com/akira-toriyama/prq/internal/gh"
)

func main() {
	d := deps{
		client:        gh.NewClient,
		currentRepo:   currentRepo,
		currentBranch: currentBranch,
		sleep:         time.Sleep,
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, d))
}

func currentRepo() (string, string, string, error) {
	r, err := repository.Current()
	if err == nil {
		return r.Host, r.Owner, r.Name, nil
	}
	// Fallback: go-gh only recognizes literal known hosts, so SSH config
	// aliases like "github.com.work:owner/repo.git" fail Current(). Parse
	// origin ourselves and accept any host that names github.com.
	if host, owner, name, ferr := originRepo(); ferr == nil {
		return host, owner, name, nil
	}
	return "", "", "", err // go-gh's error text is the more descriptive one
}

func originRepo() (host, owner, name string, err error) {
	gitExe, err := safeexec.LookPath("git")
	if err != nil {
		return "", "", "", err
	}
	// #nosec G204 -- fixed program (safeexec-resolved `git`) with prq-controlled
	// subcommands; no user input reaches the argv.
	out, err := exec.Command(gitExe, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", "", "", fmt.Errorf("no origin remote")
	}
	return parseOriginURL(strings.TrimSpace(string(out)))
}

// parseOriginURL extracts (host, owner, name) from the common git remote URL
// forms: scp-style (git@HOST:owner/repo.git, HOST:owner/repo.git), ssh://,
// and https://. Only hosts that are — or alias — github.com are accepted
// ("github.com", "github.com.personal", …); anything else is not ours to
// guess.
func parseOriginURL(url string) (host, owner, name string, err error) {
	s := url
	for _, scheme := range []string{"ssh://", "git+ssh://", "git://", "https://", "http://"} {
		if rest, ok := strings.CutPrefix(s, scheme); ok {
			s = rest
			break
		}
	}
	if _, rest, ok := strings.Cut(s, "@"); ok {
		s = rest
	}
	var path string
	if colon := strings.IndexByte(s, ':'); colon >= 0 && (strings.IndexByte(s, '/') == -1 || colon < strings.IndexByte(s, '/')) {
		host, path = s[:colon], s[colon+1:]
	} else {
		var ok bool
		host, path, ok = strings.Cut(s, "/")
		if !ok {
			return "", "", "", fmt.Errorf("cannot parse remote url %q", url)
		}
	}
	if host != "github.com" && !strings.HasPrefix(host, "github.com.") {
		return "", "", "", fmt.Errorf("host %q is not github.com", host)
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[len(parts)-2] == "" || parts[len(parts)-1] == "" {
		return "", "", "", fmt.Errorf("cannot parse owner/repo from %q", url)
	}
	return "github.com", parts[len(parts)-2], parts[len(parts)-1], nil
}

func currentBranch() (string, error) {
	gitExe, err := safeexec.LookPath("git")
	if err != nil {
		return "", err
	}
	// #nosec G204 -- fixed program (safeexec-resolved `git`) with prq-controlled
	// subcommands; no user input reaches the argv.
	out, err := exec.Command(gitExe, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("cannot determine current branch (detached HEAD?)")
	}
	return strings.TrimSpace(string(out)), nil
}
