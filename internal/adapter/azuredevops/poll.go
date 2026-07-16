package azuredevops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/baodq97/mandat/internal/task"
)

// repoTagPrefix marks the work-item tag that names the target repo-registry key.
// The skeleton carries the repo on a `repo:<key>` tag rather than a custom ADO
// field (RFC-0001 §Open questions defers the productionized per-item remit
// mapping — custom field vs naming convention — so this tag is the
// naming-convention stand-in and the single point to change when that resolves).
const repoTagPrefix = "repo:"

// Poll runs the WIQL query for the role's assigned work items, fetches each, and
// maps it to a task.TaskContract. Items whose repo is absent from the registry,
// whose assignment no longer matches, or that fail contract validation are
// skipped (recorded, not defaulted); the returned slice holds only dispatchable
// contracts. The adapter keeps no cursor, so re-polling the same item re-emits an
// identically-keyed contract (id derived from the tracker ref), and dedup is the
// dispatcher's journal check on that stable id (RFC-0001 AC-03).
func (a *Adapter) Poll(ctx context.Context) ([]task.TaskContract, error) {
	refs, err := a.queryAssigned(ctx)
	if err != nil {
		return nil, err
	}

	contracts := make([]task.TaskContract, 0, len(refs))
	for _, ref := range refs {
		// A WIQL result is only {id, url} references; the fields live behind a
		// second per-item read. A fetch failure fails the whole cycle rather than
		// silently dropping the item — the 30s re-poll retries, and partial-batch
		// tolerance is out of the skeleton's scope.
		wi, err := a.fetchWorkItem(ctx, ref.ID)
		if err != nil {
			return nil, err
		}
		if tc, ok := a.mapContract(ctx, wi); ok {
			contracts = append(contracts, tc)
		}
	}
	return contracts, nil
}

// queryAssigned posts the consent WIQL — work items assigned to the dev agent
// user — and returns the id/url references. The project scope comes from the URL
// path, so the WHERE clause carries only the assignment predicate (spec §4.2:
// consent stays in the tracker as assignment to an agent user).
func (a *Adapter) queryAssigned(ctx context.Context) ([]wiqlRef, error) {
	// WIQL string literals are single-quoted and escape an embedded quote by
	// doubling it. devAgentUserName is operator config, not attacker input, but the
	// escape keeps the query well-formed for any principal shape.
	assigned := strings.ReplaceAll(a.devAgentUserName, "'", "''")
	query := fmt.Sprintf("SELECT [System.Id] FROM WorkItems WHERE [System.AssignedTo] = '%s'", assigned)

	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, fmt.Errorf("azuredevops: encode wiql query: %w", err)
	}
	ep := a.endpoint(apiVersion, "wiql")
	var result wiqlResult
	if err := a.do(ctx, http.MethodPost, ep, jsonContentType, body, &result); err != nil {
		return nil, fmt.Errorf("azuredevops: wiql poll: %w", err)
	}
	return result.WorkItems, nil
}

func (a *Adapter) fetchWorkItem(ctx context.Context, id int) (*adoWorkItem, error) {
	ep := a.endpoint(apiVersion, "workitems", strconv.Itoa(id))
	var wi adoWorkItem
	if err := a.do(ctx, http.MethodGet, ep, "", nil, &wi); err != nil {
		return nil, fmt.Errorf("azuredevops: fetch work item %d: %w", id, err)
	}
	return &wi, nil
}

// mapContract turns one fetched work item into a validated TaskContract, or
// returns ok=false with a recorded skip. It is where RFC-0001's mapping rules
// land: state is always queued at creation, role and remit come from config
// (not any ADO field), and a repo-absent, reassigned, or malformed item is a
// skip rather than a dispatch (AC-4.5..AC-4.8).
func (a *Adapter) mapContract(ctx context.Context, wi *adoWorkItem) (task.TaskContract, bool) {
	workItemID := strconv.Itoa(wi.ID)

	// Belt-and-suspenders consent: WIQL already filters on assignment, but a
	// re-check on the fetched item keeps assignment to the dev agent user the
	// only path to a dispatch even if the query ever widens. UPNs are
	// case-insensitive, so compare fold-wise.
	if !strings.EqualFold(wi.Fields.AssignedTo.UniqueName, a.devAgentUserName) {
		a.skip(ctx, workItemID, "assigned_to no longer matches the dev agent user")
		return task.TaskContract{}, false
	}

	repoKey, ok := repoKeyFromTags(wi.Fields.Tags)
	if !ok {
		a.skip(ctx, workItemID, "no repo tag; cannot resolve the remit")
		return task.TaskContract{}, false
	}
	remit, err := a.remits.RemitDefaultsFor(repoKey)
	if err != nil {
		a.skip(ctx, workItemID, fmt.Sprintf("repo %q is not in the registry", repoKey))
		return task.TaskContract{}, false
	}

	tc := task.TaskContract{
		ID: fmt.Sprintf("ado-%s-%s", a.org, workItemID),
		TrackerRef: task.TrackerRef{
			System:     task.TrackerAzureDevOps,
			Org:        a.org,
			Project:    a.project,
			WorkItemID: workItemID,
			URL:        a.workItemURL(workItemID),
		},
		Type:       task.TypeDevTask,
		Title:      wi.Fields.Title,
		Acceptance: wi.Fields.AcceptanceCriteria,
		Refs:       []string{},
		State:      task.StateQueued,
		Role:       a.role,
		Remit: task.Remit{
			Repo:       remit.Repo,
			BaseBranch: remit.BaseBranch,
			Paths:      remit.Paths,
		},
		AssignedTo:    wi.Fields.AssignedTo.UniqueName,
		SchemaVersion: task.SchemaVersion,
	}

	// Validate before dispatch: a work item missing a required field (e.g. an
	// empty title or acceptance) is a recorded skip, never a spawned run
	// (RFC-0001 AC-09).
	if err := tc.Validate(); err != nil {
		a.skip(ctx, workItemID, fmt.Sprintf("contract failed validation: %v", err))
		return task.TaskContract{}, false
	}
	return tc, true
}

// skip records a work item the adapter declined to map. The append-only journal
// row AC-4.8/AC-08 calls for is written by the dispatcher that owns the journal
// plane; the adapter surfaces the reason on its injected logger rather than
// reaching across a plane boundary or fabricating a default contract.
func (a *Adapter) skip(ctx context.Context, workItemID, reason string) {
	a.logger.WarnContext(ctx, "azuredevops: skipping work item",
		"work_item_id", workItemID, "reason", reason)
}

// repoKeyFromTags reads the repo-registry key from the work item's System.Tags.
// ADO stores tags as one "; "-separated string; the first `repo:<key>` tag wins,
// and absence returns ok=false so the caller skips rather than guessing a repo.
func repoKeyFromTags(tags string) (string, bool) {
	for _, raw := range strings.Split(tags, ";") {
		t := strings.TrimSpace(raw)
		if key, ok := strings.CutPrefix(t, repoTagPrefix); ok {
			if key = strings.TrimSpace(key); key != "" {
				return key, true
			}
		}
	}
	return "", false
}

// wiqlResult is the WIQL response envelope: a flat query returns only work-item
// references, each an id and its API self-link, never the fields.
type wiqlResult struct {
	WorkItems []wiqlRef `json:"workItems"`
}

type wiqlRef struct {
	ID  int    `json:"id"`
	URL string `json:"url"`
}

// adoWorkItem is the subset of the work-item GET response the adapter maps.
type adoWorkItem struct {
	ID     int       `json:"id"`
	Fields adoFields `json:"fields"`
	URL    string    `json:"url"`
}

// adoFields carries the reference-name-keyed work-item fields. System.AssignedTo
// is an identity reference object whose uniqueName is the assignee's UPN, not a
// bare string, which is why it decodes into a struct.
type adoFields struct {
	Title              string      `json:"System.Title"`
	State              string      `json:"System.State"`
	Tags               string      `json:"System.Tags"`
	AcceptanceCriteria string      `json:"Microsoft.VSTS.Common.AcceptanceCriteria"`
	AssignedTo         adoIdentity `json:"System.AssignedTo"`
}

type adoIdentity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName"`
}
