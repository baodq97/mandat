package task

import (
	"encoding/json"
	"errors"
	"testing"
)

// validTask builds a fully populated, dispatchable TaskContract that every case
// below mutates exactly one field out of, so each case isolates one violation.
func validTask() TaskContract {
	var tc TaskContract
	tc.ID = "ado-baodo0220-42"
	tc.TrackerRef = TrackerRef{System: TrackerAzureDevOps, Org: "baodo0220", Project: "mandat-dogfood", WorkItemID: "42", URL: "https://dev.azure.com/baodo0220/mandat-dogfood/_workitems/edit/42"}
	tc.Type = TypeDevTask
	tc.Title = "Add the version subcommand"
	tc.Acceptance = "mandat version prints the build version and exits 0"
	tc.Refs = []string{}
	tc.State = StateQueued
	tc.Role = "dev"
	tc.Remit = Remit{Repo: "mandat", BaseBranch: "main", Paths: []string{"cmd/mandat/", "internal/buildinfo/"}}
	tc.AssignedTo = "agent-user-dev-01@baotest.onmicrosoft.com"
	tc.SchemaVersion = SchemaVersion
	return tc
}

func marshalTask(t *testing.T, tc TaskContract) []byte {
	t.Helper()
	data, err := json.Marshal(tc)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return data
}

func hasField(verrs ValidationErrors, path string) bool {
	for _, fe := range verrs {
		if fe.Path == path {
			return true
		}
	}
	return false
}

func TestValidate_ConstructedContract(t *testing.T) {
	t.Parallel()

	tc := validTask()
	if err := tc.Validate(); err != nil {
		t.Fatalf("validTask().Validate() = %v, want nil", err)
	}
}

func TestParse_Valid(t *testing.T) {
	t.Parallel()

	tc, err := Parse(marshalTask(t, validTask()))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if tc.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", tc.SchemaVersion, SchemaVersion)
	}
	if tc.State != StateQueued {
		t.Errorf("State = %q, want %q", tc.State, StateQueued)
	}
	if tc.Type != TypeDevTask {
		t.Errorf("Type = %q, want %q", tc.Type, TypeDevTask)
	}
	if tc.TrackerRef.System != TrackerAzureDevOps {
		t.Errorf("TrackerRef.System = %q, want %q", tc.TrackerRef.System, TrackerAzureDevOps)
	}
}

func TestParse_RejectsInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(tc *TaskContract)
		wantField string
	}{
		{name: "missing id", mutate: func(tc *TaskContract) { tc.ID = "" }, wantField: "id"},
		{name: "missing tracker_ref.system", mutate: func(tc *TaskContract) { tc.TrackerRef.System = "" }, wantField: "tracker_ref.system"},
		{name: "bad tracker_ref.system enum", mutate: func(tc *TaskContract) { tc.TrackerRef.System = "github" }, wantField: "tracker_ref.system"},
		{name: "missing tracker_ref.org", mutate: func(tc *TaskContract) { tc.TrackerRef.Org = "" }, wantField: "tracker_ref.org"},
		{name: "missing tracker_ref.project", mutate: func(tc *TaskContract) { tc.TrackerRef.Project = "" }, wantField: "tracker_ref.project"},
		{name: "missing tracker_ref.work_item_id", mutate: func(tc *TaskContract) { tc.TrackerRef.WorkItemID = "" }, wantField: "tracker_ref.work_item_id"},
		{name: "missing tracker_ref.url", mutate: func(tc *TaskContract) { tc.TrackerRef.URL = "" }, wantField: "tracker_ref.url"},
		{name: "missing type", mutate: func(tc *TaskContract) { tc.Type = "" }, wantField: "type"},
		{name: "bad type enum", mutate: func(tc *TaskContract) { tc.Type = "bug" }, wantField: "type"},
		{name: "missing title", mutate: func(tc *TaskContract) { tc.Title = "" }, wantField: "title"},
		{name: "missing acceptance", mutate: func(tc *TaskContract) { tc.Acceptance = "" }, wantField: "acceptance"},
		{name: "missing state", mutate: func(tc *TaskContract) { tc.State = "" }, wantField: "state"},
		{name: "bad state enum", mutate: func(tc *TaskContract) { tc.State = "parked" }, wantField: "state"},
		{name: "missing role", mutate: func(tc *TaskContract) { tc.Role = "" }, wantField: "role"},
		{name: "missing remit.repo", mutate: func(tc *TaskContract) { tc.Remit.Repo = "" }, wantField: "remit.repo"},
		{name: "missing remit.base_branch", mutate: func(tc *TaskContract) { tc.Remit.BaseBranch = "" }, wantField: "remit.base_branch"},
		{name: "empty remit.paths", mutate: func(tc *TaskContract) { tc.Remit.Paths = nil }, wantField: "remit.paths"},
		{name: "empty remit.paths entry", mutate: func(tc *TaskContract) { tc.Remit.Paths = []string{""} }, wantField: "remit.paths[0]"},
		{name: "missing assigned_to", mutate: func(tc *TaskContract) { tc.AssignedTo = "" }, wantField: "assigned_to"},
		{name: "wrong schema_version", mutate: func(tc *TaskContract) { tc.SchemaVersion = 2 }, wantField: "schema_version"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tc := validTask()
			tt.mutate(&tc)

			_, err := Parse(marshalTask(t, tc))
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

func TestParse_MalformedJSON(t *testing.T) {
	t.Parallel()

	_, err := Parse([]byte(`{"id":`))
	if err == nil {
		t.Fatal("Parse() on malformed JSON: error = nil, want non-nil")
	}
	var verrs ValidationErrors
	if errors.As(err, &verrs) {
		t.Errorf("Parse() on malformed JSON returned ValidationErrors %v, want a plain decode error", verrs)
	}
}
