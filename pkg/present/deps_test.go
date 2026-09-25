package present_test

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestPresentImportsNoRuntimePackage(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v: %s", err, stderr.String())
	}
	for _, dependency := range strings.Split(string(out), "\n") {
		for _, forbidden := range []string{
			"github.com/looprig/harness/internal/",
			"github.com/looprig/harness/pkg/rig",
			"github.com/looprig/harness/pkg/command",
			"github.com/looprig/harness/pkg/event",
		} {
			if strings.HasPrefix(dependency, forbidden) {
				t.Fatalf("present has forbidden dependency %q", dependency)
			}
		}
	}
}
