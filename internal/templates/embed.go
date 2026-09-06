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
	DBMethodDMLPlain        ID = "db_method_dml_plain"
	ControllerInterfaceFile ID = "controller_interface_file"
	ControllerMethod        ID = "controller_method"
	ControllerMethodTx      ID = "controller_method_tx"
	HandlerInterfaceFile    ID = "handler_interface_file"
	HandlerMethod           ID = "handler_method"
	RouterSnippet           ID = "router_snippet"
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
	DBMethodDMLPlain,
	ControllerInterfaceFile,
	ControllerMethod,
	ControllerMethodTx,
	HandlerInterfaceFile,
	HandlerMethod,
	RouterSnippet,
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
