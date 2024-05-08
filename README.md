# Moonject

Moonject is a wrapper library for creating Go preprocessors. It provides functionality to modify Go source code before compilation, enabling users to inject custom logic or make transformations to source files.

## Usage

Moonject allows you to create custom preprocessors by defining a struct that satisfies the `Modifier` interface. This interface has only one method, `Modify`, which accepts a `*dst.File` representing the AST of a Go source file and returns a modified `*dst.File`.

## Process Function

The `moonject.Process` function represents the generalized approach to preprocessing Go code. It performs the following steps:

1. Checks if we are at the right stage of compilation.
2. If not, runs the original command and returns.
3. Extracts the files that Go is about to compile.
4. Makes changes to the AST of all the files (without modifying the source code).
5. Writes the modified files to a temporary directory.
6. Resolves all missing imports that were added as part of the modification.
7. Substitutes the path to the original files with the path to the modified files and passes them to the compiler command.
8. Runs the original command with the substituted files to be compiled.

## Example

Here's an example of how you can use Moonject to modify a Go source file:

```go
package main

import (
	"github.com/pijng/moonject"
)

// CustomModifier implements the Modifier interface
type CustomModifier struct{}

// Modify implements the Modify method of the Modifier interface
func (cm CustomModifier) Modify(f *dst.File) *dst.File {
	// Add custom modification logic here
	return f
}

func main() {
	// Call moonject.Process with an instance of your modifier struct
	moonject.Process(CustomModifier{})
}
```

In this example, `CustomModifier` is a struct that satisfies the `Modifier` interface. It implements the `Modify` method, where you can define your custom modification logic.

## Demonstration

- [moonjectlog](https://github.com/pijng/moonjectlog): `moonjectlog` is a preprocessor that inserts a simple `fmt.Println` statement at the beginning of each function in a Go project. It demonstrates the usage of Moonject for injecting custom logic into source files.
- [go-ifdef](https://github.com/pijng/go-ifdef): `go-ifdef` is a preprocessor that let you use trivial `#ifdef` and `#else` directive based on `GOOS` env variable.
