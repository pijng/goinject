package goinject

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator"
	"github.com/dave/dst/decorator/resolver/goast"
	"github.com/dave/dst/decorator/resolver/guess"
	"golang.org/x/tools/go/packages"
)

type Modifier interface {
	Modify(*dst.File, *decorator.Decorator, *decorator.Restorer) *dst.File
}

// How to use this library to build you own preprocessor:
//
//  1. Create a new project for your own preprocessor.
//
//  2. Define a struct that will satisfy [Modifier] interface.
//     Modifier has only one method [Modify] that must accept (*dst.File, *decorator.Decorator, *decorator.Restorer) as arguments,
//     and must return a modified *dst.File.
//
//     All the modifications you want to make should be called inside your Modify method.
//
//  3. In a main function of your preprocessor project simply call:
//
//     goinject.Process(YourModifierStruct{})
//
//  4. Build your preprocessor with just a `go build`
//
//  5. Call a newly compiled preprocessor on your target project like this:
//
//     go build -a -toolexec="absolute/path/to/your/preprocessor/binary $PWD" main.go
//
// IMPORTANT: pay attention to the -a flag in the above command.
// It is required to call compilation of all project files.
// Otherwise go compiler will not compile files that have not been changed
// since the last compilation. So if you make changes to injector.go and then try
// to call it as a preprocessor when compiling your project code, the go compiler
// may not make the changes you need if you have not changed the project code since the last compilation.
//
// IMPORTANT: pay attention to $PWD argument in the above command.
// When calling your preprocessor, you must specify the absolute path to the root of the project you want to
// compile as the first argument. If you call go build in the root of the project, it is sufficient to specify $PWD.
//
// Process function represents the generalized approach to preprocessing go code. It:
//  1. Checks if we are at the right stage of compilation;
//  2. If not, runs the original command and return;
//  3. Extract the files that go is about to compile;
//  4. Make the changes to the AST of all the files (this won't affect the source code);
//  5. Writes the modified files to the temporary directory;
//  6. Resolve all missing imports that were added as part of the modification;
//  7. Substitutes the path to the original files with the path to modified files and pass them to the compiler command;
//  8. Runs the original command with an already substituted files to be compiled.
func Process(modifier Modifier) {
	// os.Args[1] is the name of the current command called go toolchain: asm/compile/link.
	// os.Args[2:] is command arguments.
	tool, args := os.Args[2], os.Args[3:]

	// We do nothing unless it's a direct file compilation.
	// By checking for -V=full we can avoid redundant steps and just
	// run original command as is to not interfere compiler.
	if args[0] == "-V=full" {
		runCommand(tool, args)
		return
	}

	toolName := filepath.Base(tool)
	if toolName != "compile" {
		runCommand(tool, args)
		return
	}

	// fmt.Println(os.Args)

	// Extract paths/file names from the command arguments.
	// The files are listed as the last arguments after the -pack flag
	//
	// Go toolchain calls the `go tool compile` command and lists all files
	// designated for compilation at the very end of the argument list.
	//
	// Returns the index after which to specify modified .go files as a second value.
	filesToCompile, goFilesIndex, err := extractFilesFromPack(args)
	if err != nil {
		log.Fatal(err)
	}

	// Create a new set of arguments for `go tool compile`.
	// The main task is to replace the paths to the files we
	// want to compile (specified as last arguments) with our modified
	// files from the temporary directory.
	copiedArgs := make([]string, len(os.Args))
	copy(copiedArgs, os.Args)
	newArgs := copiedArgs[:goFilesIndex]

	pwd := os.Args[1]

	// Go through each file and modify it if it is a project file.
	for _, filePathToCompile := range filesToCompile {
		isGoFile := filepath.Ext(filePathToCompile) == ".go"
		hasStdFlag := slices.Contains(args, "-std")
		projectFile := strings.HasPrefix(filePathToCompile, pwd)

		// We skip non .go files, std library files, and non-project files to avoid patching them.
		if !isGoFile || hasStdFlag || !projectFile {
			runCommand(tool, args)
			return
		}

		// log.Printf("found '%s' file to modify\n", filePathToCompile)

		// Create a temporary directory to where we will write the modified files.
		// In the future, these files will be substituted for the original ones
		// when the final compilation command is called.
		tmpDir, _ := os.MkdirTemp("", "goinject")
		defer os.RemoveAll(tmpDir)

		// Retrieve the path of the modified file we want to compile,
		// including it's imports.
		// Read more about imports in [processFile]
		newFilePathToCompile, fileImports, err := processFile(tmpDir, filePathToCompile, modifier)
		if err != nil {
			log.Fatal(err)
		}

		// log.Printf("file '%s' was modified and got new path: %s \n", filePathToCompile, newFilePathToCompile)

		// Retrieve the path to the importcfg file.
		// This file is required for `go tool compile` as `-importcfg <path>` flag
		// to resolve all imports of the compiled file. Our task is to add to this file
		// all missing imports that were added during our modifications.
		// Otherwise a compilation will fail with `could not import: <package> (open : no such file or directory)`
		importCfg, err := importcfgPath(os.Args)
		if err != nil {
			log.Fatal(err)
		}

		// log.Printf("received importcfg path of '%s' for '%s' file \n", importCfg, newFilePathToCompile)

		// Add all missing packages to importcfg file.
		err = addMissingPkgs(importCfg, fileImports)
		if err != nil {
			log.Fatal(err)
		}

		newArgs = append(newArgs, newFilePathToCompile)
	}

	// Run the the original `go tool compile` command with new arguments
	// to propagate our changes to the compiler.
	runCommand(newArgs[2], newArgs[3:])
}

// extractFilesFromPack extracts all the go files from args.
// Files are specified after a -pack flag.
func extractFilesFromPack(args []string) ([]string, int, error) {
	packIndex := -1
	for i, arg := range args {
		if arg == "-pack" {
			packIndex = i
			break
		}
	}

	if packIndex == -1 {
		return nil, 0, fmt.Errorf("-pack flag is not found")
	}

	var goFiles []string
	for i := packIndex + 1; i < len(args); i++ {
		goFiles = append(goFiles, args[i])
	}

	goFilesIndex := packIndex + 4

	return goFiles, goFilesIndex, nil
}

// addMissingPkgs will go through all passed imports and if the importcfg file
// does not yet contain this package, it will add its declaration as a new line in importcfg.
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

		packages, err := resolvePkg(pkgName)
		if err != nil {
			return fmt.Errorf("failed resolving packages: %w", err)
		}

		pkgPath, pkgFound := packages[pkgName]
		if !pkgFound {
			return fmt.Errorf("package '%s' not found after resolving", pkgName)
		}

		// log.Printf("adding '%s' package to '%s' importcfg\n", pkgName, importCfgPath)

		err = addMissingPkgToImportcfg(importCfgPath, pkgName, pkgPath)
		if err != nil {
			return fmt.Errorf("failed adding pkg '%s' to importcfg: %w", pkgName, err)
		}
	}

	return nil
}

// addMissingPkgToImportcfg writes a given package to importcfg file, so that
// compiler can resolce them during compile/link process.
func addMissingPkgToImportcfg(importcfgPath string, pkgName string, pkgPath string) error {
	file, err := os.OpenFile(importcfgPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("error opening file: %w", err)
	}
	defer file.Close()

	content := fmt.Sprintf("packagefile %s=%s", pkgName, pkgPath)
	if _, err := file.WriteString(content); err != nil {
		return fmt.Errorf("error appending content to file: %w", err)
	}

	return nil
}

// processFile performs all necessary manipulations on a file, including
// parsing its AST, making changes to that AST, and writing the modified AST as
// a new file to a temporary directory.
// processFile returns the path to the modified file, as well as all its relevant imports,
// which we will need when patching importcfg file.
func processFile(tmpDir string, path string, modifier Modifier) (string, []*dst.ImportSpec, error) {
	// Obtain a packages resolver to automatically manage trivial and non-trivial imports.
	resolver, err := packagesResolver(path)
	if err != nil {
		return "", nil, err
	}

	// NewRestorerWithImports is needed to add imports to the file that
	// are required for the code we injected as part of the modifications.
	// For example, if the original file does not have an import of the "fmt" package,
	// but we added code that uses this package, then
	// NewRestorerWithImports will add "fmt" to the imports list.
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
	err = restorer.Fprint(&out, f)
	if err != nil {
		return "", nil, err
	}

	// Write our modified file to the temporary directory we created at the beginning.
	newFileName := tmpDir + string(os.PathSeparator) + filepath.Base(path)
	output(newFileName, &out)

	// Read modified file to retrieve relevant imports.
	// Since apparently it is impossible to see changed imports in
	// the already decorated file. I could be wrong.
	// But explicit rereading definitely works.
	f, err = dstFile(newFileName, decorator)
	if err != nil {
		return "", nil, err
	}

	return newFileName, f.Imports, nil
}

// dstFile parses the .go file at the specified path and returns an
// AST node, which we will further modify.
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

// packagesResolver composes a [guess.RestorerResolver], that can be used in [NewDecoratorWithImports] and
// [NewRestorerWithImports] to automatically manage imports on file AST modifications.
func packagesResolver(path string) (guess.RestorerResolver, error) {
	packagesMap, err := loadPackages(path)
	if err != nil {
		return nil, fmt.Errorf("failed composing packages resolver: %w", err)
	}

	resolver := guess.WithMap(packagesMap)

	return resolver, nil
}

// loadPackages loads all the packages from the path dir to
// resolve non-trivial imports later on.
func loadPackages(path string) (map[string]string, error) {
	loadedPackages, err := packages.Load(&packages.Config{
		// Dir:  filepath.Dir(path),
		Mode: packages.NeedName | packages.NeedImports | packages.NeedTypes},
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

// resolvePkg will try to collect all the named go packages.
// It utilizes `go list -deps -export -json -- <pkgName>` command.
// The most important part here is the -export flag, because it will give us
// the actual path to the compiled package by its name. Then, we can use this path
// as a value when adding missing package to importcfg in form of `packagefile {pkgName}={path}`
func resolvePkg(pkgName string) (map[string]string, error) {
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

// importcfgPath will try to extract the path to the importcfg file from the passed arguments.
// importcfg is usually specified in the following format in the arguments:
//
//	-importcfg $WORK/b0XX/importcfg
//
// Where:
//   - $WORK: temporary directory that the go toolchain itself creates;
//   - b0XX: temporary directory for a certain compiler step
//
// Unfortunately, we have no way of knowing the exact path to $WORK, but apparently we don't need to:
// https://github.com/golang/go/issues/45864
// It is enough just to manipulate the path with $WORK specified at the beginning.
func importcfgPath(args []string) (string, error) {
	for idx := range args {
		if args[idx] != "-importcfg" {
			continue
		}

		return args[idx+1], nil
	}

	return "", fmt.Errorf("failed retrieving importcfg")
}

// isPkgInImportCfg checks whether provided package name already present
// in the given importcfg file.
// isPkgInImportCfg will open an importcfg file by [importcfgPath] path and
// then scan it's content for the pattern `packagefile {pkgName}=`.
func isPkgInImportCfg(importcfgPath string, pkgName string) bool {
	file, err := os.Open(importcfgPath)
	if err != nil {
		return false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	// The typical content of an importcfg file might be as follows:
	//
	// # import config
	// packagefile fmt=/var/folders/jt/j7pkdpss14s693b7hgk_d2z00000gs/T/go-build2972637916/b002/_pkg_.a
	// So our task is to check if there is a line with the packagefile of our package in the file.
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
			log.Fatal(err)
		}
	}

	err := os.WriteFile(fullName, txt, os.ModePerm)
	if err != nil {
		log.Fatal(err)
	}
}

// runCommand executes the provided go toolchain command (with modifier args or not).
func runCommand(tool string, args []string) {
	cmd := exec.Command(tool, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
