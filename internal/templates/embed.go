package templates

import (
	"embed"
	"fmt"
	"text/template"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// Version stamps the embedded template set (PRD: templates are versioned data
// files so eval can pin template versions).
const Version = "v1"

// ID identifies one template in the set.
type ID string

const (
	ModelFile               ID = "model_file"
	DBInterfaceFile         ID = "db_interface_file"
	DBMethodSelectMulti     ID = "db_method_select_multi"
	DBMethodSelectSingle    ID = "db_method_select_single"
	DBMethodSelectSingleTx  ID = "db_method_select_single_tx"
	DBMethodInsertTx        ID = "db_method_insert_tx"
	DBMethodUpdateTx        ID = "db_method_update_tx"
	DBMethodDeleteTx        ID = "db_method_delete_tx"
	DBMethodMerge           ID = "db_method_merge"
	DBMethodDMLPlain        ID = "db_method_dml_plain"
	ControllerInterfaceFile ID = "controller_interface_file"
	ControllerMethod        ID = "controller_method"
	ControllerMethodTx      ID = "controller_method_tx"
	HandlerInterfaceFile    ID = "handler_interface_file"
	HandlerMethod           ID = "handler_method"
	RouterSnippet           ID = "router_snippet"

	// Python batch target (PRD-2026-09-08 BP-3): the batchpy emitter's
	// section templates, distilled from the batchExamples reference
	// conversions (pythonEqTux1/2 shapes).
	PyBatchHeader       ID = "py_batch_header"
	PyBatchConst        ID = "py_batch_const"
	PyBatchDALFetch     ID = "py_batch_dal_fetch"
	PyBatchDALDML       ID = "py_batch_dal_dml"
	PyBatchPhase        ID = "py_batch_phase"
	PyBatchEntrypoint   ID = "py_batch_entrypoint"
	PyBatchServiceHead  ID = "py_batch_service_head"
	PyBatchRepoFetchIt  ID = "py_batch_repo_fetch_iterator"
	PyBatchRepoFetchOne ID = "py_batch_repo_fetch_single"
	PyBatchRepoDML      ID = "py_batch_repo_dml"
	PyBatchRepoRebuild  ID = "py_batch_repo_rebuild"
	PyBatchServiceShell ID = "py_batch_service_shell"
)

// AllIDs lists every template in the embedded set.
var AllIDs = []ID{
	ModelFile,
	DBInterfaceFile,
	DBMethodSelectMulti,
	DBMethodSelectSingle,
	DBMethodSelectSingleTx,
	DBMethodInsertTx,
	DBMethodUpdateTx,
	DBMethodDeleteTx,
	DBMethodMerge,
	DBMethodDMLPlain,
	ControllerInterfaceFile,
	ControllerMethod,
	ControllerMethodTx,
	HandlerInterfaceFile,
	HandlerMethod,
	RouterSnippet,
	PyBatchHeader,
	PyBatchConst,
	PyBatchDALFetch,
	PyBatchDALDML,
	PyBatchPhase,
	PyBatchEntrypoint,
	PyBatchServiceHead,
	PyBatchRepoFetchIt,
	PyBatchRepoFetchOne,
	PyBatchRepoDML,
	PyBatchRepoRebuild,
	PyBatchServiceShell,
}

func (id ID) path() string { return "templates/" + string(id) + ".tmpl" }

// load parses and returns the template for id.
func load(id ID) (*template.Template, error) {
	if !registered(id) {
		return nil, fmt.Errorf("templates: unknown template id %q", id)
	}
	t, err := template.ParseFS(templateFS, id.path())
	if err != nil {
		return nil, fmt.Errorf("templates: parse %s: %w", id, err)
	}
	return t.Templates()[0], nil
}

func registered(id ID) bool {
	for _, known := range AllIDs {
		if known == id {
			return true
		}
	}
	return false
}
