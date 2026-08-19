package ast

import (
	goast "go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// extFileCache caches parsed source files that live outside the analyzed
// project (stdlib and module dependencies) so their doc comments are resolved
// once per file instead of once per type extraction.
var (
	extFileCacheMu sync.Mutex
	extFileCache   = map[string]*parsedExternalFile{}

	gomodCacheOnce sync.Once
	gomodCachePath string
)

// parsedExternalFile holds a parsed external source file together with the
// fset used to parse it, so declaration lines can be looked up by position.
type parsedExternalFile struct {
	file *goast.File
	fset *token.FileSet
}

// resolveGoSourcePath converts the virtual path prefixes emitted by go/packages
// into real filesystem paths. go list -compiled reports stdlib and module
// cache files as $GOROOT/... and $GOMODCACHE/... respectively.
func resolveGoSourcePath(filename string) string {
	switch {
	case strings.HasPrefix(filename, "$GOROOT/"):
		if root := runtime.GOROOT(); root != "" {
			return filepath.Join(root, filename[len("$GOROOT/"):])
		}
	case strings.HasPrefix(filename, "$GOMODCACHE/"):
		if mc := gomodcache(); mc != "" {
			return filepath.Join(mc, filename[len("$GOMODCACHE/"):])
		}
	}
	return filename
}

// gomodcache returns the module cache directory, resolved once via `go env`.
func gomodcache() string {
	gomodCacheOnce.Do(func() {
		if v := os.Getenv("GOMODCACHE"); v != "" {
			gomodCachePath = v
			return
		}
		if out, err := exec.Command("go", "env", "GOMODCACHE").Output(); err == nil {
			gomodCachePath = strings.TrimSpace(string(out))
		}
	})
	return gomodCachePath
}

// parseExternalFile reads and parses (with comments) the source file at the
// given path, caching the result. Returns nil when the file cannot be read.
func parseExternalFile(filename string) *parsedExternalFile {
	realPath := resolveGoSourcePath(filename)
	if realPath == "" {
		return nil
	}

	extFileCacheMu.Lock()
	defer extFileCacheMu.Unlock()

	if pf, ok := extFileCache[realPath]; ok {
		return pf
	}

	src, err := os.ReadFile(realPath)
	if err != nil {
		extFileCache[realPath] = nil
		return nil
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, realPath, src, parser.ParseComments)
	if err != nil {
		extFileCache[realPath] = nil
		return nil
	}

	pf := &parsedExternalFile{file: file, fset: fset}
	extFileCache[realPath] = pf
	return pf
}

// funcDocAt returns the doc comment of the function or method declared at the
// given line in filename, or "" when no doc comment exists or the file cannot
// be resolved. It works for external (stdlib / module dependency) files whose
// source is not part of the analyzed project's filesMap.
func funcDocAt(filename string, line int) string {
	if filename == "" || line <= 0 {
		return ""
	}

	pf := parseExternalFile(filename)
	if pf == nil {
		return ""
	}

	for _, decl := range pf.file.Decls {
		fd, ok := decl.(*goast.FuncDecl)
		if !ok {
			continue
		}
		if pf.fset.Position(fd.Pos()).Line != line {
			continue
		}
		if fd.Doc != nil {
			return strings.TrimSpace(fd.Doc.Text())
		}
		return ""
	}
	return ""
}

// typeDocAt returns the doc comment of the type declared at the given line in
// filename, or "" when no doc comment exists or the file cannot be resolved.
// It handles grouped type declarations (type ( Foo struct{} )) by falling back
// to the enclosing GenDecl's doc comment.
func typeDocAt(filename string, line int) string {
	if filename == "" || line <= 0 {
		return ""
	}

	pf := parseExternalFile(filename)
	if pf == nil {
		return ""
	}

	var found *goast.TypeSpec
	goast.Inspect(pf.file, func(n goast.Node) bool {
		typeSpec, ok := n.(*goast.TypeSpec)
		if !ok {
			return true
		}

		if pf.fset.Position(typeSpec.Pos()).Line != line {
			return true
		}

		found = typeSpec
		return false
	})
	if found == nil {
		return ""
	}

	if found.Doc != nil {
		return strings.TrimSpace(found.Doc.Text())
	}
	if genDecl, ok := findEnclosingGenDecl(pf.file, found); ok && genDecl.Doc != nil {
		return strings.TrimSpace(genDecl.Doc.Text())
	}
	if found.Comment != nil {
		return strings.TrimSpace(found.Comment.Text())
	}
	return ""
}

// InvalidateExternalDocCache clears cached parsed external files. Intended for
// tests that mutate source under a shared cache.
func InvalidateExternalDocCache() {
	extFileCacheMu.Lock()
	clear(extFileCache)
	extFileCacheMu.Unlock()
}
