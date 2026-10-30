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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	resolver, err := newPackageNameResolver(filesToCompile)
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
	filesImports := make([][]*dst.ImportSpec, len(filesToCompile))
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
	err = addMissingPkgs(importCfg, slices.Concat(filesImports...))
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
// For each import, if it's not already present in importcfg and is not "unsafe",
// it resolves the package path and appends it to importcfg.
func addMissingPkgs(importCfgPath string, fileImports []*dst.ImportSpec) error {
	for _, fileImport := range fileImports {
		pkgName := strings.ReplaceAll(fileImport.Path.Value, `"`, "")
		pkgFound := isPkgInImportCfg(importCfgPath, pkgName)

		if pkgFound {
			continue
		}

		if pkgName == "unsafe" {
			continue
		}

		packages, err := ResolvePkg(pkgName)
		if err != nil {
			return fmt.Errorf("failed resolving packages: %w", err)
		}

		pkgPath, pkgFound := packages[pkgName]
		if !pkgFound {
			return fmt.Errorf("package '%s' not found after resolving", pkgName)
		}

		err = addMissingPkgToImportcfg(importCfgPath, pkgName, pkgPath)
		if err != nil {
			return fmt.Errorf("failed adding pkg '%s' to importcfg: %w", pkgName, err)
		}
	}

	return nil
}

// addMissingPkgToImportcfg appends a packagefile entry to the given importcfg file,
// allowing the Go compiler to resolve the specified package during compilation and linking.
func addMissingPkgToImportcfg(importcfgPath string, pkgName string, pkgPath string) error {
	file, err := os.OpenFile(importcfgPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("error opening file: %w", err)
	}
	defer file.Close()

	content := fmt.Sprintf("packagefile %s=%s\n", pkgName, pkgPath)
	if _, err := file.WriteString(content); err != nil {
		return fmt.Errorf("error appending content to file: %w", err)
	}

	return nil
}

// processFile parses and modifies the AST of a Go source file, writes the updated
// version to a temporary directory, and returns the path to the modified file along
// with its relevant imports (used for patching importcfg).
//
// It automatically manages import resolution and injects missing imports required
// by modifications. It also adds `/*line*/` directives to preserve accurate
// compiler errors and stack traces that refer back to the original file.
func processFile(tmpDir string, path string, modifier Modifier, resolver *packageNameResolver) (string, []*dst.ImportSpec, error) {
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
	output(newFileName, &out)

	// Re-read the modified file to retrieve updated imports.
	// The decorator might not reflect changes to imports made during modification,
	// so reading the file again ensures we capture the correct list.
	f, err = dstFile(newFileName, decorator)
	if err != nil {
		return "", nil, err
	}

	return newFileName, f.Imports, nil
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

// newPackageNameResolver creates a resolver preloaded with the names of all packages imported by files,
// using a single `go list` call. Imports added by modifications are resolved lazily in [ResolvePackage].
func newPackageNameResolver(files []string) (*packageNameResolver, error) {
	fset := token.NewFileSet()
	var importPaths []string
	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}

		for _, spec := range f.Imports {
			importPath := strings.Trim(spec.Path.Value, `"`)
			// "C" is the cgo pseudo-package, unknown to `go list`.
			if importPath != "C" && !slices.Contains(importPaths, importPath) {
				importPaths = append(importPaths, importPath)
			}
		}
	}

	names, err := loadPackageNames(importPaths)
	if err != nil {
		return nil, fmt.Errorf("failed composing packages resolver: %w", err)
	}

	return &packageNameResolver{names: names}, nil
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
	names := make(map[string]string, len(importPaths))
	if len(importPaths) == 0 {
		return names, nil
	}

	args := append([]string{"list", "-e", "-find", "-f", "{{.ImportPath}} {{.Name}}", "--"}, importPaths...)
	cmd := exec.Command("go", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("running %q: %w: %s", cmd.Args, err, stderr.String())
	}

	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		importPath, name, _ := strings.Cut(line, " ")
		if name != "" {
			names[importPath] = name
		}
	}

	return names, nil
}

// ResolvePkg attempts to collect and return the paths to the compiled Go packages
// corresponding to the given package name. It runs the `go list -deps -export -json -- <pkgName>`
// command to retrieve package details, where the `-export` flag is crucial for obtaining
// the actual path to the compiled package by its name.
// Special handling is applied for the "unsafe" package since it doesn't follow the
// standard module export format.
func ResolvePkg(pkgName string) (map[string]string, error) {
	args := []string{"list", "-json", "-deps", "-export", "--", pkgName}

	cmd := exec.Command("go", args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("running %q: %w", cmd.Args, err)
	}

	type listItem struct {
		ImportPath string // The import path of the package
		Export     string // The path to its archive, if any
		BuildID    string // The build ID for the package
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

// isPkgInImportCfg checks if the specified package name is present in the given importcfg file.
// It opens the importcfg file at the provided [importcfgPath], scans its contents,
// and looks for a line matching the pattern `packagefile {pkgName}=`.
func isPkgInImportCfg(importcfgPath string, pkgName string) bool {
	file, err := os.Open(importcfgPath)
	if err != nil {
		return false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	// A typical importcfg file might look like this:
	//
	// # import config
	// packagefile fmt=/var/folders/jt/j7pkdpss14s693b7hgk_d2z00000gs/T/go-build2972637916/b002/_pkg_.a
	//
	// The function returns true if it finds a line with the packagefile for the given package,
	// and false otherwise.
	pattern := fmt.Sprintf("packagefile %s=", pkgName)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, pattern) {
			return true
		}
	}

	return false
}

// output writes the content of [out] to the file by the given [fullName] path.
func output(fullName string, out io.Reader) {
	txt, _ := io.ReadAll(out)

	if _, err := os.Stat(fullName); os.IsNotExist(err) {
		dirPath := filepath.Dir(fullName)

		err := os.MkdirAll(dirPath, os.ModePerm)
		if err != nil {
			panic(err)
		}
	}

	err := os.WriteFile(fullName, txt, 0o644)
	if err != nil {
		panic(err)
	}
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
