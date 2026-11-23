package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOpenAPIMatchesRoutes holds openapi.yaml, the definition orrery.yaml
// names for the costs and usage APIs, to the /v1 routes server.go
// registers: every route is in it, and it has no other.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	routes := registeredRoutes(t, "server.go")
	documented := documentedRoutes(t, "../../openapi.yaml")
	for _, r := range routes {
		if !slices.Contains(documented, r) {
			t.Errorf("server.go serves %q, which openapi.yaml leaves out: update openapi.yaml", r)
		}
	}
	for _, d := range documented {
		if !slices.Contains(routes, d) {
			t.Errorf("openapi.yaml documents %q, which server.go does not serve: update openapi.yaml", d)
		}
	}
	if len(routes) == 0 {
		t.Fatal("found no /v1 routes in server.go")
	}
}

// TestOrreryNamesOpenAPI holds orrery.yaml, which the forge reader reads
// from the repository, to the files beside it: every API it provides names
// openapi.yaml as its spec, and the logo it names is there.
func TestOrreryNamesOpenAPI(t *testing.T) {
	raw, err := os.ReadFile("../../orrery.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Icon struct {
			File string `yaml:"file"`
		} `yaml:"icon"`
		Provides []struct {
			ID   string `yaml:"id"`
			Spec string `yaml:"spec"`
		} `yaml:"provides"`
	}
	if err := yaml.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Provides) == 0 {
		t.Fatal("orrery.yaml provides no API")
	}
	for _, p := range d.Provides {
		if p.Spec != "openapi.yaml" {
			t.Errorf("orrery.yaml API %q names spec %q, want openapi.yaml", p.ID, p.Spec)
		}
	}
	if _, err := os.Stat("../../" + d.Icon.File); d.Icon.File == "" || err != nil {
		t.Errorf("orrery.yaml names logo %q, which is not in the repository", d.Icon.File)
	}
}

// registeredRoutes is every "METHOD /v1/..." pattern server.go passes as
// the first argument of a call.
func registeredRoutes(t *testing.T, file string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		pattern, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if method, path, ok := strings.Cut(pattern, " "); ok && strings.HasPrefix(path, "/v1/") {
			out = append(out, method+" "+path)
		}
		return true
	})
	slices.Sort(out)
	return out
}

// documentedRoutes is every operation in the OpenAPI file, as
// "METHOD /path".
func documentedRoutes(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	methods := []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}
	var out []string
	for path, item := range doc.Paths {
		for key := range item {
			if slices.Contains(methods, key) {
				out = append(out, strings.ToUpper(key)+" "+path)
			}
		}
	}
	slices.Sort(out)
	return out
}
