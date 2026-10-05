package web

import (
	"encoding/json"
	"html/template"
)

// Funcs are the template helpers.
var Funcs = template.FuncMap{
	// jsonList emits a []string as a JavaScript array, safely. The values are
	// model names, project paths and agent ids taken from session logs, so they
	// are untrusted: encoding them rather than pasting them keeps a project path
	// containing a quote from breaking the script.
	"jsonList": func(v []string) template.JS {
		if v == nil {
			return template.JS("[]")
		}
		b, err := json.Marshal(v)
		if err != nil {
			return template.JS("[]")
		}
		return template.JS(b)
	},
}
