// Package helm tests the Helm chart in deploy/helm/vx with the helm CLI:
// lint and template only, never a cluster. Without helm on PATH (or in
// VX_HELM) the tests skip.
package helm

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func helmBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("VX_HELM"); p != "" {
		return p
	}
	p, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed; install it (or set VX_HELM) to lint and render the chart")
	}
	return p
}

func helm(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(helmBin(t), args...)
	cmd.Env = append(os.Environ(), "HELM_CACHE_HOME="+t.TempDir(), "HELM_CONFIG_HOME="+t.TempDir(), "HELM_DATA_HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestHelmLint(t *testing.T) {
	out, err := helm(t, "lint", "--strict", "vx", "--set", "database.existingSecret=vx-db")
	if err != nil {
		t.Fatalf("helm lint: %v\n%s", err, out)
	}
}

func TestHelmTemplate(t *testing.T) {
	out, err := helm(t, "template", "vx", "vx",
		"--set", "database.existingSecret=vx-db",
		"--set", "secrets.existingSecret=vx-secrets",
		"--set", "publicURL=https://vx.example.com",
		"--set", "ingress.enabled=true",
		"--set", "metrics.serviceMonitor.enabled=true",
		"--set", "blobStore.url=s3://bucket/vx?region=auto",
		"--set", "blobStore.existingSecret=vx-s3",
		"--set", "rateLimits.signin=10/m",
		"--set", "quotas.stored=20GB",
		"--set", "auditRetention=365d")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	for _, want := range []string{
		"kind: Deployment", "kind: Service", "kind: Ingress", "kind: PersistentVolumeClaim", "kind: ServiceMonitor",
		"name: VX_DATABASE_URL", "name: vx-db", "name: VX_METRICS_TOKEN", "name: VX_BLOB_URL",
		"name: VX_RATE_SIGNIN", `value: "10/m"`, "name: VX_QUOTA_STORED", "name: VX_AUDIT_RETENTION",
		"path: /healthz", "startupProbe:", "readOnlyRootFilesystem: true", "type: Recreate",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered chart lacks %q", want)
		}
	}
	if strings.Contains(out, "kind: Secret\n") {
		t.Error("the chart made a database Secret although database.existingSecret is set")
	}

	// Without a database the chart refuses to render, saying what to set.
	out, err = helm(t, "template", "vx", "vx")
	if err == nil || !strings.Contains(out, "database.existingSecret") {
		t.Fatalf("helm template without a database: err=%v\n%s", err, out)
	}
}
