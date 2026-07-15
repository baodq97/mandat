package result

import (
	"errors"
	"testing"
)

func hasField(verrs ValidationErrors, path string) bool {
	for _, fe := range verrs {
		if fe.Path == path {
			return true
		}
	}
	return false
}

func TestParse_Valid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		json string
	}{
		{name: "completed with full artifact", json: `{"schema_version":1,"task_id":"t-1","status":"completed","artifacts":[{"repo":"mandat","branch":"dev/t-1","pr_url":"https://dev.azure.com/o/p/_git/mandat/pullrequest/7"}]}`},
		{name: "completed with repo and branch only", json: `{"schema_version":1,"task_id":"t-1","status":"completed","artifacts":[{"repo":"mandat","branch":"dev/t-1"}]}`},
		{name: "needs_human with reason", json: `{"schema_version":1,"task_id":"t-1","status":"needs_human","reason":"acceptance criteria are ambiguous"}`},
		{name: "failed with reason", json: `{"schema_version":1,"task_id":"t-1","status":"failed","reason":"the gate re-run stayed red"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rc, err := Parse([]byte(tt.json))
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}
			if rc.SchemaVersion != SchemaVersion {
				t.Errorf("SchemaVersion = %d, want %d", rc.SchemaVersion, SchemaVersion)
			}
		})
	}
}

func TestParse_RejectsSchemaViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		json      string
		wantField string
	}{
		{name: "completed with zero artifacts", json: `{"schema_version":1,"task_id":"t-1","status":"completed"}`, wantField: "artifacts"},
		{name: "completed with empty artifacts", json: `{"schema_version":1,"task_id":"t-1","status":"completed","artifacts":[]}`, wantField: "artifacts"},
		{name: "completed artifact missing repo", json: `{"schema_version":1,"task_id":"t-1","status":"completed","artifacts":[{"branch":"dev/t-1"}]}`, wantField: "artifacts[0].repo"},
		{name: "completed artifact missing branch", json: `{"schema_version":1,"task_id":"t-1","status":"completed","artifacts":[{"repo":"mandat"}]}`, wantField: "artifacts[0].branch"},
		{name: "needs_human without reason", json: `{"schema_version":1,"task_id":"t-1","status":"needs_human"}`, wantField: "reason"},
		{name: "failed without reason", json: `{"schema_version":1,"task_id":"t-1","status":"failed"}`, wantField: "reason"},
		{name: "missing schema_version", json: `{"task_id":"t-1","status":"needs_human","reason":"held"}`, wantField: "schema_version"},
		{name: "wrong schema_version", json: `{"schema_version":2,"task_id":"t-1","status":"needs_human","reason":"held"}`, wantField: "schema_version"},
		{name: "missing task_id", json: `{"schema_version":1,"status":"needs_human","reason":"held"}`, wantField: "task_id"},
		{name: "missing status", json: `{"schema_version":1,"task_id":"t-1","reason":"held"}`, wantField: "status"},
		{name: "unknown status enum", json: `{"schema_version":1,"task_id":"t-1","status":"done","reason":"held"}`, wantField: "status"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse([]byte(tt.json))
			if err == nil {
				t.Fatalf("Parse() error = nil, want a violation naming %q", tt.wantField)
			}
			var verrs ValidationErrors
			if !errors.As(err, &verrs) {
				t.Fatalf("Parse() error type = %T, want ValidationErrors", err)
			}
			if !hasField(verrs, tt.wantField) {
				t.Errorf("ValidationErrors = %v, want one naming field %q", verrs, tt.wantField)
			}
		})
	}
}

func TestParse_RejectsUnknownAndMalformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		json string
	}{
		// The extra "needs_human" key doubles as proof there is no needs_human
		// boolean field to disagree with the status enum: additionalProperties is
		// false, so it is rejected at decode, not silently absorbed.
		{name: "unknown top-level property", json: `{"schema_version":1,"task_id":"t-1","status":"needs_human","reason":"held","needs_human":true}`},
		{name: "unknown artifact property", json: `{"schema_version":1,"task_id":"t-1","status":"completed","artifacts":[{"repo":"mandat","branch":"b","extra":1}]}`},
		{name: "malformed json", json: `{"schema_version":1,`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse([]byte(tt.json))
			if err == nil {
				t.Fatalf("Parse() error = nil, want a decode rejection")
			}
			var verrs ValidationErrors
			if errors.As(err, &verrs) {
				t.Errorf("Parse() returned ValidationErrors %v, want a plain decode error", verrs)
			}
		})
	}
}

func TestResultPathConstants(t *testing.T) {
	t.Parallel()

	if Path != ".mandat/result.json" {
		t.Errorf("Path = %q, want %q", Path, ".mandat/result.json")
	}
	if EnvVar != "MANDAT_RESULT_PATH" {
		t.Errorf("EnvVar = %q, want %q", EnvVar, "MANDAT_RESULT_PATH")
	}
}
