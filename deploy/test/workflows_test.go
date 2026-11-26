package test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

type concurrencyConfig struct {
	Group  string `yaml:"group"`
	Cancel any    `yaml:"cancel-in-progress"`
}

type workflow struct {
	On          map[string]any    `yaml:"on"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency concurrencyConfig `yaml:"concurrency"`
	Jobs        map[string]job    `yaml:"jobs"`
}

type job struct {
	Needs       []string          `yaml:"needs"`
	If          string            `yaml:"if"`
	Uses        string            `yaml:"uses"`
	Permissions map[string]string `yaml:"permissions"`
	Concurrency concurrencyConfig `yaml:"concurrency"`
	Strategy    struct {
		Matrix struct {
			Arch []string `yaml:"arch"`
		} `yaml:"matrix"`
	} `yaml:"strategy"`
	Steps []step `yaml:"steps"`
}

type step struct {
	ID   string            `yaml:"id"`
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	If   string            `yaml:"if"`
	With map[string]any    `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

func readWorkflow(t *testing.T, file string) workflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", file))
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		t.Fatal(err)
	}
	return w
}

func pushFilter(t *testing.T, w workflow) (branches, tags []string) {
	t.Helper()
	data, err := yaml.Marshal(w.On["push"])
	if err != nil {
		t.Fatal(err)
	}
	var filter struct {
		Branches []string `yaml:"branches"`
		Tags     []string `yaml:"tags"`
	}
	if err := yaml.Unmarshal(data, &filter); err != nil {
		t.Fatal(err)
	}
	return filter.Branches, filter.Tags
}

func findStep(t *testing.T, j job, name string) step {
	t.Helper()
	for _, s := range j.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("missing step %q", name)
	return step{}
}

func TestCIRequiresRealChecksAndAllResults(t *testing.T) {
	w := readWorkflow(t, "ci.yml")
	if _, ok := w.On["push"]; ok {
		t.Fatal("CI must not rerun on branch pushes after a PR merge")
	}
	for _, event := range []string{"pull_request", "workflow_dispatch", "workflow_call"} {
		if _, ok := w.On[event]; !ok {
			t.Errorf("CI missing trigger %s", event)
		}
	}
	if len(w.On) != 3 {
		t.Fatalf("unexpected CI triggers: %v", w.On)
	}
	if !reflect.DeepEqual(w.Permissions, map[string]string{"contents": "read"}) {
		t.Fatalf("CI must have read-only contents permission: %v", w.Permissions)
	}
	if w.Concurrency.Group != "${{ github.workflow }}-${{ github.ref }}" ||
		w.Concurrency.Cancel != "${{ github.event_name == 'pull_request' }}" {
		t.Fatalf("CI must cancel only superseded pull request runs: %+v", w.Concurrency)
	}
	if !reflect.DeepEqual(w.Jobs["container"].Strategy.Matrix.Arch, []string{"amd64", "arm64"}) {
		t.Fatal("both architectures must be checked")
	}
	build := findStep(t, w.Jobs["container"], "Build and load single-platform image")
	if build.If != "" || !strings.HasPrefix(build.Uses, "docker/build-push-action@") || build.With["push"] != false ||
		build.With["load"] != true || build.With["platforms"] != "linux/${{ matrix.arch }}" || build.With["build-args"] != nil {
		t.Fatalf("CI must build and load, not push, each architecture: %+v", build)
	}
	smoke := findStep(t, w.Jobs["container"], "Exercise image")
	if smoke.If != "" || !strings.Contains(smoke.Run, "python3 deploy/test/docker_smoke.py") {
		t.Fatal("container check must run the actual image smoke test")
	}
	qemu := findStep(t, w.Jobs["container"], "Set up ARM64 emulation")
	if !strings.HasPrefix(qemu.Uses, "docker/setup-qemu-action@") ||
		qemu.With["image"] != "tonistiigi/binfmt:qemu-v10.0.4" || !strings.Contains(qemu.If, "arm64") {
		t.Fatal("ARM64 execution must explicitly use the selected emulator")
	}
	gate := w.Jobs["gate"]
	if gate.If != "${{ always() }}" || !reflect.DeepEqual(gate.Needs, []string{"backend", "frontend", "container"}) {
		t.Fatal("gate must inspect every required job even on failure/skip")
	}
	gateStep := findStep(t, gate, "Require every check to succeed")
	if gateStep.Run != "python3 deploy/ci_gate.py" ||
		gateStep.Env["BACKEND_RESULT"] != "${{ needs.backend.result }}" ||
		gateStep.Env["FRONTEND_RESULT"] != "${{ needs.frontend.result }}" ||
		gateStep.Env["CONTAINER_RESULT"] != "${{ needs.container.result }}" {
		t.Fatal("gate must pass all actual job results to the tested gate script")
	}
	for _, id := range []string{"backend", "frontend", "container", "gate"} {
		j := w.Jobs[id]
		if len(j.Permissions) != 0 || (id != "gate" && j.If != "") {
			t.Errorf("CI job %s must not elevate permissions or skip its checks", id)
		}
	}
	if findStep(t, w.Jobs["backend"], "Test deployment policies").Run != "python3 -m unittest discover -s deploy/test -p 'test_*.py'" {
		t.Fatal("policy and gate tests must execute in CI")
	}
	goChecks := findStep(t, w.Jobs["backend"], "Vet and test Go").Run
	if !strings.Contains(goChecks, "go mod tidy -diff") || !strings.Contains(goChecks, "go vet ./...") || !strings.Contains(goChecks, "go test -p 2 ./...") {
		t.Fatal("CI must check module metadata and run actual Go vet and tests")
	}
	frontendCommands := ""
	for _, s := range w.Jobs["frontend"].Steps {
		frontendCommands += s.Run + "\n"
	}
	for _, command := range []string{"npm --prefix web ci", "npm --prefix web run check", "npm --prefix web run build"} {
		if !strings.Contains(frontendCommands, command) {
			t.Errorf("missing real frontend command %s", command)
		}
	}
}

func TestPublicationNeedsChecksAndPolicyAndIsOnlyWriter(t *testing.T) {
	w := readWorkflow(t, "publish.yml")
	if len(w.On) != 2 {
		t.Fatalf("unexpected publication triggers: %v", w.On)
	}
	for _, event := range []string{"push", "workflow_dispatch"} {
		if _, ok := w.On[event]; !ok {
			t.Errorf("missing publication event %s", event)
		}
	}
	if !reflect.DeepEqual(w.Permissions, map[string]string{"contents": "read"}) {
		t.Fatal("workflow permissions must be read-only by default")
	}
	branches, tags := pushFilter(t, w)
	if len(branches) != 0 || !reflect.DeepEqual(tags, []string{"v*"}) {
		t.Fatal("automatic publication must be triggered only by v-prefixed tags")
	}
	if w.Jobs["policy"].If != "" || w.Jobs["checks"].If != "" {
		t.Fatal("policy and full CI must not be conditionally skipped")
	}
	if w.Jobs["checks"].Uses != "./.github/workflows/ci.yml" ||
		!reflect.DeepEqual(w.Jobs["checks"].Permissions, map[string]string{"contents": "read"}) {
		t.Fatal("publication must call the full read-only CI")
	}
	pub := w.Jobs["publish"]
	if !reflect.DeepEqual(pub.Needs, []string{"policy", "checks"}) ||
		pub.If != "${{ needs.policy.result == 'success' && needs.checks.result == 'success' }}" {
		t.Fatal("publication must require successful policy and CI, never failure/skip")
	}
	if !reflect.DeepEqual(pub.Permissions, map[string]string{"contents": "read", "packages": "write"}) ||
		pub.Concurrency.Group != "ghcr-${{ github.repository }}" || pub.Concurrency.Cancel != false {
		t.Fatal("only the serialized publish job may write packages")
	}
	for id, j := range w.Jobs {
		if id != "publish" && j.Permissions["packages"] == "write" {
			t.Errorf("job %s must not write packages", id)
		}
	}
	for _, pair := range []struct{ job, step string }{{"policy", "Validate publication policy"}, {"publish", "Revalidate publication policy"}} {
		steps := w.Jobs[pair.job].Steps
		if len(steps) == 0 || !strings.HasPrefix(steps[0].Uses, "actions/checkout@") ||
			steps[0].With["ref"] != "${{ github.sha }}" || fmt.Sprint(steps[0].With["fetch-depth"]) != "0" ||
			steps[0].With["persist-credentials"] != false {
			t.Fatal("both policy checks need a full checkout at the event commit")
		}
		s := findStep(t, w.Jobs[pair.job], pair.step)
		if s.Run != "python3 deploy/ghcr_policy.py" ||
			s.Env["DEFAULT_BRANCH"] != "${{ github.event.repository.default_branch }}" ||
			s.Env["DEV_TAG"] != "${{ inputs.dev_tag }}" {
			t.Fatal("both policy checks must execute the tested script with inputs in env")
		}
	}
	build := findStep(t, pub, "Build and publish both architectures")
	if !strings.HasPrefix(build.Uses, "docker/build-push-action@") || build.With["push"] != true ||
		build.With["platforms"] != "linux/amd64,linux/arm64" || build.With["tags"] != "${{ steps.policy.outputs.tags }}" ||
		build.With["build-args"] != nil {
		t.Fatal("publish must use validated tags, both platforms and Dockerfile build defaults")
	}
	policyIndex, loginIndex, buildIndex := -1, -1, -1
	for i, s := range pub.Steps {
		switch s.Name {
		case "Revalidate publication policy":
			policyIndex = i
			if s.ID != "policy" {
				t.Fatal("publication must use the revalidated policy outputs")
			}
		case "Log in to GHCR":
			loginIndex = i
			if !strings.HasPrefix(s.Uses, "docker/login-action@") || s.With["registry"] != "ghcr.io" ||
				s.With["password"] != "${{ secrets.GITHUB_TOKEN }}" {
				t.Fatal("GHCR login must use the built-in token")
			}
		case "Build and publish both architectures":
			buildIndex = i
		}
	}
	if policyIndex < 0 || loginIndex <= policyIndex || buildIndex <= loginIndex {
		t.Fatal("publication must revalidate before login and push")
	}
	for _, workflow := range []workflow{w, readWorkflow(t, "ci.yml")} {
		for id, j := range workflow.Jobs {
			for _, s := range j.Steps {
				if strings.Contains(s.Run, "${{") {
					t.Errorf("%s/%s interpolates an expression into shell", id, s.Name)
				}
				if strings.HasPrefix(s.Uses, "docker/login-action@") && id != "publish" {
					t.Errorf("%s/%s must not log in to a registry", id, s.Name)
				}
			}
		}
	}
}
