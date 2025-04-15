package goinject

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
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
	"golang.org/x/tools/go/packages"
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

	toolName := filepath.Base(tool)
	if toolName != "compile" {
		runCommand(tool, args)
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
		runCommand(tool, args)
		return
	}

	// Creates a temporary directory where modified files will be written.
	// These files will later replace the original ones during the final compilation.
	// The temporary directory must be cleaned up after use.
	tmpDir, _ := os.MkdirTemp("", goinject)
	defer os.RemoveAll(tmpDir)
	config.logger.Printf("Created tmp dir: %s", tmpDir)

	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(len(filesToCompile))

	var fileImports []*dst.ImportSpec

	// Modify each file.
	for _, filePathToCompile := range filesToCompile {
		go func() {
			defer wg.Add(-1)

			// Retrieve the path of the modified file we want to compile, including it's imports.
			// Read more about imports in [processFile]
			var newFilePathToCompile string
			newFilePathToCompile, fileImports, err = processFile(tmpDir, filePathToCompile, modifier)
			if err != nil {
				panic(err)
			}
			config.logger.Printf("Code modifications completed for file: %s", filePathToCompile)

			mu.Lock()
			newArgs = append(newArgs, newFilePathToCompile)
			mu.Unlock()
		}()
	}
	wg.Wait()

	// Add all missing packages to importcfg file.
	err = addMissingPkgs(importCfg, fileImports)
	if err != nil {
		panic(err)
	}
	config.logger.Printf("Missing packages added to importcfg file: %s", importCfg)

	// Run the the original `go tool compile` command with new arguments
	// to propagate our changes to the compiler.
	runCommand(newArgs[toolOffset], newArgs[argsOffset:])
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

	hasNonProjectFile := slices.ContainsFunc(files, func(s string) bool {
		return !strings.HasPrefix(s, wd)
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

	filesCount := len(args) - packIndex
	files := make([]string, filesCount)
	goFiles := args[packIndex+1:]
	copy(files, goFiles)

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
// by modifications. It also prepends a `/*line*/` directive to preserve accurate
// stack traces that refer back to the original file.
func processFile(tmpDir string, path string, modifier Modifier) (string, []*dst.ImportSpec, error) {
	// Obtain a packages resolver to automatically manage trivial and non-trivial imports.
	resolver, err := packagesResolver()
	if err != nil {
		return "", nil, err
	}

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

	var out bytes.Buffer

	// Add a /*line */ directive so that stack traces and caller info
	// point to the original source file instead of the generated one.
	// Especially important since the generated file is deleted after compilation.
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

// packagesResolver builds a [guess.RestorerResolver] that can be passed to
// [NewDecoratorWithImports] and [NewRestorerWithImports] to automatically handle
// imports when modifying a file’s AST.
func packagesResolver() (guess.RestorerResolver, error) {
	packagesMap, err := loadPackages()
	if err != nil {
		return nil, fmt.Errorf("failed composing packages resolver: %w", err)
	}

	resolver := guess.WithMap(packagesMap)

	return resolver, nil
}

// loadPackages retrieves all Go packages under the current module using "./...".
// It returns a map of import paths to package names, which is later used
// to resolve non-trivial imports when modifying source files.
func loadPackages() (map[string]string, error) {
	loadedPackages, err := packages.Load(&packages.Config{
		// Dir:  filepath.Dir(path),
		Mode: packages.NeedName | packages.NeedImports | packages.NeedFiles},
		"./...",
	)
	if err != nil {
		return nil, fmt.Errorf("failed loading packages: %w", err)
	}

	pkgs := make(map[string]string)
	for _, loadedPkg := range loadedPackages {
		for _, imp := range loadedPkg.Imports {
			pkgs[imp.PkgPath] = imp.Name
		}
	}

	return pkgs, nil
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

	err := os.WriteFile(fullName, txt, os.ModePerm)
	if err != nil {
		panic(err)
	}
}

// runCommand executes the specified Go toolchain command with the provided arguments.
func runCommand(tool string, args []string) {
	cmd := exec.Command(tool, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// getwd retrieves the working directory of the current Go module by running `go env GOMOD`.
// It checks if the `GOMOD` environment variable is set, which indicates the location of the Go module file.
// If the `GOMOD` variable is valid, it returns the directory containing the Go module file.
// If the `GOMOD` value is empty or invalid, it falls back to using the current working directory
// and returns an error indicating that the module directory could not be determined.
func getwd() (string, error) {
	cmd := exec.Command("go", "env", "GOMOD")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("runnning %q: %w", cmd.Args, err)
	}

	if goMod := strings.TrimSpace(stdout.String()); goMod != "" && goMod != os.DevNull {
		return filepath.Dir(goMod), nil
	}

	wd, _ := os.Getwd()

	return "", fmt.Errorf("in %q: %s", wd, "`go env GOMOD` returned a blank string")
}
