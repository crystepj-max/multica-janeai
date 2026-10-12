package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// createProjectForIdentifierTest creates a project via the API and returns
// its ID and the code the server derived (or accepted).
func createProjectForIdentifierTest(t *testing.T, payload map[string]any) ProjectResponse {
	t.Helper()
	req := newRequest("POST", "/api/projects?workspace_id="+testWorkspaceID, payload)
	w := testutil.Call(t, testHandler.CreateProject, req).Want(http.StatusCreated)
	var project ProjectResponse
	json.NewDecoder(w.Body).Decode(&project)
	t.Cleanup(func() {
		cleanup := newRequest("DELETE", "/api/projects/"+project.ID, nil)
		cleanup = withURLParam(cleanup, "id", project.ID)
		testHandler.DeleteProject(httptest.NewRecorder(), cleanup)
	})
	return project
}

func createIssueForIdentifierTest(t *testing.T, payload map[string]any) IssueResponse {
	t.Helper()
	req := newRequest("POST", "/api/issues?workspace_id="+testWorkspaceID, payload)
	w := testutil.Call(t, testHandler.CreateIssue, req).Want(http.StatusCreated)
	var issue IssueResponse
	json.NewDecoder(w.Body).Decode(&issue)
	t.Cleanup(func() {
		del := newRequest("DELETE", "/api/issues/"+issue.ID, nil)
		del = withURLParam(del, "id", issue.ID)
		testHandler.DeleteIssue(httptest.NewRecorder(), del)
	})
	return issue
}

func getIssueByIdentifier(t *testing.T, id string) *IssueResponse {
	t.Helper()
	req := newRequest("GET", "/api/issues/"+url.PathEscape(id), nil)
	req = withURLParam(req, "id", id)
	w := testutil.Call(t, testHandler.GetIssue, req)
	if w.Code != http.StatusOK {
		return nil
	}
	var got IssueResponse
	json.NewDecoder(w.Body).Decode(&got)
	return &got
}

// TestProjectNumberedIssueIdentifier covers the three-segment identifier
// contract: an issue created inside a project is numbered from that project's
// own sequence, renders "PREFIX-CODE-N", and resolves through the code
// snapshot — including case-insensitively and after the project code is
// renamed. Projectless issues keep the plain workspace sequence, and the
// two-part form must never resolve to a project-numbered issue that happens
// to reuse the number.
func TestProjectNumberedIssueIdentifier(t *testing.T) {
	setWorkspaceIssuePrefixForTest(t, "MUL")

	// Chinese titles derive pinyin-initial codes (擎天柱工程 → QTZ).
	project := createProjectForIdentifierTest(t, map[string]any{"title": "擎天柱工程"})
	if project.Code != "QTZ" {
		t.Fatalf("derived project code = %q; want QTZ", project.Code)
	}

	// Seed the workspace-numbered counterpart deterministically at number 1
	// (the fixture's workspace counter advances across test runs, so an
	// API-created projectless issue would get an arbitrary number and the
	// shared-number ambiguity below would not hold).
	plainID := dbfx.Issue(t, "Workspace-numbered issue", testutil.Cols{
		"workspace_id": testWorkspaceID,
		"creator_type": "member",
		"creator_id":   testUserID,
		"number":       1,
		"status":       "todo",
		"priority":     "medium",
	})
	plainByID := getIssueByIdentifier(t, plainID)
	if plainByID == nil || plainByID.Identifier != "MUL-1" {
		t.Fatalf("projectless issue renders %+v; want MUL-1", plainByID)
	}

	first := createIssueForIdentifierTest(t, map[string]any{
		"title":      "First project issue",
		"project_id": project.ID,
	})
	if first.Identifier != "MUL-QTZ-1" {
		t.Fatalf("project issue identifier = %q; want MUL-QTZ-1", first.Identifier)
	}
	second := createIssueForIdentifierTest(t, map[string]any{
		"title":      "Second project issue",
		"project_id": project.ID,
	})
	if second.Identifier != "MUL-QTZ-2" {
		t.Fatalf("second project issue identifier = %q; want MUL-QTZ-2", second.Identifier)
	}

	// A second project counts independently from 1 (阿尔法计划 → AEF).
	other := createProjectForIdentifierTest(t, map[string]any{"title": "阿尔法计划"})
	if other.Code != "AEF" {
		t.Fatalf("derived project code = %q; want AEF", other.Code)
	}
	otherIssue := createIssueForIdentifierTest(t, map[string]any{
		"title":      "Other project issue",
		"project_id": other.ID,
	})
	if otherIssue.Identifier != "MUL-AEF-1" {
		t.Fatalf("other project issue identifier = %q; want MUL-AEF-1", otherIssue.Identifier)
	}

	// The three-part identifier resolves, case-insensitively too.
	for _, id := range []string{"MUL-QTZ-1", "mul-qtz-1"} {
		got := getIssueByIdentifier(t, id)
		if got == nil || got.ID != first.ID {
			t.Fatalf("GetIssue(%q) resolved %+v; want issue %s", id, got, first.ID)
		}
	}

	// The two-part form with the same number resolves ONLY the
	// workspace-numbered issue — the project issue reusing number 1 must not
	// hijack the legacy identifier.
	got := getIssueByIdentifier(t, "MUL-1")
	if got == nil || got.ID != plainID {
		t.Fatalf("GetIssue(MUL-1) resolved %+v; want projectless issue %s", got, plainID)
	}

	// Wrong code and wrong prefix never resolve.
	for _, missing := range []string{"MUL-ZZZ-1", "MULX-QTZ-1", "OTHER-QTZ-1"} {
		if resolved := getIssueByIdentifier(t, missing); resolved != nil {
			t.Fatalf("GetIssue(%q) unexpectedly resolved to %s", missing, resolved.ID)
		}
	}

	// Renaming the project code affects only issues created afterwards; the
	// issued snapshot keeps resolving under the old code.
	req := newRequest("PUT", "/api/projects/"+project.ID+"?workspace_id="+testWorkspaceID, map[string]any{"code": "APP"})
	req = withURLParam(req, "id", project.ID)
	w := testutil.Call(t, testHandler.UpdateProject, req).Want(http.StatusOK)
	var renamed ProjectResponse
	json.NewDecoder(w.Body).Decode(&renamed)
	if renamed.Code != "APP" {
		t.Fatalf("renamed project code = %q; want APP", renamed.Code)
	}

	renamedIssue := createIssueForIdentifierTest(t, map[string]any{
		"title":      "Issue after rename",
		"project_id": project.ID,
	})
	if renamedIssue.Identifier != "MUL-APP-3" {
		t.Fatalf("post-rename identifier = %q; want MUL-APP-3", renamedIssue.Identifier)
	}
	if got := getIssueByIdentifier(t, "MUL-QTZ-1"); got == nil || got.ID != first.ID {
		t.Fatalf("old code snapshot stopped resolving after rename: %+v", got)
	}
	if got := getIssueByIdentifier(t, "MUL-QTZ-2"); got == nil || got.ID != second.ID {
		t.Fatalf("old code snapshot stopped resolving after rename: %+v", got)
	}

	// An explicit code that collides with another project is a 409, and an
	// invalid one is a 400.
	dupReq := newRequest("POST", "/api/projects?workspace_id="+testWorkspaceID, map[string]any{
		"title": "Another project",
		"code":  "app",
	})
	testutil.Call(t, testHandler.CreateProject, dupReq).Want(http.StatusConflict)
	badReq := newRequest("POST", "/api/projects?workspace_id="+testWorkspaceID, map[string]any{
		"title": "Bad code project",
		"code":  "TOOLONGCODE123",
	})
	testutil.Call(t, testHandler.CreateProject, badReq).Want(http.StatusBadRequest)
}

// TestLegacyProjectLazyCodeDerivation covers projects created before project
// codes existed (code = ”): their first project issue derives and persists a
// code inside the create transaction, and later issues reuse it.
func TestLegacyProjectLazyCodeDerivation(t *testing.T) {
	setWorkspaceIssuePrefixForTest(t, "MUL")

	projectID := dbfx.Project(t, "客户管理系统")
	var code string
	dbfx.QueryRow(t, `SELECT code FROM project WHERE id = $1`, projectID).Scan(&code)
	if code != "" {
		t.Fatalf("legacy project code = %q; want empty before first issue", code)
	}

	issue := createIssueForIdentifierTest(t, map[string]any{
		"title":      "First issue in legacy project",
		"project_id": projectID,
	})
	// 客户管理 → KHG.
	if !strings.HasPrefix(issue.Identifier, "MUL-KHG-") {
		t.Fatalf("lazy-derived identifier = %q; want MUL-KHG-N", issue.Identifier)
	}

	dbfx.QueryRow(t, `SELECT code FROM project WHERE id = $1`, projectID).Scan(&code)
	if code != "KHG" {
		t.Fatalf("lazy project code not persisted: %q", code)
	}

	again := createIssueForIdentifierTest(t, map[string]any{
		"title":      "Second issue in legacy project",
		"project_id": projectID,
	})
	if !strings.HasPrefix(again.Identifier, "MUL-KHG-") {
		t.Fatalf("second identifier = %q; want MUL-KHG-N", again.Identifier)
	}
	if again.Identifier == issue.Identifier {
		t.Fatalf("second identifier %q repeats the first project number", again.Identifier)
	}
}

// TestReusedProjectCodeSkipsGhostSnapshotNumbers covers the delete-and-recreate
// path: deleting a project leaves its issues' code snapshots in place, so a
// new project deriving the same code must skip numbers those ghosts hold
// instead of failing the create.
func TestReusedProjectCodeSkipsGhostSnapshotNumbers(t *testing.T) {
	setWorkspaceIssuePrefixForTest(t, "MUL")

	first := createProjectForIdentifierTest(t, map[string]any{"title": "擎天柱工程"})
	ghost := createIssueForIdentifierTest(t, map[string]any{
		"title":      "Ghost holder",
		"project_id": first.ID,
	})
	if ghost.Identifier != "MUL-QTZ-1" {
		t.Fatalf("ghost identifier = %q; want MUL-QTZ-1", ghost.Identifier)
	}

	// Delete the project; the issue survives with its snapshot code.
	del := newRequest("DELETE", "/api/projects/"+first.ID, nil)
	del = withURLParam(del, "id", first.ID)
	testHandler.DeleteProject(httptest.NewRecorder(), del)

	// A new project with the same title derives the same code.
	second := createProjectForIdentifierTest(t, map[string]any{"title": "擎天柱工程"})
	reborn := createIssueForIdentifierTest(t, map[string]any{
		"title":      "Reborn project issue",
		"project_id": second.ID,
	})
	if reborn.Identifier != "MUL-QTZ-2" {
		t.Fatalf("reborn identifier = %q; want MUL-QTZ-2 (skipping the ghost's number)", reborn.Identifier)
	}
}

// TestProjectIssueIdentifierAcrossSurfaces is the regression net for
// render-side query drift: several surfaces hand-write their issue SELECT
// (project board rows, issue list, grouped list, search). Any of them
// omitting the number_code column degrades project issues to "PREFIX-N" even
// though the row stores the code — the board showed MARS-1 while the database
// held MARS-QTZ-1 until this was covered.
func TestProjectIssueIdentifierAcrossSurfaces(t *testing.T) {
	setWorkspaceIssuePrefixForTest(t, "MUL")

	project := createProjectForIdentifierTest(t, map[string]any{"title": "擎天柱工程"})
	created := createIssueForIdentifierTest(t, map[string]any{
		"title":      "Surface render issue",
		"project_id": project.ID,
	})
	if created.Identifier != "MUL-QTZ-1" {
		t.Fatalf("created identifier = %q; want MUL-QTZ-1", created.Identifier)
	}

	// Project board rows — the project page surface (dynamic SELECT).
	groupKey := "status:todo"
	tableW := testutil.Call(t, testHandler.ListIssueTableRows, newRequest("POST", "/api/issues/table/rows", issueTableRowsRequest{
		Query: issueTableQuerySpec{
			Scope: issueTableScope{Kind: "project", ProjectID: project.ID},
			Sort:  issueTableSortRequest{Field: "position", Direction: "asc"},
		},
		Group:     issueTableGroupSpec{Kind: "status"},
		GroupKey:  &groupKey,
		Hierarchy: issueTableHierarchyRequest{Enabled: false},
		Page:      issueTablePageRequest{Limit: 10},
	})).Want(http.StatusOK)
	var tableResponse issueTableRowsResponse
	json.NewDecoder(tableW.Body).Decode(&tableResponse)
	if len(tableResponse.Rows) != 1 || tableResponse.Rows[0].Issue.ID != created.ID ||
		tableResponse.Rows[0].Issue.Identifier != "MUL-QTZ-1" {
		t.Fatalf("table rows = %+v; want issue %s as MUL-QTZ-1", tableResponse.Rows, created.ID)
	}

	// Issue list (dynamic SELECT).
	listW := testutil.Call(t, testHandler.ListIssues, newRequest("GET", "/api/issues?project_id="+project.ID, nil)).Want(http.StatusOK)
	var listResponse struct {
		Issues []IssueResponse `json:"issues"`
	}
	json.NewDecoder(listW.Body).Decode(&listResponse)
	if len(listResponse.Issues) != 1 || listResponse.Issues[0].Identifier != "MUL-QTZ-1" {
		t.Fatalf("list = %+v; want MUL-QTZ-1", listResponse.Issues)
	}

	// Grouped list.
	groupedW := testutil.Call(t, testHandler.ListGroupedIssues, newRequest("GET", "/api/issues/grouped?group_by=assignee", nil)).Want(http.StatusOK)
	var grouped GroupedIssuesResponse
	json.NewDecoder(groupedW.Body).Decode(&grouped)
	found := false
	for _, g := range grouped.Groups {
		for _, gi := range g.Issues {
			if gi.ID == created.ID {
				found = true
				if gi.Identifier != "MUL-QTZ-1" {
					t.Fatalf("grouped identifier = %q; want MUL-QTZ-1", gi.Identifier)
				}
			}
		}
	}
	if !found {
		t.Fatalf("grouped response missing issue %s: %+v", created.ID, grouped)
	}

	// Search.
	searchW := testutil.Call(t, testHandler.SearchIssues, newRequest("GET", "/api/issues/search?q="+url.QueryEscape("Surface render issue"), nil)).Want(http.StatusOK)
	var searchResponse struct {
		Issues []IssueResponse `json:"issues"`
	}
	json.NewDecoder(searchW.Body).Decode(&searchResponse)
	if len(searchResponse.Issues) != 1 || searchResponse.Issues[0].Identifier != "MUL-QTZ-1" {
		t.Fatalf("search = %+v; want MUL-QTZ-1", searchResponse.Issues)
	}
}
