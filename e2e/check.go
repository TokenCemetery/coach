package e2e

import (
	"fmt"
	"strings"

	"github.com/ozontech/allure-go/pkg/framework/provider"
)

// Check collects the problems found by one test so the outcome can be
// settled against knownFailures instead of failing immediately.
type Check struct {
	problems []string
}

// Errorf records a problem.
func (c *Check) Errorf(format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

// Failed reports whether any problem was recorded.
func (c *Check) Failed() bool { return len(c.problems) > 0 }

// Settle reports the check strictly, like pytest's xfail(strict=True):
//   - listed in knownFailures and failing: skipped with the reason;
//   - listed and passing: fails, so the stale entry gets removed;
//   - not listed and failing: fails as a regression.
func Settle(t provider.T, key string, c *Check) {
	kf, known := knownFailures[key]
	if known {
		t.SetIssue(kf.Link())
	}
	switch {
	case known && c.Failed():
		t.Skipf("known failure (#%d): %s\n%s", kf.Issue, kf.Reason, strings.Join(c.problems, "\n"))
	case known:
		t.Errorf("%s passes but is listed in knownFailures (#%d: %s); remove the entry", key, kf.Issue, kf.Reason)
	default:
		for _, p := range c.problems {
			t.Error(p)
		}
	}
}
