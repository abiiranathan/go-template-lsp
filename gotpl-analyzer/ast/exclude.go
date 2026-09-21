package ast

import (
	goast "go/ast"
	"go/types"
	"path/filepath"
	"strings"
)

// isPackageExcluded reports whether a call expression belongs to an excluded
// package listed in config.ExcludePackages. It is checked before render/set
// detection so non-template Render methods (e.g. a PDF library with
// Render(string, map)) don't produce false "missing template" errors.
//
// Resolution strategy (best-effort, type-directed with AST fallback):
//  1. Method calls (pkg.Fn / recv.Method): resolve the callee's defining
//     package via types info (method object, receiver named type, or imported
//     package name).
//  2. Plain function calls (Render(...)): resolve the function object's package.
//  3. Fallback when type info is unavailable: compare the selector qualifier
//     (e.g. `pdf` in `pdf.Render(...)`) against the exclude list by name.
func isPackageExcluded(call *goast.CallExpr, info *types.Info, config *AnalysisConfig) bool {
	if config == nil || len(config.ExcludePackages) == 0 || call == nil {
		return false
	}

	excludes := config.ExcludePackages

	switch fn := call.Fun.(type) {
	case *goast.SelectorExpr:
		// Collect candidate (path, name) pairs from type info.
		if info != nil {
			// Method selection: receiver type package + method defining package.
			if sel, ok := info.Selections[fn]; ok && sel != nil {
				if obj := sel.Obj(); obj != nil {
					if pkg := obj.Pkg(); pkg != nil {
						if matchesExcludedPackage(pkg.Path(), pkg.Name(), excludes) {
							return true
						}
					}
				}
				if recv := sel.Recv(); recv != nil {
					if pkgPath, pkgName := namedTypePackage(recv); pkgPath != "" || pkgName != "" {
						if matchesExcludedPackage(pkgPath, pkgName, excludes) {
							return true
						}
					}
				}
			}

			// Function/method object behind the selector.
			if obj := info.ObjectOf(fn.Sel); obj != nil {
				if pkg := obj.Pkg(); pkg != nil {
					if matchesExcludedPackage(pkg.Path(), pkg.Name(), excludes) {
						return true
					}
				}
			}

			// Package qualifier: pkg.Func(...).
			if ident, ok := fn.X.(*goast.Ident); ok {
				if obj := info.ObjectOf(ident); obj != nil {
					if pkgName, ok := obj.(*types.PkgName); ok {
						imported := pkgName.Imported()
						if imported != nil {
							if matchesExcludedPackage(imported.Path(), imported.Name(), excludes) {
								return true
							}
						}
					}
				}
				// Receiver value type: recv.Method(...).
				if tv, ok := info.Types[fn.X]; ok && tv.Type != nil {
					if pkgPath, pkgName := namedTypePackage(tv.Type); pkgPath != "" || pkgName != "" {
						if matchesExcludedPackage(pkgPath, pkgName, excludes) {
							return true
						}
					}
				}
			} else if tv, ok := info.Types[fn.X]; ok && tv.Type != nil {
				if pkgPath, pkgName := namedTypePackage(tv.Type); pkgPath != "" || pkgName != "" {
					if matchesExcludedPackage(pkgPath, pkgName, excludes) {
						return true
					}
				}
			}
		}

		// AST-only fallback: match qualifier name (alias or package name).
		if ident, ok := fn.X.(*goast.Ident); ok {
			if matchesExcludedPackage("", ident.Name, excludes) {
				return true
			}
		}
	case *goast.Ident:
		if info != nil {
			if obj := info.ObjectOf(fn); obj != nil {
				if pkg := obj.Pkg(); pkg != nil {
					if matchesExcludedPackage(pkg.Path(), pkg.Name(), excludes) {
						return true
					}
				}
			}
		}
	}

	return false
}

// namedTypePackage unwraps pointer/alias types to the named type's package.
func namedTypePackage(t types.Type) (path, name string) {
	if t == nil {
		return "", ""
	}
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	if alias, ok := t.(*types.Alias); ok {
		t = types.Unalias(t)
		_ = alias
	}
	named, ok := t.(*types.Named)
	if !ok {
		return "", ""
	}
	obj := named.Obj()
	if obj == nil {
		return "", ""
	}
	if pkg := obj.Pkg(); pkg != nil {
		return pkg.Path(), pkg.Name()
	}
	return "", ""
}

// matchesExcludedCaller reports whether a caller file (path relative to the
// analysis root, e.g. "internal/tui/app.go") matches any entry in the exclude
// list. This covers the case where the user wants to ignore render-like calls
// *located in* certain packages/directories, regardless of which package
// defines the called method (e.g. lipgloss Style.Render calls inside
// internal/tui).
//
// An entry matches when:
//   - it equals the slashified relative file path, or is a directory prefix
//     of it ("internal/tui" matches "internal/tui/app.go");
//   - it equals the file's directory or is a suffix of it, so full import
//     paths work ("example.com/mod/internal/tui" matches dir "internal/tui");
//   - it is a plain name (no "/") equal to any path segment, so bare package
//     dir names work ("tui" matches "internal/tui/app.go").
//
// Empty entries are ignored.
func matchesExcludedCaller(relFile string, excludes []string) bool {
	slash := filepath.ToSlash(relFile)
	if slash == "" {
		return false
	}
	// Directory portion of the caller file ("internal/tui" for
	// "internal/tui/app.go"; "." for root-level files).
	dir := slash
	if i := strings.LastIndex(slash, "/"); i >= 0 {
		dir = slash[:i]
	} else {
		dir = ""
	}

	for _, e := range excludes {
		ex := strings.Trim(filepath.ToSlash(strings.TrimSpace(e)), "/")
		if ex == "" {
			continue
		}
		// Exact file match or directory-prefix match.
		if slash == ex || strings.HasPrefix(slash, ex+"/") {
			return true
		}
		if dir == "" {
			continue
		}
		// Directory match: exact, entry-is-suffix (full import path given),
		// or dir-is-suffix (relative sub-path given, e.g. "tui" vs
		// "internal/tui" handled below via segments).
		if dir == ex || strings.HasSuffix(ex, "/"+dir) || strings.HasSuffix(dir, "/"+ex) {
			return true
		}
		// Plain name matches any single path segment.
		if !strings.Contains(ex, "/") {
			for _, seg := range strings.Split(dir, "/") {
				if seg == ex {
					return true
				}
			}
		}
	}
	return false
}

// matchesExcludedPackage reports whether pkgPath or pkgName matches any entry
// in the exclude list. An entry matches when it equals the path, is a "/"
// -suffixed suffix of the path, or equals the package name. Empty entries are
// ignored. Matching on names is exact to avoid over-exclusion.
func matchesExcludedPackage(pkgPath, pkgName string, excludes []string) bool {
	for _, e := range excludes {
		ex := strings.TrimSpace(e)
		if ex == "" {
			continue
		}
		if pkgPath != "" && (pkgPath == ex || strings.HasSuffix(pkgPath, "/"+ex)) {
			return true
		}
		if pkgName != "" && pkgName == ex {
			return true
		}
	}
	return false
}
