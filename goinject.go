package goinject

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator"
	"github.com/dave/dst/decorator/resolver/goast"
	"github.com/dave/dst/decorator/resolver/guess"
)

const toolOffset = 1
const argsOffset = 2

const goinject = "goinject"

type Modifier interface {
	Modify(*dst.File, *decorator.Decorator, *decorator.Restorer) *dst.File
}

// How to use this library to create your own preprocessor:
//
//  1. Start a new project for your preprocessor.
//
//  2. Define a struct that implements the [Modifier] interface.
//     The Modifier interface has a single method, [Modify], which accepts the following arguments:
//     (*dst.File, *decorator.Decorator, *decorator.Restorer) and returns a modified *dst.File.
//
//     All modifications should be performed within the Modify method.
//
//  3. In the main function of your preprocessor project, simply invoke:
//
//     goinject.Process(YourModifierStruct{})
//
//  4. Build your preprocessor using `go build`.
//
//  5. To run your preprocessor on a target project, use the following command:
//
//     go build -toolexec="absolute/path/to/your/preprocessor/binary" main.go
//
// The Process function provides a generalized approach to preprocessing Go code. It:
//  1. Verifies if we are at the correct stage of the compilation;
//  2. If not, runs the original command and returns;
//  3. Extracts the files that Go is about to compile;
//  4. Modifies the AST of all files (without altering the source code);
//  5. Writes the modified files to a temporary directory;
//  6. Resolves any missing imports added during the modification;
//  7. Replaces the paths to the original files with the modified file paths, passing them to the compiler;
//  8. Executes the original command with the substituted files for compilation.
func Process(modifier Modifier, opts ...Option) {
	config := &config{
		logger: noopLogger{},
	}
	for _, opt := range opts {
		opt(config)
	}

	// os.Args[toolOffset] is the name of the current command called go toolchain: asm/compile/link.
	// os.Args[argsOffset:] is command arguments.
	tool, args := os.Args[toolOffset], os.Args[argsOffset:]

	// The Go compiler uses the output of the `compile -V=full` command to check if there is an up-to-date version
	// of the current package in the cache, avoiding unnecessary recompilation.
	// Since goinject works with copies of the original files (instead of modifying them directly),
	// the Go compiler assumes that each build command can use the cached packages, as the file contents
	// and their build IDs remain unchanged.
	// To address this, we need to generate a custom hash for the build ID, which we will substitute as the result
	// of `compile -V=full`.
	// The key task is to create a hash by combining the package ID (packageID) with the ID of the current tool
	// invoked with goinject (toolID).
	// This ensures that compilation with `-toolexec` has a distinct cache that doesn't overlap with the
	// cache used in regular compilation.
	if len(args) == 1 && args[0] == "-V=full" {
		if err := alterToolVersion(tool, args); err != nil {
			panic(err)
		}

		return
	}

	// On Windows the toolchain binaries carry an .exe suffix.
	toolName := strings.TrimSuffix(filepath.Base(tool), ".exe")
	if toolName != "compile" {
		exitOnError(runCommand(tool, args))
		return
	}

	// Extracts file paths and names from the command arguments.
	// These files are listed at the end of the argument list, after the `-pack` flag.
	//
	// The Go toolchain calls the `go tool compile` command, and the files designated for compilation
	// are always specified at the end of the argument list.
	filesToCompile, goFilesIndex, err := extractFilesFromPack(args)
	if err != nil {
		panic(err)
	}

	wd, err := getwd()
	if err != nil {
		panic(err)
	}

	// Retrieves the path to the importcfg file, which is required by `go tool compile`
	// through the `-importcfg <path>` flag to resolve all imports during compilation.
	// The goal is to add any missing imports (introduced during modifications) to this file.
	// Failing to do so will result in a compilation error: `could not import: <package> (open : no such file or directory)`.
	importCfg, err := importcfgPath(os.Args)
	if err != nil {
		panic(err)
	}

	// Creates a new set of arguments for `go tool compile`.
	// The main objective is to replace the paths of the original files (specified as the last arguments)
	// with the paths to the modified files from the temporary directory.
	copiedArgs := make([]string, len(os.Args))
	copy(copiedArgs, os.Args)
	newArgs := copiedArgs[:goFilesIndex]

	// Skip preprocessing all non relevant files
	if hasNonRelevantFiles(args, filesToCompile, wd) {
		exitOnError(runCommand(tool, args))
		return
	}

	// Obtain a packages resolver to automatically manage trivial and non-trivial imports.
	// It is created once per package and shared by all files.
	// importcfg lives in $WORK/bNNN/, and $WORK is shared by all invocations of the current build.
	workDir := filepath.Dir(filepath.Dir(importCfg))
	resolver, err := newPackageNameResolver(filesToCompile, workDir, wd)
	if err != nil {
		panic(err)
	}

	// Creates a temporary directory where modified files will be written.
	// These files will later replace the original ones during the final compilation.
	// The temporary directory must be cleaned up after use.
	tmpDir, err := os.MkdirTemp("", goinject)
	if err != nil {
		panic(fmt.Errorf("creating tmp dir: %w", err))
	}
	defer os.RemoveAll(tmpDir)
	config.logger.Printf("Created tmp dir: %s", tmpDir)

	// Every goroutine writes only to its own index, so no locking is needed,
	// and the original file order is preserved for the compiler:
	// package-level variable initialization order across files depends on it.
	newFilePaths := make([]string, len(filesToCompile))
	filesImports := make([][]string, len(filesToCompile))
	errs := make([]error, len(filesToCompile))

	var wg sync.WaitGroup
	wg.Add(len(filesToCompile))

	// Modify each file.
	for i, filePathToCompile := range filesToCompile {
		go func() {
			defer wg.Done()

			// Retrieve the path of the modified file we want to compile, including it's imports.
			// Read more about imports in [processFile]
			newFilePaths[i], filesImports[i], errs[i] = processFile(tmpDir, filePathToCompile, modifier, resolver)
			if errs[i] != nil {
				errs[i] = fmt.Errorf("processing %s: %w", filePathToCompile, errs[i])
				return
			}
			config.logger.Printf("Code modifications completed for file: %s", filePathToCompile)
		}()
	}
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		panic(err)
	}

	newArgs = append(newArgs, newFilePaths...)

	// Add all missing packages to importcfg file.
	err = addMissingPkgs(importCfg, workDir, slices.Concat(filesImports...))
	if err != nil {
		panic(err)
	}
	config.logger.Printf("Missing packages added to importcfg file: %s", importCfg)

	// Run the the original `go tool compile` command with new arguments
	// to propagate our changes to the compiler.
	err = runCommand(newArgs[toolOffset], newArgs[argsOffset:])
	if err != nil {
		// exitOnError terminates the process, so deferred cleanup would not run.
		os.RemoveAll(tmpDir)
		exitOnError(err)
	}
	config.logger.Printf("Package compiled")
}

// hasNonRelevantFiles determines whether any file in the provided list should be ignored.
// A file is considered non-relevant if it meets any of the following criteria:
// - It belongs to the Go standard library.
// - It is not a .go file.
// - It does not originate from the target project.
//
// If any file meets these conditions, the entire collection should be skipped.
func hasNonRelevantFiles(args []string, files []string, wd string) bool {
	hasStdFlag := slices.Contains(args, "-std")
	if hasStdFlag {
		return true
	}

	hasNonGoFile := slices.ContainsFunc(files, func(s string) bool {
		return filepath.Ext(s) != ".go"
	})
	if hasNonGoFile {
		return true
	}

	// Compare against wd with a trailing separator, so that /proj does not match /proj-other.
	projectPrefix := wd + string(filepath.Separator)
	hasNonProjectFile := slices.ContainsFunc(files, func(s string) bool {
		return !strings.HasPrefix(s, projectPrefix)
	})

	return hasNonProjectFile
}

// extractFilesFromPack locates the -pack flag in args, and returns the Go source files listed after it.
// It also returns the index offset at which the Go files begin in the original args slice.
func extractFilesFromPack(args []string) ([]string, int, error) {
	packIndex := slices.Index(args, "-pack")

	if packIndex == -1 {
		return nil, 0, fmt.Errorf("-pack flag is not found")
	}

	goFiles := args[packIndex+1:]

	goFilesIndex := packIndex + argsOffset + 1

	return goFiles, goFilesIndex, nil
}

// addMissingPkgs ensures that all provided import paths are declared in the importcfg file.
// Imports that are neither present in importcfg nor "unsafe" are taken from the export paths shared
// by the other compile invocations of the build (see [buildExportsFile]); the rest are resolved with
// a single `go list` call and shared in turn. All entries are appended to importcfg in a single write.
func addMissingPkgs(importCfgPath, workDir string, importPaths []string) error {
	present, err := importCfgPackages(importCfgPath)
	if err != nil {
		return err
	}

	var missing []string
	for _, importPath := range importPaths {
		if importPath == "unsafe" || present[importPath] || slices.Contains(missing, importPath) {
			continue
		}
		missing = append(missing, importPath)
	}

	if len(missing) == 0 {
		return nil
	}

	exportsPath := filepath.Join(workDir, buildExportsFile)
	exports, err := readSharedExports(exportsPath)
	if err != nil {
		return err
	}

	var unresolved []string
	for _, importPath := range missing {
		if _, ok := exports[importPath]; !ok {
			unresolved = append(unresolved, importPath)
		}
	}

	if len(unresolved) > 0 {
		resolved, err := ResolvePkg(unresolved...)
		if err != nil {
			return fmt.Errorf("failed resolving packages: %w", err)
		}

		var shared strings.Builder
		for _, importPath := range unresolved {
			export, ok := resolved[importPath]
			if !ok {
				return fmt.Errorf("package '%s' not found after resolving", importPath)
			}
			exports[importPath] = export
			fmt.Fprintf(&shared, "%s\t%s\n", importPath, export)
		}

		if err := appendToFile(exportsPath, shared.String()); err != nil {
			return fmt.Errorf("sharing export paths: %w", err)
		}
	}

	var entries strings.Builder
	for _, importPath := range missing {
		fmt.Fprintf(&entries, "packagefile %s=%s\n", importPath, exports[importPath])
	}

	if err := appendToFile(importCfgPath, entries.String()); err != nil {
		return fmt.Errorf("appending to importcfg: %w", err)
	}

	return nil
}

// buildExportsFile is the file in the build's $WORK directory where compile invocations share the export
// data paths of packages added by modifications. Modifications tend to add the same packages everywhere,
// so each of them is resolved with `go list -export` about once per build instead of once per package.
// Within a build a package has a single export path, so entries never conflict.
//
// The file is an append-only log of "importPath\texport\n" lines: every invocation appends the entries
// it resolved with a single write, so concurrent invocations never lose each other's entries.
const buildExportsFile = "goinject-exports"

// readSharedExports reads the export paths shared through [buildExportsFile].
// Only complete lines are used: the last one may still be being appended by another invocation.
func readSharedExports(path string) (map[string]string, error) {
	exports := make(map[string]string)

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return exports, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading shared export paths: %w", err)
	}

	data = data[:bytes.LastIndexByte(data, '\n')+1]
	for _, line := range strings.Split(string(data), "\n") {
		if importPath, export, ok := strings.Cut(line, "\t"); ok {
			exports[importPath] = export
		}
	}

	return exports, nil
}

// appendToFile appends content to the file at path with a single write, creating the file if needed.
func appendToFile(path, content string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}

	_, err = file.WriteString(content)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}

	return err
}

// processFile parses and modifies the AST of a Go source file, writes the updated
// version to a temporary directory, and returns the path to the modified file along
// with its relevant imports (used for patching importcfg).
//
// It automatically manages import resolution and injects missing imports required
// by modifications. It also adds `/*line*/` directives to preserve accurate
// compiler errors and stack traces that refer back to the original file.
func processFile(tmpDir string, path string, modifier Modifier, resolver *packageNameResolver) (string, []string, error) {
	// NewRestorerWithImports ensures newly required imports (e.g., "fmt")
	// are added to the file if they're used in the injected code but
	// missing from the original source.
	restorer := decorator.NewRestorerWithImports(path, resolver)
	decorator := decorator.NewDecoratorWithImports(restorer.Fset, path, goast.WithResolver(resolver))

	f, err := dstFile(path, decorator)
	if err != nil {
		return "", nil, err
	}

	if f == nil {
		return "", nil, fmt.Errorf("received nil dst.File for: %s", path)
	}

	// Make the necessary changes to the AST file
	f = modifier.Modify(f, decorator, restorer)

	// Restoring the file reflows it (added imports, injected code), so positions
	// of the original nodes no longer match the original file without directives.
	addLineDirectives(f, decorator)

	var out bytes.Buffer

	// Add a /*line */ directive so that stack traces and caller info
	// point to the original source file instead of the generated one.
	// Especially important since the generated file is deleted after compilation.
	// It covers the package clause and imports; the rest is covered by [addLineDirectives].
	_, err = out.WriteString(fmt.Sprintf("/*line %s:1:1*/\n", path))
	if err != nil {
		return "", nil, fmt.Errorf("appending line directive: %w", err)
	}

	err = restorer.Fprint(&out, f)
	if err != nil {
		return "", nil, err
	}

	// Write our modified file to the temporary directory we created at the beginning.
	newFileName := tmpDir + string(os.PathSeparator) + filepath.Base(path)
	if err := os.WriteFile(newFileName, out.Bytes(), 0o644); err != nil {
		return "", nil, err
	}

	// Collect the imports of the resulting code: the decorator does not reflect imports
	// added by the restorer, so they are read from the printed output.
	importPaths, err := fileImportPaths(newFileName, out.Bytes())
	if err != nil {
		return "", nil, err
	}

	return newFileName, importPaths, nil
}

// fileImportPaths parses only the import declarations of a Go source file and returns their paths.
// src is handled as in [parser.ParseFile]: if it is nil, the file is read from path.
func fileImportPaths(path string, src any) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}

	importPaths := make([]string, 0, len(f.Imports))
	for _, spec := range f.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("parsing import %s in %s: %w", spec.Path.Value, path, err)
		}
		importPaths = append(importPaths, importPath)
	}

	return importPaths, nil
}

// addLineDirectives prefixes every top-level declaration and every statement of a block or
// case body that comes from the original source with a line directive pointing at its
// original position. Code injected by the modifier has no original position and inherits the
// position of the nearest preceding directive, so it is attributed to the place where it was injected.
func addLineDirectives(f *dst.File, dec *decorator.Decorator) {
	for _, decl := range f.Decls {
		pos, ok := originalPosition(decl, dec)
		if !ok {
			continue
		}

		decs := decl.Decorations()
		if decs.Before == dst.None {
			decs.Before = dst.NewLine
		}
		// Top-level declarations start at column 1, where the //line form is valid:
		// it sets the position of the first character of the next line.
		// Appended after existing start decorations (e.g. doc comments), so it immediately precedes the declaration.
		decs.Start.Append(fmt.Sprintf("//line %s:%d:%d", pos.Filename, pos.Line, pos.Column))
	}

	dst.Inspect(f, func(n dst.Node) bool {
		switch n := n.(type) {
		case *dst.BlockStmt:
			addStmtLineDirectives(n.List, dec)
		case *dst.CaseClause:
			addStmtLineDirectives(n.Body, dec)
		case *dst.CommClause:
			addStmtLineDirectives(n.Body, dec)
		}

		return true
	})
}

// addStmtLineDirectives prefixes statements with a `/*line*/` directive, which, unlike `//line`,
// is valid in indented code: it sets the position of the character right after the comment.
func addStmtLineDirectives(stmts []dst.Stmt, dec *decorator.Decorator) {
	for _, stmt := range stmts {
		pos, ok := originalPosition(stmt, dec)
		if !ok {
			continue
		}

		decs := stmt.Decorations()
		// Without a line break before the statement, the printer moves the comment to the end
		// of the previous line, where the directive would describe the line break instead.
		if decs.Before == dst.None {
			decs.Before = dst.NewLine
		}

		// The printer separates the comment from the statement with a single space,
		// so the directive describes that space, one column before the statement.
		directive := fmt.Sprintf("/*line %s:%d:%d*/", pos.Filename, pos.Line, pos.Column-1)
		if pos.Column <= 1 {
			// There is no column before the first one: leave the column unknown instead.
			directive = fmt.Sprintf("/*line %s:%d*/", pos.Filename, pos.Line)
		}
		decs.Start.Append(directive)
	}
}

// originalPosition returns the position of n in the original source file.
// Nodes created by the modifier have no original position.
func originalPosition(n dst.Node, dec *decorator.Decorator) (token.Position, bool) {
	astNode, ok := dec.Ast.Nodes[n]
	if !ok {
		return token.Position{}, false
	}

	return dec.Fset.Position(astNode.Pos()), true
}

// dstFile parses the Go source file at the given path and returns a
// decorated dst.File, ready for further AST modifications.
func dstFile(path string, dec *decorator.Decorator) (*dst.File, error) {
	astFile, err := parser.ParseFile(dec.Fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}

	f, err := dec.DecorateFile(astFile)
	if err != nil {
		return nil, err
	}

	return f, err
}

// packageNameResolver resolves import paths to package names for [decorator.NewDecoratorWithImports]
// and [decorator.NewRestorerWithImports]. Names are needed whenever a package name differs from the
// last element of its import path. It is safe for concurrent use.
type packageNameResolver struct {
	mu    sync.Mutex
	names map[string]string
}

// newPackageNameResolver creates a resolver preloaded with the names of the packages of the build
// when they are already available (see [loadBuildPackageNames]). Imports of files missing from that list
// are resolved with a single `go list` call; imports added by modifications are resolved lazily in [ResolvePackage].
func newPackageNameResolver(files []string, workDir, modRoot string) (*packageNameResolver, error) {
	names, err := loadBuildPackageNames(workDir, modRoot)
	if err != nil {
		return nil, fmt.Errorf("failed composing packages resolver: %w", err)
	}
	if names == nil {
		names = make(map[string]string)
	}

	var missing []string
	for _, file := range files {
		importPaths, err := fileImportPaths(file, nil)
		if err != nil {
			return nil, err
		}

		for _, importPath := range importPaths {
			// "C" is the cgo pseudo-package, unknown to `go list`.
			if _, ok := names[importPath]; ok || importPath == "C" || slices.Contains(missing, importPath) {
				continue
			}
			missing = append(missing, importPath)
		}
	}

	missingNames, err := loadPackageNames(missing)
	if err != nil {
		return nil, fmt.Errorf("failed composing packages resolver: %w", err)
	}
	maps.Copy(names, missingNames)

	return &packageNameResolver{names: names}, nil
}

// buildPackageNamesFile is the file in the build's $WORK directory that holds the names of the packages
// of the main module and their dependencies. $WORK is created for every `go build` invocation and removed
// after it, so the list is shared by all compile invocations of a build and never outlives it.
const buildPackageNamesFile = "goinject-pkgnames"

// packageNameFormat is the `go list -f` template parsed by [parsePackageNames].
// Test variants have import paths like "p [p.test]", hence the tab separator.
const packageNameFormat = "{{.ImportPath}}\t{{.Name}}"

// loadBuildPackageNames returns the names of all packages of the main module, including their tests,
// and of all their dependencies, shared by the compile invocations of a build through workDir.
//
// Only the first invocation lists them, and publishes the list atomically, so readers see either
// no list or the complete one. Invocations running concurrently with it do not wait: they get nil and
// resolve their own imports, which is cheaper than waiting for the list of the whole module.
// Later invocations, which are the majority, read the list instead of running `go list`.
func loadBuildPackageNames(workDir, modRoot string) (map[string]string, error) {
	namesPath := filepath.Join(workDir, buildPackageNamesFile)
	if data, err := os.ReadFile(namesPath); err == nil {
		return parsePackageNames(data), nil
	}

	// The lock is never released: it only elects the single invocation that lists the packages.
	// If that invocation fails, the others keep resolving their own imports.
	lock, err := os.OpenFile(namesPath+".lock", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lock.Close()

	data, err := goList(modRoot, "-e", "-deps", "-test", "-f", packageNameFormat, "./...")
	if err != nil {
		return nil, err
	}

	if err := writeFileAtomic(namesPath, data); err != nil {
		return nil, fmt.Errorf("sharing package names: %w", err)
	}

	return parsePackageNames(data), nil
}

// writeFileAtomic writes data to a temporary file next to path and renames it to path,
// so concurrent readers see either no file or the complete one.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		// On Windows renaming over an existing file fails: another invocation already published the list.
		if _, statErr := os.Stat(path); statErr == nil {
			return nil
		}
		return err
	}

	return nil
}

// ResolvePackage returns the name of the package with the given import path.
// Packages unknown to `go list` fall back to guessing the name from the import path.
func (r *packageNameResolver) ResolvePackage(importPath string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if name, ok := r.names[importPath]; ok {
		return name, nil
	}

	names, err := loadPackageNames([]string{importPath})
	if err != nil {
		return "", err
	}

	name, ok := names[importPath]
	if !ok {
		name, err = guess.New().ResolvePackage(importPath)
		if err != nil {
			return "", err
		}
	}
	r.names[importPath] = name

	return name, nil
}

// loadPackageNames returns a map of import paths to package names.
// `go list -find` only locates the packages, without resolving their dependencies, which keeps it cheap.
// Packages that cannot be found are omitted.
func loadPackageNames(importPaths []string) (map[string]string, error) {
	if len(importPaths) == 0 {
		return make(map[string]string), nil
	}

	args := append([]string{"-e", "-find", "-f", packageNameFormat, "--"}, importPaths...)
	data, err := goList("", args...)
	if err != nil {
		return nil, err
	}

	return parsePackageNames(data), nil
}

// parsePackageNames parses `go list` output produced with [packageNameFormat].
// Packages without a name (not found or broken) are omitted.
func parsePackageNames(data []byte) map[string]string {
	names := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		importPath, name, _ := strings.Cut(line, "\t")
		if name != "" {
			names[importPath] = name
		}
	}

	return names
}

// goList runs `go list` with args in dir (the current directory if empty) and returns its stdout.
func goList(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("running %q: %w: %s", cmd.Args, err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// ResolvePkg attempts to collect and return the paths to the compiled Go packages
// corresponding to the given package names and all their dependencies. It runs a single
// `go list -deps -export -json -- <pkgNames...>` command to retrieve package details, where the `-export`
// flag is crucial for obtaining the actual path to the compiled package by its name.
// Special handling is applied for the "unsafe" package since it doesn't follow the
// standard module export format.
func ResolvePkg(pkgNames ...string) (map[string]string, error) {
	// Only the decoded fields are requested: full `go list -json` output is an order of magnitude larger.
	args := append([]string{"list", "-json=ImportPath,Export,Standard", "-deps", "-export", "--"}, pkgNames...)

	cmd := exec.Command("go", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("running %q: %w", cmd.Args, err)
	}

	type listItem struct {
		ImportPath string // The import path of the package
		Export     string // The path to its archive, if any
		Standard   bool   // Whether this is from the standard library
	}
	var items []listItem

	dec := json.NewDecoder(&stdout)
	for {
		var item listItem
		if err := dec.Decode(&item); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("parsing `go list` output: %w", err)
		}
		items = append(items, item)
	}

	output := make(map[string]string, len(items))
	for _, item := range items {
		if item.Standard && item.ImportPath == "unsafe" && item.Export == "" {
			// Special-casing "unsafe", because it's not provided like other modules
			continue
		}
		if item.Export == "" {
			continue
		}
		output[item.ImportPath] = item.Export
	}

	return output, nil
}

// importcfgPath extracts the path to the importcfg file from the provided arguments.
// The importcfg file is typically specified using the `-importcfg` flag in the following format:
//
//	-importcfg $WORK/b0XX/importcfg
//
// Where:
//   - $WORK is a temporary directory created by the Go toolchain;
//   - b0XX is a temporary directory for a specific compiler step.
//
// Although the exact $WORK path is not known (see: https://github.com/golang/go/issues/45864),
// it's sufficient to manipulate the path starting with $WORK, as the Go toolchain handles this internally.
func importcfgPath(args []string) (string, error) {
	for idx := range args {
		if args[idx] != "-importcfg" {
			continue
		}

		return args[idx+1], nil
	}

	return "", fmt.Errorf("failed retrieving importcfg")
}

// importCfgPackages returns the set of packages declared in the given importcfg file.
// A typical importcfg file looks like this:
//
//	# import config
//	packagefile fmt=/var/folders/jt/j7pkdpss14s693b7hgk_d2z00000gs/T/go-build2972637916/b002/_pkg_.a
func importCfgPackages(importcfgPath string) (map[string]bool, error) {
	file, err := os.Open(importcfgPath)
	if err != nil {
		return nil, fmt.Errorf("error opening importcfg: %w", err)
	}
	defer file.Close()

	packages := make(map[string]bool)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		entry, ok := strings.CutPrefix(scanner.Text(), "packagefile ")
		if !ok {
			continue
		}
		if importPath, _, ok := strings.Cut(entry, "="); ok {
			packages[importPath] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading importcfg: %w", err)
	}

	return packages, nil
}

// runCommand executes the specified Go toolchain command with the provided arguments.
// The command's stdout and stderr are forwarded, so its diagnostics are already printed when it fails.
func runCommand(tool string, args []string) error {
	cmd := exec.Command(tool, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// exitOnError terminates the process if err is not nil.
// A failed toolchain command exits with its own exit code, since it has already reported the problem;
// any other error is printed and exits with code 1.
func exitOnError(err error) {
	if err == nil {
		return
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() > 0 {
		os.Exit(exitErr.ExitCode())
	}

	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// getwd returns the root directory of the main Go module: the nearest directory containing go.mod,
// starting from the current working directory. The Go toolchain runs every tool in the directory
// where `go build` was invoked, so this is the module being built, same as reported by `go env GOMOD`,
// without spawning a `go` process for every tool invocation.
func getwd() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting working directory: %w", err)
	}

	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}

		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("in %q: go.mod not found", wd)
		}
	}
}
