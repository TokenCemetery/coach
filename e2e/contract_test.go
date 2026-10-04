package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ozontech/allure-go/pkg/allure"
	"github.com/ozontech/allure-go/pkg/framework/provider"
	"github.com/ozontech/allure-go/pkg/framework/runner"
)

// environment is created on first use so unit tests of the harness run
// without FFmpeg or a build.
type environment struct {
	contract *Contract
	suite    *Suite
	shared   *Instance
	dir      string
}

var (
	envOnce sync.Once
	env     *environment
	envErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if env != nil {
		if env.shared != nil {
			env.shared.Stop()
		}
		_ = os.RemoveAll(env.dir)
	}
	os.Exit(code)
}

func getEnv(t *testing.T) *environment {
	t.Helper()
	envOnce.Do(func() {
		e := &environment{}
		if e.contract, envErr = LoadContract(contractPath); envErr != nil {
			return
		}
		if e.dir, envErr = os.MkdirTemp("", "coach-e2e-"); envErr != nil {
			return
		}
		env = e
		if e.suite, envErr = NewSuite(e.dir); envErr != nil {
			return
		}
		e.shared, envErr = e.suite.Start()
	})
	if envErr != nil {
		t.Fatal(envErr)
	}
	return env
}

// TestContract runs one test per contract operation. Read-only operations
// share an instance; each mutating operation gets its own.
func TestContract(t *testing.T) {
	e := getEnv(t)
	for _, op := range e.contract.Operations {
		runner.Run(t, op.ID, func(pt provider.T) {
			pt.Parallel()
			title := op.Summary
			if title == "" {
				title = op.ID
			}
			pt.Title(title)
			pt.Description(op.Method + " " + op.Path)
			pt.Epic("Contract")
			pt.Feature(op.Tag)
			pt.Story(op.ID)
			inst := e.shared
			if op.Mutating() {
				var err error
				if inst, err = e.suite.Start(); err != nil {
					pt.Fatalf("start instance: %v", err)
				}
				defer inst.Stop()
			}
			check := &Check{}
			checkOperation(pt, e.contract, op, inst, check)
			Settle(pt, op.ID, check)
		})
	}
}

func checkOperation(pt provider.T, c *Contract, op Operation, inst *Instance, check *Check) {
	req := c.BuildRequest(op, inst.IDs)
	// Without a token first: a successful mutation (logout, shutdown) could
	// otherwise invalidate the second request.
	if op.Secured {
		pt.WithNewStep("Without token: expect 401", func(sCtx provider.StepCtx) {
			resp, err := inst.Send(req, false)
			attach(sCtx, req, resp, err)
			switch {
			case err != nil:
				check.Errorf("without token: %v", err)
			case resp.Status != 401:
				check.Errorf("without token: status %d, want 401", resp.Status)
			}
		})
	}
	pt.WithNewStep("With token: expect documented success", func(sCtx provider.StepCtx) {
		resp, err := inst.Send(req, true)
		attach(sCtx, req, resp, err)
		if err != nil {
			check.Errorf("with token: %v", err)
			return
		}
		checkSuccess(c, op, resp, check)
	})
}

// checkSuccess applies the contract to an authorized response. Operations
// documented with a JSON body must answer 200 with a matching body; the
// contract documents every bodiless success as 200, so any 2xx is accepted.
func checkSuccess(c *Contract, op Operation, resp Response, check *Check) {
	if op.Response == nil || op.Method == http.MethodHead {
		if resp.Status < 200 || resp.Status > 299 {
			check.Errorf("status %d, want 2xx: %s", resp.Status, snippet(resp.Body))
		}
		return
	}
	if resp.Status != 200 {
		check.Errorf("status %d, want 200: %s", resp.Status, snippet(resp.Body))
		return
	}
	if !strings.HasPrefix(resp.ContentType, "application/json") {
		check.Errorf("Content-Type %q, want application/json", resp.ContentType)
		return
	}
	d := json.NewDecoder(bytes.NewReader(resp.Body))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		check.Errorf("invalid JSON: %v", err)
		return
	}
	for _, p := range c.Validate(v, op.Response) {
		check.Errorf("schema: %s", p)
	}
}

func attach(sCtx provider.StepCtx, req Request, resp Response, err error) {
	sCtx.WithNewAttachment("request", allure.Text, fmt.Appendf(nil, "%s %s\n\n%s", req.Method, req.URL(""), req.Body))
	if err != nil {
		sCtx.WithNewAttachment("error", allure.Text, []byte(err.Error()))
		return
	}
	sCtx.WithNewAttachment("response", allure.Text, fmt.Appendf(nil, "%d %s\n\n%s", resp.Status, resp.ContentType, snippet(resp.Body)))
}

func snippet(b []byte) string {
	const limit = 4096
	if len(b) > limit {
		return string(b[:limit]) + "…"
	}
	return string(b)
}

// TestKnownFailuresExist keeps knownFailures free of keys that no longer name
// a contract operation or behavior check.
func TestKnownFailuresExist(t *testing.T) {
	c, err := LoadContract(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, op := range c.Operations {
		keys[op.ID] = true
	}
	for _, b := range behaviors {
		keys[b.Key] = true
	}
	for k := range knownFailures {
		if !keys[k] {
			t.Errorf("knownFailures has unknown key %q", k)
		}
	}
}
