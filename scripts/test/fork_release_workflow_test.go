package scripts

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestForkReleaseKeepsPublicationIsolated(t *testing.T) {
	workflow := readWorkflow(t, "fork-test-release.yml")
	var config struct {
		On          map[string]any    `yaml:"on"`
		Permissions map[string]string `yaml:"permissions"`
		Jobs        map[string]struct {
			If          string            `yaml:"if"`
			Needs       []string          `yaml:"needs"`
			Permissions map[string]string `yaml:"permissions"`
			Steps       []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(workflow), &config); err != nil {
		t.Fatal(err)
	}
	if len(config.On) != 1 || config.On["workflow_dispatch"] == nil {
		t.Fatal("fork releases must require explicit workflow dispatch")
	}
	if config.Permissions["contents"] != "read" {
		t.Fatal("build jobs must not have publication permission")
	}
	if !strings.Contains(config.Jobs["source"].If, "github.repository != 'uvwt/agentdock'") {
		t.Fatal("fork workflow must not publish from the official repository")
	}
	for name, job := range config.Jobs {
		if name != "publish" && job.Permissions["contents"] == "write" {
			t.Fatalf("build job %s has publication permission", name)
		}
	}
	publish := config.Jobs["publish"]
	if publish.Permissions["contents"] != "write" {
		t.Fatal("publish job needs contents: write")
	}
	for _, required := range []string{"source", "windows", "linux"} {
		found := false
		for _, dependency := range publish.Needs {
			found = found || dependency == required
		}
		if !found {
			t.Fatalf("publication can bypass %s validation", required)
		}
	}
	var commands string
	for _, step := range publish.Steps {
		commands += step.Run + "\n"
	}
	for _, required := range []string{"sha256sum --check", "--prerelease", "--latest=false", "--target", "SOURCE_COMMIT"} {
		if !strings.Contains(commands, required) {
			t.Fatalf("publication is missing %s", required)
		}
	}
	for _, forbidden := range []string{"secrets.", "download.nexusdock.co", "docker/login-action", "gh release delete", "--clobber"} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("fork release must not use official services or replace releases: %s", forbidden)
		}
	}
}
