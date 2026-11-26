package test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	With        map[string]any    `yaml:"with"`
	Outputs     map[string]string `yaml:"outputs"`
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

func workflowCallFullInput(t *testing.T, w workflow) (typ string, def any) {
	t.Helper()
	data, err := yaml.Marshal(w.On["workflow_call"])
	if err != nil {
		t.Fatal(err)
	}
	var call struct {
		Inputs map[string]struct {
			Type    string `yaml:"type"`
			Default any    `yaml:"default"`
		} `yaml:"inputs"`
	}
	if err := yaml.Unmarshal(data, &call); err != nil {
		t.Fatal(err)
	}
	full, ok := call.Inputs["full"]
	if !ok {
		t.Fatal("workflow_call must declare boolean input 'full'")
	}
	return full.Type, full.Default
}

func parsePathFilters(t *testing.T, raw any) map[string][]string {
	t.Helper()
	text, ok := raw.(string)
	if !ok || text == "" {
		t.Fatalf("expected non-empty filters YAML string, got %T", raw)
	}
	var filters map[string][]string
	if err := yaml.Unmarshal([]byte(text), &filters); err != nil {
		t.Fatalf("invalid paths-filter YAML: %v", err)
	}
	return filters
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
	if typ, def := workflowCallFullInput(t, w); typ != "boolean" || def != false {
		t.Fatalf("workflow_call full input must be boolean defaulting to false: type=%q default=%v", typ, def)
	}
	if !reflect.DeepEqual(w.Permissions, map[string]string{"contents": "read", "pull-requests": "read"}) {
		t.Fatalf("CI must have read-only contents and pull-requests permissions: %v", w.Permissions)
	}
	if w.Concurrency.Group != "${{ github.workflow }}-${{ github.ref }}" ||
		w.Concurrency.Cancel != "${{ github.event_name == 'pull_request' }}" {
		t.Fatalf("CI must cancel only superseded pull request runs: %+v", w.Concurrency)
	}

	changes := w.Jobs["changes"]
	if changes.If != "" || len(changes.Needs) != 0 {
		t.Fatalf("changes job must always run first: %+v", changes)
	}
	for _, key := range []string{"backend", "frontend", "container"} {
		want := fmt.Sprintf("${{ (github.event_name != 'pull_request' || inputs.full == true) && 'true' || steps.filter.outputs.%s }}", key)
		if changes.Outputs[key] != want {
			t.Fatalf("changes output %s = %q, want %q", key, changes.Outputs[key], want)
		}
	}
	if changes.Outputs["other"] != "${{ steps.filter.outputs.other }}" {
		t.Fatalf("unexpected other output: %q", changes.Outputs["other"])
	}
	filterStep := findStep(t, changes, "Detect changed paths")
	if filterStep.ID != "filter" || filterStep.If != "${{ github.event_name == 'pull_request' && !inputs.full }}" ||
		filterStep.Uses != "dorny/paths-filter@v4" || filterStep.With["predicate-quantifier"] != "some-with-excludes" {
		t.Fatalf("unexpected paths-filter configuration: %+v", filterStep)
	}
	filters := parsePathFilters(t, filterStep.With["filters"])
	for group, required := range map[string][]string{
		"backend":  {"cmd/**", "internal/**", "config/**", "deploy/**", "Dockerfile", "Makefile", "go.mod", ".github/workflows/**"},
		"frontend": {"web/**", "THIRD_PARTY_NOTICES.md", "third_party/**", ".github/workflows/ci.yml"},
		"container": {
			"Dockerfile", ".dockerignore", "compose.yaml", "deploy/**", "cmd/ranking/**",
			"internal/storage/**", "internal/avatars/*.go", "internal/avatars/assets/catalog.json", "internal/avatars/assets/images/**",
			"config/models.json", "THIRD_PARTY_NOTICES.md", "third_party/**",
			"web/index.html", "web/tsconfig.json", "web/vite.config.ts",
		},
		"other": {"**", "!docs/**", "!README.md", "!README.zh.md", "!LICENSE", "!assets/**", "!.gitignore"},
	} {
		for _, pattern := range required {
			if !slices.Contains(filters[group], pattern) {
				t.Errorf("paths-filter group %s missing required pattern %q", group, pattern)
			}
		}
	}
	if slices.Contains(filters["container"], "internal/avatars/**") {
		t.Error("container filter must not match avatar provenance markdown files")
	}

	expectedConditions := map[string]string{
		"changes":   "",
		"backend":   "${{ needs.changes.outputs.backend == 'true' || needs.changes.outputs.other == 'true' }}",
		"frontend":  "${{ needs.changes.outputs.frontend == 'true' || needs.changes.outputs.other == 'true' }}",
		"container": "${{ needs.changes.outputs.container == 'true' }}",
		"gate":      "${{ always() }}",
	}
	for id, wantIf := range expectedConditions {
		j := w.Jobs[id]
		if len(j.Permissions) != 0 {
			t.Errorf("CI job %s must not elevate permissions: %v", id, j.Permissions)
		}
		if j.If != wantIf {
			t.Errorf("CI job %s if = %q, want %q", id, j.If, wantIf)
		}
		if (id == "backend" || id == "frontend" || id == "container") && !reflect.DeepEqual(j.Needs, []string{"changes"}) {
			t.Errorf("CI job %s must depend on changes: %v", id, j.Needs)
		}
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
	if gate.If != "${{ always() }}" || !reflect.DeepEqual(gate.Needs, []string{"changes", "backend", "frontend", "container"}) {
		t.Fatal("gate must inspect changes and every required job even on failure/skip")
	}
	gateStep := findStep(t, gate, "Require every check to succeed")
	if gateStep.Run != "python3 deploy/ci_gate.py" ||
		gateStep.Env["FULL_CHECK"] != "${{ github.event_name != 'pull_request' || inputs.full == true }}" ||
		gateStep.Env["CHANGES_RESULT"] != "${{ needs.changes.result }}" ||
		gateStep.Env["BACKEND_RESULT"] != "${{ needs.backend.result }}" ||
		gateStep.Env["FRONTEND_RESULT"] != "${{ needs.frontend.result }}" ||
		gateStep.Env["CONTAINER_RESULT"] != "${{ needs.container.result }}" {
		t.Fatal("gate must pass full-check mode and all actual job results to the tested gate script")
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
		w.Jobs["checks"].With["full"] != true ||
		!reflect.DeepEqual(w.Jobs["checks"].Permissions, map[string]string{"contents": "read", "pull-requests": "read"}) {
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
