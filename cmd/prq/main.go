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
	if err != nil {
		return "", "", "", err
	}
	return r.Host, r.Owner, r.Name, nil
}

func currentBranch() (string, error) {
	gitExe, err := safeexec.LookPath("git")
	if err != nil {
		return "", err
	}
	out, err := exec.Command(gitExe, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("cannot determine current branch (detached HEAD?)")
	}
	return strings.TrimSpace(string(out)), nil
}
