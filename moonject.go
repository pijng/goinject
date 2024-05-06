package moonject

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/dave/dst"
	"github.com/dave/dst/decorator"
	"github.com/dave/dst/decorator/resolver/goast"
	"github.com/dave/dst/decorator/resolver/guess"
)

type Modifier interface {
	Modify(*dst.File) *dst.File
}

// How to use this code:
//
//  1. Compile it somewhere
//
//     go build injector.go
//
//  2. Then build your code, while specifying the path to the compiled injector (this file) as an argument to the toolexec flag:
//
//     go build -a -toolexec=absolute/path/to/injector/binary main.go
//
// IMPORTANT: pay attention to the -a flag in the above command.
// It is required to call compilation of all project files.
// Otherwise go compiler will not compile files that have not been changed
// since the last compilation. So if you make changes to injector.go and then try
// to call it as a preprocessor when compiling your project code, the go compiler
// may not make the changes you need if you have not changed the project code since the last compilation.
//
// Generalized description of the approach to preprocessing go code:
//  1. Check if we are at the right stage of compilation;
//  2. If not, run the original command and return;
//  3. Find the file that go is about to compile;
//  4. Make the changes we need to make to the AST of this file (this won't affect the source code);
//  5. Write the modified file to the temporary directory;
//  6. Resolve all missing imports that we added as part of the modification;
//  7. Substitute the path to the original file with the path to our modified file and pass it to the compiler command;
//  8. Run the original command with an already substituted file to be compiled.
func Process(modifier Modifier) {
	// os.Args[1] is the name of the current command called go toolchain: asm/compile/link.
	// os.Args[2:] is command arguments.
	tool, args := os.Args[1], os.Args[2:]

	// We do nothing unless it's a direct file compilation.
	// By checking for -V=full we can avoid redundant steps and just
	// run original command as is to not interfere compiler.
	if args[0] == "-V=full" {
		runCommand(tool, args)
		return
	}

	// We take exactly the last argument from the command arguments
	// as the path/name of the file to be compiled.
	//
	// Go toolchain calls the `go tool compile` command and lists all files
	// designated for compilation at the very end of the argument list.
	// Go compiles the files of the project itself one at a time, so it's safe to
	// pick the last one.
	//
	// When compiling a standard library, several files may be specified in
	// the arguments, but we don't really care about that because we won't process them anyway.
	filePathToCompile := args[len(args)-1]

	isGoFile := filepath.Ext(filePathToCompile) == ".go"
	hasStdFlag := slices.Contains(args, "-std")

	// We skip non .go files and std library files to avoid patching them.
	if !isGoFile || hasStdFlag {
		runCommand(os.Args[1], os.Args[2:])
		return
	}

	// Create a temporary directory to where we will write the modified files.
	// In the future, these files will be substituted for the original ones
	// when the final compilation command is called.
	tmpDir, _ := os.MkdirTemp("", "moontrace")
	defer os.RemoveAll(tmpDir)

	// Retrieve the path of the modified file we want to compile,
	// including it's imports.
	// Read more about imports in [processFile]
	newFilePathToCompile, fileImports, err := processFile(tmpDir, filePathToCompile, modifier)
	if err != nil {
		log.Fatal(err)
	}

	// Retrieve the path to the importcfg file.
	// This file is required for `go tool compile` as `-importcfg <path>` flag
	// to resolve all imports of the compiled file. Our task is to add to this file
	// all missing imports that were added during our modifications.
	// Otherwise a compilation will fail with `could not import: <package> (open : no such file or directory)`
	importCfg, err := importcfgPath(os.Args)
	if err != nil {
		log.Fatal(err)
	}

	// Add all missing packages to importcfg file.
	err = addMissingPkgs(importCfg, fileImports)
	if err != nil {
		log.Fatal(err)
	}

	// Compose a new set of arguments to `go tool compile`.
	// The main task is to replace the path to the file we want
	// to compile (specified by the last argument) with our modified file
	// from the temporary directory.
	newArgs := append(os.Args[:len(os.Args)-1], newFilePathToCompile)

	// Run the the original `go tool compile` command with new arguments
	// to propagate our changes to the compiler.
	runCommand(newArgs[1], newArgs[2:])
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

		packages, err := resolvePkg(pkgName)
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

// addMissingPkgToImportcfg writes a given package to importcfg file, so that
// compiler can resolce them during compile/link process.
func addMissingPkgToImportcfg(importcfgPath string, pkgName string, pkgPath string) error {
	file, err := os.OpenFile(importcfgPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("Error opening file: %w", err)
	}
	defer file.Close()

	content := fmt.Sprintf("packagefile %s=%s", pkgName, pkgPath)
	if _, err := file.WriteString(content); err != nil {
		return fmt.Errorf("Error appending content to file: %w", err)
	}

	return nil
}

// processFile performs all necessary manipulations on a file, including
// parsing its AST, making changes to that AST, and writing the modified AST as
// a new file to a temporary directory.
// processFile returns the path to the modified file, as well as all its relevant imports,
// which we will need when patching importcfg file.
func processFile(tmpDir string, path string, modifier Modifier) (string, []*dst.ImportSpec, error) {
	f, err := dstFile(path)
	if err != nil {
		return "", nil, err
	}

	// Make the necessary changes to the AST file
	f = modifier.Modify(f)

	// NewRestorerWithImports is needed to add imports to the file that
	// are required for the code we injected as part of the modifications.
	// For example, if the original file does not have an import of the "fmt" package,
	// but we added code that uses this package, then
	// NewRestorerWithImports will add "fmt" to the imports list.
	restorer := decorator.NewRestorerWithImports(path, guess.New())

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
	f, err = dstFile(newFileName)
	if err != nil {
		return "", nil, err
	}

	return newFileName, f.Imports, nil
}

// dstFile parses the .go file at the specified path and returns an
// AST node, which we will further modify.
func dstFile(path string) (*dst.File, error) {
	fset := token.NewFileSet()
	astFile, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	dec := decorator.NewDecoratorWithImports(fset, path, goast.New())
	f, err := dec.DecorateFile(astFile)
	if err != nil {
		return nil, err
	}

	return f, err
}

// modifyFileAST is a function that will make all the modifications
// we need in AST before we write the new content to a temporary file,
// which will then be compiled in place of the original.
// This is where we should describe the modification logic.
//
// As an example, we find all the functions in the file and add a call
// to fmt.Prin on the first line of their bodies, but you can do whatever you want
func modifyFileAST(f *dst.File) *dst.File {
	for _, decl := range f.Decls {
		decl, isFunc := decl.(*dst.FuncDecl)
		if !isFunc {
			continue
		}

		span := buildSpan(decl.Name.Name)
		decl.Body.List = append(span, decl.Body.List...)
	}

	return f
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

// Just a simple ast statement that we will substitute into the file as a test.
// This particular function constructs the `fmt.Println("Calling [funName] func")` call.
func buildSpan(funcName string) []dst.Stmt {
	return []dst.Stmt{
		&dst.ExprStmt{
			X: &dst.CallExpr{
				Fun: &dst.Ident{Path: "fmt", Name: "Println"},
				Args: []dst.Expr{
					&dst.BasicLit{Kind: token.STRING, Value: strconv.Quote(fmt.Sprintf("Calling [%s] func", funcName))},
				},
			},
		},
	}
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
