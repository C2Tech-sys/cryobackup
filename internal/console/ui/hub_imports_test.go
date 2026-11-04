package ui

import (
	"go/parser"
	gotoken "go/token"
	"strings"
	"testing"
)

// M3-T3 delta L6: the SSE hub does not import the admin API; the Signal type
// both share lives in internal/console/hubsig. (Other ui files use the agent
// download and snippet helpers of adminapi; the hub must stay free of it.)
func TestTheHubFilesDoNotImportTheAdminAPI(t *testing.T) {
	for _, f := range []string{"hub.go", "hub_watch.go"} {
		file, err := parser.ParseFile(gotoken.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		sawSig := false
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasSuffix(path, "/console/adminapi") {
				t.Errorf("%s imports %s", f, path)
			}
			sawSig = sawSig || strings.HasSuffix(path, "/console/hubsig")
		}
		if !sawSig {
			t.Errorf("%s does not use hubsig", f)
		}
	}
}
